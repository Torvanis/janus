package httpapi

import (
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
)

func TestModelMetadataPatchContract(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{Upstream: map[string]int64{"context_window": 100, "rate_in_nanousd": 2000000000, "rate_cached_nanousd": 0}, Reference: map[string]int64{"rate_out_nanousd": 3000000000}}); err != nil {
		t.Fatal(err)
	}
	patch := func(body map[string]any) *store.Model {
		t.Helper()
		rec := h.patchModel(session, h.model.ID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH=%d %s", rec.Code, rec.Body.String())
		}
		var doc struct {
			Model *store.Model `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Model
	}
	m := patch(map[string]any{"display_name": "metadata-alias", "context_window": 200, "rate_in_usd_per_mtok": 7, "rate_cached_usd_per_mtok": 0})
	if m.Metadata["rate_cached_nanousd"].Source != "admin" || m.Metadata["context_window"].Source != "admin" {
		t.Fatal("PATCH lacks provenance")
	}
	m = patch(map[string]any{"context_window": nil})
	if m.ContextWindow != 100 || m.Metadata["context_window"].Source != "upstream" || m.RateInNano != 7000000000 {
		t.Fatalf("independent context reset: %+v", m)
	}
	m = patch(map[string]any{"rate_in_usd_per_mtok": nil})
	if m.RateInNano != 2000000000 || m.Metadata["rate_in_nanousd"].Source != "upstream" || m.Metadata["rate_cached_nanousd"].Source != "admin" {
		t.Fatal("independent price reset")
	}
	m = patch(map[string]any{"rate_out_usd_per_mtok": nil, "rate_cache_write_1h_usd_per_mtok": nil})
	if m.RateOutNano != 3000000000 || m.Metadata["rate_out_nanousd"].Source != "reference" || m.Metadata["rate_cache_write_1h_nanousd"].Value != nil {
		t.Fatal("reference/unknown reset")
	}
	// Null resets are audited, including source even when effective value is unchanged.
	audits, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "model_updated"})
	if err != nil || len(audits) < 4 {
		t.Fatalf("reset audit entries=%d err=%v", len(audits), err)
	}
	for _, body := range []map[string]any{
		{"display_name": "bad alias", "context_window": 999, "rate_in_usd_per_mtok": 99},
		{"status": "bogus", "context_window": 999, "rate_in_usd_per_mtok": 99},
		{"context_window": 1.5, "rate_in_usd_per_mtok": 99},
		{"rate_in_usd_per_mtok": 1e100},
		{"rate_in_usd_per_mtok": -1},
	} {
		rec := h.patchModel(session, h.model.ID, body)
		if rec.Code != 400 {
			t.Fatalf("invalid PATCH=%d %s", rec.Code, rec.Body.String())
		}
		after, _ := h.store.ModelByID(ctx, h.model.ID)
		if after.RateInNano != m.RateInNano || after.ContextWindow != m.ContextWindow || after.DisplayName != m.DisplayName {
			t.Fatal("invalid PATCH partially persisted")
		}
	}
	// The routing cache observes both admin writes and scheduled discovery.
	if _, err := h.server.cachedResolveModel(ctx, "metadata-alias"); err != nil {
		t.Fatal(err)
	}
	patch(map[string]any{"rate_in_usd_per_mtok": 9})
	resolved, err := h.server.cachedResolveModel(ctx, "metadata-alias")
	if err != nil || resolved.Model.RateInNano != 9000000000 {
		t.Fatal("stale PATCH cache")
	}
	patch(map[string]any{"rate_in_usd_per_mtok": nil})
	if _, err := h.server.cachedResolveModel(ctx, "metadata-alias"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{Upstream: map[string]int64{"context_window": 500, "rate_in_nanousd": 4000000000}}); err != nil {
		t.Fatal(err)
	}
	resolved, err = h.server.cachedResolveModel(ctx, "metadata-alias")
	if err != nil || resolved.Model.ContextWindow != 500 || resolved.Model.RateInNano != 4000000000 {
		t.Fatal("stale discovery cache")
	}
}

func TestReferenceApplyPreservesFreeAndContextOverrides(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "gpt-4o", nil); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, h.model.UpstreamID, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	rec := h.patchModel(session, m.ID, map[string]any{"context_window": 0, "rate_in_usd_per_mtok": 0, "rate_out_usd_per_mtok": 0})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	m, err = h.store.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 0 || m.RateInNano != 0 || m.RateOutNano != 0 {
		t.Fatal("reference apply overwrote explicit zero overrides")
	}
}

func TestReferenceApplyAtomicMetadata(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "gpt-4o", nil); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, h.model.UpstreamID, "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `CREATE TRIGGER reject_apply BEFORE INSERT ON rate_card_version BEGIN SELECT RAISE(ABORT,'test rejection'); END`); err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != 500 {
		t.Fatalf("expected write failure, got %d", rec.Code)
	}
	after, err := h.store.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ContextWindow != 0 || after.Metadata["context_window"].Source != "unknown" || !after.RateEffectiveFrom.IsZero() {
		t.Fatal("failed apply left partial metadata")
	}
}

func TestDiscoveryStaleModelsInvalidateCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.server.cachedResolveModel(ctx, h.model.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.MarkStaleModels(ctx, h.model.UpstreamID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.cachedResolveModel(ctx, h.model.Name); err == nil {
		t.Fatal("scheduled discovery left a stale model routable in cache")
	}
}

func TestDiscoveryRefreshInvalidatesBillingCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{Upstream: map[string]int64{"rate_in_nanousd": 1000000000}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PatchModel(ctx, h.model.ID, store.ModelPatch{Overrides: map[string]*int64{"rate_in_nanousd": nil}}); err != nil {
		t.Fatal(err)
	}
	before := h.server.cachedRateCard(ctx, h.model.ID)
	if before.In != 1000000000 {
		t.Fatal("initial billing rate")
	}
	if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{Upstream: map[string]int64{"rate_in_nanousd": 2000000000}}); err != nil {
		t.Fatal(err)
	}
	after := h.server.cachedRateCard(ctx, h.model.ID)
	if after.In != 2000000000 {
		t.Fatalf("scheduled discovery left billing rate stale: %d", after.In)
	}
}
