package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// chatWithHeaders sends one chat request through the proxy with extra
// caller headers and returns the headers the fake upstream received.
func (h *harness) chatWithHeaders(t *testing.T, headers map[string]string) http.Header {
	t.Helper()
	before := len(h.upstreamRequests)
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	if len(h.upstreamRequests) != before+1 {
		t.Fatalf("upstream calls = %d, want %d", len(h.upstreamRequests), before+1)
	}
	return h.upstreamRequests[len(h.upstreamRequests)-1].Header
}

var tunnelHeaders = map[string]string{
	"Cf-Connecting-Ip":        "203.0.113.9",
	"Cf-Access-Jwt-Assertion": "eyJhbGciOi.secret-access-token",
	"X-Forwarded-For":         "203.0.113.9",
	"Cdn-Loop":                "cloudflare",
	"X-Request-Id":            "req-123",
}

// The built-in default (seeded by the migration) strips edge headers for
// every upstream; caller headers that are not edge headers still pass.
func TestHeaderPolicyDefaultStripsEdgeHeaders(t *testing.T) {
	h := newHarness(t)
	got := h.chatWithHeaders(t, tunnelHeaders)
	for _, gone := range []string{"Cf-Connecting-Ip", "Cf-Access-Jwt-Assertion", "X-Forwarded-For", "Cdn-Loop"} {
		if v := got.Get(gone); v != "" {
			t.Errorf("%s reached the upstream: %q", gone, v)
		}
	}
	if got.Get("X-Request-Id") != "req-123" {
		t.Errorf("X-Request-Id dropped")
	}

	// The request log has the header record: inbound shows what the caller
	// sent (Access token masked), changes name the removals.
	events := h.waitForUsageEvents(1)
	var l *store.RequestHeaderLog
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if l, err = h.store.RequestHeaderLogByUsageID(context.Background(), events[0].ID); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l == nil {
		t.Fatal("no header log written")
	}
	if l.PolicyID != store.BuiltinEdgePolicyID || l.Target != store.HeaderTargetDefault {
		t.Errorf("policy=%q target=%q", l.PolicyID, l.Target)
	}
	removed := map[string]bool{}
	for _, c := range l.Changes {
		if c.Action == "removed" {
			removed[c.Name] = true
		}
	}
	if !removed["Cf-Connecting-Ip"] || !removed["Cf-Access-Jwt-Assertion"] || !removed["X-Forwarded-For"] {
		t.Errorf("changes = %+v", l.Changes)
	}
	for _, in := range l.Inbound {
		if strings.Contains(strings.Join(in.Values, ""), "secret-access-token") {
			t.Errorf("Access token logged in clear: %+v", in)
		}
		if strings.Contains(strings.Join(in.Values, ""), h.token) {
			t.Errorf("caller key logged in clear: %+v", in)
		}
	}
	for _, out := range l.Outbound {
		if strings.EqualFold(out.Name, "Cf-Connecting-Ip") {
			t.Errorf("outbound log shows a stripped header")
		}
	}
}

// A per-upstream policy beats the default, admin-set values reach the
// provider, secret values are encrypted at rest and masked in the log, and
// unassigning falls back to the default.
func TestHeaderPolicyPerUpstreamOverride(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sess := adminSession(t, h)

	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/header-policies", map[string]any{
		"name": "Acme org header",
		"rules": []map[string]any{
			{"action": "remove", "name": "x-request-id"},
			{"action": "set", "name": "OpenAI-Organization", "value": "org-acme"},
			{"action": "set", "name": "X-Partner-Key", "value": "very-secret-partner", "secret": true},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "very-secret-partner") {
		t.Fatal("API echoed a secret value")
	}
	var raw string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT rules FROM header_policy WHERE name = 'Acme org header'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "very-secret-partner") {
		t.Fatal("secret value stored in clear")
	}
	policies, _ := h.store.ListHeaderPolicies(ctx)
	var pid string
	for _, p := range policies {
		if p.Name == "Acme org header" {
			pid = p.ID
		}
	}
	upID := h.model.UpstreamID
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{
		"target_type": "upstream", "target_id": upID, "policy_id": pid,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}

	got := h.chatWithHeaders(t, tunnelHeaders)
	if got.Get("X-Request-Id") != "" {
		t.Error("upstream policy did not remove X-Request-Id")
	}
	if got.Get("Openai-Organization") != "org-acme" || got.Get("X-Partner-Key") != "very-secret-partner" {
		t.Errorf("set headers missing: %v", got)
	}
	// The override REPLACES the default: edge headers are no longer
	// stripped for this upstream (admins compose by editing their policy).
	if got.Get("Cf-Connecting-Ip") == "" {
		t.Error("default still applied under an override")
	}
	// Credentials always come from the adapter, never the caller.
	if got.Get("Authorization") == "Bearer "+h.token {
		t.Error("caller key forwarded")
	}

	events := h.waitForUsageEvents(1)
	var l *store.RequestHeaderLog
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && l == nil {
		l, _ = h.store.RequestHeaderLogByUsageID(ctx, events[len(events)-1].ID)
		time.Sleep(20 * time.Millisecond)
	}
	if l == nil || l.PolicyID != pid || l.Target != store.HeaderTargetUpstream {
		t.Fatalf("header log = %+v", l)
	}
	for _, out := range l.Outbound {
		if strings.EqualFold(out.Name, "X-Partner-Key") && (!out.Masked || strings.Contains(strings.Join(out.Values, ""), "very-secret")) {
			t.Errorf("secret rule value logged: %+v", out)
		}
	}

	// Update keeping the secret (keep_value) leaves the value working.
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/"+pid, map[string]any{
		"name": "Acme org header",
		"rules": []map[string]any{
			{"action": "set", "name": "X-Partner-Key", "secret": true, "keep_value": true},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got := h.chatWithHeaders(t, nil); got.Get("X-Partner-Key") != "very-secret-partner" {
		t.Errorf("kept secret lost: %q", got.Get("X-Partner-Key"))
	}

	// Unassign: the default applies again.
	h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{
		"target_type": "upstream", "target_id": upID, "policy_id": "",
	})
	if got := h.chatWithHeaders(t, tunnelHeaders); got.Get("Cf-Connecting-Ip") != "" || got.Get("X-Partner-Key") != "" {
		t.Errorf("default not restored: %v", got)
	}
}

