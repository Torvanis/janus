package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

type resetReply struct {
	Plan struct {
		Available      bool                `json:"available"`
		Reason         string              `json:"reason"`
		Message        string              `json:"message"`
		Catalog        string              `json:"catalog"`
		HasAdminPrices bool                `json:"has_admin_prices"`
		ChangedLines   int                 `json:"changed_lines"`
		Changes        []store.PriceChange `json:"changes"`
	} `json:"plan"`
	Applied bool `json:"applied"`
	Model   *struct {
		CatalogAligned bool                           `json:"catalog_aligned"`
		RateInNano     int64                          `json:"rate_in_nanousd"`
		Metadata       map[string]store.MetadataField `json:"metadata"`
	} `json:"model"`
}

func decodeReset(t *testing.T, body []byte) resetReply {
	t.Helper()
	var r resetReply
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResetModelPricesToCatalogAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)
	up, err := h.store.CreateUpstream(ctx, "openai-catalog", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string, mods ...string) *store.Model {
		if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, name, mods); err != nil {
			t.Fatal(err)
		}
		m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	gpt := add("gpt-4o", "chat")
	embed := add("text-embedding-zz", "embedding")
	unknown := add("not-in-the-catalog-zz", "chat")
	_, selfHosted := h.reasoningModel(t, "self-hosted-zz", false)

	// Old rate-card values saved as administrator prices ($0).
	path := "/api/v1/admin/models/" + gpt.ID
	if rec := h.doAsSession(admin, http.MethodPatch, path, map[string]any{"rate_in_usd_per_mtok": 0, "rate_out_usd_per_mtok": 0}); rec.Code != http.StatusOK {
		t.Fatalf("legacy prices: %d %s", rec.Code, rec.Body.String())
	}
	history := func(id string) int {
		v, err := h.store.RateCardHistory(ctx, id, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(v)
	}
	before := history(gpt.ID)

	reset := path + "/pricing/reset-to-catalog"
	preview := path + "/pricing/catalog-preview"
	audits := func() int {
		entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	auditsBefore := audits()
	rec := h.doAsSession(admin, http.MethodGet, preview, nil)
	plan := decodeReset(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || !plan.Plan.Available || plan.Applied || !plan.Plan.HasAdminPrices || plan.Plan.Catalog == "" || strings.Contains(plan.Plan.Catalog, "default") {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, c := range plan.Plan.Changes {
		if c.Line == "rate_in_nanousd" {
			found = c.Before != nil && *c.Before == 0 && c.After != nil && *c.After == 2.5e9 && c.Changed && c.BeforeSource == "admin" && c.AfterSource == "reference"
		}
	}
	if !found || plan.Plan.ChangedLines == 0 {
		t.Fatalf("preview changes: %+v", plan.Plan.Changes)
	}
	// The preview writes nothing: no price, no price history, no audit entry.
	if m, _ := h.store.ModelByID(ctx, gpt.ID); m.RateInNano != 0 || m.CatalogAligned || !m.HasAdminPrices() || history(gpt.ID) != before {
		t.Fatal("a preview changed the model")
	}
	if n := audits(); n != auditsBefore {
		t.Fatalf("a preview wrote %d audit entries", n-auditsBefore)
	}
	// The old dry-run body is refused, never applied as a reset.
	if rec := h.doAsSession(admin, http.MethodPost, reset, map[string]any{"dry_run": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("dry_run on reset: %d %s", rec.Code, rec.Body.String())
	}
	if m, _ := h.store.ModelByID(ctx, gpt.ID); m.RateInNano != 0 || audits() != auditsBefore {
		t.Fatal("a dry_run body changed the model")
	}

	rec = h.doAsSession(admin, http.MethodPost, reset, nil)
	got := decodeReset(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || !got.Applied || got.Model == nil || !got.Model.CatalogAligned || got.Model.RateInNano != 2.5e9 {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if history(gpt.ID) != before+1 {
		t.Fatal("the reset was not recorded in the price history")
	}
	if entries, _, _ := h.store.ListAudit(ctx, store.AuditFilter{Action: "model_prices_reset_to_catalog"}); len(entries) != 1 || entries[0].ResourceID != gpt.ID {
		t.Fatalf("audit: %+v", entries)
	}

	// Editing a price while aligned stops following the catalog.
	rec = h.doAsSession(admin, http.MethodPatch, path, map[string]any{"price_lines": map[string]any{"rate_in_nanousd@prompt_over=200000": 5}})
	var patched struct {
		Model struct {
			CatalogAligned bool `json:"catalog_aligned"`
		} `json:"model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if rec.Code != http.StatusOK || patched.Model.CatalogAligned {
		t.Fatalf("edit while aligned: %d %s", rec.Code, rec.Body.String())
	}
	// Turning the switch on through PATCH is the same reset.
	if rec := h.doAsSession(admin, http.MethodPatch, path, map[string]any{"catalog_aligned": true, "rate_in_usd_per_mtok": 1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("aligned and a price at once: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(admin, http.MethodPatch, path, map[string]any{"catalog_aligned": true})
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if m, _ := h.store.ModelByID(ctx, gpt.ID); rec.Code != http.StatusOK || !patched.Model.CatalogAligned || m.HasAdminPrices() {
		t.Fatalf("PATCH catalog_aligned=true: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(admin, http.MethodPatch, path, map[string]any{"catalog_aligned": false})
	_ = json.Unmarshal(rec.Body.Bytes(), &patched)
	if m, _ := h.store.ModelByID(ctx, gpt.ID); rec.Code != http.StatusOK || patched.Model.CatalogAligned || m.RateInNano != 2.5e9 {
		t.Fatalf("PATCH catalog_aligned=false: %d %s", rec.Code, rec.Body.String())
	}

	// No exact entry: the catalog's default for the provider and type.
	rec = h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+embed.ID+"/pricing/reset-to-catalog", map[string]any{})
	got = decodeReset(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || !strings.Contains(got.Plan.Catalog, "default for embedding") || got.Model == nil || got.Model.RateInNano != 2e7 {
		t.Fatalf("default by type: %d %s", rec.Code, rec.Body.String())
	}

	// Nothing in the catalog: the reason, and a refused reset.
	for _, c := range []struct {
		id, reason string
	}{{unknown.ID, "no_entry"}, {selfHosted.ID, "no_provider"}} {
		p := "/api/v1/admin/models/" + c.id + "/pricing/reset-to-catalog"
		rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models/"+c.id+"/pricing/catalog-preview", nil)
		got := decodeReset(t, rec.Body.Bytes())
		if rec.Code != http.StatusOK || got.Plan.Available || got.Plan.Reason != c.reason || got.Plan.Message == "" {
			t.Fatalf("%s preview: %d %s", c.reason, rec.Code, rec.Body.String())
		}
		if rec := h.doAsSession(admin, http.MethodPost, p, nil); rec.Code != http.StatusConflict {
			t.Fatalf("%s reset: %d %s", c.reason, rec.Code, rec.Body.String())
		}
	}
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/nope/pricing/reset-to-catalog", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model: %d", rec.Code)
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models/nope/pricing/catalog-preview", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model preview: %d", rec.Code)
	}

	// Bulk: keep aligned only where nothing changes.
	if rec := h.doAsSession(admin, http.MethodPatch, path, map[string]any{"rate_in_usd_per_mtok": 2.5}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String()) // an admin price equal to the catalog's
	}
	stale := add("gpt-4o-mini", "chat")
	if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+stale.ID, map[string]any{"rate_in_usd_per_mtok": 9}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	bulk := "/api/v1/admin/models/pricing/reset-to-catalog"
	rec = h.doAsSession(admin, http.MethodPost, bulk, map[string]any{"ids": []string{gpt.ID, stale.ID, unknown.ID, "nope"}, "only_if_unchanged": true})
	var out struct {
		Results []struct {
			ModelID string `json:"model_id"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		} `json:"results"`
		Counts map[string]int `json:"counts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	want := map[string]string{gpt.ID: "reset", stale.ID: "skipped/prices_would_change", unknown.ID: "skipped/no_entry", "nope": "not_found"}
	if rec.Code != http.StatusOK || len(out.Results) != 4 {
		t.Fatalf("bulk: %d %s", rec.Code, rec.Body.String())
	}
	for _, r := range out.Results {
		got := r.Outcome
		if r.Reason != "" {
			got += "/" + r.Reason
		}
		if want[r.ModelID] != got {
			t.Errorf("bulk %s: %s, want %s", r.ModelID, got, want[r.ModelID])
		}
	}
	if m, _ := h.store.ModelByID(ctx, gpt.ID); !m.CatalogAligned || m.RateInNano != 2.5e9 {
		t.Fatalf("bulk keep aligned: %+v", m.CatalogAligned)
	}
	if m, _ := h.store.ModelByID(ctx, stale.ID); m.CatalogAligned || m.RateInNano != 9e9 {
		t.Fatal("bulk keep aligned changed a price")
	}
	// Bulk reset: preview first (writes nothing), then for real.
	bulkPreview := "/api/v1/admin/models/pricing/catalog-preview"
	auditsBefore = audits()
	rec = h.doAsSession(admin, http.MethodPost, bulkPreview, map[string]any{"ids": []string{stale.ID}})
	if m, _ := h.store.ModelByID(ctx, stale.ID); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"would_reset"`) || !strings.Contains(rec.Body.String(), `"applied":false`) || m.RateInNano != 9e9 || m.CatalogAligned || audits() != auditsBefore {
		t.Fatalf("bulk preview: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(admin, http.MethodPost, bulk, map[string]any{"ids": []string{stale.ID}, "dry_run": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("dry_run on bulk reset: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(admin, http.MethodPost, bulk, map[string]any{"ids": []string{stale.ID}})
	if m, _ := h.store.ModelByID(ctx, stale.ID); rec.Code != http.StatusOK || !m.CatalogAligned || m.RateInNano == 9e9 {
		t.Fatalf("bulk reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(admin, http.MethodPost, bulk, map[string]any{"ids": []string{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty bulk: %d", rec.Code)
	}

	// Members can neither reset prices nor see the previews.
	_, member := h.otherUser(t, "reset-member", "reset-member@example.com")
	for _, p := range []string{reset, bulk, bulkPreview} {
		if rec := h.doWithToken(member, http.MethodPost, p, map[string]any{"ids": []string{gpt.ID}}); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("a member reached %s: %d", p, rec.Code)
		}
	}
	if rec := h.doWithToken(member, http.MethodGet, preview, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Errorf("a member reached the preview: %d", rec.Code)
	}
	if rec := h.doAsSession(h.sessionFor(t, "preview-member@example.com"), http.MethodGet, preview, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a signed-in member reached the preview: %d", rec.Code)
	}
	anon := httptest.NewRecorder()
	h.handler.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, preview, nil))
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous caller reached the preview: %d", anon.Code)
	}
}
