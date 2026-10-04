package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// fakeXAI is a stand-in for auth.x.ai + api.x.ai: device-code grant with one
// pending round, rotating refresh tokens, userinfo, revoke, and an
// OpenAI-shaped inference API that checks the bearer token.
type fakeXAI struct {
	mu            sync.Mutex
	srv           *httptest.Server
	subject       string
	pendingLeft   int
	accessSeq     int
	liveAccess    string
	liveRefresh   string
	refreshFails  bool
	revoked       []string
	inferenceAuth []string
	inferenceBody []string
}

func newFakeXAI(t *testing.T) *fakeXAI {
	f := &fakeXAI{subject: "xai-account-1", pendingLeft: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	p, ok := subscription.Get("xai")
	if !ok {
		t.Fatal("xai provider not registered")
	}
	x := p.(*subscription.XAI)
	saved := *x
	x.ClientID = "test-client"
	x.DeviceURL = f.srv.URL + "/oauth2/device/code"
	x.TokenURL = f.srv.URL + "/oauth2/token"
	x.UserinfoURL = f.srv.URL + "/oauth2/userinfo"
	x.RevokeURL = f.srv.URL + "/oauth2/revoke"
	x.InferenceURL = f.srv.URL + "/v1"
	t.Cleanup(func() { *x = saved })
	return f
}

func (f *fakeXAI) mint() map[string]any {
	f.accessSeq++
	f.liveAccess = "access-" + itoa(f.accessSeq)
	f.liveRefresh = "refresh-" + itoa(f.accessSeq)
	return map[string]any{"access_token": f.liveAccess, "refresh_token": f.liveRefresh, "expires_in": 3600, "token_type": "Bearer"}
}

func (f *fakeXAI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	w.Header().Set("Content-Type", "application/json")
	writeJSON := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/oauth2/device/code":
		writeJSON(200, map[string]any{"device_code": "dev-123", "user_code": "ABCD-EFGH",
			"verification_uri": "https://accounts.x.ai/device", "verification_uri_complete": "https://accounts.x.ai/device?code=ABCD-EFGH",
			"expires_in": 600, "interval": 1})
	case "/oauth2/token":
		switch form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if f.pendingLeft > 0 {
				f.pendingLeft--
				writeJSON(400, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(200, f.mint())
		case "refresh_token":
			if f.refreshFails || form.Get("refresh_token") != f.liveRefresh {
				writeJSON(400, map[string]any{"error": "invalid_grant", "error_description": "refresh token revoked"})
				return
			}
			writeJSON(200, f.mint())
		}
	case "/oauth2/userinfo":
		if bearer != f.liveAccess {
			writeJSON(401, map[string]any{"error": "invalid_token"})
			return
		}
		writeJSON(200, map[string]any{"sub": f.subject, "email": "owner@example.com", "name": "Owner"})
	case "/oauth2/revoke":
		f.revoked = append(f.revoked, form.Get("token"))
		w.WriteHeader(200)
	case "/v1/models":
		if bearer != f.liveAccess {
			writeJSON(401, map[string]any{"error": "invalid_token"})
			return
		}
		writeJSON(200, map[string]any{"data": []map[string]any{{"id": "grok-test"}, {"id": "grok-test-fast"}}})
	case "/v1/chat/completions":
		f.inferenceAuth = append(f.inferenceAuth, bearer)
		f.inferenceBody = append(f.inferenceBody, string(body))
		if bearer != f.liveAccess {
			writeJSON(401, map[string]any{"error": "invalid_token"})
			return
		}
		// xAI reports a list-price cost; for a personal plan it must not
		// become organization spend.
		writeJSON(200, map[string]any{"id": "x-1", "choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 5, "cost_in_usd_ticks": 123456789}})
	default:
		w.WriteHeader(404)
	}
}

func (h *harness) enableXAISubscriptions(t *testing.T, sess *auth.Session, on bool) {
	t.Helper()
	if on {
		h.setSubscriptionFeature(t, sess, true)
	}
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"xai": on}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set providers: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
}

func (h *harness) setSubscriptionFeature(t *testing.T, sess *auth.Session, on bool) {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/feature", map[string]any{"enabled": on})
	if rec.Code != http.StatusOK {
		t.Fatalf("set feature: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
}

func (h *harness) selectModels(t *testing.T, sess *auth.Session, connID string, models ...string) *httptest.ResponseRecorder {
	t.Helper()
	return h.doAsSession(sess, http.MethodPut, "/api/v1/me/subscriptions/"+connID+"/models", map[string]any{"selected": models})
}

// connectXAI drives the device flow through the public API and returns the
// connection id.
func (h *harness) connectXAI(t *testing.T, sess *auth.Session) (string, map[string]any) {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/xai/connect", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start connect: %d %s", rec.Code, rec.Body.String())
	}
	var start struct {
		PendingID string `json:"pending_id"`
		UserCode  string `json:"user_code"`
		URI       string `json:"verification_uri_complete"`
	}
	decodeInto(t, rec, &start)
	if start.UserCode != "ABCD-EFGH" || !strings.Contains(start.URI, "accounts.x.ai") {
		t.Fatalf("start payload: %+v", start)
	}
	// Polling before the interval elapses never reaches the vendor.
	var out map[string]any
	for i := 0; i < 10; i++ {
		time.Sleep(1100 * time.Millisecond)
		rec = h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/connect/"+start.PendingID+"/poll", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
		}
		out = map[string]any{}
		decodeInto(t, rec, &out)
		if out["status"] != "pending" {
			break
		}
	}
	if out["status"] != "connected" {
		return "", out
	}
	return out["connection"].(map[string]any)["id"].(string), out
}

func TestPersonalSubscriptionEndToEnd(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	ctx := context.Background()
	sess := adminSession(t, h)
	chat := func(token, model string) *httptest.ResponseRecorder {
		return h.doAsServiceToken(token, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	}

	// Off by default: neither connecting nor calling works.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/xai/connect", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("connect while disabled: want 403, got %d", rec.Code)
	}
	if rec := chat(h.token, "my/xai/grok-test"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "turned off") {
		t.Fatalf("proxy while disabled: %d %s", rec.Code, rec.Body.String())
	}

	h.enableXAISubscriptions(t, sess, true)
	if rec := chat(h.token, "my/xai/grok-test"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("proxy before connect: %d %s", rec.Code, rec.Body.String())
	}

	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect did not complete: %v", out)
	}
	conn, err := h.store.SubscriptionByID(ctx, connID)
	if err != nil || conn.UserID != h.user.ID || conn.AccountEmail != "owner@example.com" || len(conn.Models) != 2 || len(conn.SelectedModels) != 0 {
		t.Fatalf("stored connection: %+v err=%v", conn, err)
	}
	// A new connection offers nothing until the owner picks models.
	if rec := h.do(http.MethodGet, "/v1/models", nil); strings.Contains(rec.Body.String(), "my/xai/") {
		t.Fatalf("/v1/models lists unselected models: %s", rec.Body.String())
	}
	if rec := chat(h.token, "my/xai/grok-test"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not turned on") {
		t.Fatalf("unselected model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.selectModels(t, sess, connID, "grok-nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("selecting an unknown model: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"selected_models":["grok-test"]`) {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/v1/models", nil); !strings.Contains(rec.Body.String(), `"my/xai/grok-test"`) || strings.Contains(rec.Body.String(), "grok-test-fast") {
		t.Fatalf("/v1/models after selecting one: %s", rec.Body.String())
	}
	if rec := h.selectModels(t, sess, connID, "grok-test", "grok-test-fast"); rec.Code != http.StatusOK {
		t.Fatalf("select both: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(conn.EncryptedAccessToken(), "access-1") || strings.Contains(conn.EncryptedRefreshToken(), "refresh-1") {
		t.Fatal("tokens stored in plaintext")
	}
	// The API never returns token material.
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/me/subscriptions", nil); rec.Code != 200 ||
		strings.Contains(rec.Body.String(), "access-1") || strings.Contains(rec.Body.String(), "refresh-1") || !strings.Contains(rec.Body.String(), `"grok-test"`) {
		t.Fatalf("list subscriptions: %d %s", rec.Code, rec.Body.String())
	}

	// /v1/models lists the personal names for the owner.
	rec := h.do(http.MethodGet, "/v1/models", nil)
	if !strings.Contains(rec.Body.String(), `"my/xai/grok-test"`) || !strings.Contains(rec.Body.String(), `"test-model"`) {
		t.Fatalf("/v1/models: %s", rec.Body.String())
	}

	// Inference through the owner's plan.
	rec = chat(h.token, "my/xai/grok-test")
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	gotAuth, gotBody := fake.inferenceAuth[len(fake.inferenceAuth)-1], fake.inferenceBody[len(fake.inferenceBody)-1]
	fake.mu.Unlock()
	if gotAuth != "access-1" || !strings.Contains(gotBody, `"model":"grok-test"`) {
		t.Fatalf("upstream saw auth=%q body=%s", gotAuth, gotBody)
	}
	var ev *store.UsageEvent
	for deadline := time.Now().Add(5 * time.Second); ev == nil && time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		evs, _, err := h.store.ListRequests(ctx, store.RequestFilter{Limit: 100, Source: "subscription"})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.HTTPStatus == http.StatusOK {
				ev = e
			}
		}
	}
	if ev == nil {
		t.Fatal("no successful personal usage event recorded")
	}
	if ev.SubscriptionID != connID || ev.CostNano != 0 || ev.TokensIn != 12 || ev.TokensOut != 5 ||
		ev.ModelName != "my/xai/grok-test" || ev.UserID != h.user.ID {
		t.Fatalf("usage event: %+v", ev)
	}
	// Reporting facts: known-free (not "unpriced"), classified as xAI.
	var costStatus, provider, hosting string
	var classified int
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := h.store.DB().QueryRowContext(ctx, `SELECT cost_status, provider, hosting, classification_known FROM reporting_usage_snapshot WHERE usage_id = ?`, ev.ID).
			Scan(&costStatus, &provider, &hosting, &classified)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if costStatus != "known_free" || provider != "xai" || hosting != "external" || classified != 1 {
		t.Fatalf("report fact: cost_status=%q provider=%q hosting=%q classified=%d", costStatus, provider, hosting, classified)
	}

	// The request log separates personal from organization traffic.
	var list struct {
		Total int `json:"total_count"`
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/requests?source=organization&status=success", nil)
	decodeInto(t, rec, &list)
	if list.Total != 0 {
		t.Fatalf("organization filter should exclude personal traffic: %s", rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/requests?source=subscription&status=success", nil)
	decodeInto(t, rec, &list)
	if list.Total != 1 {
		t.Fatalf("subscription filter: %s", rec.Body.String())
	}

	// A different user cannot reach the owner's plan and cannot connect
	// the same vendor account.
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other", false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := h.store.CreateToken(ctx, other.ID, "other token")
	if err != nil {
		t.Fatal(err)
	}
	if rec := chat(otherToken, "my/xai/grok-test"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("other user proxy: %d %s", rec.Code, rec.Body.String())
	}
	otherSess, err := h.server.Sessions.Create(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(otherSess, http.MethodDelete, "/api/v1/me/subscriptions/"+connID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other user deleting owner's connection: want 404, got %d", rec.Code)
	}
	fake.mu.Lock()
	fake.pendingLeft = 0
	fake.mu.Unlock()
	if id, out := h.connectXAI(t, otherSess); id != "" || out["status"] != "failed" || !strings.Contains(out["message"].(string), "another Janus user") {
		t.Fatalf("second user connecting same account: id=%q out=%v", id, out)
	}
	// That attempt minted tokens at the fake and revoked them; the owner's
	// stored refresh token (refresh-1) is now stale at the fake, exactly
	// like a real rotation by another client. Put the owner's back.
	fake.mu.Lock()
	fake.liveAccess, fake.liveRefresh = "access-1", "refresh-1"
	fake.mu.Unlock()

	// Service tokens can never use a personal plan.
	_, svc := h.newServiceToken(t, "bot", false)
	if rec := chat(svc, "my/xai/grok-test"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "service token") {
		t.Fatalf("service token: %d %s", rec.Code, rec.Body.String())
	}

	// Expired access token: refreshed transparently, rotated refresh stored.
	if err := h.store.ExpireSubscriptionAccess(ctx, connID); err != nil {
		t.Fatal(err)
	}
	if rec := chat(h.token, "my/xai/grok-test-fast"); rec.Code != http.StatusOK {
		t.Fatalf("proxy after expiry: %d %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	gotAuth, live := fake.inferenceAuth[len(fake.inferenceAuth)-1], fake.liveAccess
	fake.mu.Unlock()
	if gotAuth != live || gotAuth == "access-1" {
		t.Fatalf("refresh not used: sent %q, live %q", gotAuth, live)
	}

	// Refresh revoked at the vendor: the connection flips to reconnect and
	// callers get a code they can branch on.
	fake.mu.Lock()
	fake.refreshFails = true
	fake.mu.Unlock()
	_ = h.store.ExpireSubscriptionAccess(ctx, connID)
	rec = chat(h.token, "my/xai/grok-test")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeSubscriptionReauth) {
		t.Fatalf("revoked refresh: %d %s", rec.Code, rec.Body.String())
	}
	if c, _ := h.store.SubscriptionByID(ctx, connID); c.Status != store.SubscriptionReauthRequired || c.LastError == "" {
		t.Fatalf("connection not marked for reconnect: %+v", c)
	}
	if rec := h.do(http.MethodGet, "/v1/models", nil); strings.Contains(rec.Body.String(), "my/xai/") {
		t.Fatalf("/v1/models still lists a broken subscription: %s", rec.Body.String())
	}

	// Disconnect: revoked at the vendor, removed locally, audited.
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/me/subscriptions/"+connID, nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.store.SubscriptionByID(ctx, connID); err != store.ErrNotFound {
		t.Fatalf("connection survived disconnect: %v", err)
	}
	fake.mu.Lock()
	revoked := append([]string{}, fake.revoked...)
	fake.mu.Unlock()
	if len(revoked) < 2 {
		t.Fatalf("vendor revoke calls: %v", revoked)
	}
	audits, _, err := h.store.ListAudit(ctx, store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
	}
	for _, want := range []string{"personal_subscriptions_updated", "subscription_providers_updated", "subscription_connected",
		"subscription_models_selected", "subscription_disconnected"} {
		if !seen[want] {
			t.Fatalf("missing audit %s in %v", want, seen)
		}
	}
	// Historical usage keeps its attribution after the connection is gone.
	if evs, _, _ := h.store.ListRequests(ctx, store.RequestFilter{Limit: 100, Source: "subscription"}); len(evs) < 2 {
		t.Fatalf("usage lost its subscription attribution: %d events", len(evs))
	}
}

func TestPersonalModelNameParsing(t *testing.T) {
	cases := map[string][3]string{
		"my/xai/grok-4.3":  {"xai", "grok-4.3", "ok"},
		"my/xai/org/model": {"xai", "org/model", "ok"},
		"my/xai/":          {"", "", ""},
		"my//grok":         {"", "", ""},
		"xai/grok":         {"", "", ""},
		"grok-4.3":         {"", "", ""},
	}
	for in, want := range cases {
		p, m, ok := subscription.ParseModel(in)
		if ok != (want[2] == "ok") || (ok && (p != want[0] || m != want[1])) {
			t.Errorf("ParseModel(%q) = %q %q %v", in, p, m, ok)
		}
	}
}

// The organization switch hides the whole feature without losing anyone's
// connection; turning it back on restores exactly what was there.
func TestPersonalSubscriptionMasterSwitch(t *testing.T) {
	h := newHarness(t)
	newFakeXAI(t)
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d", rec.Code)
	}
	me := func() string { return h.doAsSession(sess, http.MethodGet, "/api/v1/me", nil).Body.String() }
	if !strings.Contains(me(), `"personal_subscriptions":true`) {
		t.Fatalf("/me with feature on: %s", me())
	}

	h.setSubscriptionFeature(t, sess, false)
	if !strings.Contains(me(), `"personal_subscriptions":false`) {
		t.Fatalf("/me with feature off: %s", me())
	}
	if rec := h.do(http.MethodGet, "/v1/models", nil); strings.Contains(rec.Body.String(), "my/xai/") {
		t.Fatalf("/v1/models with feature off: %s", rec.Body.String())
	}
	rec := h.doAsServiceToken(h.token, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"my/xai/grok-test","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "turned off") {
		t.Fatalf("proxy with feature off: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/me/subscriptions", nil); !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Fatalf("list with feature off: %s", rec.Body.String())
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusForbidden {
		t.Fatalf("select with feature off: %d", rec.Code)
	}
	// Binding a policy to personal traffic needs the feature.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": "x", "scope_type": "personal_subscription", "scope_id": "*"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "turned off") {
		t.Fatalf("binding with feature off: %d %s", rec.Code, rec.Body.String())
	}

	h.setSubscriptionFeature(t, sess, true)
	if conn, err := h.store.SubscriptionByID(context.Background(), connID); err != nil || len(conn.SelectedModels) != 1 {
		t.Fatalf("connection lost across the switch: %+v %v", conn, err)
	}
	if rec := h.do(http.MethodGet, "/v1/models", nil); !strings.Contains(rec.Body.String(), `"my/xai/grok-test"`) {
		t.Fatalf("/v1/models after re-enabling: %s", rec.Body.String())
	}
}

// A policy bound to personal subscriptions reaches personal traffic on the
// real proxy path, and leaves organization traffic alone.
func TestPersonalSubscriptionSecurityBinding(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // blocking checks are Business
	newFakeXAI(t)
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d", rec.Code)
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{
		"name": "Personal plans", "enabled": true,
		"checks": []map[string]any{{"kind": "secrets", "enabled": true, "mode": "block", "direction": "ingress"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create policy: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Policy store.SecgwPolicy `json:"policy"`
	}
	decodeInto(t, rec, &created)
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": created.Policy.ID, "scope_type": "personal_subscription", "scope_id": "anthropic"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider binding: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": created.Policy.ID, "scope_type": "personal_subscription", "scope_id": "xai"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/bindings", nil); !strings.Contains(rec.Body.String(), `"scope_name":"xAI`) {
		t.Fatalf("binding scope name: %s", rec.Body.String())
	}
	leak := `{"model":"my/xai/grok-test","messages":[{"role":"user","content":"key ` + testAWSKey + `"}]}`
	if rec := h.doAsServiceToken(h.token, http.MethodPost, "/v1/chat/completions", []byte(leak)); rec.Code != http.StatusForbidden {
		t.Fatalf("personal traffic not covered: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("key "+testAWSKey, false)); rec.Code != http.StatusOK {
		t.Fatalf("organization traffic caught by a personal binding: %d %s", rec.Code, rec.Body.String())
	}
}

// Offline installs never offer personal subscriptions, whatever is stored.
func TestPersonalSubscriptionsHiddenOffline(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	h.setSubscriptionFeature(t, sess, true)
	h.server.Config.Offline = true
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/me", nil); !strings.Contains(rec.Body.String(), `"personal_subscriptions":false`) {
		t.Fatalf("/me offline: %s", rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/subscriptions", nil); !strings.Contains(rec.Body.String(), `"offline":true`) {
		t.Fatalf("admin view offline: %s", rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/feature", map[string]any{"enabled": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("enable offline: %d", rec.Code)
	}
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/xai/connect", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("connect offline: %d", rec.Code)
	}
}

func TestAdminSubscriptionProvidersRejectsUnknown(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"anthropic": true}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Unknown subscription provider") {
		t.Fatalf("unknown provider: %d %s", rec.Code, rec.Body.String())
	}
}
