package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

type capabilityView struct {
	Keys     []adapter.CapabilityKey        `json:"keys"`
	Resolved map[string]capability.Resolved `json:"resolved"`
	Provider map[string]capability.Fact     `json:"provider"`
	Probe    map[string]capability.Fact     `json:"probe"`
	Admin    map[string]capability.Fact     `json:"admin"`
}

func TestModelCapabilitiesAdminAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)
	path := "/api/v1/admin/models/" + h.model.ID + "/capabilities"
	if err := h.store.SetDiscoveredCapabilities(ctx, h.model.UpstreamID, h.model.Name, capability.FromProvider(adapter.Capabilities{adapter.CapInImage: false, adapter.CapTools: true}, time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	view := func(body []byte) capabilityView {
		t.Helper()
		var v capabilityView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return v
	}

	rec := h.doAsSession(admin, http.MethodGet, path, nil)
	v := view(rec.Body.Bytes())
	if rec.Code != http.StatusOK || len(v.Keys) < 20 || v.Resolved[adapter.CapInImage].Source != capability.SourceProvider || v.Resolved[adapter.CapInImage].Value != false || len(v.Admin) != 0 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	rec = h.doAsSession(admin, http.MethodPut, path, map[string]any{"overrides": map[string]any{
		adapter.CapInImage: true, adapter.CapThinkingLevels: []string{"low", "high"}, adapter.CapOutputCap: adapter.OutputCapMaxCompletionTokens}})
	v = view(rec.Body.Bytes())
	if rec.Code != http.StatusOK || v.Resolved[adapter.CapInImage].Value != true || v.Resolved[adapter.CapInImage].Source != capability.SourceAdmin || v.Provider[adapter.CapInImage].Value != false {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	// The catalog carries the value in force.
	rec = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models", nil)
	if !strings.Contains(rec.Body.String(), `"input.image":{"value":true,"source":"admin"`) {
		t.Errorf("the model list does not carry capabilities: %.400s", rec.Body.String())
	}
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "model_capabilities_updated", Limit: 5})
	if err != nil || len(entries) != 1 || !strings.Contains(string(entries[0].NewValue), "max_completion_tokens") {
		t.Errorf("override not audited: %v %+v", err, entries)
	}

	// Clearing returns to what the provider said.
	rec = h.doAsSession(admin, http.MethodPut, path, map[string]any{"overrides": map[string]any{adapter.CapInImage: nil}})
	if v = view(rec.Body.Bytes()); rec.Code != http.StatusOK || v.Resolved[adapter.CapInImage].Source != capability.SourceProvider || len(v.Admin) != 2 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}

	for name, body := range map[string]any{
		"unknown key": map[string]any{"overrides": map[string]any{"input.smell": true}},
		"wrong type":  map[string]any{"overrides": map[string]any{adapter.CapInImage: "yes"}},
		"bad enum":    map[string]any{"overrides": map[string]any{adapter.CapThinkingOff: "sometimes"}},
		"empty":       map[string]any{"overrides": map[string]any{}},
	} {
		if rec := h.doAsSession(admin, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if got, _ := h.store.ModelByID(ctx, h.model.ID); len(got.CapabilityState().Admin) != 2 {
		t.Errorf("a refused edit changed the overrides: %+v", got.CapabilityState().Admin)
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models/nope/capabilities", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown model: %d", rec.Code)
	}
	// Not an administrator.
	_, token := h.otherUser(t, "caps-member", "member@example.com")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if rec := h.doWithToken(token, method, path, map[string]any{"overrides": map[string]any{adapter.CapInImage: true}}); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin %s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
}
