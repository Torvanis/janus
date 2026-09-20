package store

import (
	"context"
	"testing"
	"time"
)

// TestModelSetHealthVerdicts pins the derivation rules the catalog badges
// depend on: an unreachable upstream is always "down", a run of failures is
// "down" only past the minimum sample, anything over the degraded threshold
// is "degraded", and a never-probed upstream with no traffic is "unknown".
func TestModelSetHealthVerdicts(t *testing.T) {
	probedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	reachable := &Upstream{ID: "up", LastCheckAt: probedAt, LastLatencyMs: 42}
	unreachable := &Upstream{ID: "up", LastCheckAt: probedAt, LastError: "dial tcp: connection refused", LastLatencyMs: 0}
	never := &Upstream{ID: "up"}

	cases := []struct {
		name        string
		up          *Upstream
		stats       ModelHealthStats
		wantHealth  string
		wantRate    float64
		wantReach   bool
		wantLatency int
	}{
		{"reachable, no traffic", reachable, ModelHealthStats{}, ModelHealthHealthy, 0, true, 42},
		{"reachable, clean traffic", reachable, ModelHealthStats{Requests: 40, Errors: 0}, ModelHealthHealthy, 0, true, 42},
		{"reachable, below threshold", reachable, ModelHealthStats{Requests: 40, Errors: 3}, ModelHealthHealthy, 7.5, true, 42},
		{"reachable, at threshold", reachable, ModelHealthStats{Requests: 40, Errors: 4}, ModelHealthDegraded, 10, true, 42},
		{"reachable, two of two failed", reachable, ModelHealthStats{Requests: 2, Errors: 2}, ModelHealthDegraded, 100, true, 42},
		{"reachable, three of three failed", reachable, ModelHealthStats{Requests: 3, Errors: 3}, ModelHealthDown, 100, true, 42},
		{"reachable, one of three failed", reachable, ModelHealthStats{Requests: 3, Errors: 1}, ModelHealthDegraded, 33.3, true, 42},
		{"unreachable, no traffic", unreachable, ModelHealthStats{}, ModelHealthDown, 0, false, 0},
		{"unreachable, clean traffic still down", unreachable, ModelHealthStats{Requests: 10}, ModelHealthDown, 0, false, 0},
		{"never probed, no traffic", never, ModelHealthStats{}, ModelHealthUnknown, 0, false, 0},
		{"never probed, clean traffic", never, ModelHealthStats{Requests: 5}, ModelHealthHealthy, 0, false, 0},
		{"never probed, failing traffic", never, ModelHealthStats{Requests: 5, Errors: 5}, ModelHealthDown, 100, false, 0},
		{"upstream missing", nil, ModelHealthStats{}, ModelHealthUnknown, 0, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{ID: "m", UpstreamID: "up"}
			m.SetHealth(tc.up, tc.stats)
			if m.Health != tc.wantHealth {
				t.Errorf("health = %q, want %q", m.Health, tc.wantHealth)
			}
			if m.ErrorRatePercent != tc.wantRate {
				t.Errorf("error_rate_percent = %v, want %v", m.ErrorRatePercent, tc.wantRate)
			}
			if m.UpstreamReachable != tc.wantReach {
				t.Errorf("upstream_reachable = %v, want %v", m.UpstreamReachable, tc.wantReach)
			}
			if m.UpstreamLastLatencyMs != tc.wantLatency {
				t.Errorf("upstream_last_latency_ms = %d, want %d", m.UpstreamLastLatencyMs, tc.wantLatency)
			}
			if m.RequestCount10m != tc.stats.Requests || m.ErrorCount10m != tc.stats.Errors {
				t.Errorf("counts = %d/%d, want %d/%d", m.ErrorCount10m, m.RequestCount10m, tc.stats.Errors, tc.stats.Requests)
			}
			if tc.up != nil {
				if !m.UpstreamLastCheckAt.Equal(tc.up.LastCheckAt) || m.UpstreamLastError != tc.up.LastError {
					t.Errorf("probe state not copied: at=%v err=%q", m.UpstreamLastCheckAt, m.UpstreamLastError)
				}
			}
		})
	}
}

// TestModelHealthStatsWindowAndClassification covers the rollup query: only
// events inside the window count, policy rejections are excluded from both
// sides of the ratio, and 5xx plus upstream.* codes are the errors.
func TestModelHealthStatsWindowAndClassification(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-ModelHealthWindow)

	insert := func(modelID string, age time.Duration, status int, code string) {
		t.Helper()
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: now.Add(-age), UserID: "u1", TokenID: "t1", UpstreamID: "up",
			ModelID: modelID, ModelName: "m-" + modelID, Modality: "chat", HTTPStatus: status, ErrorCode: code,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}

	// model A: 4 in-window attempts, 2 upstream errors, plus noise.
	insert("A", time.Minute, 200, "")
	insert("A", 2*time.Minute, 200, "")
	insert("A", 3*time.Minute, 503, "upstream.unavailable")
	insert("A", 4*time.Minute, 429, "upstream.rate_limit")
	insert("A", 5*time.Minute, 429, "policy.quota_exceeded")    // policy: excluded entirely
	insert("A", 6*time.Minute, 403, "policy.model_not_granted") // policy: excluded entirely
	insert("A", 11*time.Minute, 500, "server_error")            // outside the window
	// model B: a single client-side 400 is a request, not an upstream error.
	insert("B", time.Minute, 400, "invalid_request_error")
	// events with no model never count.
	insert("", time.Minute, 500, "server_error")

	stats, err := s.ModelHealthStats(ctx, since)
	if err != nil {
		t.Fatalf("ModelHealthStats: %v", err)
	}
	if got := stats["A"]; got.Requests != 4 || got.Errors != 2 {
		t.Errorf("model A = %+v, want {Requests:4 Errors:2}", got)
	}
	if got := stats["A"].ErrorRatePercent(); got != 50 {
		t.Errorf("model A error rate = %v, want 50", got)
	}
	if got := stats["B"]; got.Requests != 1 || got.Errors != 0 {
		t.Errorf("model B = %+v, want {Requests:1 Errors:0}", got)
	}
	if _, ok := stats[""]; ok {
		t.Errorf("events without a model_id must not appear in the rollup")
	}
	if len(stats) != 2 {
		t.Errorf("rollup has %d models, want 2: %v", len(stats), stats)
	}
}
