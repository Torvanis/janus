// Package e2e drives the full user journey against a running gateway instance:
// dev-auth sign-in, admin upstream/model/grant setup, token creation, a proxied
// completion against a mock upstream, and the request-log / dashboard surfaces
// that must reflect it. It is the executable verification Task 64 promised.
//
// The instance is real: configuration comes from the environment via
// config.Load (exactly what cmd/janus does), storage is SQLite, and the fully
// routed handler serves plain HTTP on a random port. Only process supervision
// (signals, graceful drain) is out of scope here.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

const (
	adminEmail = "admin@e2e.test"
	modelName  = "e2e-model"
)

// startGateway boots the gateway exactly as cmd/janus wires it, minus the
// background jobs the journey does not need, and returns its base URL. The
// mock upstream is registered later through the admin API, like any real one.
func startGateway(t *testing.T) string {
	t.Helper()

	t.Setenv("JANUS_DATABASE_URL", "sqlite://"+filepath.Join(t.TempDir(), "janus.db"))
	t.Setenv("JANUS_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("JANUS_ENV", "development")
	t.Setenv("JANUS_DEV_AUTH", "true")
	t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("JANUS_BOOTSTRAP_ADMIN_EMAILS", adminEmail)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	metrics := telemetry.New(cfg.BuildVersion, cfg.BuildSHA)
	httpClient := &http.Client{Timeout: cfg.UpstreamTotalTimeout}
	alerts := alerting.New(db, alerting.SMTPConfig{}, httpClient, metrics, logger)
	quotaEngine := quota.NewEngine(db)

	server := &httpapi.Server{
		Config:    cfg,
		Store:     db,
		Sessions:  auth.NewDBSessionStore(db, cfg.SessionTTL, cfg.SessionIdleTimeout),
		Cipher:    cipher,
		Quota:     quotaEngine,
		Metrics:   metrics,
		Alerts:    alerts,
		Discovery: discovery.New(db, cipher, httpClient, metrics, alerts, logger, cfg.DiscoveryInterval),
		Logger:    logger,
		StartedAt: time.Now().UTC(),
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// browser is a cookie-carrying client that mimics the web app: it holds the
// session, echoes the CSRF cookie on writes, and never follows redirects (the
// journey asserts on them instead).
type browser struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func newBrowser(t *testing.T, base string) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &browser{
		t:    t,
		base: base,
		http: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (b *browser) signIn(email string) {
	b.t.Helper()
	resp, err := b.http.Get(b.base + "/auth/start?email=" + url.QueryEscape(email))
	if err != nil {
		b.t.Fatalf("dev-auth sign-in: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("dev-auth sign-in returned %d, want 302", resp.StatusCode)
	}
	target, err := url.Parse(b.base)
	if err != nil {
		b.t.Fatalf("parse base url: %v", err)
	}
	for _, c := range b.http.Jar.Cookies(target) {
		if c.Name == auth.CSRFCookie {
			b.csrf = c.Value
		}
	}
	if b.csrf == "" {
		b.t.Fatal("sign-in set no CSRF cookie; the session was not established")
	}
}

// call sends a JSON request. bearer overrides the session when non-empty.
func (b *browser) call(method, path string, payload any, bearer string) (int, map[string]any, http.Header) {
	b.t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			b.t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatalf("build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if b.csrf != "" {
		req.Header.Set(auth.CSRFHeader, b.csrf)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read %s %s response: %v", method, path, err)
	}
	decoded := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			b.t.Fatalf("%s %s returned non-JSON (%d): %.300s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, decoded, resp.Header
}

func (b *browser) mustCall(method, path string, payload any, bearer string, wantStatus int) map[string]any {
	b.t.Helper()
	status, decoded, _ := b.call(method, path, payload, bearer)
	if status != wantStatus {
		b.t.Fatalf("%s %s returned %d, want %d: %v", method, path, status, wantStatus, decoded)
	}
	return decoded
}

// TestFullUserJourney is the end-to-end acceptance walk: every step uses the
// public HTTP surface only, exactly as a browser plus an OpenAI SDK would.
func TestFullUserJourney(t *testing.T) {
	// A mock OpenAI-compatible upstream: serves discovery and completions.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, modelName)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			if got := r.Header.Get("Authorization"); got != "Bearer sk-e2e-upstream" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmpl-e2e","choices":[{"message":{"role":"assistant","content":"Hello from e2e"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":100,"completion_tokens":40}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	base := startGateway(t)
	b := newBrowser(t, base)

	// 1. Dev-auth sign-in; the bootstrap list makes this account an admin.
	b.signIn(adminEmail)
	me := b.mustCall(http.MethodGet, "/api/v1/me", nil, "", http.StatusOK)
	if me["email"] != adminEmail {
		t.Fatalf("/api/v1/me did not return the signed-in user: %v", me)
	}
	if me["role"] != "admin" {
		t.Fatalf("bootstrap admin list was not honoured; role = %v", me["role"])
	}
	// With JANUS_LOCAL_ONLY unset the bootstrap payload must report the
	// default mode explicitly — the SPA keys every cost surface off it.
	if me["local_only"] != false {
		t.Fatalf("/api/v1/me local_only = %v, want false with JANUS_LOCAL_ONLY unset", me["local_only"])
	}

	// 2. Register the upstream; creation triggers immediate model discovery
	//    against the mock's /v1/models.
	b.mustCall(http.MethodPost, "/api/v1/admin/upstreams", map[string]any{
		"name": "e2e-upstream", "adapter_type": "openai_compatible",
		"base_url": upstream.URL, "api_key": "sk-e2e-upstream",
	}, "", http.StatusCreated)

	// 3. Wait for discovery, then set a rate card and enable the model.
	modelID := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && modelID == "" {
		listing := b.mustCall(http.MethodGet, "/api/v1/admin/models", nil, "", http.StatusOK)
		if models, ok := listing["models"].([]any); ok {
			for _, m := range models {
				entry := m.(map[string]any)
				if entry["name"] == modelName {
					modelID = entry["id"].(string)
				}
			}
		}
		if modelID == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if modelID == "" {
		t.Fatal("discovery never surfaced the mock upstream's model")
	}
	b.mustCall(http.MethodPatch, "/api/v1/admin/models/"+modelID, map[string]any{
		"status": "enabled", "rate_in_usd_per_mtok": 1.0, "rate_out_usd_per_mtok": 2.0,
	}, "", http.StatusOK)

	// 4. Grant the model to everyone.
	b.mustCall(http.MethodPost, "/api/v1/admin/grants", map[string]any{
		"model_ids": []string{modelID}, "grantee_type": "all_users",
	}, "", http.StatusCreated)

	// 5. Create an API token; the plaintext is returned exactly once.
	created := b.mustCall(http.MethodPost, "/api/v1/tokens", map[string]any{
		"description": "e2e journey",
	}, "", http.StatusCreated)
	tokenValue, _ := created["value"].(string)
	if tokenValue == "" {
		t.Fatalf("token creation returned no plaintext value: %v", created)
	}

	// 6. The token sees its grant on the OpenAI-compatible listing.
	models := b.mustCall(http.MethodGet, "/v1/models", nil, tokenValue, http.StatusOK)
	listed := false
	if data, ok := models["data"].([]any); ok {
		for _, m := range data {
			if m.(map[string]any)["id"] == modelName {
				listed = true
			}
		}
	}
	if !listed {
		t.Fatalf("GET /v1/models does not list the granted model: %v", models)
	}

	// 7. A proxied completion round-trips through the mock upstream and comes
	//    back costed.
	status, completion, header := b.call(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    modelName,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, tokenValue)
	if status != http.StatusOK {
		t.Fatalf("proxied completion returned %d: %v", status, completion)
	}
	if completion["id"] != "cmpl-e2e" {
		t.Fatalf("upstream response was not relayed verbatim: %v", completion)
	}
	// 100 in-tokens at $1/mtok + 40 out-tokens at $2/mtok = $0.00018.
	if cost := header.Get("X-Janus-Cost-USD"); cost != "0.00018" {
		t.Fatalf("X-Janus-Cost-USD = %q, want 0.00018", cost)
	}

	// 8. The request log shows the metered event (recording is async).
	var event map[string]any
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && event == nil {
		log := b.mustCall(http.MethodGet, "/api/v1/requests", nil, "", http.StatusOK)
		if reqs, ok := log["requests"].([]any); ok {
			for _, r := range reqs {
				entry := r.(map[string]any)
				if entry["model"] == modelName && entry["http_status"] == float64(200) {
					event = entry
				}
			}
		}
		if event == nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if event == nil {
		t.Fatal("the proxied request never appeared in /api/v1/requests")
	}
	if event["tokens_in"] != float64(100) || event["tokens_out"] != float64(40) {
		t.Fatalf("request log metered (%v, %v) tokens, want (100, 40)", event["tokens_in"], event["tokens_out"])
	}
	if event["token_accounting_method"] != "upstream_reported" {
		t.Fatalf("accounting mode = %v, want upstream_reported", event["token_accounting_method"])
	}

	// 9. The personal dashboard aggregates it.
	dash := b.mustCall(http.MethodGet, "/api/v1/dashboard/personal", nil, "", http.StatusOK)
	totals, _ := dash["totals"].(map[string]any)
	if totals == nil || totals["tokens_in"] != float64(100) || totals["tokens_out"] != float64(40) {
		t.Fatalf("dashboard totals = %v, want tokens_in=100 tokens_out=40", totals)
	}
	if totals["request_count"] != float64(1) {
		t.Fatalf("dashboard request_count = %v, want 1", totals["request_count"])
	}
	if totals["cost_nanousd"] == float64(0) {
		t.Fatal("dashboard cost is zero; the rate card was not applied")
	}
}
