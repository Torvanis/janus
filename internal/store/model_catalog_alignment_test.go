package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func alignmentModel(t *testing.T, s *Store, upstreamID, name string) *Model {
	t.Helper()
	ctx := context.Background()
	if _, err := s.UpsertDiscoveredModel(ctx, upstreamID, name, []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModelByUpstreamAndName(ctx, upstreamID, name)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func n64(v int64) *int64 { return &v }

// undecide makes a model look as it did before "keep aligned" existed.
func undecide(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	m, err := s.ModelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m.metadataState.CatalogAligned = nil
	raw, err := json.Marshal(m.metadataState)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE model SET metadata_json=? WHERE id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
}

var catalogSrc = ReferenceSource{Catalog: "Janus price catalog 2026.11.16.1", Kind: "model", URL: "https://example.com/p", ReadAt: "2026-11-16"}

func catalogSources(ref map[string]int64) map[string]ReferenceSource {
	out := map[string]ReferenceSource{}
	for k := range ref {
		out[k] = catalogSrc
	}
	return out
}

// Upgrading decides "keep aligned" for every existing model without
// changing what anything bills: aligned only with no administrator price
// (the legacy token rates included) and prices in force equal to the
// catalog's; otherwise the model keeps its prices.
func TestCatalogAlignmentDecisionChangesNothingBilled(t *testing.T) {
	s := newMetadataTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "align", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	catalog := map[string]int64{"rate_in_nanousd": 3e9, "rate_out_nanousd": 15e9}

	matches := alignmentModel(t, s, up.ID, "matches")
	legacy := alignmentModel(t, s, up.ID, "legacy-zero")
	stale := alignmentModel(t, s, up.ID, "older-catalog")
	unpriced := alignmentModel(t, s, up.ID, "never-priced")
	noEntry := alignmentModel(t, s, up.ID, "no-entry")
	for _, m := range []*Model{matches, legacy} {
		if _, err := s.RefreshModelReference(ctx, m.ID, catalog, catalogSources(catalog)); err != nil {
			t.Fatal(err)
		}
	}
	// Older code saved the old rate card's values as administrator prices
	// ($0 here): those are admin prices.
	if err := s.PatchModel(ctx, legacy.ID, ModelPatch{Overrides: map[string]*int64{"rate_in_nanousd": n64(0), "rate_out_nanousd": n64(0)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshModelReference(ctx, stale.ID, map[string]int64{"rate_in_nanousd": 1e9}, nil); err != nil {
		t.Fatal(err)
	}

	for _, m := range []*Model{matches, legacy, stale, unpriced, noEntry} {
		undecide(t, s, m.ID)
	}
	type snapshot struct {
		card    *RateCard
		history int
	}
	billed := func(id string) snapshot {
		rc, err := s.RateCardAt(ctx, id, nowUTC())
		if err != nil {
			t.Fatal(err)
		}
		h, err := s.RateCardHistory(ctx, id, 100)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot{rc, len(h)}
	}
	cases := []struct {
		m    *Model
		ref  map[string]int64
		want bool
	}{
		{matches, catalog, true},
		{legacy, catalog, false},
		{stale, catalog, false},    // the catalog would change its prices
		{unpriced, catalog, false}, // it would become priced
		{noEntry, map[string]int64{}, true},
	}
	for _, c := range cases {
		if fresh, _ := s.ModelByID(ctx, c.m.ID); fresh.CatalogAlignmentDecided() {
			t.Fatalf("%s: decided before the upgrade", c.m.Name)
		}
		before := billed(c.m.ID)
		decided, aligned, err := s.DecideCatalogAlignment(ctx, c.m.ID, c.ref, catalogSources(c.ref))
		if err != nil || !decided || aligned != c.want {
			t.Fatalf("%s: decided=%v aligned=%v err=%v, want aligned=%v", c.m.Name, decided, aligned, err, c.want)
		}
		if after := billed(c.m.ID); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: deciding changed what it bills: %+v -> %+v", c.m.Name, before, after)
		}
		got, err := s.ModelByID(ctx, c.m.ID)
		if err != nil || got.CatalogAligned != c.want || !got.CatalogAlignmentDecided() {
			t.Fatalf("%s: stored %+v %v", c.m.Name, got.CatalogAligned, err)
		}
		// Decided once: a second pass keeps the answer.
		if decided, again, err := s.DecideCatalogAlignment(ctx, c.m.ID, map[string]int64{}, nil); err != nil || decided || again != c.want {
			t.Fatalf("%s: second decision %v %v %v", c.m.Name, decided, again, err)
		}
	}
}

// Reset clears every administrator price (legacy token rates and price
// lines), applies the catalog, keeps the context window override, turns
// alignment on and records the change in the price history.
func TestResetModelPricesToCatalog(t *testing.T) {
	s := newMetadataTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "reset", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m := alignmentModel(t, s, up.ID, "custom")
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{
		"rate_in_nanousd": n64(0), "rate_out_nanousd": n64(0), "price_image_nanousd": n64(5e7), "context_window": n64(1000),
	}}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	historyBefore, _ := s.RateCardHistory(ctx, m.ID, 100)
	catalog := map[string]int64{"rate_in_nanousd": 3e9, "rate_out_nanousd": 15e9, "rate_cached_nanousd": 3e8, "context_window": 200000}
	preview := m.CatalogResetPreview(catalog, catalogSources(catalog))
	got := map[string]PriceChange{}
	for _, c := range preview {
		got[c.Line] = c
	}
	if _, ok := got["context_window"]; ok {
		t.Fatal("the context window is not a price")
	}
	if c := got["rate_in_nanousd"]; c.Before == nil || *c.Before != 0 || c.BeforeSource != "admin" || c.After == nil || *c.After != 3e9 || c.AfterSource != "reference" || !c.Changed {
		t.Fatalf("input rate: %+v", c)
	}
	if c := got["rate_cached_nanousd"]; c.Before != nil || c.After == nil || *c.After != 3e8 || !c.Changed {
		t.Fatalf("an unpriced line gets the catalog price: %+v", c)
	}
	if c := got["price_image_nanousd"]; c.Before == nil || c.After != nil || !c.Changed {
		t.Fatalf("an admin-only line is removed: %+v", c)
	}
	if !reflect.DeepEqual(preview, m.CatalogResetPreview(catalog, catalogSources(catalog))) {
		t.Fatal("the preview is not stable")
	}
	if fresh, _ := s.ModelByID(ctx, m.ID); fresh.RateInNano != 0 || !fresh.HasAdminPrices() {
		t.Fatal("a preview changed the model")
	}

	if err := s.ResetModelPricesToCatalog(ctx, m.ID, catalog, catalogSources(catalog)); err != nil {
		t.Fatal(err)
	}
	after, err := s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HasAdminPrices() || !after.CatalogAligned || after.RateInNano != 3e9 || after.RateOutNano != 15e9 || after.RateCachedNano != 3e8 {
		t.Fatalf("after reset: aligned=%v admin=%v in=%d out=%d cached=%d", after.CatalogAligned, after.HasAdminPrices(), after.RateInNano, after.RateOutNano, after.RateCachedNano)
	}
	if _, ok := after.Metadata["price_image_nanousd"]; ok {
		t.Fatal("the admin-only line is still there")
	}
	if f := after.Metadata["context_window"]; f.Source != "admin" || f.Value == nil || *f.Value != 1000 {
		t.Fatalf("the context window override was lost: %+v", f)
	}
	historyAfter, _ := s.RateCardHistory(ctx, m.ID, 100)
	if len(historyAfter) != len(historyBefore)+1 || historyAfter[0].RateInNano != 3e9 {
		t.Fatalf("history: %d -> %d %+v", len(historyBefore), len(historyAfter), historyAfter[0])
	}

	// A model that was never priced gets the catalog's prices.
	bare := alignmentModel(t, s, up.ID, "bare")
	if err := s.ResetModelPricesToCatalog(ctx, bare.ID, catalog, catalogSources(catalog)); err != nil {
		t.Fatal(err)
	}
	if bare, _ = s.ModelByID(ctx, bare.ID); bare.RateInNano != 3e9 || bare.RateEffectiveFrom.IsZero() || !bare.CatalogAligned {
		t.Fatalf("unpriced model after reset: %+v", bare)
	}
}

// Setting a price stops the model following the catalog; the context window
// does not; turning alignment on over admin prices is refused (that is a
// reset); turning it off changes no price.
func TestEditingAPriceTurnsAlignmentOff(t *testing.T) {
	s := newMetadataTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "edit", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	catalog := map[string]int64{"rate_in_nanousd": 3e9}
	created, err := s.UpsertDiscoveredMetadata(ctx, up.ID, "new-model", []string{"chat"}, AutomaticMetadata{Reference: catalog, ReferenceSources: catalogSources(catalog)})
	if err != nil || !created {
		t.Fatal(created, err)
	}
	m, _ := s.ModelByUpstreamAndName(ctx, up.ID, "new-model")
	if !m.CatalogAligned {
		t.Fatal("a newly discovered model starts aligned")
	}
	// Rediscovery keeps the setting.
	if _, err := s.UpsertDiscoveredMetadata(ctx, up.ID, "new-model", []string{"chat"}, AutomaticMetadata{Reference: catalog}); err != nil {
		t.Fatal(err)
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"context_window": n64(5000)}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = s.ModelByID(ctx, m.ID); !m.CatalogAligned {
		t.Fatal("a context window edit stopped alignment")
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"rate_out_nanousd": nil}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = s.ModelByID(ctx, m.ID); !m.CatalogAligned {
		t.Fatal("returning an unset line to automatic stopped alignment")
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"price_image_nanousd": n64(4e7)}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = s.ModelByID(ctx, m.ID); m.CatalogAligned || !m.HasAdminPrices() {
		t.Fatal("setting a price kept the model aligned")
	}
	on := true
	var verr *ValidationError
	if err := s.PatchModel(ctx, m.ID, ModelPatch{CatalogAligned: &on}); !errors.As(err, &verr) || verr.Field != "catalog_aligned" {
		t.Fatalf("aligned over admin prices: %v", err)
	}
	if err := s.ResetModelPricesToCatalog(ctx, m.ID, catalog, catalogSources(catalog)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ModelByID(ctx, m.ID)
	offv := false
	if err := s.PatchModel(ctx, m.ID, ModelPatch{CatalogAligned: &offv}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ModelByID(ctx, m.ID)
	if after.CatalogAligned || after.RateInNano != before.RateInNano || !after.RateEffectiveFrom.Equal(before.RateEffectiveFrom) {
		t.Fatalf("turning alignment off: %+v", after)
	}
}
