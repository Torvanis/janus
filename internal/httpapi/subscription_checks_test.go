package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// fakeMistral is api.mistral.ai: a key-authenticated model list and chat.
type fakeMistral struct {
	mu       sync.Mutex
	liveKey  string
	chatAuth []string
}

func newFakeMistral(t *testing.T) *fakeMistral {
	f := &fakeMistral{liveKey: "vibe-key-0123456789abcdef"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+f.liveKey {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"devstral-latest","capabilities":{"completion_chat":true}}]}`))
		case "/v1/chat/completions":
			f.chatAuth = append(f.chatAuth, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"id":"m-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	p, _ := subscription.Get("mistral")
	m := p.(*subscription.Mistral)
	saved := *m
	m.APIURL = srv.URL + "/v1"
	t.Cleanup(func() { *m = saved })
	return f
}

// A pasted key connects without a device sign-in, never expires on a
// timer (so it is never "refreshed"), carries traffic, and the check marks
// it for reconnect once the vendor stops accepting it.
func TestPersonalSubscriptionKeyConnectAndCheck(t *testing.T) {
	h := newHarness(t)
	fake := newFakeMistral(t)
	ctx := context.Background()
	sess := adminSession(t, h)
	h.setSubscriptionFeature(t, sess, true)
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"mistral": true}}); rec.Code != http.StatusOK {
		t.Fatalf("enable mistral: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()

	// The list tells the SPA to ask for a key, with where to make one.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/me/subscriptions", nil)
	if !strings.Contains(rec.Body.String(), `"auth":"key"`) || !strings.Contains(rec.Body.String(), "chat.mistral.ai") {
		t.Fatalf("list: %s", rec.Body.String())
	}
	// Device sign-in is refused for a key provider.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/mistral/connect", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("device connect on key provider: %d", rec.Code)
	}
	// A wrong key is refused with the vendor's reason and stores nothing.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/mistral/key", map[string]any{"key": "wrong-key-0123456789abcdef"})
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) || !strings.Contains(rec.Body.String(), "does not accept this key") {
		t.Fatalf("wrong key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.store.UserSubscription(ctx, h.user.ID, "mistral"); err == nil {
		t.Fatal("failed key connect stored a connection")
	}
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/mistral/key", map[string]any{"key": " " + fake.liveKey + " "})
	if !strings.Contains(rec.Body.String(), `"status":"connected"`) || strings.Contains(rec.Body.String(), fake.liveKey) {
		t.Fatalf("key connect: %d %s", rec.Code, rec.Body.String())
	}
	conn, err := h.store.UserSubscription(ctx, h.user.ID, "mistral")
	if err != nil || !conn.AccessExpiresAt.IsZero() || conn.AutoRenews || conn.CheckedAt.IsZero() || conn.CheckError != "" ||
		strings.Contains(conn.EncryptedAccessToken(), fake.liveKey) {
		t.Fatalf("stored key connection: %+v %v", conn, err)
	}
	if rec := h.selectModels(t, sess, conn.ID, "devstral-latest"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsServiceToken(h.token, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"my/mistral/devstral-latest","messages":[{"role":"user","content":"ping"}]}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("chat through key: %d %s", rec.Code, rec.Body.String())
	}
	// Janus's own count shows up on the card.
	var listed struct {
		Providers []struct {
			ID         string `json:"id"`
			Connection *struct {
				Activity map[string]store.SubscriptionActivity `json:"activity"`
			} `json:"connection"`
		} `json:"providers"`
	}
	found := false
	for deadline := time.Now().Add(5 * time.Second); !found && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/me/subscriptions", nil), &listed)
		for _, p := range listed.Providers {
			if p.ID == "mistral" && p.Connection != nil && p.Connection.Activity["day"].Requests == 1 && p.Connection.Activity["week"].TokensIn == 3 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("activity never showed the request: %+v", listed)
	}

	// The vendor revokes the key: "check now" marks the connection for
	// reconnect instead of waiting for the next request to fail.
	fake.mu.Lock()
	fake.liveKey = "rotated-key-000000000000"
	fake.mu.Unlock()
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+conn.ID+"/check", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"reauth_required"`) {
		t.Fatalf("check after revoke: %d %s", rec.Code, rec.Body.String())
	}
}

// The background job claims each due connection once, even when two
// replicas run it at the same moment.
func TestSubscriptionCheckClaimIsExclusive(t *testing.T) {
	h := newHarness(t)
	newFakeMistral(t)
	ctx := context.Background()
	sess := adminSession(t, h)
	h.setSubscriptionFeature(t, sess, true)
	_ = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"mistral": true}})
	h.server.InvalidateConfigCache()
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/mistral/key", map[string]any{"key": "vibe-key-0123456789abcdef"})
	if !strings.Contains(rec.Body.String(), `"connected"`) {
		t.Fatalf("connect: %s", rec.Body.String())
	}
	conn, _ := h.store.UserSubscription(ctx, h.user.ID, "mistral")
	won1, err1 := h.store.ClaimSubscriptionCheck(ctx, conn.ID, conn.CheckedAt)
	won2, err2 := h.store.ClaimSubscriptionCheck(ctx, conn.ID, conn.CheckedAt)
	if err1 != nil || err2 != nil || !won1 || won2 {
		t.Fatalf("claims: %v %v %v %v", won1, won2, err1, err2)
	}
}

