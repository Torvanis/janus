// Command janus runs the corporate API gateway: the OpenAI-compatible proxy,
// the application API, and the web interface, in a single binary.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/auditlog"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/httpapi"
	"github.com/torvanis/janus/internal/jobs"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/listenertls"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/revocation"
	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/troubleshoot"
	"github.com/torvanis/janus/internal/truststore"
	"github.com/torvanis/janus/internal/updates"
	"github.com/torvanis/janus/internal/websearch"
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
	if len(os.Args) > 1 && os.Args[1] == "--fips-status" {
		// Evidence for assessors and support: which edition this binary is,
		// which Go Cryptographic Module it links, and whether FIPS 140-3
		// mode is on. Exits non-zero when the FIPS edition would refuse to
		// start.
		st := fipsmode.Current()
		out, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(out))
		if err := fipsmode.CheckStartup(); err != nil {
			fmt.Fprintf(os.Stderr, "janus: %v\n", err)
			os.Exit(1)
		}
		return
	}
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
	if certPath := os.Getenv("JANUS_TLS_CERT_PATH"); certPath != "" {
		return probeHealthz(addr, certPath)
	}
	if v, _ := strconv.ParseBool(os.Getenv("JANUS_TLS_SELF_SIGNED")); v {
		return probeSelfSigned(addr)
	}
	return probeHealthz(addr, "")
}

// probeSelfSigned is the liveness probe for a self-signed listener: the
// certificate lives in the database, which the probe cannot read, so it
// pins the certificate the server presents on this connection and asks
// /healthz over it. It proves the TLS listener is up and answering; it is
// not an identity check (the probe runs inside the pod).
func probeSelfSigned(addr string) error {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		//nolint:gosec // in-pod liveness probe of our own self-signed listener; see the comment above
		InsecureSkipVerify: true,
	}}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// probeHealthz issues the liveness request over the same scheme the server is
// serving: when JANUS_TLS_CERT_PATH is set the listener is TLS-only, so an
// http:// probe can never pass (regression: the scheme was hardcoded and
// direct-TLS deployments crash-looped on their own liveness probe).
//
// Over TLS the probe verifies the server: the only trust anchors are the
// certificates in the server's own JANUS_TLS_CERT_PATH file, and the expected
// name is taken from that certificate's subjectAltName, so a different
// process answering on the port fails the probe. Certificate expiry is not a
// liveness question (an expired certificate must not crash-loop the pod; it is
// for monitoring to report), so the check runs at a moment inside the
// certificate's validity window when the clock has left it. A certificate with
// no subjectAltName cannot be verified by Go's TLS stack at all; for those
// the probe only checks that the listener accepts connections.
func probeHealthz(addr, certPath string) error {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	client := &http.Client{Timeout: 5 * time.Second}
	if certPath != "" {
		scheme = "https"
		tlsCfg, err := selfProbeTLSConfig(certPath)
		if errors.Is(err, errNoSAN) {
			conn, derr := net.DialTimeout("tcp", addr, 5*time.Second)
			if derr != nil {
				return derr
			}
			return conn.Close()
		}
		if err != nil {
			return err
		}
		client.Transport = &http.Transport{TLSClientConfig: tlsCfg}
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

var errNoSAN = errors.New("serving certificate has no subjectAltName")

// selfProbeTLSConfig builds a verifying client config that trusts exactly the
// certificates in the server's own certificate file.
func selfProbeTLSConfig(certPath string) (*tls.Config, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read serving certificate: %w", err)
	}
	pool := x509.NewCertPool()
	var leaf *x509.Certificate
	for rest := pemBytes; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse serving certificate: %w", err)
		}
		if leaf == nil {
			leaf = cert
		}
		pool.AddCert(cert)
	}
	if leaf == nil {
		return nil, fmt.Errorf("no certificate found in %s", certPath)
	}
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
		if strings.HasPrefix(name, "*.") {
			name = "healthz" + name[1:]
		}
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return nil, errNoSAN
	}
	return &tls.Config{
		RootCAs:    pool,
		ServerName: name,
		MinVersion: tls.VersionTLS12,
		Time: func() time.Time {
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
			}
			return now
		},
	}, nil
}

