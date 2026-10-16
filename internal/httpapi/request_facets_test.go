package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestRequestFacetsAndKindSorts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	rows := []struct {
		modality, model string
		status          int
		ttfb            int
	}{
		{"chat", "gpt-4o", 200, 300}, {"chat", "gpt-4o", 500, 100}, {"chat", "claude", 200, 900},
		{"decision", "typesafe/jev-router", 200, 50}, {"decision", "gpt-6-luna:decisions", 200, 40},
	}
	for i, r := range rows {
		e := &store.UsageEvent{UserID: h.user.ID, ModelName: r.model, Modality: r.modality, HTTPStatus: r.status, TTFBMs: r.ttfb,
			EndpointPath: "/v1/x", HTTPMethod: "POST", CreatedAt: now.Add(-time.Duration(i) * time.Minute)}
		if err := h.store.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Internal classifier rows stay out unless asked for.
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{UserID: h.user.ID, ModelName: "guard", Modality: "classification",
		ClientApp: store.InternalClientApp, HTTPStatus: 200, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Facets []store.RequestFacet `json:"facets"`
	}
	// The kind and model filters do not narrow their own facets; other
	// filters (status) do.
	rec := h.do(http.MethodGet, "/api/v1/requests/facets?range=day&modality=decision&model=claude&status=success", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	counts := map[string]int64{}
	for _, f := range got.Facets {
		counts[f.Modality+"/"+f.Model] = f.Count
	}
	want := map[string]int64{"chat/gpt-4o": 1, "chat/claude": 1, "decision/typesafe/jev-router": 1, "decision/gpt-6-luna:decisions": 1}
	if len(counts) != len(want) {
		t.Fatalf("facets %v, want %v", counts, want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("facets %v, want %v", counts, want)
		}
	}

	order := func(sort string) []string {
		var page struct {
			Requests []store.UsageEvent `json:"requests"`
		}
		rec := h.do(http.MethodGet, "/api/v1/requests?range=day&sort="+sort, nil)
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		out := []string{}
		for _, e := range page.Requests {
			out = append(out, e.Modality+"/"+e.ModelName)
		}
		return out
	}
	if o := order("modality_asc"); o[0][:5] != "chat/" || o[len(o)-1][:9] != "decision/" {
		t.Fatalf("modality_asc: %v", o)
	}
	if o := order("model_asc"); o[0] != "chat/claude" {
		t.Fatalf("model_asc: %v", o)
	}
	if o := order("ttfb"); o[0] != "chat/claude" {
		t.Fatalf("ttfb: %v", o)
	}
	if o := order("status"); o[0] != "chat/gpt-4o" {
		t.Fatalf("status (errors first): %v", o)
	}
}
