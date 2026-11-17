package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// A quoted image call keeps Janus's own price for what it measured (two
// images at the model's line) beside the quote, which is still the cost.
func TestQuotedImageCallKeepsJanusEstimate(t *testing.T) {
	h := newHarness(t)
	admin := adminSession(t, h)
	rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+h.model.ID, map[string]any{"price_lines": map[string]any{
		"price_image_nanousd": 0.03, // $0.03 an image
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("lines: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	// xAI quotes $0.04 for the two images it returned.
	quotedUpstream(t, h, `{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"aGk="}],"usage":{"cost_in_usd_ticks":400000000}}`, false, nil)
	if rec := h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat", "n": 2}); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ev := h.waitForUsageEvents(1)[0]
	if ev.AccountingMode != usage.AccountingUpstreamCost || ev.CostNano != 40_000_000 {
		t.Fatalf("billed %d (%s), want the quote 40000000", ev.CostNano, ev.AccountingMode)
	}
	if ev.EstimatedCostNano == nil || *ev.EstimatedCostNano != 60_000_000 {
		t.Fatalf("Janus's estimate = %v, want 2 x $0.03", ev.EstimatedCostNano)
	}
	if ev.BilledUnit != "image" || ev.BilledQuantity != 2 || ev.BilledLine != "" {
		t.Fatalf("measured %q %v line %q, want 2 images and no billing line (the quote billed it)", ev.BilledUnit, ev.BilledQuantity, ev.BilledLine)
	}
}

// A personal subscription is paid by its owner: no quote is recorded, so
// there is no estimate to compare either.
func TestApplyUsageSubscriptionDropsEstimate(t *testing.T) {
	h := newHarness(t)
	ev := &store.UsageEvent{SubscriptionID: "sub-1", Modality: "chat"}
	h.server.applyUsage(ev, adapterUsageQuoted(10, 2, 5_000), usage.Rates{InNano: usage.NanoPerUSD, OutNano: usage.NanoPerUSD}, -1)
	if ev.CostNano != 0 || ev.EstimatedCostNano != nil || ev.AccountingMode != usage.AccountingUpstream {
		t.Fatalf("subscription: cost %d estimate %v mode %s", ev.CostNano, ev.EstimatedCostNano, ev.AccountingMode)
	}
	ev = &store.UsageEvent{Modality: "chat"}
	h.server.applyUsage(ev, adapterUsageQuoted(10, 2, 5_000), usage.Rates{InNano: usage.NanoPerUSD, OutNano: usage.NanoPerUSD}, -1)
	if ev.CostNano != 5_000 || ev.EstimatedCostNano == nil || *ev.EstimatedCostNano != 12_000 {
		t.Fatalf("organization: cost %d estimate %v", ev.CostNano, ev.EstimatedCostNano)
	}
	h.server.Config.LocalOnly = true
	defer func() { h.server.Config.LocalOnly = false }()
	ev = &store.UsageEvent{Modality: "chat"}
	h.server.applyUsage(ev, adapterUsageQuoted(10, 2, 5_000), usage.Rates{InNano: usage.NanoPerUSD, OutNano: usage.NanoPerUSD}, -1)
	if ev.CostNano != 0 || ev.EstimatedCostNano != nil {
		t.Fatalf("local-only: cost %d estimate %v", ev.CostNano, ev.EstimatedCostNano)
	}
}

// The report endpoint is for administrators only and aggregates the
// stored quotes and estimates.
func TestPricingAccuracyEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	for _, e := range []struct {
		quote int64
		est   *int64
	}{{1_000_000, ptr(int64(1_000_000))}, {1_000_000, ptr(int64(1_200_000))}, {2_000_000, ptr(int64(0))}, {3_000_000, nil}} {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{CreatedAt: at, UserID: h.user.ID, UpstreamID: h.model.UpstreamID,
			ModelID: h.model.ID, ModelName: h.model.Name, RequestID: "req-" + store.NewID(), HTTPStatus: 200, Modality: "chat",
			AccountingMode: usage.AccountingUpstreamCost, CostNano: e.quote, EstimatedCostNano: e.est}); err != nil {
			t.Fatal(err)
		}
	}

	nonAdmin, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-plain", "plain@example.com", "Plain User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := h.server.Sessions.Create(ctx, nonAdmin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(plain, http.MethodGet, "/api/v1/admin/pricing/accuracy", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d %s", rec.Code, rec.Body.String())
	}

	rec := h.doAsSession(adminSession(t, h), http.MethodGet, "/api/v1/admin/pricing/accuracy?range=week&worst=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Range  string                    `json:"range"`
		Report store.PriceAccuracyReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	tot := body.Report.Totals
	if body.Range != "week" || tot.QuotedCalls != 4 || tot.ComparedCalls != 3 || tot.QuotedNano != 4_000_000 || tot.EstimateNano != 2_200_000 ||
		tot.Within1 != 1 || tot.Within5 != 1 || tot.NoPriceCalls != 1 || tot.DiffPercent == nil || *tot.DiffPercent != -45 {
		t.Fatalf("totals = %+v", tot)
	}
	if len(body.Report.Models) != 1 || len(body.Report.Providers) != 1 || len(body.Report.Worst) != 2 ||
		body.Report.Worst[0].DiffNano != -2_000_000 || body.Report.Models[0].HasPriceNow == nil || !*body.Report.Models[0].HasPriceNow {
		t.Fatalf("report = %+v", body.Report)
	}

	// The worst call's link finds that one request, whatever the range.
	id := body.Report.Worst[0].RequestID
	rec = h.doAsSession(adminSession(t, h), http.MethodGet, "/api/v1/requests?scope=all&range=day&include_internal=true&request_id="+id, nil)
	var list struct {
		Requests []store.UsageEvent `json:"requests"`
		Total    int                `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Total != 1 || list.Requests[0].RequestID != id ||
		list.Requests[0].EstimatedCostNano == nil {
		t.Fatalf("request_id link: %d %s", rec.Code, rec.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }

// adapterUsageQuoted is a provider usage block with a quoted cost.
func adapterUsageQuoted(in, out, costNano int64) adapter.Usage {
	return adapter.Usage{Reported: true, TokensIn: in, TokensOut: out, CostReported: true, CostNano: costNano}
}
