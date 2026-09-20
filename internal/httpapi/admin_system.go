package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/store"
)

// handleSystemStatus serves GET /api/v1/admin/system/status: one document an
// administrator can read to know whether every dependency is healthy and how
// the gateway is configured. Secrets never appear here — the database block
// exposes only the backend name and a credential-free location.
func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbOK := s.Store.Ping(ctx) == nil
	upstreams, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	upstreamStatus := make([]map[string]any, 0, len(upstreams))
	for _, up := range upstreams {
		upstreamStatus = append(upstreamStatus, map[string]any{
			"id": up.ID, "name": up.Name, "adapter_type": up.AdapterType, "enabled": up.Enabled,
			"reachable":     up.LastError == "" && !up.LastCheckAt.IsZero(),
			"last_check_at": up.LastCheckAt, "last_error": up.LastError,
			"latency_ms": up.LastLatencyMs, "model_count": up.ModelCount,
		})
	}
	flags, err := s.Store.FeatureFlags(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	feedback, err := s.Store.ListDocsFeedback(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	migrations, err := s.Store.AppliedMigrations(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	timeoutsDoc, err := s.upstreamTimeoutsDocument(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	discoveryDoc, err := s.discoveryIntervalDocument(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Metering health: how the last 30 days of successful requests were
	// accounted. Unmetered media responses are a configuration gap (the
	// upstream reports no usage the adapter can read) and are listed per
	// model so the operator can see exactly which extraction is missing.
	accounting, err := s.Store.AccountingSummarySince(ctx, time.Now().UTC().AddDate(0, 0, -30))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	backend, location := sanitizeDatabaseURL(s.Config.DatabaseURL)
	if backend == "" {
		// No configured URL to describe (test wiring); the live connection
		// still knows which engine it speaks.
		backend = string(s.Store.Dialect())
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"database": map[string]any{
			"ok": dbOK, "engine": string(s.Store.Dialect()), "migrations": migrations,
			"backend": backend, "location": location, "defaulted": s.Config.DatabaseURLDefaulted,
		},
		"identity_provider": map[string]any{"ok": s.OIDC != nil || s.Config.DevAuthEnabled, "provider": s.providerLabel(), "dev_auth": s.Config.DevAuthEnabled},
		"quota_ledger":      map[string]any{"ok": true, "mode": "durable ledger with event-log reconciliation"},
		"email":             map[string]any{"ok": s.Alerts.EmailEnabled(), "detail": smtpDetail(s.Alerts.EmailEnabled())},
		"telemetry":         map[string]any{"prometheus": "/metrics"},
		"upstreams":         upstreamStatus,
		"metering": map[string]any{
			"ok":          accounting.Unmetered == 0,
			"window_days": 30,
			"summary":     accounting,
		},
		"discovery": map[string]any{
			"last_run_at": s.Discovery.LastRun(),
			// interval_minutes is the EFFECTIVE cadence (override or
			// default); the full provenance document sits beside it.
			"interval_minutes": discoveryDoc.EffectiveMinutes,
			"interval":         discoveryDoc,
		},
		"retention":     map[string]any{"usage_days": s.Config.UsageRetentionDays, "audit_days": s.Config.AuditRetentionDays, "purge_at_utc": s.Config.PurgeJobTimeUTC},
		"build":         map[string]any{"version": s.Config.BuildVersion, "sha": s.Config.BuildSHA, "started_at": s.StartedAt, "uptime_seconds": int(time.Since(s.StartedAt).Seconds())},
		"feature_flags": flags,
		"docs_feedback": feedback,
		"adapters":      adapter.Types(),
		// Local-only mode (JANUS_LOCAL_ONLY): cost tracking disabled instance-wide.
		"local_only": s.Config.LocalOnly,
		// Per-hop upstream timeouts in force right now, with their provenance,
		// so the System page can offer the same document the dedicated
		// endpoint serves without a second request.
		"upstream_timeouts": timeoutsDoc,
	})
}

// --- Upstream timeouts ---------------------------------------------------------
//
// The connect / time-to-first-byte / total upstream bounds are loaded from
// JANUS_UPSTREAM_*_TIMEOUT_SECONDS at boot, but a busy provider queue can push
// first-byte latency past the 30s default at any time of day, and the only
// remedy used to be a redeploy. These endpoints let an administrator override
// any hop at runtime. Overrides are persisted (store.SetUpstreamTimeoutOverrides)
// so they survive restarts, take effect on this replica immediately (the config
// cache is flushed) and on every other replica within the cache TTL, and every
// change is audit-logged with the before/after effective values.

// upstreamTimeoutsDocument is the shape served by GET/PATCH/DELETE
// /api/v1/admin/system/upstream-timeouts and embedded in the status document.
type upstreamTimeoutsDocument struct {
	// Effective is what the proxy enforces right now.
	Effective upstreamTimeoutValues `json:"effective"`
	// Defaults are the environment-derived values (JANUS_UPSTREAM_*).
	Defaults upstreamTimeoutValues `json:"defaults"`
	// Overrides are the stored administrator values; zero means "not
	// overridden, the default applies" for that hop.
	Overrides upstreamTimeoutValues `json:"overrides"`
	// Source names, per hop, whether the effective value comes from the
	// environment ("env") or an administrator override ("admin").
	Source upstreamTimeoutSources `json:"source"`
	// UpdatedAt is when an administrator last saved overrides; omitted when
	// nothing is overridden.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// MaxSeconds is the ceiling any hop may be set to, so the UI can validate
	// before submitting.
	MaxSeconds int `json:"max_seconds"`
}

type upstreamTimeoutValues struct {
	ConnectSeconds int `json:"connect_seconds"`
	TTFBSeconds    int `json:"ttfb_seconds"`
	TotalSeconds   int `json:"total_seconds"`
}

type upstreamTimeoutSources struct {
	Connect string `json:"connect"`
	TTFB    string `json:"ttfb"`
	Total   string `json:"total"`
}

func timeoutValues(t config.UpstreamTimeouts) upstreamTimeoutValues {
	return upstreamTimeoutValues{
		ConnectSeconds: int(t.Connect.Seconds()),
		TTFBSeconds:    int(t.TTFB.Seconds()),
		TotalSeconds:   int(t.Total.Seconds()),
	}
}

func timeoutSource(overrideSeconds int) string {
	if overrideSeconds > 0 {
		return "admin"
	}
	return "env"
}

// upstreamTimeoutsDocument assembles the document from the environment
// defaults and the stored overrides. It reads the store directly (not the
// cache) so an administrator always sees what is persisted.
func (s *Server) upstreamTimeoutsDocument(ctx context.Context) (upstreamTimeoutsDocument, error) {
	overrides, err := s.Store.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		return upstreamTimeoutsDocument{}, err
	}
	return s.buildTimeoutsDocument(overrides), nil
}

func (s *Server) buildTimeoutsDocument(overrides store.UpstreamTimeoutOverrides) upstreamTimeoutsDocument {
	defaults := s.Config.UpstreamTimeoutDefaults()
	effective := defaults.WithOverrides(overrides.ConnectSeconds, overrides.TTFBSeconds, overrides.TotalSeconds)
	doc := upstreamTimeoutsDocument{
		Effective: timeoutValues(effective),
		Defaults:  timeoutValues(defaults),
		Overrides: upstreamTimeoutValues{
			ConnectSeconds: overrides.ConnectSeconds, TTFBSeconds: overrides.TTFBSeconds, TotalSeconds: overrides.TotalSeconds,
		},
		Source: upstreamTimeoutSources{
			Connect: timeoutSource(overrides.ConnectSeconds),
			TTFB:    timeoutSource(overrides.TTFBSeconds),
			Total:   timeoutSource(overrides.TotalSeconds),
		},
		MaxSeconds: store.MaxUpstreamTimeoutSeconds,
	}
	if !overrides.UpdatedAt.IsZero() {
		updatedAt := overrides.UpdatedAt
		doc.UpdatedAt = &updatedAt
	}
	return doc
}

// handleGetUpstreamTimeouts serves GET /api/v1/admin/system/upstream-timeouts.
func (s *Server) handleGetUpstreamTimeouts(w http.ResponseWriter, r *http.Request) {
	doc, err := s.upstreamTimeoutsDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// handlePatchUpstreamTimeouts serves PATCH /api/v1/admin/system/upstream-timeouts.
// The body names any subset of connect_seconds / ttfb_seconds / total_seconds:
// a positive value overrides that hop, zero reverts it to the environment
// default, and an omitted hop is left as it was. The resulting EFFECTIVE set
// must be consistent (first-byte and connect bounds no longer than the total),
// so an administrator who raises the TTFB past the total is told to raise the
// total too rather than being handed a limit that can never fire.
func (s *Server) handlePatchUpstreamTimeouts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConnectSeconds *int `json:"connect_seconds"`
		TTFBSeconds    *int `json:"ttfb_seconds"`
		TotalSeconds   *int `json:"total_seconds"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.ConnectSeconds == nil && body.TTFBSeconds == nil && body.TotalSeconds == nil {
		WriteError(w, r, ErrInvalidRequest("Provide at least one of connect_seconds, ttfb_seconds, or total_seconds (0 reverts a hop to its environment default)."))
		return
	}
	before, err := s.Store.UpstreamTimeoutOverrides(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := before
	if body.ConnectSeconds != nil {
		next.ConnectSeconds = *body.ConnectSeconds
	}
	if body.TTFBSeconds != nil {
		next.TTFBSeconds = *body.TTFBSeconds
	}
	if body.TotalSeconds != nil {
		next.TotalSeconds = *body.TotalSeconds
	}
	if err := next.Validate(); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	effective := s.Config.UpstreamTimeoutDefaults().WithOverrides(next.ConnectSeconds, next.TTFBSeconds, next.TotalSeconds)
	if err := effective.Validate(); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()+". Raise total_seconds as well, or lower the other value."))
		return
	}
	if err := s.Store.SetUpstreamTimeoutOverrides(r.Context(), next); err != nil {
		WriteError(w, r, err)
		return
	}
	s.finishUpstreamTimeoutChange(w, r, before)
}

// handleDeleteUpstreamTimeouts serves DELETE /api/v1/admin/system/upstream-timeouts:
// every hop reverts to its environment default.
func (s *Server) handleDeleteUpstreamTimeouts(w http.ResponseWriter, r *http.Request) {
	before, err := s.Store.UpstreamTimeoutOverrides(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.ClearUpstreamTimeoutOverrides(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	s.finishUpstreamTimeoutChange(w, r, before)
}

// finishUpstreamTimeoutChange flushes the config cache so this replica's next
// proxied request rebuilds its upstream client, audit-logs the effective
// before/after values, and answers with the new document.
func (s *Server) finishUpstreamTimeoutChange(w http.ResponseWriter, r *http.Request, before store.UpstreamTimeoutOverrides) {
	after, err := s.upstreamTimeoutsDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	previous := s.buildTimeoutsDocument(before)
	s.audit(r, "upstream_timeouts_changed", "system_setting", store.SettingUpstreamTimeouts,
		map[string]any{"effective": previous.Effective, "overrides": previous.Overrides},
		map[string]any{"effective": after.Effective, "overrides": after.Overrides})
	s.Logger.InfoContext(r.Context(), "upstream timeouts changed by administrator",
		"connect_seconds", after.Effective.ConnectSeconds,
		"ttfb_seconds", after.Effective.TTFBSeconds,
		"total_seconds", after.Effective.TotalSeconds)
	WriteJSON(w, http.StatusOK, after)
}

// --- Discovery interval ---------------------------------------------------------
//
// The model-discovery poll doubles as the upstream reachability probe (each
// run records the upstream's last check and error), so its cadence decides how
// quickly an outage becomes visible. It is loaded from
// JANUS_DISCOVERY_INTERVAL_MINUTES at boot; these endpoints let an
// administrator override it at runtime, persisted in system_setting so it
// survives restarts and reaches every replica, and applied by the scheduler
// without a restart.

// discoveryIntervalDocument is the shape served by GET/PATCH/DELETE
// /api/v1/admin/system/discovery-interval and embedded in the status document.
type discoveryIntervalDocument struct {
	// EffectiveMinutes is the cadence the scheduler honours right now.
	EffectiveMinutes int `json:"effective_minutes"`
	// DefaultMinutes is the environment value (JANUS_DISCOVERY_INTERVAL_MINUTES).
	DefaultMinutes int `json:"default_minutes"`
	// OverrideMinutes is the stored override; 0 when none is set.
	OverrideMinutes int `json:"override_minutes"`
	// Source is "override" or "default".
	Source     string     `json:"source"`
	MinMinutes int        `json:"min_minutes"`
	MaxMinutes int        `json:"max_minutes"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	LastRunAt  time.Time  `json:"last_run_at"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
}

func (s *Server) discoveryIntervalDocument(ctx context.Context) (discoveryIntervalDocument, error) {
	override, err := s.Store.DiscoveryIntervalOverride(ctx)
	if err != nil {
		return discoveryIntervalDocument{}, err
	}
	return s.buildDiscoveryIntervalDocument(override), nil
}

func (s *Server) buildDiscoveryIntervalDocument(override store.DiscoveryIntervalOverride) discoveryIntervalDocument {
	doc := discoveryIntervalDocument{
		DefaultMinutes:  int(s.Config.DiscoveryInterval.Minutes()),
		OverrideMinutes: override.Minutes,
		Source:          "default",
		MinMinutes:      store.MinDiscoveryIntervalMinutes,
		MaxMinutes:      store.MaxDiscoveryIntervalMinutes,
	}
	doc.EffectiveMinutes = doc.DefaultMinutes
	if override.Minutes > 0 {
		doc.EffectiveMinutes = override.Minutes
		doc.Source = "override"
	}
	if !override.UpdatedAt.IsZero() {
		t := override.UpdatedAt
		doc.UpdatedAt = &t
	}
	if s.Discovery != nil {
		doc.LastRunAt = s.Discovery.LastRun()
		if !doc.LastRunAt.IsZero() {
			next := doc.LastRunAt.Add(time.Duration(doc.EffectiveMinutes) * time.Minute)
			doc.NextRunAt = &next
		}
	}
	return doc
}

// handleGetDiscoveryInterval serves GET /api/v1/admin/system/discovery-interval.
func (s *Server) handleGetDiscoveryInterval(w http.ResponseWriter, r *http.Request) {
	doc, err := s.discoveryIntervalDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// handlePatchDiscoveryInterval serves PATCH /api/v1/admin/system/discovery-interval.
// The body carries minutes: a positive value (1 minute to 7 days) overrides
// the environment default, zero reverts to it.
func (s *Server) handlePatchDiscoveryInterval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minutes *int `json:"minutes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Minutes == nil {
		WriteError(w, r, ErrInvalidRequest("Provide minutes (0 reverts to the environment default)."))
		return
	}
	before, err := s.Store.DiscoveryIntervalOverride(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := store.DiscoveryIntervalOverride{Minutes: *body.Minutes}
	if err := next.Validate(); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	if err := s.Store.SetDiscoveryIntervalOverride(r.Context(), next); err != nil {
		WriteError(w, r, err)
		return
	}
	s.finishDiscoveryIntervalChange(w, r, before)
}

// handleDeleteDiscoveryInterval serves DELETE /api/v1/admin/system/discovery-interval:
// the cadence reverts to the environment default.
func (s *Server) handleDeleteDiscoveryInterval(w http.ResponseWriter, r *http.Request) {
	before, err := s.Store.DiscoveryIntervalOverride(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.ClearDiscoveryIntervalOverride(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	s.finishDiscoveryIntervalChange(w, r, before)
}

// finishDiscoveryIntervalChange wakes the scheduler so the new cadence applies
// now, audit-logs the before/after effective values, and answers with the new
// document.
func (s *Server) finishDiscoveryIntervalChange(w http.ResponseWriter, r *http.Request, before store.DiscoveryIntervalOverride) {
	after, err := s.discoveryIntervalDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if s.Discovery != nil {
		s.Discovery.Reschedule()
	}
	previous := s.buildDiscoveryIntervalDocument(before)
	s.audit(r, "discovery_interval_changed", "system_setting", store.SettingDiscoveryInterval,
		map[string]any{"effective_minutes": previous.EffectiveMinutes, "override_minutes": previous.OverrideMinutes, "source": previous.Source},
		map[string]any{"effective_minutes": after.EffectiveMinutes, "override_minutes": after.OverrideMinutes, "source": after.Source})
	s.Logger.InfoContext(r.Context(), "discovery interval changed by administrator",
		"effective_minutes", after.EffectiveMinutes, "source", after.Source)
	WriteJSON(w, http.StatusOK, after)
}

// sanitizeDatabaseURL reduces a database URL to what an administrator needs
// to identify the running backend, with everything sensitive removed:
//
//	sqlite:///data/janus.db                                → sqlite, /data/janus.db
//	postgresql://user:pass@db.example.test:5432/janus?sslmode… → postgres, db.example.test:5432/janus
//
// Credentials, query parameters, and the raw DSN never appear in the result.
// A URL that cannot be parsed yields the detected backend and an empty
// location — never the original string, which could carry a password.
func sanitizeDatabaseURL(raw string) (backend, location string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	switch {
	case strings.HasPrefix(raw, "postgres://"), strings.HasPrefix(raw, "postgresql://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "postgres", ""
		}
		// Host is "host:port" (or just "host"); Path is "/dbname". Userinfo
		// and the query string are deliberately dropped.
		return "postgres", u.Host + strings.TrimSuffix(u.Path, "/")
	case strings.HasPrefix(raw, "sqlite://"):
		return "sqlite", trimDSNOptions(strings.TrimPrefix(raw, "sqlite://"))
	case strings.HasPrefix(raw, "file:"):
		return "sqlite", trimDSNOptions(strings.TrimPrefix(raw, "file:"))
	default:
		// store.resolveDSN treats anything else (a bare *.db path, :memory:)
		// as sqlite; the value is a local path with no credentials in it.
		return "sqlite", trimDSNOptions(raw)
	}
}

// trimDSNOptions drops driver options ("?cache=shared&mode=rwc") from a
// sqlite path so only the file location remains.
func trimDSNOptions(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func smtpDetail(enabled bool) string {
	if enabled {
		return "SMTP is configured; email alerts will be delivered."
	}
	return "Email delivery is off — JANUS_SMTP_HOST is not set."
}