func TestHeaderPolicyAPIValidation(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	for name, rules := range map[string][]map[string]any{
		"authorization": {{"action": "set", "name": "Authorization", "value": "Bearer x"}},
		"crlf":          {{"action": "set", "name": "X-A", "value": "a\r\nX-B: 1"}},
	} {
		rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/header-policies", map[string]any{"name": name, "rules": rules})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{
		"target_type": "personal_subscription", "target_id": "nope", "policy_id": store.BuiltinEdgePolicyID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider accepted: %d", rec.Code)
	}
	// Logging mode switch.
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/settings", map[string]any{"mode": "names"})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	h.chatWithHeaders(t, map[string]string{"X-Request-Id": "r1"})
	events := h.waitForUsageEvents(1)
	var l *store.RequestHeaderLog
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && l == nil {
		l, _ = h.store.RequestHeaderLogByUsageID(context.Background(), events[0].ID)
		time.Sleep(20 * time.Millisecond)
	}
	if l == nil {
		t.Fatal("no log")
	}
	for _, in := range l.Inbound {
		if len(in.Values) != 0 {
			t.Errorf("names mode stored values for %s", in.Name)
		}
	}
	// Listing shows every target: default, the upstream, each provider.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/header-policies", nil)
	for _, want := range []string{`"target_type":"default"`, `"target_type":"upstream"`, `"target_id":"openai"`, `"target_id":"xai"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("listing missing %s", want)
		}
	}
	// Request-log endpoint.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/requests/"+events[0].ID+"/headers", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"X-Request-Id"`) {
		t.Errorf("request headers endpoint: %d %s", rec.Code, rec.Body.String())
	}
}

// A policy assigned to a personal-subscription provider applies to every
// user's connection to it, and the default applies to providers with none.
func TestHeaderPolicyPersonalSubscription(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d", rec.Code)
	}
	send := func() http.Header {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"my/xai/grok-test","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.token)
		for k, v := range tunnelHeaders {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.inferenceHeader[len(fake.inferenceHeader)-1]
	}
	// Default (edge strip) applies to the provider.
	if got := send(); got.Get("Cf-Connecting-Ip") != "" || got.Get("Cf-Access-Jwt-Assertion") != "" {
		t.Errorf("edge headers reached xAI: %v", got)
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/header-policies", map[string]any{
		"name": "xAI only", "allow_only": []string{"content-type", "accept"},
		"rules": []map[string]any{{"action": "set", "name": "X-Grok-Conv-Id", "value": "janus"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	pid := decodeBody(t, rec)["policy"].(map[string]any)["id"].(string)
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{
		"target_type": "personal_subscription", "target_id": "xai", "policy_id": pid,
	}); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}
	got := send()
	if got.Get("X-Request-Id") != "" || got.Get("Cf-Connecting-Ip") != "" {
		t.Errorf("allow-list let unlisted headers through: %v", got)
	}
	if got.Get("X-Grok-Conv-Id") != "janus" {
		t.Errorf("set header missing: %v", got)
	}
	if !strings.HasPrefix(got.Get("Authorization"), "Bearer ") || got.Get("Authorization") == "Bearer "+h.token {
		t.Errorf("provider credential wrong: %q", got.Get("Authorization"))
	}
}
