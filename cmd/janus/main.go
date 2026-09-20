// Command janus runs the corporate API gateway: the OpenAI-compatible proxy,
// the application API, and the web interface, in a single binary.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/httpapi"
	"github.com/torvanis/janus/internal/jobs"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/troubleshoot"
	"github.com/torvanis/janus/internal/updates"
	"github.com/torvanis/janus/web"
)

// shutdownGrace is how long in-flight requests, including streams, may take to
// finish after SIGTERM before the process exits.
const shutdownGrace = 30 * time.Second

// drainMinimum is the floor on the post-listener drain of usage-event and
// quota-ledger writers: even when in-flight streams consumed
// the entire shutdownGrace, the final metering writes still get this long to
// land instead of inheriting an already-expired context.
const drainMinimum = 5 * time.Second

func main() {
	// Container images ship without a shell or curl, so the binary can probe
	// itself for the Docker/Kubernetes health check.
	if len(os.Args) > 1 && os.Args[1] == "--health-probe" {
		if err := healthProbe(); err != nil {
			fmt.Fprintf(os.Stderr, "janus: health probe failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		// Startup problems must be legible to an operator reading container logs.
		fmt.Fprintf(os.Stderr, "janus: %v\n", err)
		os.Exit(1)
	}
}

// healthProbe calls this process's own liveness endpoint.
func healthProbe() error {
	addr := os.Getenv("JANUS_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return probeHealthz(addr, os.Getenv("JANUS_TLS_CERT_PATH") != "")
}

// probeHealthz issues the liveness request over the same scheme the server is
// serving: when JANUS_TLS_CERT_PATH is set the listener is TLS-only, so an
// http:// probe can never pass (regression: the scheme was hardcoded and
// direct-TLS deployments crash-looped on their own liveness probe). The probe
// targets this very process over loopback, so certificate verification proves
// nothing (the serving cert names the public host, not 127.0.0.1) and is
// skipped for this loopback self-probe only.
func probeHealthz(addr string, useTLS bool) error {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	client := &http.Client{Timeout: 5 * time.Second}
	if useTLS {
		scheme = "https"
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	resp, err := client.Get(scheme + "://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	if cfg.DatabaseURLDefaulted {
		// JANUS_DATABASE_URL was unset, so the gateway is about to run on the
		// embedded default. This is deliberate for evaluation, but silently
		// dangerous in production (data lives in one file on one node), so
		// say it once, loudly, before the database opens. An explicitly
		// configured URL — even a sqlite one — never triggers this.
		logger.Warn("embedded SQLite, single-node evaluation mode — set JANUS_DATABASE_URL for PostgreSQL",
			"database_url", cfg.DatabaseURL,
			"detail", "JANUS_DATABASE_URL is unset; data is stored in a single local file and this node is the only writer")
	}
	if cfg.UsageRetentionDays < 30 {
		// Rolling quotas are recomputed from usage_event; the purge job
		// deletes events past the retention window. New rolling rules longer
		// than the retention are refused at creation, but rules created
		// before a retention downgrade would silently undercount — say so.
		logger.Warn("usage retention is shorter than the longest rolling quota window: any existing rolling-window quota longer than the retention silently undercounts",
			"usage_retention_days", cfg.UsageRetentionDays)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := telemetry.New(cfg.BuildVersion, cfg.BuildSHA)

	// Perpetual keys: a build newer than maintenance_until must refuse BEFORE
	// migrations so the previous release restarts cleanly against an
	// unchanged schema. File key first, then the database copy, no Store yet.
	if err := preflightLicense(ctx, cfg); err != nil {
		return err
	}

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetLogger(logger)
	if err := db.EnsureReportQuotaHistory(ctx); err != nil {
		return fmt.Errorf("initialize reporting policy history: %w", err)
	}
	logger.Info("database ready", "engine", string(db.Dialect()))

	licenseMgr := license.NewManager(cfg.LicenseFile, db, nil, logger)
	if err := licenseMgr.Refresh(ctx); err != nil {
		return fmt.Errorf("load license: %w", err)
	}
	go licenseMgr.Run(ctx)
	// Opt-in daily version check (JANUS_UPDATE_CHECK=true; never under
	// JANUS_OFFLINE). Sends version, edition and instance id — nothing else.
	updateInstanceID, _ := db.InstanceID(ctx)
	updater := updates.New(updates.Options{
		Version:    cfg.BuildVersion,
		Edition:    func() string { return string(licenseMgr.State().Edition) },
		InstanceID: updateInstanceID,
		Enabled:    cfg.UpdateCheck,
		Offline:    cfg.Offline,
		Logger:     logger,
	})
	go updater.Run(ctx)
	if id, err := db.InstanceID(ctx); err == nil {
		ls := licenseMgr.State()
		logger.Info("license", "edition", ls.Edition, "status", ls.Status, "seats", ls.Seats, "nodes", ls.Nodes, "instance_id", id, "source", ls.Source)
	}

	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}

	// The shared outbound client (discovery, alert webhooks, OIDC) must trust
	// the JANUS_CA_BUNDLE roots just like the proxy transport does, or every
	// call to an internally-signed endpoint fails with x509 errors.
	httpClient := &http.Client{Timeout: cfg.UpstreamTotalTimeout}
	if tlsCfg := cfg.TLSClientConfig(); tlsCfg != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsCfg
		httpClient.Transport = transport
	}

	alerts := alerting.New(db, alerting.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom,
	}, httpClient, metrics, logger)

	quotaEngine := quota.NewEngine(db)
	quotaEngine.SetNotifier(quotaNotifier(alerts))
	quotaEngine.SetLocalOnly(cfg.LocalOnly)

	discoverySvc := discovery.New(db, cipher, httpClient, metrics, alerts, logger, cfg.DiscoveryInterval)

	var oidcProvider *auth.OIDCProvider
	if !cfg.DevAuthEnabled && !cfg.OIDCEnabled() {
		logger.Info("no identity provider configured: sign-in is by Janus account; the first visit creates the administrator")
	}
	if !cfg.DevAuthEnabled && cfg.OIDCEnabled() {
		startupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		oidcProvider, err = auth.NewOIDCProvider(startupCtx, auth.OIDCConfig{
			ProviderURL:  cfg.OIDCProviderURL,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  cfg.PublicURL + "/auth/callback",
			Scopes:       cfg.OIDCScopes,
			Claims:       auth.ClaimMapping{Email: cfg.OIDCEmailClaim, Name: cfg.OIDCNameClaim, Groups: cfg.OIDCGroupsClaim},
		}, httpClient)
		cancel()
		if err != nil {
			return fmt.Errorf("identity provider is not usable: %w", err)
		}
		// Pending sign-ins live in the database so the OIDC callback can land
		// on any replica, not just the one that started the flow.
		oidcProvider.UseStateStore(auth.NewDBOIDCStateStore(db))
		// Readiness-probe failures against the IdP are logged with
		// the discovery URL and the underlying error so a "connection
		// refused" incident is diagnosable from the deployment logs.
		oidcProvider.UseLogger(logger)
		logger.Info("identity provider ready", "issuer", oidcProvider.Metadata().Issuer)
	} else if cfg.DevAuthEnabled {
		logger.Warn("JANUS_DEV_AUTH is enabled: local sign-in is active and no identity provider is contacted. Do not use this in production.")
	}
	// Admin-configured providers (Business: multi_oidc) are built lazily on
	// first use, so an unreachable one never blocks boot.
	registry := auth.NewRegistry(db, cipher, httpClient, cfg.PublicURL, auth.NewDBOIDCStateStore(db), logger)

	webAssets, err := web.Assets()
	if err != nil {
		logger.Warn("web interface assets unavailable", "detail", err.Error())
	}

	// OTLP trace + metric export, active only when an endpoint is
	// configured. NewOTLP returns nil otherwise, and every call on a nil
	// exporter is a no-op, so nothing else needs to branch on it.
	otlp := telemetry.NewOTLP(telemetry.OTLPConfig{
		Endpoint:       cfg.OTELEndpoint,
		Interval:       cfg.OTELExportInterval,
		ServiceName:    "janus",
		ServiceVersion: cfg.BuildVersion,
		TLSConfig:      cfg.TLSClientConfig(),
	}, metrics, logger)
	if otlp != nil {
		// the otel_enabled feature flag can pause export at runtime
		// while the endpoint stays configured. A broken flag read must not
		// silence telemetry, so errors fail open. (Feature flags — including
		// spend_emphasis for dashboard metric defaults — live in
		// store.DefaultFeatureFlags and are toggled at Admin → System.)
		otlp.SetEnabledFunc(func(ctx context.Context) bool {
			flags, err := db.FeatureFlags(ctx)
			if err != nil {
				return true
			}
			return flags["otel_enabled"]
		})
		logger.Info("OTLP export enabled", "endpoint", cfg.OTELEndpoint, "interval", cfg.OTELExportInterval.String())
	}

	server := &httpapi.Server{
		Config: cfg,
		Store:  db,
		// Built here rather than lazily in Handler() because the instance
		// heartbeat feeds it the replica count before the first request.
		RateLimits: quota.NewRateLimiter(),
		Sessions:   auth.NewDBSessionStore(db, cfg.SessionTTL, cfg.SessionIdleTimeout),
		OIDC:       oidcProvider,
		Registry:   registry,
		Cipher:     cipher,
		Quota:      quotaEngine,
		Metrics:    metrics,
		Trace:      otlp,
		Alerts:     alerts,
		Discovery:  discoverySvc,
		License:    licenseMgr,
		Updates:    updater,
		Logger:     logger,
		WebAssets:  webAssets,
		StartedAt:  time.Now().UTC(),
		// Troubleshooting mode: captures only while an admin has enabled a
		// session; the recorder itself is inert otherwise.
		Troubleshoot: troubleshoot.New(db, cipher, cfg.TroubleshootDir, logger),
	}
	if cfg.TroubleshootDir != "" {
		logger.Info("troubleshooting disk backend available", "dir", cfg.TroubleshootDir)
	}

	purger := jobs.NewPurger(db, quotaEngine, metrics, logger, cfg.UsageRetentionDays, cfg.AuditRetentionDays, cfg.PurgeJobTimeUTC)

	var wg sync.WaitGroup
	discoverySvc.Start(ctx, &wg)
	purger.Start(ctx, &wg)
	jobs.StartQuotaCheckpoint(ctx, &wg, quotaEngine, cfg.QuotaCheckpointEvery, logger)
	jobs.StartTroubleshootRetention(ctx, &wg, server.Troubleshoot, jobs.TroubleshootRetentionInterval, logger)
	jobs.StartSecgwRetention(ctx, &wg, db, jobs.SecgwRetentionInterval, logger)
	jobs.StartInstanceHeartbeat(ctx, &wg, db, server.RateLimits, jobs.NewInstanceID(), logger, jobs.WithLicensedNodes(func() int { return licenseMgr.State().Nodes }))
	jobs.NewReportWorker(db, cfg.LocalOnly, logger).Start(ctx, &wg)
	startActiveUserGauge(ctx, &wg, db, metrics, logger)
	otlp.Start(ctx, &wg)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: streamed completions legitimately run for minutes.
		IdleTimeout: 120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("janus listening",
			"addr", cfg.ListenAddr, "public_url", cfg.PublicURL,
			"version", cfg.BuildVersion, "tls", cfg.TLSCertPath != "")
		var err error
		if cfg.TLSCertPath != "" {
			err = httpServer.ListenAndServeTLS(cfg.TLSCertPath, cfg.TLSKeyPath)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining in-flight requests", "grace_seconds", int(shutdownGrace.Seconds()))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown incomplete", "error", err.Error())
	}
	// The listener has drained its handlers, but usage-event and quota-ledger
	// writes run in post-response goroutines; wait for them too so the final
	// requests before a deploy are never lost from metering.
	// Shutdown above may have consumed the whole grace period on long-lived
	// streams, so the drain gets its own floor: whatever remains of the grace
	// period, but never less than drainMinimum — otherwise the writers behind
	// the very requests that exhausted the grace period would be abandoned
	// with an already-expired context.
	drainBudget := drainMinimum
	if deadline, ok := shutdownCtx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > drainBudget {
			drainBudget = remaining
		}
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainBudget)
	defer cancelDrain()
	if err := server.Drain(drainCtx); err != nil {
		logger.Error("background usage writers did not finish before the grace period", "error", err.Error())
	}
	wg.Wait()
	logger.Info("janus stopped")
	return nil
}

