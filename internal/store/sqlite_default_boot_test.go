// Package store_test hosts the sqlite-default boot test as an external test
// package: it wires the real httpapi server around the store (exactly as
// cmd/janus does) which an internal store test could not do without an
// import cycle.
package store_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/httpapi"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// shippedMigrations is every migration the binary applies on boot, in order.
// The boot test asserts each one is recorded in schema_migration after
// store.Open, so a future migration that fails against the embedded engine is
// caught here rather than on a customer's first zero-config run. Appending a
// new migration to internal/store/schema.go requires appending its name here.
var shippedMigrations = []string{
	"0001_core_identity",
	"0002_tokens",
	"0003_audit",
	"0004_upstreams_models",
	"0005_grants",
	"0006_usage",
	"0007_quotas",
	"0008_rules_alerts_flags",
	"0009_sessions_oidc_state",
	"0010_quota_alert_thresholds",
	"0011_admin_via_group",
	"0012_model_display_name",
	"0013_bigint_money_cache_write_rates",
	"0014_model_context_window",
}

// TestSQLiteDefaultBootPath proves the zero-configuration boot contract: with
// JANUS_DATABASE_URL unset, config loads with the embedded-SQLite default,
// store.Open creates the file and applies every migration, /healthz answers
// 200 on the real router, and a full write/read round-trip (user + token
// create, bearer auth, revoke, reject) works against the sqlite file — then
// the file survives a close/reopen, the container-restart-with-volume case.
//
// The default URL points at /data/janus.db (the container volume). Tests must
// never write outside the test sandbox, so the directory portion is relocated
// into t.TempDir() — a JANUS_DATA_DIR-style override — while the scheme and
// file name stay exactly what the default ships.
func TestSQLiteDefaultBootPath(t *testing.T) {
	// t.Setenv registers restoration of the original value; the follow-up
	// Unsetenv makes the variable truly absent (not merely empty), which is
	// the condition the default is specified against.
	t.Setenv("JANUS_DATABASE_URL", "")
	os.Unsetenv("JANUS_DATABASE_URL")
	t.Setenv("JANUS_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("JANUS_ENV", "development")
	t.Setenv("JANUS_DEV_AUTH", "true")
	t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")

	// 1. Config loads with the sqlite default and flags it as defaulted (the
	// flag gates the "embedded SQLite, single-node evaluation mode" startup
	// warning in cmd/janus/main.go).
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load with JANUS_DATABASE_URL unset must succeed, got: %v", err)
	}
	if cfg.DatabaseURL != config.DefaultDatabaseURL {
		t.Fatalf("DatabaseURL = %q, want the shipped default %q", cfg.DatabaseURL, config.DefaultDatabaseURL)
	}
	if !cfg.DatabaseURLDefaulted {
		t.Fatal("DatabaseURLDefaulted must be true when the default applies: it drives the startup warning")
	}

	// 2. Relocate the default's directory into the test sandbox and open the
	// store: this is the same store.Open(ctx, url) call main.go makes.
	dataDir := t.TempDir()
	dbURL := strings.Replace(cfg.DatabaseURL, "sqlite:///data/", "sqlite://"+dataDir+"/", 1)
	if dbURL == cfg.DatabaseURL {
		t.Fatalf("could not relocate the default URL %q into the temp dir; has the default changed shape?", cfg.DatabaseURL)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.Open on the defaulted sqlite URL failed: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.Dialect() != store.DialectSQLite {
		t.Fatalf("dialect = %q, want %q", db.Dialect(), store.DialectSQLite)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "janus.db")); err != nil {
		t.Fatalf("sqlite file was not created at the default file name: %v", err)
	}

	// 3. Every shipped migration is recorded as applied.
	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read applied migrations: %v", err)
	}
	appliedSet := map[string]bool{}
	for _, name := range applied {
		appliedSet[name] = true
	}
	for _, name := range shippedMigrations {
		if !appliedSet[name] {
			t.Errorf("migration %s not recorded as applied on the sqlite default path", name)
		}
	}
	if len(applied) < len(shippedMigrations) {
		t.Fatalf("applied %d migrations, want at least %d", len(applied), len(shippedMigrations))
	}

	// 4. Schema spot-check: tables from three different migrations answer.
	for _, table := range []string{"app_user", "api_token", "audit_log"} {
		var n int
		if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("schema spot-check on %s failed: %v", table, err)
		}
	}

	// 5. Boot the HTTP surface exactly as cmd/janus wires it (minus the
	// background jobs) and probe liveness + readiness.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	metrics := telemetry.New(cfg.BuildVersion, cfg.BuildSHA)
	httpClient := &http.Client{Timeout: cfg.UpstreamTotalTimeout}
	alerts := alerting.New(db, alerting.SMTPConfig{}, httpClient, metrics, logger)
	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	server := &httpapi.Server{
		Config:    cfg,
		Store:     db,
		Sessions:  auth.NewDBSessionStore(db, cfg.SessionTTL, cfg.SessionIdleTimeout),
		Cipher:    cipher,
		Quota:     quota.NewEngine(db),
		Metrics:   metrics,
		Alerts:    alerts,
		Discovery: discovery.New(db, cipher, httpClient, metrics, alerts, logger, cfg.DiscoveryInterval),
		Logger:    logger,
		StartedAt: time.Now().UTC(),
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	client := &http.Client{Timeout: 10 * time.Second}

	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// 5b. Readiness detail: on the sqlite default path the database
	// check is a plain Ping against the embedded file, and with JANUS_DEV_AUTH
	// the identity-provider check is short-circuited to "local evaluation
	// sign-in" — no OIDC probe (and none of its fault-tolerance machinery) is
	// involved. Pin both so a change to the readiness handler cannot silently
	// alter the evaluation-mode contract.
	readyResp, err := client.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	var ready struct {
		Ready  bool `json:"ready"`
		Checks map[string]struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(readyResp.Body).Decode(&ready); err != nil {
		t.Fatalf("decode /readyz body: %v", err)
	}
	_ = readyResp.Body.Close()
	if !ready.Ready {
		t.Fatalf("/readyz reported ready=false on a healthy sqlite boot: %+v", ready.Checks)
	}
	if c, ok := ready.Checks["database"]; !ok || !c.OK {
		t.Fatalf("/readyz database check = %+v, want ok=true", c)
	}
	if c, ok := ready.Checks["identity_provider"]; !ok || !c.OK || c.Detail != "local evaluation sign-in" {
		t.Fatalf("/readyz identity_provider check = %+v, want ok=true detail=%q (dev-auth short-circuit)", c, "local evaluation sign-in")
	}

	// 6. Write/read round-trip against the sqlite file: first-login user
	// creation, token issue, bearer auth over HTTP, revocation, rejection.
	user, created, err := db.UpsertUserFromIdentity(ctx, "dev:boot@example.com", "boot@example.com", "Boot Test", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !created {
		t.Fatal("first upsert must create the user")
	}
	loaded, err := db.UserByAuthID(ctx, "dev:boot@example.com")
	if err != nil {
		t.Fatalf("read user back: %v", err)
	}
	if loaded.ID != user.ID || loaded.Email != "boot@example.com" {
		t.Fatalf("read-back mismatch: got id=%s email=%s", loaded.ID, loaded.Email)
	}

	token, plaintext, err := db.CreateToken(ctx, user.ID, "sqlite boot round-trip")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(plaintext, store.TokenPrefix) {
		t.Fatalf("token plaintext %q missing the %q prefix", plaintext, store.TokenPrefix)
	}
	row, err := db.TokenByDigest(ctx, store.HashToken(plaintext))
	if err != nil {
		t.Fatalf("token lookup by digest: %v", err)
	}
	if row.ID != token.ID || row.Revoked() {
		t.Fatalf("digest lookup returned id=%s revoked=%v, want id=%s unrevoked", row.ID, row.Revoked(), token.ID)
	}

	// The freshly minted credential authenticates a real request.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatalf("build /api/v1/me request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/me with bearer token: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer-authenticated /api/v1/me = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "boot@example.com") {
		t.Fatalf("/api/v1/me response does not name the caller: %s", body)
	}

	// Revoke and confirm the credential is dead, both in the store and on the
	// proxy surface. The revocation handler clears the token cache in
	// production; this test revokes through the store directly, so it clears
	// the cache the same way before re-presenting the credential.
	if err := db.RevokeToken(ctx, token.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	row, err = db.TokenByDigest(ctx, store.HashToken(plaintext))
	if err != nil {
		t.Fatalf("token lookup after revoke: %v", err)
	}
	if !row.Revoked() {
		t.Fatal("token must read back as revoked")
	}
	server.InvalidateTokenCache()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("build /v1/models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models with revoked token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer token on /v1/models = %d, want 401", resp.StatusCode)
	}

	// 7. Durability: close everything and reopen the same file — the
	// container-restart-with-a-/data-volume case. Migrations are already
	// recorded, and the data written above is still there.
	ts.Close()
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("reopen sqlite file: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	again, err := reopened.UserByAuthID(ctx, "dev:boot@example.com")
	if err != nil {
		t.Fatalf("user must survive a close/reopen: %v", err)
	}
	if again.ID != user.ID {
		t.Fatalf("reopened user id = %s, want %s", again.ID, user.ID)
	}
	appliedAgain, err := reopened.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read migrations after reopen: %v", err)
	}
	if len(appliedAgain) != len(applied) {
		t.Fatalf("migration ledger changed across reopen: %d != %d (Migrate must be idempotent)", len(appliedAgain), len(applied))
	}
}
