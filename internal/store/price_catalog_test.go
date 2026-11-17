package store

import (
	"context"
	"testing"
	"time"
)

// TestPriceCatalogSettingsRoundTrip runs on SQLite and, with
// JANUS_ROLLUP_TEST_DATABASE_URL, on PostgreSQL: refresh is off until set,
// and the stored catalog keeps its exact bytes.
func TestPriceCatalogSettingsRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	got, err := s.PriceCatalogSettings(ctx)
	if err != nil || got.RefreshEnabled || got.URL != "" {
		t.Fatalf("default settings: %+v %v", got, err)
	}
	if err := s.SetPriceCatalogSettings(ctx, PriceCatalogSettings{RefreshEnabled: true, URL: "https://mirror.example/c.json"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.PriceCatalogSettings(ctx); !got.RefreshEnabled || got.URL != "https://mirror.example/c.json" {
		t.Fatalf("settings: %+v", got)
	}
	if a, err := s.PriceCatalogActive(ctx); err != nil || a != nil {
		t.Fatalf("no catalog yet: %+v %v", a, err)
	}
	body := "{\n \"exact\": \"bytes \\u00e9\"\n}\n"
	at := time.Date(2026, 11, 16, 1, 2, 3, 0, time.UTC)
	if err := s.SetPriceCatalogActive(ctx, PriceCatalogActive{Version: "2026.11.16.2", Body: body, Signature: "c2ln\n", URL: "u", ETag: `"e1"`, FetchedAt: at}); err != nil {
		t.Fatal(err)
	}
	a, err := s.PriceCatalogActive(ctx)
	if err != nil || a == nil || a.Body != body || a.Signature != "c2ln\n" || a.ETag != `"e1"` || !a.FetchedAt.Equal(at) {
		t.Fatalf("active: %+v %v", a, err)
	}
	st := PriceCatalogStatus{CheckedAt: &at, Result: "updated", Version: "2026.11.16.2", ModelsChanged: 3, LinesChanged: 7}
	if err := s.SetPriceCatalogStatus(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PriceCatalogStatus(ctx); err != nil || got.Result != "updated" || got.LinesChanged != 7 || !got.CheckedAt.Equal(at) {
		t.Fatalf("status: %+v %v", got, err)
	}
}

// TestRefreshModelReferenceKeepsOtherLayers: a catalog update rewrites the
// catalog layer only. The administrator's override and the provider's API
// price stay in force, the catalog's own values move, and the count says
// how many catalog values changed.
func TestRefreshModelReferenceKeepsOtherLayers(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "cat", "openai_compatible", "http://example.invalid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const long = "rate_in_nanousd@prompt_over=272000"
	src := map[string]ReferenceSource{
		"rate_in_nanousd":  {Catalog: "Janus price catalog 1", Kind: "model", URL: "https://p.example", ReadAt: "2026-11-16"},
		"rate_out_nanousd": {Catalog: "Janus price catalog 1", Kind: "model"},
		long:               {Catalog: "Janus price catalog 1", Kind: "model"},
	}
	if _, err := s.UpsertDiscoveredMetadata(ctx, up.ID, "m", []string{"chat"}, AutomaticMetadata{
		Upstream:         map[string]int64{"rate_out_nanousd": 7e9},
		Reference:        map[string]int64{"rate_in_nanousd": 1e9, "rate_out_nanousd": 2e9, long: 3e9},
		ReferenceSources: src,
		Warnings:         []string{"keep me"},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModelByUpstreamAndName(ctx, up.ID, "m")
	if err != nil {
		t.Fatal(err)
	}
	if f := m.Metadata["rate_in_nanousd"]; f.SourceDetail == nil || f.SourceDetail.Label != "Janus price catalog 1" || f.SourceDetail.URL != "https://p.example" {
		t.Fatalf("catalog label: %+v", f.SourceDetail)
	}
	if f := m.Metadata["rate_out_nanousd"]; f.Source != "upstream" || f.SourceDetail.Label != "Provider API" {
		t.Fatalf("provider API wins over the catalog: %+v", f)
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{long: i64(9e9)}}); err != nil {
		t.Fatal(err)
	}

	src2 := map[string]ReferenceSource{
		"rate_in_nanousd":  {Catalog: "Janus price catalog 2", Kind: "model"},
		"rate_out_nanousd": {Catalog: "Janus price catalog 2", Kind: "model"},
		long:               {Catalog: "Janus price catalog 2", Kind: "model"},
	}
	n, err := s.RefreshModelReference(ctx, m.ID, map[string]int64{"rate_in_nanousd": 1.5e9, "rate_out_nanousd": 2e9, long: 4e9}, src2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("changed catalog values = %d, want 2 (rate_in and the long line)", n)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	if f := m.Metadata["rate_in_nanousd"]; *f.Value != 1.5e9 || f.SourceDetail.Label != "Janus price catalog 2" || m.RateInNano != 1.5e9 {
		t.Fatalf("catalog value not refreshed: %+v", f)
	}
	if f := m.Metadata["rate_out_nanousd"]; *f.Value != 7e9 || f.Source != "upstream" {
		t.Fatalf("provider API price changed: %+v", f)
	}
	if f := m.Metadata[long]; *f.Value != 9e9 || f.Source != "admin" || *f.AutomaticValue != 4e9 || f.AutomaticDetail.Label != "Janus price catalog 2" {
		t.Fatalf("admin override changed: %+v", f)
	}
	if len(m.MetadataWarnings) != 1 {
		t.Fatalf("warnings dropped: %v", m.MetadataWarnings)
	}
	// A catalog that no longer lists the model removes its values.
	if n, err = s.RefreshModelReference(ctx, m.ID, map[string]int64{}, nil); err != nil || n != 3 {
		t.Fatalf("removal: %d %v", n, err)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	if f := m.Metadata["rate_in_nanousd"]; f.Value != nil {
		t.Fatalf("removed catalog value still in force: %+v", f)
	}
	if f := m.Metadata["rate_out_nanousd"]; *f.Value != 7e9 {
		t.Fatalf("provider API price lost: %+v", f)
	}
}