// quotaNotifier turns a threshold crossing into an alert with copy that tells
// the recipient exactly what happened and when access resumes.
func quotaNotifier(alerts *alerting.Dispatcher) quota.Notifier {
	return func(ctx context.Context, ev quota.ThresholdEvent) {
		// Custom per-rule thresholds map onto the nearest trigger
		// tier: anything at or above 95% is a critical-warning-tier alert,
		// anything below is a warning-tier alert, and 100% is the breach.
		trigger := alerting.TriggerQuota80
		severity := "warning"
		title := fmt.Sprintf("Quota at %d%%: %s %s", ev.Threshold, quota.MetricLabel(ev.Quota.Metric), quota.WindowLabel(ev.Quota.Window))
		switch {
		case ev.Threshold >= 100:
			trigger = alerting.TriggerQuotaBreach
			severity = "critical"
			title = fmt.Sprintf("Quota exceeded: %s %s", quota.MetricLabel(ev.Quota.Metric), quota.WindowLabel(ev.Quota.Window))
		case ev.Threshold >= 95:
			trigger = alerting.TriggerQuota95
		}
		body := fmt.Sprintf("Used %d of %d (%s, %s). Access resumes at %s.",
			ev.Current, ev.Limit, quota.MetricLabel(ev.Quota.Metric), quota.WindowLabel(ev.Quota.Window),
			ev.ResetAt.Format(time.RFC3339))

		event := alerting.Event{
			Trigger: trigger, Severity: severity, Title: title, Body: body,
			NotifyAdmins: ev.Threshold >= 100,
			Data: map[string]any{
				"quota_id": ev.Quota.ID, "metric": ev.Quota.Metric, "window": ev.Quota.Window,
				"current": ev.Current, "limit": ev.Limit, "reset_at": ev.ResetAt,
			},
		}
		if ev.Quota.SubjectType == "user" {
			event.UserID = ev.Quota.SubjectID
		} else {
			event.NotifyAdmins = true
		}
		alerts.Dispatch(ctx, event)
	}
}

