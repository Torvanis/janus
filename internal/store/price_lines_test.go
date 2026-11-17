package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

// Price lines ride in the same three sources as the five rates, are dated
// with them, and a past request is priced by the lines of its day.
func TestPriceLinesLayeringAndHistory(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	up, err := s.CreateUpstream(ctx, "up", "openai_compatible", "http://example.invalid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const long, video = "rate_in_nanousd@prompt_over=272000", "price_video_second_nanousd@resolution=720p"
	// The provider's API lists a long-prompt rate; the bundled list has a video price.
	if _, err := s.UpsertDiscoveredMetadata(ctx, up.ID, "m", []string{"chat"}, AutomaticMetadata{
		Upstream:  map[string]int64{"rate_in_nanousd": 10e9, "rate_out_nanousd": 50e9, long: 20e9, "not a line": 5},
		Reference: map[string]int64{long: 19e9, video: 100e6},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModelByUpstreamAndName(ctx, up.ID, "m")
	if err != nil {
		t.Fatal(err)
	}
	if f := m.Metadata[long]; f.Value == nil || *f.Value != 20e9 || f.Source != "upstream" {
		t.Errorf("the provider's price must win over the bundled one: %+v", f)
	}
	if f := m.Metadata[video]; f.Value == nil || *f.Value != 100e6 || f.Source != "reference" {
		t.Errorf("bundled line: %+v", f)
	}
	if _, kept := m.Metadata["not a line"]; kept {
		t.Error("a key that is not a price line was taken from a provider")
	}
	if got := m.PriceLines(); len(got) != 2 || got[long] != 20e9 || got[video] != 100e6 {
		t.Errorf("lines: %v", got)
	}
	first, err := s.RateCardAt(ctx, m.ID, time.Now().UTC().Add(time.Second))
	if err != nil || first.Lines[long] != 20e9 || first.RateInNano != 10e9 {
		t.Fatalf("card: %+v %v", first, err)
	}

	// An administrator sets one line and adds another, effective tomorrow.
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if err := s.PatchModel(ctx, m.ID, ModelPatch{EffectiveFrom: tomorrow, Overrides: map[string]*int64{long: i64(25e9), "price_image_nanousd": i64(40e6)}}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	if f := m.Metadata[long]; *f.Value != 25e9 || f.Source != "admin" || *f.AutomaticValue != 20e9 || f.AutomaticSource != "upstream" {
		t.Errorf("override: %+v", f)
	}
	if today, _ := s.RateCardAt(ctx, m.ID, time.Now().UTC().Add(time.Second)); today.Lines[long] != 20e9 || today.Lines["price_image_nanousd"] != 0 {
		t.Errorf("today's requests must keep today's lines: %+v", today.Lines)
	}
	if later, _ := s.RateCardAt(ctx, m.ID, tomorrow.Add(time.Hour)); later.Lines[long] != 25e9 || later.Lines["price_image_nanousd"] != 40e6 || later.Lines[video] != 100e6 {
		t.Errorf("tomorrow's lines: %+v", later.Lines)
	}

	// Discovery again: the administrator's lines stay, the provider's move under them.
	if _, err := s.UpsertDiscoveredMetadata(ctx, up.ID, "m", []string{"chat"}, AutomaticMetadata{Upstream: map[string]int64{"rate_in_nanousd": 10e9, "rate_out_nanousd": 50e9, long: 22e9}}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	if f := m.Metadata[long]; *f.Value != 25e9 || *f.AutomaticValue != 22e9 {
		t.Errorf("after discovery: %+v", f)
	}
	if _, still := m.Metadata[video]; still {
		t.Error("a line no source carries any more must go")
	}
	// Clearing the override returns to the provider's price; clearing a line only the administrator had removes it.
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{long: nil, "price_image_nanousd": nil}}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ModelByID(ctx, m.ID)
	if got := m.PriceLines(); len(got) != 1 || got[long] != 22e9 {
		t.Errorf("after clearing: %v", got)
	}

	for _, bad := range []string{"price_song_nanousd", "rate_in_nanousd@quality=high", "price_image_nanousd@size=B"} {
		if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{bad: i64(1)}}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"price_image_nanousd": i64(-1)}}); err == nil {
		t.Error("a negative price was accepted")
	}
}

// A model priced only per unit (no token rate at all) counts as priced.
func TestUnitPricedModelCanBeEnabled(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	up, _ := s.CreateUpstream(ctx, "up", "openai_compatible", "http://example.invalid", "", "")
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "video-model", []string{"video"}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.ModelByUpstreamAndName(ctx, up.ID, "video-model")
	enabled := ModelEnabled
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Status: &enabled}); err == nil {
		t.Fatal("an unpriced model was enabled")
	}
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Status: &enabled, Overrides: map[string]*int64{"price_video_second_nanousd": i64(100e6)}}); err != nil {
		t.Fatalf("a model with a per-second price must be enabled: %v", err)
	}
}
