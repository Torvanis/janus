package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// GET /api/v1/models/{id}/performance answers only for a model in the
// caller's catalog; every other id is a 404, so it never confirms that a
// model exists. The admin Overview plots any model the admin picks.
func TestModelPerformanceEndpointScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A second enabled, priced model that nobody is granted.
	secret := h.seedUnpricedModel("secret-model")
	if err := h.store.SetModelRates(ctx, secret.ID, store.RateCard{
		RateInNano: usage.NanoPerUSD, RateOutNano: usage.NanoPerUSD, EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, secret.ID, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, m := range []*store.Model{h.model, secret} {
		for _, ttfb := range []int{200, 400} {
			if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
				CreatedAt: now.Add(-time.Hour), UserID: h.user.ID, UpstreamID: m.UpstreamID, ModelID: m.ID, ModelName: m.Name,
				Modality: "chat", HTTPStatus: 200, Streaming: true, AccountingMode: "upstream_reported",
				TokensIn: 1000, TokensOut: 100, TokensCached: 250, TTFBMs: ttfb,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	member := h.sessionFor(t, "member@example.com")
	rec := h.doAsSession(member, http.MethodGet, "/api/v1/models/"+h.model.ID+"/performance", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("granted model = %d: %s", rec.Code, rec.Body.String())
	}
	var got ModelPerformanceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AvgTTFTMs == nil || *got.AvgTTFTMs != 300 || got.AvgOutput == nil || *got.AvgOutput != 100 ||
		!got.CacheReported || got.CacheHitRate == nil || *got.CacheHitRate != 0.25 {
		t.Errorf("figures = %+v", got)
	}
	if got.Days != 7 || len(got.TTFTSer) != 7 || got.TTFTSer[6] == nil {
		t.Errorf("want 7 daily points, newest measured: %+v", got.TTFTSer)
	}

	for _, id := range []string{secret.ID, "no-such-model"} {
		rec = h.doAsSession(member, http.MethodGet, "/api/v1/models/"+id+"/performance", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("model %s outside the catalog = %d, want 404: %s", id, rec.Code, rec.Body.String())
		}
	}

	// The admin Overview: the default is the most-used models; ?models=
	// plots exactly the admin's choice, and available lists both.
	admin, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/overview?range=week&part=performance&models="+secret.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin overview = %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Perf store.ModelPerformanceSeries `json:"model_performance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Perf.Models) != 1 || doc.Perf.Models[0].ModelID != secret.ID || len(doc.Perf.Available) != 2 {
		t.Fatalf("chosen models = %+v, available = %+v", doc.Perf.Models, doc.Perf.Available)
	}
	if doc.Perf.Available[0].UpstreamName != "test-upstream" {
		t.Errorf("available entries carry the upstream name: %+v", doc.Perf.Available[0])
	}
	// A member cannot read the admin Overview at all.
	if rec = h.doAsSession(member, http.MethodGet, "/api/v1/admin/overview?part=performance", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member admin overview = %d, want 403", rec.Code)
	}
}
