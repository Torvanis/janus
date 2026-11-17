package store

import (
	"context"
	"testing"
	"time"
)

// The response figures on the admin Overview and the user's model pop-up:
// time to first token (streamed successes only), output tokens per request
// and the token-weighted cache hit rate. Runs on SQLite and, with
// JANUS_ROLLUP_TEST_DATABASE_URL, on PostgreSQL.
func TestModelResponseFigures(t *testing.T) {
	s := newRollupStore(t)
	bucket := time.Hour
	end := time.Now().UTC().Truncate(time.Minute)
	// Bucket 3 (newest) holds rows aged < 1h, bucket 2 rows aged 1-2h.
	newest, older := 10*time.Minute, 90*time.Minute
	ok := func(age time.Duration, streaming bool, ttfb int, in, out, cached int64) perfRow {
		return perfRow{model: "A", age: age, status: 200, streaming: streaming, ttfb: ttfb, in: in, out: out, cached: cached}
	}
	insertPerf(t, s, end,
		// A, newest bucket: TTFT mean of the two streamed rows = (200+400)/2.
		ok(newest, true, 200, 1000, 100, 900),
		ok(newest, true, 400, 3000, 300, 100),
		// Buffered: its ttfb is the whole answer, so it never counts toward
		// TTFT; its tokens and cache do.
		ok(newest, false, 5000, 1000, 200, 0),
		// Excluded from every response figure: a failure, an interrupted
		// stream, a job-status check, the gateway's own classifier and probe
		// calls, and a gateway refusal.
		perfRow{model: "A", age: newest, status: 503, streaming: true, ttfb: 9000, in: 9000, out: 9000, cached: 9000},
		perfRow{model: "A", age: newest, status: 200, code: "upstream.stream_interrupted", streaming: true, ttfb: 9000, in: 9000, out: 9000},
		perfRow{model: "A", age: newest, status: 200, accounting: "not_billable", ttfb: 9000},
		perfRow{model: "A", age: newest, status: 200, streaming: true, ttfb: 9000, in: 9000, out: 9000, app: InternalClientApp},
		perfRow{model: "A", age: newest, status: 200, streaming: true, ttfb: 9000, in: 9000, out: 9000, app: InternalProbeClientApp},
		perfRow{model: "A", age: newest, status: 429, code: "policy.quota_exceeded", streaming: true, ttfb: 9000},
		// A byte estimate has a real first byte but no real token counts.
		perfRow{model: "A", age: newest, status: 200, streaming: true, accounting: "byte_count_fallback", ttfb: 600, in: 9000, out: 9000},
		// A, older bucket: reports input but no cache hit -> 0%, not a gap.
		ok(older, true, 100, 500, 50, 0),
		// B never reports cached tokens: "not reported", never 0%.
		perfRow{model: "B", age: newest, status: 200, streaming: true, ttfb: 50, in: 400, out: 40},
		// C reports only cache writes (Anthropic style): reported, rate 0.
		perfRow{model: "C", age: newest, status: 200, streaming: true, ttfb: 70, in: 100, out: 10, write5m: 900},
	)
	got, err := s.ModelPerformanceSeriesFor(context.Background(), UsageScope{Start: end.Add(-4 * bucket), End: end}, bucket, 4, PerfSelect{})
	if err != nil {
		t.Fatalf("ModelPerformanceSeriesFor: %v", err)
	}
	by := map[string]ModelPerformance{}
	for _, m := range got.Models {
		by[m.ModelID] = m
	}
	a := by["A"]
	// TTFT: newest bucket (200+400+600)/3 = 400 (the byte estimate's first
	// byte is real); older 100; range (200+400+600+100)/4 = 325.
	wantPtr(t, "A ttft[3]", a.TTFTSer[3], 400)
	wantPtr(t, "A ttft[2]", a.TTFTSer[2], 100)
	wantNil(t, "A ttft[0]", a.TTFTSer[0])
	wantPtr(t, "A avg ttft", a.AvgTTFTMs, 325)
	if a.TTFTSamples != 4 {
		t.Errorf("A ttft samples = %d, want 4", a.TTFTSamples)
	}
	// Output: newest (100+300+200)/3 = 200; range (100+300+200+50)/4 = 162.5.
	wantPtr(t, "A out[3]", a.OutputTokensSer[3], 200)
	wantPtr(t, "A avg out", a.AvgOutputTokens, 162.5)
	// Cache, token-weighted: newest (900+100)/(1000+3000+1000) = 0.2, NOT
	// the mean of per-request rates (0.9, 0.033, 0) = 0.311. Older bucket
	// had input and no hits: 0. Range 1000/5500 = 0.182.
	if !a.CacheReported {
		t.Fatal("A reported cached tokens")
	}
	wantPtr(t, "A cache[3]", a.CacheHitSer[3], 0.2)
	wantPtr(t, "A cache[2]", a.CacheHitSer[2], 0)
	wantNil(t, "A cache[0] (no traffic)", a.CacheHitSer[0])
	wantPtr(t, "A cache rate", a.CacheHitRate, 0.182)

	b := by["B"]
	if b.CacheReported || b.CacheHitRate != nil || b.CacheHitSer[3] != nil {
		t.Errorf("B never reported cache: reported=%v rate=%v series=%v", b.CacheReported, b.CacheHitRate, b.CacheHitSer[3])
	}
	wantPtr(t, "B avg ttft", b.AvgTTFTMs, 50)
	c := by["C"]
	if !c.CacheReported {
		t.Error("C wrote to the cache, so it reports cache use")
	}
	wantPtr(t, "C cache rate", c.CacheHitRate, 0)

	if len(got.Available) != 3 || got.Available[0].ModelID != "A" {
		t.Errorf("available = %+v, want A, B, C with A first", got.Available)
	}
}

