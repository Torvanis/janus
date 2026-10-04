package store

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"
)

type perfRow struct {
	model      string
	age        time.Duration
	status     int
	code       string
	streaming  bool
	accounting string
	in, out    int64
	latency    int
	ttfb       int
	source     string
	tps        float64
}

func insertPerf(t *testing.T, s *Store, now time.Time, rows ...perfRow) {
	t.Helper()
	for _, r := range rows {
		accounting := r.accounting
		if accounting == "" {
			accounting = "upstream_reported"
		}
		if err := s.InsertUsageEvent(context.Background(), &UsageEvent{
			CreatedAt: now.Add(-r.age), UserID: "u1", TokenID: "t1", UpstreamID: "up",
			ModelID: r.model, ModelName: "name-" + r.model, Modality: "chat",
			HTTPStatus: r.status, ErrorCode: r.code, Streaming: r.streaming,
			AccountingMode: accounting, TokensIn: r.in, TokensOut: r.out,
			LatencyMs: r.latency, TTFBMs: r.ttfb, ThroughputSource: r.source, TokensOutPerSecond: r.tps,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}
}

func TestModelActivityJobsPerDayAndTrend(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	day := 24 * time.Hour
	var rows []perfRow
	// A: 21 jobs this week, 14 last week -> 3.0/day vs 2.0/day = +50% up.
	for i := 0; i < 21; i++ {
		rows = append(rows, perfRow{model: "A", age: time.Minute + time.Duration(i)*7*day/22, status: 200})
	}
	for i := 0; i < 14; i++ {
		rows = append(rows, perfRow{model: "A", age: 7*day + time.Hour + time.Duration(i)*time.Hour, status: 200})
	}
	// Policy refusals are not jobs.
	rows = append(rows, perfRow{model: "A", age: time.Hour, status: 429, code: "policy.quota_exceeded"})
	// B: 7 this week, 14 last week -> -50% down.
	for i := 0; i < 7; i++ {
		rows = append(rows, perfRow{model: "B", age: time.Duration(i+1) * time.Hour, status: 200})
	}
	for i := 0; i < 14; i++ {
		rows = append(rows, perfRow{model: "B", age: 8*day + time.Duration(i)*time.Hour, status: 200})
	}
	// C: 10 vs 10 -> flat. D: traffic only this week -> new. E: older than 14 days -> absent.
	for i := 0; i < 10; i++ {
		rows = append(rows,
			perfRow{model: "C", age: time.Duration(i+1) * time.Hour, status: 200},
			perfRow{model: "C", age: 9*day + time.Duration(i)*time.Hour, status: 200})
	}
	rows = append(rows, perfRow{model: "D", age: time.Hour, status: 200},
		perfRow{model: "E", age: 15 * day, status: 200})
	insertPerf(t, s, now, rows...)

	stats, err := s.ModelActivityStats(context.Background(), now)
	if err != nil {
		t.Fatalf("ModelActivityStats: %v", err)
	}
	check := func(id string, perDay, prev float64, trend string, change float64) {
		t.Helper()
		got := stats[id]
		if got.JobsPerDay7d != perDay || got.JobsPerDayPrev7d != prev || got.JobsTrend != trend || got.JobsChangePercent != change {
			t.Errorf("%s = %+v, want perDay=%v prev=%v trend=%s change=%v", id, got, perDay, prev, trend, change)
		}
	}
	check("A", 3, 2, TrendUp, 50)
	check("B", 1, 2, TrendDown, -50)
	check("C", 1.4, 1.4, TrendFlat, 0)
	check("D", 0.1, 0, TrendNew, 0)
	if _, ok := stats["E"]; ok {
		t.Errorf("model E has no traffic in 14 days and must be absent, got %+v", stats["E"])
	}
}

// Speed is the plain average of the tokens/s already recorded on each request
// (the request log's figure), minus the rows whose figure is noise.
func TestModelActivitySpeedAveragesRecordedRate(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	ok := func(tps float64, source string, streaming bool) perfRow {
		return perfRow{model: "A", age: time.Hour, status: 200, streaming: streaming, out: 200, source: source, tps: tps}
	}
	insertPerf(t, s, now,
		// Qualifying: mean of 40, 60 and 20 = 40.
		ok(40, "calculated", true),
		ok(60, "upstream", true),
		ok(20, "upstream", false), // provider-measured, so buffered is fine
		// Excluded: gateway-calculated on a buffered response (prefill inside),
		// tiny reply, failure, interrupted stream, byte estimate, no figure.
		ok(2, "calculated", false),
		perfRow{model: "A", age: time.Hour, status: 200, streaming: true, out: 3, source: "calculated", tps: 0.5},
		perfRow{model: "A", age: time.Hour, status: 503, streaming: true, out: 500, source: "calculated", tps: 900},
		perfRow{model: "A", age: time.Hour, status: 200, code: "upstream.stream_interrupted", streaming: true, out: 900, source: "calculated", tps: 900},
		perfRow{model: "A", age: time.Hour, status: 200, streaming: true, accounting: "byte_count_fallback", out: 12_000, source: "calculated", tps: 2600},
		perfRow{model: "A", age: time.Hour, status: 200, streaming: true, out: 300},
		// Older than 7 days: counts toward last week's jobs, never toward speed.
		perfRow{model: "A", age: 8 * 24 * time.Hour, status: 200, streaming: true, out: 1_000, source: "upstream", tps: 999},
	)
	stats, err := s.ModelActivityStats(context.Background(), now)
	if err != nil {
		t.Fatalf("ModelActivityStats: %v", err)
	}
	got := stats["A"]
	if got.TokensPerSecond7d != 40 || got.SpeedSamples7d != 3 {
		t.Errorf("speed = %v tok/s from %d samples, want 40 from 3", got.TokensPerSecond7d, got.SpeedSamples7d)
	}
}

func TestModelPerformanceSeries(t *testing.T) {
	s := newTestStore(t)
	end := time.Now().UTC().Truncate(time.Second)
	bucket := time.Hour
	var rows []perfRow
	// Model A (most used): in the newest bucket, 3 requests of 20 minutes each
	// in flight -> 60 min in flight over a 60 min bucket = concurrency 1.0.
	for i := 0; i < 3; i++ {
		rows = append(rows, perfRow{model: "A", age: time.Duration(i+1) * time.Minute, status: 200, streaming: true,
			in: int64(1000 * (i + 1)), out: 100, latency: 20 * 60 * 1000, source: "calculated", tps: float64(40 + 10*i)})
	}
	// One failure in the same bucket: counts toward concurrency (it held a
	// slot) but not toward input context or speed.
	rows = append(rows, perfRow{model: "A", age: 5 * time.Minute, status: 503, code: "upstream.unavailable", in: 99_999, latency: 0})
	// Model A in the oldest bucket only: 1 request.
	rows = append(rows, perfRow{model: "A", age: 3*time.Hour + 30*time.Minute, status: 200, in: 500, latency: 1000})
	// Model B: fewer requests. Model C: only policy refusals -> not a model.
	rows = append(rows, perfRow{model: "B", age: 10 * time.Minute, status: 200, in: 10, latency: 100})
	rows = append(rows, perfRow{model: "C", age: 10 * time.Minute, status: 403, code: "policy.model_not_granted"},
		perfRow{model: "C", age: 11 * time.Minute, status: 403, code: "policy.model_not_granted"},
		perfRow{model: "C", age: 12 * time.Minute, status: 403, code: "policy.model_not_granted"},
		perfRow{model: "C", age: 13 * time.Minute, status: 403, code: "policy.model_not_granted"},
		perfRow{model: "C", age: 14 * time.Minute, status: 403, code: "policy.model_not_granted"},
		perfRow{model: "C", age: 15 * time.Minute, status: 403, code: "policy.model_not_granted"})
	insertPerf(t, s, end, rows...)

	got, err := s.ModelPerformanceSeriesFor(context.Background(), UsageScope{Start: end.Add(-4 * bucket), End: end}, bucket, 4, 1)
	if err != nil {
		t.Fatalf("ModelPerformanceSeriesFor: %v", err)
	}
	if len(got.Buckets) != 4 || got.BucketSeconds != 3600 {
		t.Fatalf("buckets = %d (%ds), want 4 (3600s)", len(got.Buckets), got.BucketSeconds)
	}
	if len(got.Models) != 1 || got.Models[0].ModelID != "A" || got.Models[0].Requests != 5 {
		t.Fatalf("top models = %+v, want only A with 5 requests (top=1, policy refusals ignored)", got.Models)
	}
	a := got.Models[0]
	if a.Concurrency[3] != 1 {
		t.Errorf("newest-bucket concurrency = %v, want 1", a.Concurrency[3])
	}
	if a.Concurrency[1] != 0 || a.TokensPerSecSer[1] != nil || a.InputTokensSer[1] != nil {
		t.Errorf("empty bucket must be 0 concurrency and nil speed/input, got %v %v %v", a.Concurrency[1], a.TokensPerSecSer[1], a.InputTokensSer[1])
	}
	if v := a.InputTokensSer[3]; v == nil || *v != 2000 {
		t.Errorf("newest-bucket input context = %v, want 2000 (failure excluded)", v)
	}
	if v := a.TokensPerSecSer[3]; v == nil || *v != 50 {
		t.Errorf("newest-bucket speed = %v, want 50 (mean of 40, 50, 60)", v)
	}
	if v := a.InputTokensSer[0]; v == nil || *v != 500 {
		t.Errorf("oldest-bucket input context = %v, want 500", v)
	}
	// Range average: (3×20min + 1s) in flight over 4h.
	wantConc := math.Round((3*20*60*1000.0+1000)/(4*3600*1000.0)*100) / 100
	if a.AvgConcurrency != wantConc {
		t.Errorf("avg concurrency = %v, want %v", a.AvgConcurrency, wantConc)
	}
	if a.AvgInputTokens != 1625 {
		t.Errorf("avg input = %v, want 1625 ((1000+2000+3000+500)/4)", a.AvgInputTokens)
	}

	all, err := s.ModelPerformanceSeriesFor(context.Background(), UsageScope{Start: end.Add(-4 * bucket), End: end}, bucket, 4, 5)
	if err != nil {
		t.Fatalf("ModelPerformanceSeriesFor top=5: %v", err)
	}
	if len(all.Models) != 2 || all.Models[0].ModelID != "A" || all.Models[1].ModelID != "B" {
		t.Errorf("ranking = %+v, want A then B", all.Models)
	}
}

// perfReference is the previous per-event implementation, kept as the oracle
// for the grouped query: same buckets, same rules, every event in Go.
func perfReference(t *testing.T, s *Store, end time.Time, bucket time.Duration, buckets int) map[string][]float64 {
	t.Helper()
	start := end.Add(-bucket * time.Duration(buckets))
	rows, err := s.query(context.Background(), `SELECT model_id, created_at, streaming, http_status, error_code,
		token_accounting_method, tokens_in, tokens_out, latency_ms, throughput_source, tokens_out_per_second
		FROM usage_event WHERE created_at >= ? AND created_at <= ? AND model_id <> '' AND error_code NOT LIKE 'policy.%'`,
		FormatTime(start), FormatTime(end))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	// per model: [requests, then per bucket: latency, tpsSum, tpsN, inputTokens, inputN]
	out := map[string][]float64{}
	for rows.Next() {
		var model, created, code, accounting, source string
		var streaming, status int
		var in, outTok, latency int64
		var tps float64
		if err := rows.Scan(&model, &created, &streaming, &status, &code, &accounting, &in, &outTok, &latency, &source, &tps); err != nil {
			t.Fatal(err)
		}
		idx := buckets - 1 - int(end.Sub(ParseTime(created))/bucket)
		if idx < 0 || idx >= buckets {
			continue
		}
		v := out[model]
		if v == nil {
			v = make([]float64, 1+5*buckets)
		}
		v[0]++
		b := v[1+5*idx:]
		if latency > 0 {
			b[0] += float64(latency)
		}
		if sp, ok := speedSample(streaming != 0, status, code, accounting, outTok, source, tps); ok {
			b[1] += sp
			b[2]++
		}
		if status < 300 && code == "" {
			b[3] += float64(in)
			b[4]++
		}
		out[model] = v
	}
	return out
}

// The grouped query returns exactly what the per-event pass did, for random
// traffic including events exactly on bucket edges.
func TestModelPerformanceSeriesMatchesPerEventPass(t *testing.T) {
	s := newTestStore(t)
	end := time.Now().UTC()
	bucket, buckets := 4*time.Hour, 42
	rng := rand.New(rand.NewSource(7))
	models := []string{"A", "B", "C", "D"}
	codes := []string{"", "", "", "upstream.stream_interrupted", "policy.model_not_granted"}
	sources := []string{"upstream", "calculated", ""}
	var rows []perfRow
	for i := 0; i < 600; i++ {
		age := time.Duration(rng.Int63n(int64(bucket) * int64(buckets+2)))
		if i%15 == 0 {
			age = bucket * time.Duration(rng.Intn(buckets)) // exactly on an edge
		}
		rows = append(rows, perfRow{model: models[rng.Intn(len(models))], age: age, status: []int{200, 200, 200, 429, 503}[rng.Intn(5)],
			code: codes[rng.Intn(len(codes))], streaming: rng.Intn(2) == 0, in: rng.Int63n(5000), out: rng.Int63n(400),
			latency: rng.Intn(30000), source: sources[rng.Intn(len(sources))], tps: float64(rng.Intn(200))})
	}
	insertPerf(t, s, end, rows...)
	got, err := s.ModelPerformanceSeriesFor(context.Background(), UsageScope{Start: end.Add(-bucket * time.Duration(buckets)), End: end}, bucket, buckets, 10)
	if err != nil {
		t.Fatal(err)
	}
	ref := perfReference(t, s, end, bucket, buckets)
	if len(got.Models) != len(ref) {
		t.Fatalf("models = %d, want %d", len(got.Models), len(ref))
	}
	bucketMs := float64(bucket / time.Millisecond)
	for _, m := range got.Models {
		v := ref[m.ModelID]
		if float64(m.Requests) != v[0] {
			t.Errorf("%s requests = %d, want %v", m.ModelID, m.Requests, v[0])
		}
		for i := 0; i < buckets; i++ {
			b := v[1+5*i:]
			if want := round2(b[0] / bucketMs); m.Concurrency[i] != want {
				t.Errorf("%s bucket %d concurrency = %v, want %v", m.ModelID, i, m.Concurrency[i], want)
			}
			if b[2] > 0 {
				if want := round1(b[1] / b[2]); m.TokensPerSecSer[i] == nil || *m.TokensPerSecSer[i] != want {
					t.Errorf("%s bucket %d speed = %v, want %v", m.ModelID, i, m.TokensPerSecSer[i], want)
				}
			} else if m.TokensPerSecSer[i] != nil {
				t.Errorf("%s bucket %d speed = %v, want nil", m.ModelID, i, *m.TokensPerSecSer[i])
			}
			if b[4] > 0 {
				if want := round1(b[3] / b[4]); m.InputTokensSer[i] == nil || *m.InputTokensSer[i] != want {
					t.Errorf("%s bucket %d input = %v, want %v", m.ModelID, i, m.InputTokensSer[i], want)
				}
			} else if m.InputTokensSer[i] != nil {
				t.Errorf("%s bucket %d input = %v, want nil", m.ModelID, i, *m.InputTokensSer[i])
			}
		}
	}
}
