package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// seedFallbackPair creates a second enabled model on the harness upstream and
// an alias whose target is the harness model and whose fallback is the second
// model, granted to all users. It returns the alias and the fallback model.
func seedFallbackPair(t *testing.T, h *harness, triggers []string) (*store.ManagedModel, *store.Model) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "backup-model", []string{"chat"}); err != nil {
		t.Fatalf("seed fallback model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	var backup *store.Model
	for _, m := range models {
		if m.Name == "backup-model" {
			backup = m
		}
	}
	if backup == nil {
		t.Fatal("expected the backup model")
	}
	if err := h.store.SetModelStatus(ctx, backup.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable backup: %v", err)
	}
	mm, err := h.store.CreateManagedModel(ctx, "current-best", "", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, backup.ID, triggers); err != nil {
		t.Fatalf("set fallback: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache(mm.Name)
	mm, err = h.store.ManagedModelByID(ctx, mm.ID)
	if err != nil {
		t.Fatalf("reload alias: %v", err)
	}
	return mm, backup
}

func (h *harness) callAlias(t *testing.T) (int, http.Header, string) {
	t.Helper()
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "current-best", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	return rec.Code, rec.Header(), rec.Body.String()
}

// Store-level contract: a fallback must be a real, different catalog model —
// never an alias (no chains) and never the target itself — and defaults its
// triggers to the outage set when none are chosen.
func TestManagedModelFallbackValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mm, backup := seedFallbackPair(t, h, nil)

	if got := strings.Join(mm.FallbackTriggers, ","); got != strings.Join(store.DefaultFallbackTriggers, ",") {
		t.Fatalf("default triggers = %q, want %q", got, store.DefaultFallbackTriggers)
	}
	if mm.FallbackPublicName != "backup-model" || mm.FallbackBroken {
		t.Fatalf("fallback decoration = %+v", mm)
	}

	other, err := h.store.CreateManagedModel(ctx, "other-alias", "", backup.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create second alias: %v", err)
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, other.ID, nil); err == nil {
		t.Fatal("a managed model must be refused as a fallback (aliases do not chain)")
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, h.model.ID, nil); err == nil {
		t.Fatal("the target itself must be refused as a fallback")
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, "nope", nil); err == nil {
		t.Fatal("an unknown model must be refused as a fallback")
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, backup.ID, []string{"on_monday"}); err == nil {
		t.Fatal("an unknown trigger must be refused")
	}
	// Repointing the target at the fallback is refused too.
	if err := h.store.SetManagedModelTarget(ctx, mm.ID, backup.ID); err == nil {
		t.Fatal("repointing the target onto its own fallback must be refused")
	}
	// Clearing works and drops the triggers.
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, "", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	mm, _ = h.store.ManagedModelByID(ctx, mm.ID)
	if mm.FallbackModelID != "" || len(mm.FallbackTriggers) != 0 {
		t.Fatalf("after clear = %+v", mm)
	}
}

// Configuration-level unavailability: the target is disabled, so every request
// to the alias is served by the fallback and says so.
func TestManagedModelFallsBackWhenTargetDisabled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedFallbackPair(t, h, nil)

	if code, _, body := h.callAlias(t); code != http.StatusOK {
		t.Fatalf("healthy alias returned %d: %s", code, body)
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if !strings.Contains(string(last.Body), `"model":"test-model"`) {
		t.Fatalf("healthy alias must reach the target, upstream saw %s", last.Body)
	}

	if err := h.store.SetModelStatus(ctx, h.model.ID, store.ModelDisabled); err != nil {
		t.Fatalf("disable target: %v", err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("current-best")

	code, hdr, body := h.callAlias(t)
	if code != http.StatusOK {
		t.Fatalf("alias with a fallback returned %d, want 200: %s", code, body)
	}
	if got := hdr.Get(headerFallbackReason); got != store.FallbackTriggerTargetUnavailable {
		t.Errorf("%s = %q, want %q", headerFallbackReason, got, store.FallbackTriggerTargetUnavailable)
	}
	last = h.upstreamRequests[len(h.upstreamRequests)-1]
	if !strings.Contains(string(last.Body), `"model":"backup-model"`) {
		t.Errorf("fallback request must reach the backup model, upstream saw %s", last.Body)
	}
	events := h.waitForUsageEvents(2)
	if events[0].ModelName != "backup-model" || events[0].RequestedModelName != "current-best" {
		t.Errorf("event = model %q requested %q, want backup-model / current-best", events[0].ModelName, events[0].RequestedModelName)
	}
	if events[0].FallbackReason != store.FallbackTriggerTargetUnavailable {
		t.Errorf("event fallback_reason = %q, want %q recorded for audit", events[0].FallbackReason, store.FallbackTriggerTargetUnavailable)
	}
	if events[1].FallbackReason != "" {
		t.Errorf("the earlier request served by the target must carry no fallback_reason, got %q", events[1].FallbackReason)
	}

	// The alias is servable in the admin view because its fallback covers it.
	mm, _ := h.store.ListManagedModels(ctx, store.ManagedModelFilter{})
	if len(mm) != 1 || !mm[0].Broken || !mm[0].Servable {
		t.Errorf("admin view = broken %v servable %v, want broken-but-servable", mm[0].Broken, mm[0].Servable)
	}
}

// Runtime unavailability: the target's upstream failed its latest probe. The
// fallback lives on a healthy upstream and takes over; when the probe clears,
// traffic returns to the target without any admin action.
func TestManagedModelFallsBackWhenUpstreamUnreachable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mm, _ := seedFallbackPair(t, h, nil)

	// Move the fallback to its own (healthy) upstream so the probe state of
	// the target's upstream does not also condemn the fallback.
	healthy, err := h.store.CreateUpstream(ctx, "healthy", "openai_compatible", h.upstream.URL, "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, healthy.ID, "backup-model", []string{"chat"}); err != nil {
		t.Fatalf("seed backup on healthy upstream: %v", err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: healthy.ID})
	if len(models) != 1 {
		t.Fatalf("expected one model on the healthy upstream, got %d", len(models))
	}
	backup := models[0]
	if err := h.store.SetModelStatus(ctx, backup.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := h.store.SetManagedModelFallback(ctx, mm.ID, backup.ID, nil); err != nil {
		t.Fatalf("repoint fallback: %v", err)
	}
	if err := h.store.RecordUpstreamCheck(ctx, healthy.ID, 5*time.Millisecond, ""); err != nil {
		t.Fatalf("probe healthy: %v", err)
	}
	if err := h.store.RecordUpstreamCheck(ctx, h.model.UpstreamID, 0, "dial tcp: connection refused"); err != nil {
		t.Fatalf("probe target: %v", err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("current-best")

	code, hdr, body := h.callAlias(t)
	if code != http.StatusOK {
		t.Fatalf("returned %d: %s", code, body)
	}
	if got := hdr.Get(headerFallbackReason); got != store.FallbackTriggerUpstreamUnreachable {
		t.Errorf("%s = %q, want %q", headerFallbackReason, got, store.FallbackTriggerUpstreamUnreachable)
	}
	if last := h.upstreamRequests[len(h.upstreamRequests)-1]; !strings.Contains(string(last.Body), `"model":"backup-model"`) {
		t.Errorf("upstream saw %s, want the backup model", last.Body)
	}

	// Probe recovers → the target serves again.
	if err := h.store.RecordUpstreamCheck(ctx, h.model.UpstreamID, 5*time.Millisecond, ""); err != nil {
		t.Fatalf("probe recover: %v", err)
	}
	h.server.InvalidateConfigCache()
	code, hdr, body = h.callAlias(t)
	if code != http.StatusOK || hdr.Get(headerFallbackReason) != "" {
		t.Fatalf("after recovery: %d %q %s", code, hdr.Get(headerFallbackReason), body)
	}
	if last := h.upstreamRequests[len(h.upstreamRequests)-1]; !strings.Contains(string(last.Body), `"model":"test-model"`) {
		t.Errorf("upstream saw %s, want the target again", last.Body)
	}
}

// An alias whose triggers exclude upstream_unreachable keeps serving its
// target through a failed probe: the admin chose the sensitivity.
func TestManagedModelRespectsChosenTriggers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedFallbackPair(t, h, []string{store.FallbackTriggerTargetUnavailable})
	if err := h.store.RecordUpstreamCheck(ctx, h.model.UpstreamID, 0, "boom"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	h.server.InvalidateConfigCache()
	code, hdr, body := h.callAlias(t)
	if code != http.StatusOK || hdr.Get(headerFallbackReason) != "" {
		t.Fatalf("returned %d fallback=%q: %s", code, hdr.Get(headerFallbackReason), body)
	}
}

// Target AND fallback both unavailable: the request fails with its own error
// code that names both faults.
func TestManagedModelFallbackExhausted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, backup := seedFallbackPair(t, h, nil)
	if err := h.store.SetModelStatus(ctx, h.model.ID, store.ModelDisabled); err != nil {
		t.Fatalf("disable target: %v", err)
	}
	if err := h.store.SetModelStatus(ctx, backup.ID, store.ModelDisabled); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("current-best")

	code, _, body := h.callAlias(t)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("returned %d, want 503: %s", code, body)
	}
	if !strings.Contains(body, CodeManagedModelFallbackExhausted) {
		t.Errorf("expected %s, got %s", CodeManagedModelFallbackExhausted, body)
	}
	if !strings.Contains(body, "fallback") || !strings.Contains(body, "administrator") {
		t.Errorf("message must name the fallback and point at an administrator: %s", body)
	}
}

// Admin API: PATCH sets, re-triggers and clears the fallback, audits the
// change, and refuses invalid selections with 400.
func TestManagedModelFallbackAdminAPI(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // enforcing/fallback/capture behaviour is Business
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "backup-model", []string{"chat"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{})
	var backup *store.Model
	for _, m := range models {
		if m.Name == "backup-model" {
			backup = m
		}
	}
	_ = h.store.SetModelStatus(ctx, backup.ID, store.ModelEnabled)

	// Create with a fallback in one call.
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/managed-models", map[string]any{
		"name": "current-best", "target_model_id": h.model.ID,
		"fallback_model_id": backup.ID, "fallback_triggers": []string{"model_degraded", "target_unavailable"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ManagedModel store.ManagedModel `json:"managed_model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ManagedModel.FallbackModelID != backup.ID || created.ManagedModel.FallbackPublicName != "backup-model" {
		t.Fatalf("created = %+v", created.ManagedModel)
	}
	if got := strings.Join(created.ManagedModel.FallbackTriggers, ","); got != "target_unavailable,model_degraded" {
		t.Fatalf("triggers = %q, want canonical order", got)
	}
	id := created.ManagedModel.ID

	// Creating with a bad fallback leaves nothing behind.
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/managed-models", map[string]any{
		"name": "bad-fallback", "target_model_id": h.model.ID, "fallback_model_id": h.model.ID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with target-as-fallback returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if _, err := h.store.ManagedModelByName(ctx, "bad-fallback"); err == nil {
		t.Fatal("a refused create must not leave a half-configured alias")
	}

	// Re-trigger only.
	rec = h.doAsSession(session, http.MethodPatch, "/api/v1/admin/managed-models/"+id, map[string]any{
		"fallback_triggers": []string{"model_down"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fallback_triggers":["model_down"]`) {
		t.Fatalf("retrigger returned %d: %s", rec.Code, rec.Body.String())
	}
	// Invalid fallback via PATCH.
	rec = h.doAsSession(session, http.MethodPatch, "/api/v1/admin/managed-models/"+id, map[string]any{
		"fallback_model_id": "does-not-exist",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad fallback returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Clear.
	rec = h.doAsSession(session, http.MethodPatch, "/api/v1/admin/managed-models/"+id, map[string]any{
		"fallback_model_id": "",
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fallback_model_id":""`) {
		t.Fatalf("clear returned %d: %s", rec.Code, rec.Body.String())
	}

	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "managed_model_fallback_changed", Limit: 10})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 fallback audit entries (retrigger, clear), got %d", len(entries))
	}
}