// startActiveUserGauge keeps the active-user gauge fresh without putting a
// database query on any request path.
func startActiveUserGauge(ctx context.Context, wg *sync.WaitGroup, db *store.Store, metrics *telemetry.Metrics, logger *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := db.ActiveUserCount(ctx, time.Now().UTC().Add(-15*time.Minute))
				if err != nil {
					logger.WarnContext(ctx, "refresh active user gauge", "error", err.Error())
					continue
				}
				metrics.ActiveUsers.Set(float64(n))
			}
		}
	}()
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// preflightLicense applies license.CheckMaintenance to whatever key is
// reachable before the schema is touched. Any read problem is ignored here —
// the real load happens after Open and reports properly.
func preflightLicense(ctx context.Context, cfg *config.Config) error {
	if cfg.BuildDate == "" {
		return nil
	}
	var raw string
	if cfg.LicenseFile != "" {
		if b, err := os.ReadFile(cfg.LicenseFile); err == nil {
			raw = strings.TrimSpace(string(b))
		}
	}
	if raw == "" {
		raw, _ = store.PeekLicenseKey(ctx, cfg.DatabaseURL)
	}
	if raw == "" {
		return nil
	}
	c, err := license.Verify(raw, license.TrustedKeys())
	if err != nil {
		return nil // reported as "invalid" by the manager after Open
	}
	return license.CheckMaintenance(&c, cfg.BuildDate)
}
