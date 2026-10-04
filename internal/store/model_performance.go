package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Performance figures are computed from usage_event rows at read time and are
// never persisted. Two consumers:
//
//   - the user's model catalog (ModelActivityStats): jobs per day over the
//     last seven days with a trend against the seven days before, and the
//     model's typical generation speed;
//   - the admin Overview (ModelPerformanceSeries): per-bucket concurrency,
//     generation speed and input context for the most-used models.
//
// The definitions below are shared so both surfaces always agree.

// PerfMinOutputTokens is the smallest response that counts toward a speed
// figure. One- to five-token replies are dominated by fixed overhead and read
// as ~0.5 tok/s, which says nothing about how fast the model generates.
const PerfMinOutputTokens = 16

// ActivityTrendThresholdPercent is the change in jobs per day, against the
// previous seven days, below which the trend is reported as flat.
const ActivityTrendThresholdPercent = 10.0

// Activity trend values.
const (
	TrendUp   = "up"
	TrendDown = "down"
	TrendFlat = "flat"
	// TrendNew marks a model with traffic this week and none the week before:
	// there is no baseline to compute a percentage against.
	TrendNew = "new"
)

// speedSample returns the request's recorded tokens-per-second (the figure
// shown in the request log) when it is a fair speed measurement. The model
// figure is the plain average of these.
//
// A row is skipped when:
//   - it failed (status >= 300 or an error code), so its timing says nothing
//     about the model;
//   - it produced fewer than PerfMinOutputTokens: a 3-token reply records
//     ~0.5 tok/s because fixed overhead dominates;
//   - its tokens were estimated from bytes, because that rate is meaningless
//     (old rows read 2,000+ tok/s);
//   - the gateway calculated the rate for a non-streamed response. Then only
//     the total time is known, prompt processing included, so the rate is
//     far below the real one. Provider-measured rates (llama.cpp timings,
//     Ollama eval_duration) are fine either way.
func speedSample(streaming bool, status int, errorCode, accounting string, tokensOut int64, source string, tps float64) (float64, bool) {
	if status >= 300 || errorCode != "" || accounting == "byte_count_fallback" {
		return 0, false
	}
	if tokensOut < PerfMinOutputTokens || tps <= 0 {
		return 0, false
	}
	if source != "upstream" && !streaming {
		return 0, false
	}
	return tps, true
}

// ModelActivity is the per-model figure set shown on a user's model card.
type ModelActivity struct {
	// JobsPerDay7d is the requests that reached (or tried to reach) the model
	// over the last seven days, divided by seven. Gateway refusals
	// (error_code policy.*) are not jobs and are excluded.
	JobsPerDay7d float64 `json:"jobs_per_day_7d"`
	// JobsPerDayPrev7d is the same figure for the seven days before that.
	JobsPerDayPrev7d float64 `json:"jobs_per_day_prev_7d"`
	// JobsTrend is up, down, flat or new (see ActivityTrendThresholdPercent).
	JobsTrend string `json:"jobs_trend"`
	// JobsChangePercent is the signed change against the previous seven days,
	// rounded to a whole percent; 0 when the trend is new or there is no
	// traffic at all.
	JobsChangePercent float64 `json:"jobs_change_percent"`
	// TokensPerSecond7d is the generation speed over the last seven days (see
	// speedSample); 0 when no request qualified.
	TokensPerSecond7d float64 `json:"tokens_per_second_7d"`
	// SpeedSamples7d is how many requests the speed figure is based on.
	SpeedSamples7d int64 `json:"speed_samples_7d"`
}