func run() error {
	// Before anything else: a FIPS edition binary that is not running the
	// validated module in FIPS mode must not serve a single request.
	if err := fipsmode.CheckStartup(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// STIG profile (and the FIPS edition's SQLite refusal): refuse unsafe
	// settings before anything is opened or contacted.
	stigprofile.Set(cfg.STIGProfile)
	if err := stigprofile.Check(cfg); err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	// Revocation checking of outbound TLS (JANUS_TLS_REVOCATION; hard in
	// the STIG profile). Installed before any client connects.
	revMode, _ := revocation.ParseMode(cfg.TLSRevocation)
	revocation.Configure(revocation.Options{Mode: revMode, OCSPResponder: cfg.OCSPResponder, Logger: logger})
	if revMode != revocation.ModeOff {
		logger.Info("tls revocation checking", "mode", revMode, "ocsp_responder", cfg.OCSPResponder,
			"order", "stapled OCSP, OCSP responder, CRL")
	}
	// One outbound trust store: system roots, DoD roots, JANUS_CA_BUNDLE,
	// JANUS_CA_DIR and (once the database is open) uploaded certificates.
	// http.DefaultTransport becomes a trust-store transport, so clients
	// built without one (vendor sign-in, token exchange, web search) verify
	// against the same store and carry the revocation check.
	if _, err := truststore.Configure(truststore.Options{DoD: cfg.TrustDoDRoots, BundleFile: cfg.CABundlePath, Dir: cfg.CADir, Logger: logger}); err != nil {
		return err
	}
	truststore.InstallDefault()
	truststore.SetUntrustedAllowed(stigprofile.Allowed(fipsmode.FeatureUntrustedTLS))
	if cfg.TrustDoDRoots || cfg.CADir != "" || cfg.CABundlePath != "" {
		logger.Info("trust store", "dod_roots", cfg.TrustDoDRoots, "ca_bundle", cfg.CABundlePath, "ca_dir", cfg.CADir,
			"certificates", len(truststore.Current().CAs))
	}
	if cfg.STIGProfile {
		p := stigprofile.Effective(cfg)
		logger.Info("stig profile",
			"enabled", true, "database", p.Database, "transport", p.Transport, "offline", p.Offline,
			"audit_retention_days", p.AuditRetentionDays, "audit_fail_closed", p.AuditFailClosed,
			"totp_algorithm", p.TOTPAlgorithm, "tls_revocation", p.TLSRevocation, "listener_tls_required", p.ListenerTLSRequired,
			"restricted_features", p.Restricted, "deviations", p.Deviations)
	}
	fips := fipsmode.Current()
	restricted := make([]string, 0, len(fips.Restricted))
	for _, r := range fips.Restricted {
		restricted = append(restricted, string(r.Feature))
	}
	logger.Info("cryptography",
		"edition", fips.Edition, "fips140", fips.FIPS140Enabled, "fips140_enforced", fips.FIPS140Enforced,
		"module_version", fips.ModuleVersion, "validated_module", fips.ValidatedModule,
		"cmvp_certificate", fips.CMVPCertificate, "restricted_features", restricted)
	// Chat's web-page reads: the JANUS_WEB_* policy. custom and custom-only
	// trust read the shared trust store above (so DoD roots, mounted and
	// uploaded certificates apply), and every handshake runs the
	// revocation check.
	webPolicy, err := websearch.ParsePagePolicy(os.Getenv, cfg.CABundlePath)
	if err != nil {
		return err
	}
	websearch.SetPagePolicy(webPolicy)
	if s := webPolicy.Summary(); s["default"] != true {
		logger.Info("chat web page policy", "policy", s)
	}
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

	db, err := store.OpenWithPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
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
	// The FIPS edition and the STIG profile are Business features: refuse
	// to serve without a verified Business/Enterprise key. The key comes
	// from JANUS_LICENSE_FILE or the database; no network call is made.
	if err := checkEditionLicense(licenseMgr.State(), logger); err != nil {
		return err
	}
	if err := checkStoredDirectory(ctx, db); err != nil {
		return err
	}
	if err := loadTrustStore(ctx, db, logger); err != nil {
		return err
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
	listenerTLS, err := newListenerTLS(ctx, cfg, db, cipher, logger)
	if err != nil {
		return err
	}

	metrics.SetLicenseExpiry(licenseMgr.State().ExpiresAt)
	licenseSync := license.NewSync(licenseMgr, db, cipher, license.SyncOptions{
		Offline: cfg.Offline, Enabled: cfg.LicenseSync, Token: cfg.LicenseSyncToken, TokenSet: cfg.LicenseSyncTokenSet,
		Report:  licenseSyncReport(db, cfg.BuildVersion),
		Observe: metrics.ObserveLicenseSync,
	})
	go licenseSync.Run(ctx)

	// The shared outbound client (discovery, alert webhooks, OIDC) uses the
	// trust store like the proxy transport does, so internally signed
	// endpoints verify and trust-store changes apply without a restart.
	httpClient := &http.Client{Timeout: cfg.UpstreamTotalTimeout, Transport: &truststore.Transport{}}

	alerts := alerting.New(db, alerting.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom,
	}, httpClient, metrics, logger)

	quotaEngine := quota.NewEngine(db)
	quotaEngine.SetNotifier(quotaNotifier(alerts))
	quotaEngine.SetLocalOnly(cfg.LocalOnly)
	quotaEngine.SetRuleCacheTTL(cfg.ConfigCacheTTL)

	discoverySvc := discovery.New(db, cipher, httpClient, metrics, alerts, logger, cfg.DiscoveryInterval)

	var oidcProvider *auth.OIDCProvider
	if !cfg.DevAuth() && !cfg.OIDCEnabled() {
		logger.Info("no identity provider configured: sign-in is by Janus account; the first visit creates the administrator")
	}
	if !cfg.DevAuth() && cfg.OIDCEnabled() {
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
	} else if cfg.DevAuth() {
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
		Transport:      &truststore.Transport{Tune: func(t *http.Transport) { t.Proxy = http.ProxyFromEnvironment }},
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

	// Security policy (Administration → Settings → Security). The STIG
	// profile raises whatever is saved to the STIG values on every read.
	store.SetSTIGBaseline(cfg.STIGProfile)
	if cfg.STIGProfile {
		if saved, err := db.StoredSecurityPolicy(ctx); err == nil {
			if gaps := store.STIGFindings(saved); len(gaps) > 0 {
				logger.Warn("stig profile: the saved security settings are weaker than the STIG; the STIG values apply until an administrator saves them",
					"raised", gaps)
			}
		}
	}
	if pol := db.SecurityPolicy(ctx); cfg.STIGProfile || pol.Banner.Enabled || pol.Syslog.Enabled || pol.SmartCard.Enabled {
		logger.Info("security policy",
			"idle_minutes_user", pol.Sessions.IdleMinutesUser, "idle_minutes_admin", pol.Sessions.IdleMinutesAdmin,
			"absolute_minutes", pol.Sessions.AbsoluteMinutes, "sessions_per_user", pol.Sessions.MaxPerUser,
			"banner", pol.Banner.Enabled, "lockout", fmt.Sprintf("%d in %d min, lock %d min (0 = until unlocked)", pol.Lockout.MaxFailures, pol.Lockout.WindowMinutes, pol.Lockout.LockMinutes),
			"password_min_length", pol.Passwords.MinLength, "syslog", pol.Syslog.Enabled, "syslog_address", pol.Syslog.Address,
			"smart_card", pol.SmartCard.Enabled, "smart_card_source", pol.SmartCard.Source)
	}
	// Audit forwarding to syslog / a SIEM: every committed audit record.
	instanceHost, _ := os.Hostname()
	auditFwd := auditlog.New(logger, instanceHost, cfg.BuildVersion, func(ctx context.Context) store.SyslogPolicy {
		return db.SecurityPolicy(ctx).Syslog
	})
	store.SetAuditForwarder(auditFwd.Enqueue)

	server := &httpapi.Server{
		Config: cfg,
		Store:  db,
		// Built here rather than lazily in Handler() because the instance
		// heartbeat feeds it the replica count before the first request.
		RateLimits:  quota.NewRateLimiter(),
		Sessions:    auth.NewDBSessionStore(db, cfg.SessionTTL, cfg.SessionIdleTimeout),
		OIDC:        oidcProvider,
		Registry:    registry,
		Cipher:      cipher,
		Quota:       quotaEngine,
		Metrics:     metrics,
		Trace:       otlp,
		Alerts:      alerts,
		Discovery:   discoverySvc,
		License:     licenseMgr,
		LicenseSync: licenseSync,
		Updates:     updater,
		Logger:      logger,
		WebAssets:   webAssets,
		StartedAt:   time.Now().UTC(),
		// Troubleshooting mode: captures only while an admin has enabled a
		// session; the recorder itself is inert otherwise.
		Troubleshoot: troubleshoot.New(db, cipher, cfg.TroubleshootDir, logger),
		// File storage: database by default, or S3-compatible from
		// JANUS_STORAGE_MODE / the admin setting. Objects are encrypted
		// with the gateway key before they reach either backend.
		Blobs:          blobstore.NewManager(db, cipher, httpapi.StorageEnvConfig(cfg), cfg.StorageMaxObjectBytes),
		ListenerTLS:    listenerTLS,
		AuditForwarder: auditFwd,
	}
	// The license gauge and expiry alerts follow every refresh, including an
	// admin installing or removing a key.
	licenseMgr.OnRefresh(server.OnLicenseState)
	server.OnLicenseState(licenseMgr.State())
	if cfg.StorageMode != "" {
		logger.Info("file storage set by the deployment", "mode", cfg.StorageMode, "endpoint", cfg.S3Endpoint, "bucket", cfg.S3Bucket)
	}
	if cfg.TroubleshootDir != "" {
		logger.Info("troubleshooting disk backend available", "dir", cfg.TroubleshootDir)
	}
	// A SearXNG installed with the deployment turns Chat web search on.
	server.ApplyDeploymentChatSearch(ctx)
	perf := httpapi.DecidePerformanceMode(cfg.PerformanceMode, licenseMgr.State())
	server.StartPerformanceMode(perf)
	switch {
	case perf.Active:
		logger.Warn("performance mode is on: usage is written in batches and committed without waiting for the disk flush; a database crash can lose about the last 0.5 s of usage records and a killed replica up to 100 ms",
			"db_max_conns", cfg.DBMaxConns)
	case perf.Requested:
		logger.Warn("performance mode requested but not enabled", "reason", perf.Reason)
	}

	purger := jobs.NewPurger(db, quotaEngine, metrics, logger, cfg.UsageRetentionDays, cfg.AuditRetentionDays, cfg.PurgeJobTimeUTC)
	purger.ContentPurge = server.PurgeHeldContent

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		auditFwd.Run(ctx)
	}()
	discoverySvc.Start(ctx, &wg)
	purger.Start(ctx, &wg)
	jobs.StartQuotaCheckpoint(ctx, &wg, quotaEngine, cfg.QuotaCheckpointEvery, logger)
	jobs.StartTroubleshootRetention(ctx, &wg, server.Troubleshoot, jobs.TroubleshootRetentionInterval, logger)
	jobs.StartSecgwRetention(ctx, &wg, db, jobs.SecgwRetentionInterval, logger)
	jobs.StartInstanceHeartbeat(ctx, &wg, db, server.RateLimits, jobs.NewInstanceID(), logger, jobs.WithLicensedNodes(func() int { return licenseMgr.State().Nodes }))
	server.StartSubscriptionChecks(ctx, &wg)
	jobs.NewReportWorker(db, cfg.LocalOnly, logger).Start(ctx, &wg)
	// Load-balanced model pools: probe pooled upstreams' /health and
	// /metrics. Probes nothing until an admin builds a pool.
	server.StartPoolProber(ctx, &wg)
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.CheckTrustStoreExpiry(ctx)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				server.CheckTrustStoreExpiry(ctx)
			}
		}
	}()
	startActiveUserGauge(ctx, &wg, db, metrics, logger)
	startUsageRollup(ctx, &wg, db, logger)
	otlp.Start(ctx, &wg)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: streamed completions legitimately run for minutes.
		IdleTimeout: 120 * time.Second,
		// Direct TLS: refuse TLS 1.0/1.1. Every supported browser and SDK
		// speaks 1.2+; Go's default suite selection is kept. The
		// certificate comes from listenerTLS (a mounted pair, re-read when
		// it changes, or the self-signed certificate in the database).
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if listenerTLS.Enabled() {
		httpServer.TLSConfig = listenerTLS.TLSConfig()
		// Smart-card sign-in read by Janus: ask the browser for a client
		// certificate (optional: everything else works without one) and
		// verify it against the configured authorities. Decided per
		// handshake, so turning the setting on needs no restart.
		base := httpServer.TLSConfig
		base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			sc := db.SecurityPolicy(context.Background()).SmartCard
			if !sc.Enabled || sc.Source != "direct" {
				return nil, nil
			}
			c := base.Clone()
			c.GetConfigForClient = nil
			c.ClientAuth = tls.VerifyClientCertIfGiven
			c.ClientCAs = truststore.ClientCAPool()
			if c.ClientCAs == nil {
				c.ClientAuth = tls.RequestClientCert // verified (and refused) at /auth/pki
			}
			return c, nil
		}
	}

	serveErr := make(chan error, 1)
	go func() {
		info := listenerTLS.Info()
		logger.Info("janus listening",
			"addr", cfg.ListenAddr, "public_url", cfg.PublicURL,
			"version", cfg.BuildVersion, "tls", info.Mode != listenertls.ModeNone, "tls_mode", info.Mode,
			"tls_not_after", info.NotAfter, "tls_required", cfg.TLSRequire)
		var err error
		if listenerTLS.Enabled() {
			err = httpServer.ListenAndServeTLS("", "")
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
	// Close the batched writer first: it flushes everything queued, and the
	// pending work Drain waits on completes only once its batch is written.
	server.StopPerformanceMode()
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

// startUsageRollup folds settled usage into usage_rollup once a minute (and
// keeps going without pause while catching up after a backfill or outage),
// and re-checks recent hours against the raw log every ten minutes. Every
// replica runs it; an advisory lock lets one roll at a time.
func startUsageRollup(ctx context.Context, wg *sync.WaitGroup, db *store.Store, logger *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		lastReconcile := time.Now()
		for {
			for {
				done, err := db.RollupUsage(ctx, time.Now())
				if err != nil {
					if ctx.Err() == nil {
						logger.WarnContext(ctx, "usage rollup", "error", err.Error())
					}
					break
				}
				if done || ctx.Err() != nil {
					break
				}
			}
			if time.Since(lastReconcile) >= 10*time.Minute {
				lastReconcile = time.Now()
				if n, err := db.ReconcileUsageRollup(ctx); err != nil {
					if ctx.Err() == nil {
						logger.WarnContext(ctx, "usage rollup reconcile", "error", err.Error())
					}
				} else if n > 0 {
					logger.WarnContext(ctx, "usage rollup reconcile rebuilt hours that disagreed with the request log", "hours", n)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
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

// checkEditionLicense applies stigprofile.CheckLicense and logs the
// outcome once. It is a no-op for a default-edition binary without the
// STIG profile.
func checkEditionLicense(st license.State, logger *slog.Logger) error {
	what := stigprofile.LicenseRequired()
	if what == "" {
		return nil
	}
	if err := stigprofile.CheckLicense(st); err != nil {
		return err
	}
	licenseID := ""
	if st.Claims != nil {
		licenseID = st.Claims.LicenseID
	}
	logger.Info("edition license", "requires", "business", "edition", st.Edition, "status", st.Status, "license_id", licenseID, "source", st.Source, "for", what)
	return nil
}

// newListenerTLS builds the listener certificate provider: the mounted
// pair (re-read on change), the self-signed certificate (stored encrypted
// in the database, generated on first start, followed across replicas
// every minute), or none.
func newListenerTLS(ctx context.Context, cfg *config.Config, db *store.Store, cipher *crypto.Cipher, logger *slog.Logger) (*listenertls.Provider, error) {
	names := cfg.TLSSelfSignedNames
	if len(names) == 0 {
		if u, err := url.Parse(cfg.PublicURL); err == nil && u.Hostname() != "" {
			names = append(names, u.Hostname())
		}
		names = append(names, "localhost", "127.0.0.1")
	}
	p, err := listenertls.New(ctx, listenertls.Options{
		CertPath: cfg.TLSCertPath, KeyPath: cfg.TLSKeyPath, SelfSigned: cfg.TLSSelfSigned,
		Names: names, Days: cfg.TLSSelfSignedDays, Required: cfg.TLSRequire, Logger: logger,
		Persist: listenertls.Persistence{
			Load: func(ctx context.Context) (*listenertls.Stored, error) {
				raw, err := db.ListenerTLS(ctx)
				if err != nil || raw == "" {
					return nil, err
				}
				return listenertls.UnmarshalStored(raw)
			},
			Save: func(ctx context.Context, s *listenertls.Stored) error {
				raw, err := listenertls.MarshalStored(s)
				if err != nil {
					return err
				}
				return db.SetListenerTLS(ctx, raw)
			},
			Encrypt: cipher.Encrypt, Decrypt: cipher.Decrypt,
		},
	})
	if err != nil {
		return nil, err
	}
	if cfg.TLSSelfSigned {
		info := p.Info()
		logger.Warn("serving a self-signed listener certificate (temporary): install it in the proxy as trusted, and replace it with an issued certificate",
			"names", info.Names, "not_after", info.NotAfter, "sha256", info.SHA256)
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := p.Refresh(ctx); err != nil {
						logger.Warn("listener certificate refresh", "error", err.Error())
					}
				}
			}
		}()
	}
	return p, nil
}

// loadTrustStore adds the uploaded certificates and the per-upstream
// "allow untrusted certificate" hosts, refuses saved untrusted upstreams in
// the STIG profile, and keeps both current (directory rescans, uploads on
// other replicas) every 30 seconds.
func loadTrustStore(ctx context.Context, db *store.Store, logger *slog.Logger) error {
	refresh := func(ctx context.Context) ([]*store.Upstream, error) {
		ups, err := db.UntrustedTLSUpstreams(ctx)
		if err != nil {
			return nil, err
		}
		hosts := make([]string, 0, len(ups))
		for _, u := range ups {
			if parsed, err := url.Parse(u.BaseURL); err == nil && parsed.Host != "" {
				hosts = append(hosts, truststore.HostPort(parsed.Scheme, parsed.Host))
			}
		}
		truststore.SetUntrustedHosts(hosts)
		return ups, truststore.Reload(ctx)
	}
	truststore.SetUploadedSource(func(ctx context.Context) ([]truststore.Uploaded, error) {
		cas, err := db.ListTrustedCAs(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]truststore.Uploaded, 0, len(cas))
		for _, c := range cas {
			out = append(out, truststore.Uploaded{ID: c.ID, Name: c.Name, PEM: c.PEM, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt})
		}
		return out, nil
	})
	ups, err := refresh(ctx)
	if err != nil {
		return fmt.Errorf("load trust store: %w", err)
	}
	names := make([]string, 0, len(ups))
	for _, u := range ups {
		names = append(names, u.Name)
	}
	if err := stigprofile.CheckUntrustedUpstreams(names); err != nil {
		return err
	}
	if len(names) > 0 && truststore.Current().UntrustedOK {
		logger.Warn("upstreams with unverified TLS certificates", "upstreams", names)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := refresh(ctx); err != nil {
					logger.Warn("trust store refresh", "error", err.Error())
				}
			}
		}
	}()
	return nil
}

// checkStoredDirectory refuses a saved LDAP directory with skip_verify in
// the STIG profile (the setting lives in the database).
func checkStoredDirectory(ctx context.Context, db *store.Store) error {
	if !stigprofile.Enabled() {
		return nil
	}
	d, err := db.Directory(ctx)
	if errors.Is(err, store.ErrNoDirectory) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read LDAP directory settings: %w", err)
	}
	return stigprofile.CheckDirectory(d.Enabled, d.SkipVerify)
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

// licenseSyncReport tells the portal which gateway is syncing: its stable
// instance id, version, 30-day active seats and live replicas. Counting
// failures send the field empty rather than a misleading zero.
func licenseSyncReport(db *store.Store, version string) func(context.Context) license.SyncReport {
	return func(ctx context.Context) license.SyncReport {
		rep := license.SyncReport{Version: version}
		rep.InstanceID, _ = db.InstanceID(ctx)
		if n, err := db.ActiveSeatCount(ctx, time.Now().Add(-30*24*time.Hour)); err == nil {
			rep.SeatsUsed = &n
		}
		if n, err := db.LiveInstanceCount(ctx, time.Now()); err == nil && n > 0 {
			rep.Nodes = &n
		}
		return rep
	}
}