// Copilot serves /chat/completions with no /v1 segment; <host>/v1/... is a
// 404. A client calling Janus's /v1/chat/completions must land on the bare
// path, on the account's own host, with the GitHub token as bearer.
func TestCopilotProxyPathHasNoV1(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths, auths = append(paths, r.URL.Path), append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	p, _ := subscription.Get("copilot")
	cp := p.(*subscription.Copilot)
	saved := *cp
	cp.CopilotAPI = srv.URL
	t.Cleanup(func() { *cp = saved })

	sess := adminSession(t, h)
	h.setSubscriptionFeature(t, sess, true)
	_ = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"copilot": true}})
	h.server.InvalidateConfigCache()
	enc, err := h.server.Cipher.Encrypt("gho_live")
	if err != nil {
		t.Fatal(err)
	}
	// GitHub OAuth-app tokens do not expire: zero expiry, no refresh token.
	if _, err := h.store.UpsertSubscription(ctx, store.SubscriptionUpsert{
		UserID: h.user.ID, Provider: "copilot", AccountSubject: "1", AccountEmail: "octo", Name: "octo",
		EncryptedAccess: enc, Models: []string{"gpt-4o"}, SelectedModels: []string{"gpt-4o"},
		Meta: map[string]string{"api": "https://api.individual.githubcopilot.com"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"", `,"stream":false`} {
		rec := h.doAsServiceToken(h.token, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"my/copilot/gpt-4o","messages":[{"role":"user","content":"ping"}]`+stream+`}`))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
			mu.Lock()
			t.Fatalf("copilot chat: %d %s (upstream saw %v)", rec.Code, rec.Body.String(), paths)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if paths[0] != "/chat/completions" || auths[0] != "Bearer gho_live" {
		t.Fatalf("upstream saw path=%v auth=%v", paths, auths)
	}
}

// Reasoning effort is fitted to personal-plan models: from the catalog
// before sending (no failed call at all), and, for a model the catalog is
// silent about, learned from the vendor's refusal and resent once, with the
// next request going out already fitted.
func TestPersonalReasoningEffortFitted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var seen []string // model:effort as the vendor received it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model           string  `json:"model"`
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		eff := "<omitted>"
		if in.ReasoningEffort != nil {
			eff = *in.ReasoningEffort
		}
		mu.Lock()
		seen = append(seen, in.Model+":"+eff)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// "silent" behaves like xAI grok-4.20: refuses the field outright.
		if in.Model == "silent" && in.ReasoningEffort != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid-argument","error":"Model silent does not support parameter reasoningEffort."}`))
			return
		}
		if in.Model == "catalog-none" && in.ReasoningEffort != nil {
			t.Errorf("catalog said no reasoning, yet the field was sent")
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	p, _ := subscription.Get("copilot")
	cp := p.(*subscription.Copilot)
	saved := *cp
	cp.CopilotAPI = srv.URL
	t.Cleanup(func() { *cp = saved })

	sess := adminSession(t, h)
	h.setSubscriptionFeature(t, sess, true)
	_ = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/subscriptions/providers", map[string]any{"enabled": map[string]bool{"copilot": true}})
	h.server.InvalidateConfigCache()
	enc, _ := h.server.Cipher.Encrypt("gho_live")
	facts, _ := json.Marshal(map[string]subscription.Reasoning{"catalog-none": *subscription.NoReasoning()})
	conn, err := h.store.UpsertSubscription(ctx, store.SubscriptionUpsert{
		UserID: h.user.ID, Provider: "copilot", AccountSubject: "1", AccountEmail: "octo", Name: "octo",
		EncryptedAccess: enc, Models: []string{"catalog-none", "silent"}, SelectedModels: []string{"catalog-none", "silent"},
		Reasoning: facts,
	})
	if err != nil {
		t.Fatal(err)
	}
	chat := func(model string) *httptest.ResponseRecorder {
		return h.doAsServiceToken(h.token, http.MethodPost, "/v1/chat/completions",
			[]byte(`{"model":"my/copilot/`+model+`","reasoning_effort":"medium","messages":[{"role":"user","content":"ping"}]}`))
	}
	// Catalog fit: stripped before sending, one upstream call.
	rec := chat("catalog-none")
	if rec.Code != http.StatusOK || rec.Header().Get(headerReasoningAdjusted) != "medium->omitted" {
		t.Fatalf("catalog-none: %d %q %s", rec.Code, rec.Header().Get(headerReasoningAdjusted), rec.Body.String())
	}
	// Learned: refused once, resent without it, caller sees 200.
	rec = chat("silent")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") || rec.Header().Get(headerReasoningAdjusted) != "medium->omitted" {
		t.Fatalf("silent first: %d %q %s", rec.Code, rec.Header().Get(headerReasoningAdjusted), rec.Body.String())
	}
	// Remembered: the next request is fitted before sending.
	if rec = chat("silent"); rec.Code != http.StatusOK {
		t.Fatalf("silent second: %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := strings.Join(seen, " ")
	mu.Unlock()
	if want := "catalog-none:<omitted> silent:medium silent:<omitted> silent:<omitted>"; got != want {
		t.Fatalf("vendor saw %q, want %q", got, want)
	}
	stored, _ := h.store.SubscriptionByID(ctx, conn.ID)
	if f := connReasoning(stored)["silent"]; !f.Learned || f.Supported == nil || *f.Supported {
		t.Fatalf("refusal not remembered: %+v", f)
	}
	// Recorded on the usage event.
	var adj string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT reasoning_adjustment FROM usage_event WHERE model_name = ? ORDER BY created_at DESC LIMIT 1`, "my/copilot/silent").Scan(&adj); err != nil || adj != "medium->omitted" {
		t.Fatalf("usage reasoning_adjustment = %q (%v)", adj, err)
	}
	// Advertised: the no-reasoning model lists no efforts.
	rec = h.doAsServiceToken(h.token, http.MethodGet, "/v1/models", nil)
	if !strings.Contains(rec.Body.String(), `"reasoning_efforts":[]`) {
		t.Fatalf("/v1/models does not advertise reasoning_efforts: %s", rec.Body.String())
	}
}