// ModelActivityStats computes ModelActivity for every model with traffic in
// the fourteen days before `now`, keyed by model_id. Models with no traffic in
// that window are absent from the map.
func (s *Store) ModelActivityStats(ctx context.Context, now time.Time) (map[string]ModelActivity, error) {
	now = now.UTC()
	weekAgo := now.Add(-7 * 24 * time.Hour)
	twoWeeksAgo := now.Add(-14 * 24 * time.Hour)
	rows, err := s.query(ctx, `SELECT model_id, created_at, streaming, http_status, error_code,
		token_accounting_method, tokens_out, throughput_source, tokens_out_per_second
		FROM usage_event
		WHERE created_at >= ? AND created_at < ? AND model_id <> '' AND error_code NOT LIKE 'policy.%'`,
		FormatTime(twoWeeksAgo), FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("model activity stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type acc struct {
		cur, prev int64
		tpsSum    float64
		samples   int64
	}
	byModel := map[string]*acc{}
	for rows.Next() {
		var modelID, created, errorCode, accounting, source string
		var streaming, status int
		var tokensOut int64
		var tps float64
		if err := rows.Scan(&modelID, &created, &streaming, &status, &errorCode, &accounting, &tokensOut, &source, &tps); err != nil {
			return nil, fmt.Errorf("scan model activity: %w", err)
		}
		a := byModel[modelID]
		if a == nil {
			a = &acc{}
			byModel[modelID] = a
		}
		if ParseTime(created).Before(weekAgo) {
			a.prev++
			continue
		}
		a.cur++
		if v, ok := speedSample(streaming != 0, status, errorCode, accounting, tokensOut, source, tps); ok {
			a.tpsSum += v
			a.samples++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]ModelActivity, len(byModel))
	for id, a := range byModel {
		m := ModelActivity{
			JobsPerDay7d:     round1(float64(a.cur) / 7),
			JobsPerDayPrev7d: round1(float64(a.prev) / 7),
			SpeedSamples7d:   a.samples,
		}
		m.JobsTrend, m.JobsChangePercent = activityTrend(a.cur, a.prev)
		if a.samples > 0 {
			m.TokensPerSecond7d = round1(a.tpsSum / float64(a.samples))
		}
		out[id] = m
	}
	return out, nil
}

// activityTrend compares this week's job count with last week's.
func activityTrend(cur, prev int64) (string, float64) {
	switch {
	case prev == 0 && cur == 0:
		return TrendFlat, 0
	case prev == 0:
		return TrendNew, 0
	}
	change := (float64(cur) - float64(prev)) / float64(prev) * 100
	rounded := float64(int64(change + sign(change)*0.5))
	switch {
	case change >= ActivityTrendThresholdPercent:
		return TrendUp, rounded
	case change <= -ActivityTrendThresholdPercent:
		return TrendDown, rounded
	default:
		return TrendFlat, rounded
	}
}

// ModelPerformance is one model's line on the admin performance charts.
type ModelPerformance struct {
	ModelID   string `json:"key"`
	ModelName string `json:"model_name"`
	// UpstreamID / UpstreamName tell apart same-named models on different
	// upstreams (a pool's members, for instance). The handler fills the name.
	UpstreamID   string `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	// Requests is how many requests the model served in the range (gateway
	// refusals excluded); the top models are ranked by it.
	Requests int64 `json:"requests"`
	// Range averages, for the legend.
	AvgConcurrency  float64 `json:"avg_concurrency"`
	TokensPerSecond float64 `json:"tokens_per_second"`
	AvgInputTokens  float64 `json:"avg_input_tokens"`
	// Per-bucket series, aligned with ModelPerformanceSeries.Buckets. A bucket
	// with nothing to measure is 0 for concurrency and null (nil) for speed
	// and input context, so a chart can show a gap rather than a false zero.
	Concurrency     []float64  `json:"concurrency"`
	TokensPerSecSer []*float64 `json:"tokens_per_second_series"`
	InputTokensSer  []*float64 `json:"input_tokens_series"`
}

// ModelPerformanceSeries is the admin Overview's model-performance payload.
type ModelPerformanceSeries struct {
	Buckets       []string           `json:"buckets"`
	BucketSeconds int64              `json:"bucket_seconds"`
	Models        []ModelPerformance `json:"models"`
}

// ModelPerformanceSeriesFor buckets the scope's traffic for the `top` models
// with the most requests. Bucket alignment matches UsageSeries so the charts
// line up with the org trend above them.
//
//   - Concurrency is time-in-flight ÷ bucket length (Little's law): the sum of
//     the bucket's request latencies divided by the bucket's duration. A
//     request is attributed to the bucket it finished in.
//   - Speed uses the same qualifying rule as the model catalog (speedSample).
//   - Input context is the mean tokens_in (prompt, including cached prefix)
//     of the bucket's successful requests.
func (s *Store) ModelPerformanceSeriesFor(ctx context.Context, scope UsageScope, bucket time.Duration, buckets, top int) (ModelPerformanceSeries, error) {
	if bucket <= 0 || buckets <= 0 {
		return ModelPerformanceSeries{}, fmt.Errorf("model performance requires a positive bucket size and count")
	}
	end := scope.End
	if end.IsZero() {
		end = nowUTC()
	}
	// Same bucket grid as UsageSeries, so the charts line up.
	grid := end
	start := grid.Add(-bucket * time.Duration(buckets))
	if scope.Start.After(start) {
		start = scope.Start
	}
	sc := scope
	sc.Start, sc.End = start, end
	clause, args := sc.clause()

	type bucketAcc struct {
		latencyMs           int64
		tpsSum              float64
		tpsN                int64
		inputTokens, inputN int64
	}
	type modelAcc struct {
		name       string
		upstreamID string
		requests   int64
		buckets    []bucketAcc
	}
	byModel := map[string]*modelAcc{}

	// Aggregate in the database, grouped by model and bucket: a month of
	// traffic is a few hundred rows instead of every event (1.2 M rows
	// took ~5 s to stream). The bucket index is computed from the
	// edge timestamps (fixed-width text, so string order is time order) with
	// the same rule as before: bucket k (0 = newest) holds edge(k+1) < t <=
	// edge(k). The other expressions mirror speedSample exactly.
	var idxExpr strings.Builder
	idxArgs := make([]any, 0, buckets)
	idxExpr.WriteString("CASE")
	for k := 0; k < buckets; k++ {
		idxExpr.WriteString(" WHEN created_at > ? THEN " + fmt.Sprint(buckets-1-k))
		idxArgs = append(idxArgs, FormatTime(grid.Add(-bucket*time.Duration(k+1))))
	}
	idxExpr.WriteString(" ELSE -1 END")
	ok := `(http_status < 300 AND error_code = '')`
	speed := ok + ` AND token_accounting_method <> 'byte_count_fallback' AND tokens_out >= ` +
		fmt.Sprint(PerfMinOutputTokens) + ` AND tokens_out_per_second > 0 AND (throughput_source = 'upstream' OR streaming <> 0)`
	rows, err := s.query(ctx, `SELECT model_id, MIN(model_name), MIN(upstream_id), b, COUNT(*),
		COALESCE(SUM(CASE WHEN latency_ms > 0 THEN latency_ms ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN `+ok+` THEN tokens_in ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN `+ok+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN `+speed+` THEN tokens_out_per_second ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN `+speed+` THEN 1 ELSE 0 END),0)
		FROM (SELECT *, `+idxExpr.String()+` AS b FROM usage_event WHERE `+clause+` AND model_id <> '' AND error_code NOT LIKE 'policy.%') e
		WHERE b >= 0 GROUP BY model_id, b`, append(idxArgs, args...)...)
	if err != nil {
		return ModelPerformanceSeries{}, fmt.Errorf("model performance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var modelID, modelName, upstreamID string
		var idx int
		var requests, latencyMs, inputTokens, inputN, tpsN int64
		var tpsSum float64
		if err := rows.Scan(&modelID, &modelName, &upstreamID, &idx, &requests, &latencyMs, &inputTokens, &inputN, &tpsSum, &tpsN); err != nil {
			return ModelPerformanceSeries{}, fmt.Errorf("scan model performance: %w", err)
		}
		if idx < 0 || idx >= buckets {
			continue
		}
		m := byModel[modelID]
		if m == nil {
			m = &modelAcc{name: modelName, upstreamID: upstreamID, buckets: make([]bucketAcc, buckets)}
			byModel[modelID] = m
		}
		m.requests += requests
		b := &m.buckets[idx]
		b.latencyMs += latencyMs
		b.inputTokens += inputTokens
		b.inputN += inputN
		b.tpsSum += tpsSum
		b.tpsN += tpsN
	}
	if err := rows.Err(); err != nil {
		return ModelPerformanceSeries{}, err
	}

	ids := make([]string, 0, len(byModel))
	for id := range byModel {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := byModel[ids[i]], byModel[ids[j]]
		if a.requests != b.requests {
			return a.requests > b.requests
		}
		return ids[i] < ids[j]
	})
	if top > 0 && len(ids) > top {
		ids = ids[:top]
	}

	out := ModelPerformanceSeries{
		Buckets:       make([]string, buckets),
		BucketSeconds: int64(bucket / time.Second),
		Models:        make([]ModelPerformance, 0, len(ids)),
	}
	for i := range out.Buckets {
		out.Buckets[i] = FormatTime(grid.Add(-bucket * time.Duration(buckets-i-1)).Truncate(time.Second))
	}
	// Concurrency for the first bucket is measured against the part of it the
	// range actually covers, so a range starting mid-bucket does not dilute it.
	bucketMs := func(i int) float64 {
		bEnd := grid.Add(-bucket * time.Duration(buckets-i-1))
		bStart := bEnd.Add(-bucket)
		if bStart.Before(start) {
			bStart = start
		}
		ms := float64(bEnd.Sub(bStart) / time.Millisecond)
		if ms <= 0 {
			return 0
		}
		return ms
	}
	var rangeMs float64
	for i := 0; i < buckets; i++ {
		rangeMs += bucketMs(i)
	}
	for _, id := range ids {
		m := byModel[id]
		p := ModelPerformance{
			ModelID:         id,
			ModelName:       m.name,
			UpstreamID:      m.upstreamID,
			Requests:        m.requests,
			Concurrency:     make([]float64, buckets),
			TokensPerSecSer: make([]*float64, buckets),
			InputTokensSer:  make([]*float64, buckets),
		}
		var latency, tpsN, inputTokens, inputN int64
		var tpsSum float64
		for i, b := range m.buckets {
			if ms := bucketMs(i); ms > 0 {
				p.Concurrency[i] = round2(float64(b.latencyMs) / ms)
			}
			if b.tpsN > 0 {
				v := round1(b.tpsSum / float64(b.tpsN))
				p.TokensPerSecSer[i] = &v
			}
			if b.inputN > 0 {
				v := round1(float64(b.inputTokens) / float64(b.inputN))
				p.InputTokensSer[i] = &v
			}
			latency += b.latencyMs
			tpsSum += b.tpsSum
			tpsN += b.tpsN
			inputTokens += b.inputTokens
			inputN += b.inputN
		}
		if rangeMs > 0 {
			p.AvgConcurrency = round2(float64(latency) / rangeMs)
		}
		if tpsN > 0 {
			p.TokensPerSecond = round1(tpsSum / float64(tpsN))
		}
		if inputN > 0 {
			p.AvgInputTokens = round1(float64(inputTokens) / float64(inputN))
		}
		out.Models = append(out.Models, p)
	}
	return out, nil
}

func round1(v float64) float64 { return float64(int64(v*10+sign(v)*0.5)) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+sign(v)*0.5)) / 100 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
