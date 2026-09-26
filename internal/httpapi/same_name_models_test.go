package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// A vLLM pod moved to a new node shows up as a second upstream serving the
// same model name. The admin grants and prices the new copy; the old copy
// still exists (enabled, ungranted). A request for the name must reach the
// copy the caller is granted and bill at that copy's rate — before this fix
// it resolved to whichever copy sorted first and answered
// policy.model_not_granted, so the admin had to delete grants and recreate
// them on the same name to get traffic flowing.
func TestProxyRoutesSameNameToTheGrantedUpstream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var hits = map[string]int{}
	newUpstream := func(tag string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[tag]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"from ` + tag + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	// "a-old" sorts before "z-new" by upstream name, which is exactly the
	// order the old lookup used, so the ungranted copy would win.
	oldSrv, newSrv := newUpstream("old"), newUpstream("new")
	addCopy := func(upName, url string, rateIn int64) *store.Model {
		up, err := h.store.CreateUpstream(ctx, upName, "openai_compatible", url, "", "")
		if err != nil {
			t.Fatalf("create upstream: %v", err)
		}
		if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "moved-model", []string{"chat"}); err != nil {
			t.Fatalf("discover: %v", err)
		}
		m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, "moved-model")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		enabled := store.ModelEnabled
		if err := h.store.PatchModel(ctx, m.ID, store.ModelPatch{Status: &enabled, Overrides: map[string]*int64{
			"rate_in_nanousd": &rateIn,
		}}); err != nil {
			t.Fatalf("price+enable: %v", err)
		}
		return m
	}
	addCopy("a-old-node", oldSrv.URL, 1_000_000_000) // $1/Mtok, not granted
	time.Sleep(5 * time.Millisecond)
	granted := addCopy("z-new-node", newSrv.URL, 7_000_000_000) // $7/Mtok, granted
	if _, err := h.store.CreateGrant(ctx, granted.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "moved-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200 from the granted copy: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Choices) == 0 || body.Choices[0].Message.Content != "from new" || hits["old"] != 0 {
		t.Fatalf("served by %v (hits %v), want only the granted new-node copy", body.Choices, hits)
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ModelID != granted.ID || event.UpstreamID != granted.UpstreamID {
		t.Fatalf("logged model %s on %s, want the granted copy %s on %s", event.ModelID, event.UpstreamID, granted.ID, granted.UpstreamID)
	}
	if event.CostNano != 7_000_000_000 {
		t.Fatalf("cost = %d nano-USD, want 7e9 (the granted copy's own rate card)", event.CostNano)
	}

	// Revoking the only grant still refuses the name: the ungranted copy
	// is never a silent fallback.
	grants, _ := h.store.ListGrants(ctx, granted.ID)
	for _, g := range grants {
		_ = h.store.DeleteGrant(ctx, g.ID)
	}
	h.server.InvalidateConfigCache()
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "moved-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with no grant on any copy the proxy returned %d, want 403", rec.Code)
	}
}

// The other half of "the vLLM pod moved": the admin edits the EXISTING
// upstream's address instead of adding a new one. The model row, its grants
// and its rate card belong to the upstream, not to the URL, so traffic
// follows the new address with nothing to recreate.
func TestUpstreamAddressChangeKeepsGrantsAndRates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"from the new node"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	t.Cleanup(moved.Close)
	grantsBefore, _ := h.store.ListGrants(ctx, h.model.ID)
	before, err := h.store.ModelByID(ctx, h.model.ID)
	if err != nil || before.RateInNano == 0 {
		t.Fatalf("fixture: want a priced model, got %+v (%v)", before, err)
	}
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	rec := h.doAsSession(session, http.MethodPut, "/api/v1/admin/upstreams/"+h.model.UpstreamID, map[string]any{
		"base_url": moved.URL, "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update upstream address returned %d: %s", rec.Code, rec.Body.String())
	}
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": h.model.Name, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from the new node") {
		t.Fatalf("after the address change the request returned %d: %s", rec.Code, rec.Body.String())
	}
	after, err := h.store.ModelByID(ctx, h.model.ID)
	if err != nil || after.RateInNano != before.RateInNano || after.RateOutNano != before.RateOutNano || after.Status != store.ModelEnabled {
		t.Fatalf("model after the move: status=%s rates=%d/%d (%v), want enabled at %d/%d", after.Status, after.RateInNano, after.RateOutNano, err, before.RateInNano, before.RateOutNano)
	}
	grantsAfter, _ := h.store.ListGrants(ctx, h.model.ID)
	if len(grantsAfter) != len(grantsBefore) || len(grantsAfter) == 0 {
		t.Fatalf("grants before=%d after=%d, want them untouched", len(grantsBefore), len(grantsAfter))
	}
}
