package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// The web catalog carries each model's 7-day activity and the admin overview
// carries the model-performance series. Both are read-time figures over
// usage_event, so this test seeds rows and reads them back through HTTP.
func TestModelPerformanceSurfaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 14; i++ {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Duration(i+1) * time.Hour), UserID: h.user.ID, TokenID: "t",
			UpstreamID: h.model.UpstreamID, ModelID: h.model.ID, ModelName: h.model.Name, Modality: "chat",
			HTTPStatus: 200, Streaming: true, AccountingMode: "upstream_reported",
			TokensIn: 1000, TokensOut: 100, LatencyMs: 3000, TTFBMs: 1000,
			ThroughputSource: "calculated", TokensOutPerSecond: 50,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}

	rec := h.do(http.MethodGet, "/api/v1/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models returned %d: %s", rec.Code, rec.Body.String())
	}
	models := decodeBody(t, rec)["models"].([]any)
	var activity map[string]any
	for _, m := range models {
		if mm := m.(map[string]any); mm["id"] == h.model.ID {
			activity, _ = mm["activity"].(map[string]any)
		}
	}
	if activity == nil {
		t.Fatalf("catalog model has no activity block: %v", models)
	}
	if activity["jobs_per_day_7d"] != 2.0 || activity["jobs_trend"] != store.TrendNew {
		t.Errorf("activity = %v, want 2 jobs/day, trend new", activity)
	}
	if activity["tokens_per_second_7d"] != 50.0 || activity["speed_samples_7d"] != 14.0 {
		t.Errorf("speed = %v tok/s from %v samples, want the recorded 50 from 14", activity["tokens_per_second_7d"], activity["speed_samples_7d"])
	}

	// The OpenAI-compatible listing must not grow gateway-only fields.
	v1 := h.do(http.MethodGet, "/v1/models", nil)
	if v1.Code == http.StatusOK && containsKey(decodeBody(t, v1), "activity") {
		t.Errorf("/v1/models leaked the activity block: %s", v1.Body.String())
	}

	rec = h.do(http.MethodGet, "/api/v1/admin/overview?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin overview returned %d: %s", rec.Code, rec.Body.String())
	}
	perf, ok := decodeBody(t, rec)["model_performance"].(map[string]any)
	if !ok {
		t.Fatalf("overview has no model_performance")
	}
	if b := perf["buckets"].([]any); len(b) != 48 {
		t.Errorf("buckets = %d, want 48 (day range, 30-minute buckets like the org trend)", len(b))
	}
	top := perf["models"].([]any)
	if len(top) != 1 {
		t.Fatalf("models = %v, want the one seeded model", top)
	}
	m := top[0].(map[string]any)
	if m["key"] != h.model.ID || m["requests"] != 14.0 || m["tokens_per_second"] != 50.0 || m["avg_input_tokens"] != 1000.0 {
		t.Errorf("model performance = %v", m)
	}
}

func containsKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[key]; ok {
			return true
		}
		for _, child := range t {
			if containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}
