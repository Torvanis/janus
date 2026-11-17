package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"

	"github.com/torvanis/janus/internal/store"
)

// An administrator adds price lines through the model PATCH, and a request
// whose prompt passes a long-prompt line is charged at that line's rates.
func TestPriceLinesThroughTheAPIAndTheProxy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)
	_, m := h.reasoningModel(t, "long-m", false) // every reply reports 10 prompt and 2 completion tokens
	path := "/api/v1/admin/models/" + m.ID
	const in, out = "rate_in_nanousd@prompt_over=5", "rate_out_nanousd@prompt_over=5"

	costOfNext := func() int64 {
		t.Helper()
		before, _, _ := h.store.ListRequests(ctx, store.RequestFilter{Limit: 200})
		if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "long-m", "messages": []map[string]string{{"role": "user", "content": "x"}}}); rec.Code != http.StatusOK {
			t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			evs, _, _ := h.store.ListRequests(ctx, store.RequestFilter{Limit: 200})
			if len(evs) > len(before) {
				for _, ev := range evs {
					if ev.ModelName == "long-m" && ev.HTTPStatus == http.StatusOK {
						return ev.CostNano // newest first
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no usage row")
		return 0
	}

	rec := h.doAsSession(admin, http.MethodPatch, path, map[string]any{"rate_in_usd_per_mtok": 1, "rate_out_usd_per_mtok": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("rates: %d %s", rec.Code, rec.Body.String())
	}
	if got := costOfNext(); got != 10*1000+2*2000 {
		t.Fatalf("ordinary cost = %d", got)
	}

	rec = h.doAsSession(admin, http.MethodPatch, path, map[string]any{"price_lines": map[string]any{in: 4, out: 6, "price_image_nanousd@quality=high": 0.17}})
	var reply struct {
		Model struct {
			Metadata map[string]store.MetadataField `json:"metadata"`
		} `json:"model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	view := reply.Model
	if f := view.Metadata[in]; rec.Code != http.StatusOK || f.Value == nil || *f.Value != 4e9 || f.Source != "admin" {
		t.Fatalf("lines: %d %s", rec.Code, rec.Body.String())
	}
	if f := view.Metadata["price_image_nanousd@quality=high"]; f.Value == nil || *f.Value != 170e6 {
		t.Errorf("unit line: %+v", f)
	}
	if got := costOfNext(); got != 10*4000+2*6000 {
		t.Fatalf("long-prompt cost = %d, want the whole request at the long rates", got)
	}

	// Back to automatic: the lines had no other source, so they are gone.
	rec = h.doAsSession(admin, http.MethodPatch, path, map[string]any{"price_lines": map[string]any{in: nil, out: nil}})
	reply.Model.Metadata = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	view = reply.Model
	if _, still := view.Metadata[in]; rec.Code != http.StatusOK || still {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if got := costOfNext(); got != 10*1000+2*2000 {
		t.Fatalf("cost after clearing = %d", got)
	}

	for _, bad := range []map[string]any{{"price_song_nanousd": 1}, {"rate_in_nanousd": 1}, {"price_image_nanousd": -1}, {"price_image_nanousd@size=1024x1024,quality=high": 1}} {
		if rec := h.doAsSession(admin, http.MethodPatch, path, map[string]any{"price_lines": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	_, member := h.otherUser(t, "price-member", "price-member@example.com")
	if rec := h.doWithToken(member, http.MethodPatch, path, map[string]any{"price_lines": map[string]any{in: 1}}); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Errorf("a member changed prices: %d", rec.Code)
	}
}

// The pricing builder in the model drawer saves through the same PATCH: the
// unconditional token rates as rate_*_usd_per_mtok, every other line as
// price_lines. What it saves must come back with its source and date, a
// reset must return the automatic value with the source that gives it, and
// the model's price history must list each change.
func TestPricingBuilderRoundTripsLinesAndSources(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)
	_, m := h.reasoningModel(t, "builder-m", false)
	const long, image = "rate_in_nanousd@prompt_over=200000", "price_image_nanousd@quality=high"
	// Automatic values: the provider's API gives the input rate, the price
	// catalog (its default for chat models) the output rate and the long
	// prompt line.
	if err := h.store.RefreshModelMetadata(ctx, m.ID, store.AutomaticMetadata{
		Upstream:  map[string]int64{"rate_in_nanousd": 1e9},
		Reference: map[string]int64{"rate_out_nanousd": 4e9, long: 2e9},
		ReferenceSources: map[string]store.ReferenceSource{
			"rate_out_nanousd": {Catalog: "Janus price catalog 2026.11.16.1", Kind: "default", Type: "chat", URL: "https://example.com/p", ReadAt: "2026-11-16"},
			long:               {Catalog: "Janus price catalog 2026.11.16.1", Kind: "default", Type: "chat", URL: "https://example.com/p", ReadAt: "2026-11-16"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	type view struct {
		Model struct {
			Metadata   map[string]store.MetadataField `json:"metadata"`
			CostQuoted bool                           `json:"cost_quoted"`
		} `json:"model"`
	}
	patch := func(body map[string]any) map[string]store.MetadataField {
		t.Helper()
		rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+m.ID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %v: %d %s", body, rec.Code, rec.Body.String())
		}
		var v view
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.Model.CostQuoted {
			t.Error("a local upstream does not quote call costs")
		}
		return v.Model.Metadata
	}

	// One builder save: edit the input rate, the long-prompt line, add a
	// per-image line, and mark the catalog's output rate "not charged".
	md := patch(map[string]any{
		"rate_in_usd_per_mtok":  3,
		"rate_out_usd_per_mtok": 0,
		"price_lines":           map[string]any{long: 6, image: 0.04},
	})
	for key, want := range map[string]int64{"rate_in_nanousd": 3e9, "rate_out_nanousd": 0, long: 6e9, image: 40e6} {
		f := md[key]
		if f.Value == nil || *f.Value != want || f.Source != "admin" || f.SourceDetail == nil || f.SourceDetail.SetAt != today {
			t.Errorf("%s after save: %+v %+v", key, f, f.SourceDetail)
		}
	}
	// The automatic value stays beside the override, with its own source.
	if f := md["rate_in_nanousd"]; f.AutomaticValue == nil || *f.AutomaticValue != 1e9 || f.AutomaticSource != "upstream" || f.AutomaticDetail == nil || f.AutomaticDetail.ReadAt != today {
		t.Errorf("provider value under the override: %+v %+v", f, f.AutomaticDetail)
	}
	if f := md[long]; f.AutomaticDetail == nil || f.AutomaticDetail.Label != "Janus price catalog 2026.11.16.1 (default for chat)" || f.AutomaticDetail.ReadAt != "2026-11-16" {
		t.Errorf("catalog default under the override: %+v", f.AutomaticDetail)
	}

	// The list the drawer reads says the same.
	rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models", nil)
	var list struct {
		Models []struct {
			ID       string                         `json:"id"`
			Metadata map[string]store.MetadataField `json:"metadata"`
		} `json:"models"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	found := false
	for _, lm := range list.Models {
		if lm.ID == m.ID {
			found = true
			if f := lm.Metadata[image]; f.Value == nil || *f.Value != 40e6 || f.Source != "admin" {
				t.Errorf("list: image line %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("model missing from the admin list: %s", rec.Body.String())
	}

	// Reset to automatic: back to the provider's and the catalog's values.
	md = patch(map[string]any{"rate_in_usd_per_mtok": nil, "rate_out_usd_per_mtok": nil, "price_lines": map[string]any{long: nil, image: nil}})
	if f := md["rate_in_nanousd"]; f.Value == nil || *f.Value != 1e9 || f.Source != "upstream" || f.SourceDetail == nil || f.SourceDetail.Label != "Provider API" {
		t.Errorf("input after reset: %+v", f)
	}
	if f := md["rate_out_nanousd"]; f.Value == nil || *f.Value != 4e9 || f.Source != "reference" {
		t.Errorf("output after reset: %+v", f)
	}
	if f := md[long]; f.Value == nil || *f.Value != 2e9 || f.Source != "reference" || f.SourceDetail.Kind != "default" {
		t.Errorf("long-prompt line after reset: %+v", f)
	}
	if _, still := md[image]; still {
		t.Errorf("an admin-only line removed by a reset is gone: %+v", md[image])
	}

	// Price history: newest first, one entry per change, lines included.
	rec = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models/"+m.ID+"/price-history", nil)
	var hist struct {
		Versions []store.RateCard `json:"versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	if len(hist.Versions) < 2 {
		t.Fatalf("history has %d entries, want the save and the reset", len(hist.Versions))
	}
	if newest := hist.Versions[0]; newest.RateInNano != 1e9 || newest.Lines[long] != 2e9 {
		t.Errorf("newest entry = %+v", newest)
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models/nope/price-history", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown model history: %d", rec.Code)
	}
}

func TestQuotesCallCostHosts(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.x.ai/v1":                  true,
		"https://openrouter.ai/api/v1":         true,
		"https://api.deepinfra.com/v1/openai":  true,
		"https://api.openai.com/v1":            false,
		"http://127.0.0.1:4000":                false,
		"https://openrouter.ai.example.com/v1": false,
	} {
		if got := adapter.QuotesCallCost(url); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}