// An explicit choice plots exactly those models in that order; OnlyChosen
// narrows the scan itself and leaves Available empty.
func TestModelPerformanceChoice(t *testing.T) {
	s := newRollupStore(t)
	bucket := time.Hour
	end := time.Now().UTC().Truncate(time.Minute)
	var rows []perfRow
	for i := 0; i < 5; i++ {
		rows = append(rows, perfRow{model: "busy", age: 10 * time.Minute, status: 200, streaming: true, ttfb: 100, in: 10, out: 10})
	}
	rows = append(rows,
		perfRow{model: "quiet", age: 10 * time.Minute, status: 200, streaming: true, ttfb: 300, in: 10, out: 10},
		perfRow{model: "mid", age: 10 * time.Minute, status: 200, streaming: true, ttfb: 200, in: 10, out: 10},
		perfRow{model: "mid", age: 20 * time.Minute, status: 200, streaming: true, ttfb: 200, in: 10, out: 10})
	insertPerf(t, s, end, rows...)
	scope := UsageScope{Start: end.Add(-2 * bucket), End: end}

	top, err := s.ModelPerformanceSeriesFor(context.Background(), scope, bucket, 2, PerfSelect{Top: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Models) != 1 || top.Models[0].ModelID != "busy" || len(top.Available) != 3 {
		t.Fatalf("top 1 = %d models (%v), %d available", len(top.Models), top.Models, len(top.Available))
	}

	chosen, err := s.ModelPerformanceSeriesFor(context.Background(), scope, bucket, 2, PerfSelect{Top: 1, ModelIDs: []string{"quiet", "missing", "mid", "quiet"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chosen.Models) != 2 || chosen.Models[0].ModelID != "quiet" || chosen.Models[1].ModelID != "mid" {
		t.Fatalf("chosen = %+v, want quiet then mid", chosen.Models)
	}

	only, err := s.ModelPerformanceSeriesFor(context.Background(), scope, bucket, 2, PerfSelect{ModelIDs: []string{"quiet"}, OnlyChosen: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Models) != 1 || only.Models[0].ModelID != "quiet" || len(only.Available) != 0 {
		t.Fatalf("only = %+v available=%v", only.Models, only.Available)
	}
	wantPtr(t, "quiet ttft", only.Models[0].AvgTTFTMs, 300)
}

func wantPtr(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", what, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

func wantNil(t *testing.T, what string, got *float64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %v, want nil (a gap)", what, *got)
	}
}
