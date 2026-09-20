package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// TestListModelsCarriesHealth pins the health block on GET /api/v1/models: the
// owning upstream's probe verdict (as the admin Upstreams page shows it) and
// the model's 10-minute error rollup, with the derived health verdict.
func TestListModelsCarriesHealth(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	catalog := func(t *testing.T) map[string]any {
		t.Helper()
		rec := h.do(http.MethodGet, "/api/v1/models", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/v1/models returned %d: %s", rec.Code, rec.Body.String())
		}
		models := decodeBody(t, rec)["models"].([]any)
		if len(models) != 1 {
			t.Fatalf("catalog has %d entries, want 1", len(models))
		}
		return models[0].(map[string]any)
	}

	t.Run("never probed, no traffic is unknown", func(t *testing.T) {
		entry := catalog(t)
		if entry["health"] != store.ModelHealthUnknown {
			t.Errorf("health = %v, want %q", entry["health"], store.ModelHealthUnknown)
		}
		if entry["upstream_reachable"] != false {
			t.Errorf("upstream_reachable = %v, want false before any probe", entry["upstream_reachable"])
		}
		if entry["request_count_10m"].(float64) != 0 || entry["error_count_10m"].(float64) != 0 || entry["error_rate_percent"].(float64) != 0 {
			t.Errorf("expected an empty rollup, got %v/%v (%v%%)", entry["error_count_10m"], entry["request_count_10m"], entry["error_rate_percent"])
		}
	})

	t.Run("reachable upstream with mixed traffic", func(t *testing.T) {
		if err := h.store.RecordUpstreamCheck(ctx, h.model.UpstreamID, 37*time.Millisecond, ""); err != nil {
			t.Fatalf("record upstream check: %v", err)
		}
		now := time.Now().UTC()
		insert := func(age time.Duration, status int, code string) {
			t.Helper()
			if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
				CreatedAt: now.Add(-age), UserID: h.user.ID, UpstreamID: h.model.UpstreamID,
				ModelID: h.model.ID, ModelName: h.model.Name, Modality: "chat", HTTPStatus: status, ErrorCode: code,
			}); err != nil {
				t.Fatalf("insert usage event: %v", err)
			}
		}
		insert(time.Minute, 200, "")
		insert(2*time.Minute, 200, "")
		insert(3*time.Minute, 200, "")
		insert(4*time.Minute, 502, "")
		insert(5*time.Minute, 429, CodeQuotaExceeded) // policy rejection: not counted
		insert(30*time.Minute, 503, CodeUpstreamDown) // outside the window

		entry := catalog(t)
		if entry["upstream_reachable"] != true {
			t.Errorf("upstream_reachable = %v, want true", entry["upstream_reachable"])
		}
		if got := entry["upstream_last_latency_ms"].(float64); got != 37 {
			t.Errorf("upstream_last_latency_ms = %v, want 37", got)
		}
		if entry["upstream_last_error"] != "" {
			t.Errorf("upstream_last_error = %v, want empty", entry["upstream_last_error"])
		}
		if got := entry["request_count_10m"].(float64); got != 4 {
			t.Errorf("request_count_10m = %v, want 4", got)
		}
		if got := entry["error_count_10m"].(float64); got != 1 {
			t.Errorf("error_count_10m = %v, want 1", got)
		}
		if got := entry["error_rate_percent"].(float64); got != 25 {
			t.Errorf("error_rate_percent = %v, want 25", got)
		}
		if entry["health"] != store.ModelHealthDegraded {
			t.Errorf("health = %v, want %q at a 25%% error rate", entry["health"], store.ModelHealthDegraded)
		}
	})

	t.Run("unreachable upstream marks the model down", func(t *testing.T) {
		if err := h.store.RecordUpstreamCheck(ctx, h.model.UpstreamID, 0, "dial tcp: connection refused"); err != nil {
			t.Fatalf("record upstream check: %v", err)
		}
		entry := catalog(t)
		if entry["upstream_reachable"] != false {
			t.Errorf("upstream_reachable = %v, want false", entry["upstream_reachable"])
		}
		if entry["upstream_last_error"] != "dial tcp: connection refused" {
			t.Errorf("upstream_last_error = %v, want the probe error", entry["upstream_last_error"])
		}
		if entry["health"] != store.ModelHealthDown {
			t.Errorf("health = %v, want %q", entry["health"], store.ModelHealthDown)
		}
	})

	t.Run("proxy /v1/models stays lean", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/v1/models returned %d", rec.Code)
		}
		entry := decodeBody(t, rec)["data"].([]any)[0].(map[string]any)
		janus := entry["janus"].(map[string]any)
		if _, ok := janus["health"]; ok {
			t.Errorf("OpenAI-compatible listing must not grow health fields: %v", janus)
		}
	})
}
