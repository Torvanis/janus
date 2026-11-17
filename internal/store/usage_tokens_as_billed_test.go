package store

import (
	"context"
	"testing"
	"time"
)

// Input tokens as billed: tokens_in is the whole prompt for every provider
// (cached tokens and cache writes are parts of it), so every figure reads
// tokens_in as it is. Runs on SQLite and, with JANUS_ROLLUP_TEST_DATABASE_URL,
// on PostgreSQL.

// TestTokensAsBilledFigures: the worked example (50k fresh + 40k cache reads,
// recorded as tokens_in 90k) reads 44% cached and a 90k average prompt, the
// same as an OpenAI request of 90k with 40k cached; nothing adds the cache
// parts a second time, raw or through the rollup.
func TestTokensAsBilledFigures(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	end := time.Now().UTC().Truncate(time.Minute)
	insert := func(model string, in, cached, write5m int64) {
		t.Helper()
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: end.Add(-10 * time.Minute), UserID: "u1", TokenID: "t1", UpstreamID: "up",
			ModelID: model, ModelName: "name-" + model, Modality: "chat", HTTPStatus: 200, Streaming: true,
			AccountingMode: "upstream_reported", TokensIn: in, TokensOut: 100, TokensCached: cached,
			TokensCacheWrite5m: write5m, CostNano: 177_000_000,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert("anthropic-a", 90_000, 40_000, 0) // 50k fresh + 40k read
	insert("anthropic-b", 92_000, 90_000, 0) // 2k fresh + 90k read
	insert("openai", 90_000, 40_000, 0)
	insert("anthropic-w", 10_000, 0, 9_000) // 1k fresh + 9k written
	// Bad data: more cached than prompt reads 100%, never more.
	insert("bad", 100, 500, 0)

	got, err := s.ModelPerformanceSeriesFor(ctx, UsageScope{Start: end.Add(-4 * time.Hour), End: end}, time.Hour, 4, PerfSelect{})
	if err != nil {
		t.Fatalf("ModelPerformanceSeriesFor: %v", err)
	}
	by := map[string]ModelPerformance{}
	for _, m := range got.Models {
		by[m.ModelID] = m
	}
	wantPtr(t, "anthropic-a cache rate", by["anthropic-a"].CacheHitRate, 0.444)
	wantPtr(t, "anthropic-b cache rate", by["anthropic-b"].CacheHitRate, 0.978)
	wantPtr(t, "openai cache rate", by["openai"].CacheHitRate, 0.444)
	wantPtr(t, "anthropic-w cache rate", by["anthropic-w"].CacheHitRate, 0)
	wantPtr(t, "bad cache rate", by["bad"].CacheHitRate, 1)
	for model, want := range map[string]float64{"anthropic-a": 90_000, "anthropic-b": 92_000, "openai": 90_000, "anthropic-w": 10_000, "bad": 100} {
		if got := by[model].AvgInputTokens; got != want {
			t.Errorf("%s avg input = %v, want %v (tokens_in, the whole prompt)", model, got, want)
		}
	}

	wantIn := int64(90_000 + 92_000 + 90_000 + 10_000 + 100)
	check := func(label string) {
		t.Helper()
		tot, err := s.AggregateUsage(ctx, UsageScope{UserID: "u1"})
		if err != nil {
			t.Fatalf("%s aggregate: %v", label, err)
		}
		if tot.TokensIn != wantIn || tot.TokensCached != 170_500 || tot.CostNano != 5*177_000_000 {
			t.Errorf("%s totals: in=%d cached=%d cost=%d, want in %d (no part counted twice)", label, tot.TokensIn, tot.TokensCached, tot.CostNano, wantIn)
		}
		byModel, err := s.AggregateUsageBy(ctx, UsageScope{UserID: "u1"}, "model")
		if err != nil {
			t.Fatalf("%s aggregate by model: %v", label, err)
		}
		if in := byModel["name-anthropic-a"].TokensIn; in != 90_000 {
			t.Errorf("%s anthropic-a tokens_in = %d, want 90000", label, in)
		}
		series, err := s.UsageSeries(ctx, UsageScope{UserID: "u1", End: end}, time.Hour, 3)
		if err != nil {
			t.Fatalf("%s series: %v", label, err)
		}
		var sum int64
		for _, p := range series {
			sum += p.Totals.TokensIn
		}
		if sum != wantIn {
			t.Errorf("%s series tokens_in sum = %d, want %d", label, sum, wantIn)
		}
	}
	check("raw")
	for i := 0; i < 50; i++ {
		done, err := s.RollupUsage(ctx, end.Add(time.Hour))
		if err != nil {
			t.Fatalf("rollup: %v", err)
		}
		if done {
			break
		}
	}
	check("rollup")

	// The efficiency report: cache rate = cached / tokens_in.
	facts := []*reportFact{}
	events, _, err := s.ListRequests(ctx, RequestFilter{Limit: 100, IncludeInternal: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		facts = append(facts, &reportFact{e: e})
	}
	rate := reportMetrics(facts, []string{"cache_hit_rate"})["cache_hit_rate"]
	if want := float64(170_500) / float64(wantIn); rate == nil || *rate != want {
		t.Errorf("report cache_hit_rate = %v, want %v", rate, want)
	}

	// The request log filters and sorts on the same tokens_in.
	gt := int64(89_999)
	filtered, _, err := s.ListRequests(ctx, RequestFilter{Limit: 100, IncludeInternal: true, TokensInGT: &gt})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 3 {
		t.Errorf("tokens_in > 89999 matched %d rows, want 3 (both 90k prompts and the 92k one)", len(filtered))
	}
}

// usageRow is one seeded event and what migration 0083 must leave in it.
type usageRow struct {
	upstream, modality   string
	at                   time.Time
	in, cached, w5m, w1h int64
	flagged              bool // cached_disjoint set by the adapter (0082 era)
	wantIn               int64
	cost                 int64
}

func seedTokenRows(t *testing.T, s *Store, rows map[string]usageRow) {
	t.Helper()
	ctx := context.Background()
	for id, r := range rows {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			ID: id, CreatedAt: r.at, UserID: "u1", TokenID: "tok", UpstreamID: r.upstream, ModelName: "m-" + r.upstream, Modality: r.modality,
			TokensIn: r.in, TokensOut: 10, TokensCached: r.cached, TokensCacheWrite5m: r.w5m, TokensCacheWrite1h: r.w1h,
			CostNano: r.cost, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
}

func rollTokenRows(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	for i := 0; i < 100; i++ {
		done, err := s.RollupUsage(context.Background(), now)
		if err != nil {
			t.Fatalf("rollup: %v", err)
		}
		if done {
			return
		}
	}
}

// checkTokenRows asserts each row's tokens_in and cost, that no row is left
// flagged, and that the rollup equals the raw log bucket by bucket.
func checkTokenRows(t *testing.T, s *Store, rows map[string]usageRow) {
	t.Helper()
	ctx := context.Background()
	for id, r := range rows {
		e, err := s.UsageEventByID(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if e.TokensIn != r.wantIn || e.CostNano != r.cost || e.TokensCached != r.cached || e.TokensCacheWrite5m != r.w5m || e.TokensCacheWrite1h != r.w1h {
			t.Errorf("%s: in=%d cost=%d cached=%d writes=%d/%d, want in %d, cost %d unchanged, parts unchanged",
				id, e.TokensIn, e.CostNano, e.TokensCached, e.TokensCacheWrite5m, e.TokensCacheWrite1h, r.wantIn, r.cost)
		}
	}
	var flagged int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM usage_event WHERE cached_disjoint <> 0 AND tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h > 0`).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if flagged != 0 {
		t.Errorf("%d rows still flagged with cache tokens", flagged)
	}
	// Every rollup bucket equals the raw log's sum for it.
	rows2, err := s.query(ctx, `SELECT r.bucket, r.tokens_in, COALESCE(e.s, -1) FROM usage_rollup r LEFT JOIN
		(SELECT `+rollupHourExpr+` AS b, model_name, SUM(tokens_in) AS s FROM usage_event GROUP BY `+rollupHourExpr+`, model_name) e
		ON e.b = r.bucket AND e.model_name = r.model_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows2.Close() }()
	n := 0
	for rows2.Next() {
		var bucket string
		var rolled, raw int64
		if err := rows2.Scan(&bucket, &rolled, &raw); err != nil {
			t.Fatal(err)
		}
		n++
		if rolled != raw {
			t.Errorf("rollup bucket %s tokens_in = %d, raw log = %d", bucket, rolled, raw)
		}
	}
	if n == 0 {
		t.Error("no rollup rows to check")
	}
	var wantIn int64
	for _, r := range rows {
		wantIn += r.wantIn
	}
	tot, err := s.AggregateUsage(ctx, UsageScope{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if tot.TokensIn != wantIn || tot.Requests != int64(len(rows)) {
		t.Errorf("aggregate tokens_in = %d over %d requests, want %d over %d", tot.TokensIn, tot.Requests, wantIn, len(rows))
	}
}

// TestTokensInAsBilledMigrationFromBefore0082 is the upgrade of a database
// that never ran 0082 (production): 0082 flags the Anthropic and Bedrock rows
// that carry cache tokens, 0083 moves the parts into tokens_in and corrects
// the rollup buckets those rows were folded into. OpenAI-style rows, guard
// and decision calls and personal plans are untouched; costs never move.
func TestTokensInAsBilledMigrationFromBefore0082(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	anth, err := s.CreateUpstream(ctx, "anth", "anthropic", "https://api.anthropic.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bed, err := s.CreateUpstream(ctx, "bed", "bedrock", "https://bedrock.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	oai, err := s.CreateUpstream(ctx, "oai", "openai_compatible", "https://openai.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.CreateUpstream(ctx, "gone", "anthropic", "https://old.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDeleteUpstream(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-3 * time.Hour)
	older := now.Add(-27 * time.Hour)
	rows := map[string]usageRow{
		// The worked example: 50k fresh + 40k read.
		"anthropic":       {upstream: anth.ID, modality: "chat", at: old, in: 50_000, cached: 40_000, wantIn: 90_000, cost: 177_000_000},
		"anthropic-again": {upstream: anth.ID, modality: "chat", at: old, in: 1_000, cached: 0, w5m: 2_000, w1h: 3_000, wantIn: 6_000, cost: 31},
		"bedrock":         {upstream: bed.ID, modality: "chat", at: older, in: 2_000, cached: 90_000, w5m: 500, wantIn: 92_500, cost: 7},
		"deleted":         {upstream: gone.ID, modality: "chat", at: older, in: 10, cached: 5, wantIn: 15, cost: 3},
		"anthropic-plain": {upstream: anth.ID, modality: "chat", at: old, in: 700, wantIn: 700, cost: 5},
		"openai":          {upstream: oai.ID, modality: "chat", at: old, in: 90_000, cached: 40_000, wantIn: 90_000, cost: 11},
		"guard":           {upstream: anth.ID, modality: "classification", at: old, in: 300, cached: 20, wantIn: 300, cost: 1},
		"decision":        {upstream: bed.ID, modality: "decision", at: old, in: 300, cached: 20, wantIn: 300, cost: 1},
		"personal-plan":   {modality: "chat", at: old, in: 700, cached: 100, wantIn: 700},
		// After the watermark: corrected in the log, folded later.
		"anthropic-recent": {upstream: anth.ID, modality: "chat", at: now.Add(-time.Minute), in: 100, cached: 900, wantIn: 1_000, cost: 9},
	}
	seedTokenRows(t, s, rows)
	rollTokenRows(t, s, now)
	// Back to a database from before 0082: no flag, no rollup prompt sum,
	// fresh-only tokens_in on Anthropic and Bedrock rows (as seeded).
	if err := s.exec(ctx, `DELETE FROM schema_migration WHERE name = ?`, tokensInAsBilledMigration.name); err != nil {
		t.Fatal(err)
	}
	if err := s.RollbackMigration(ctx, cacheDisjointMigration.name); err != nil {
		t.Fatalf("rollback 0082: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rollTokenRows(t, s, now.Add(time.Hour)) // folds the recent row, already corrected
	checkTokenRows(t, s, rows)

	// A second run changes nothing (the flag is cleared on corrected rows).
	for _, stmt := range tokensInAsBilledStmts {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	checkTokenRows(t, s, rows)
}

// TestTokensInAsBilledMigrationAfter0082 is the upgrade of a database that
// ran 0082 and recorded the adapter's flag on new rows: flagged
// rows with cache tokens are corrected, flagged rows without any keep their
// tokens_in, and rows written later by a previous-release pod during the
// rollout (still flagged) are corrected by running the statements again.
func TestTokensInAsBilledMigrationAfter0082(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	rows := map[string]usageRow{
		"a1":      {upstream: "anth", modality: "chat", at: now.Add(-5 * time.Hour), in: 50_000, cached: 40_000, flagged: true, wantIn: 90_000, cost: 177_000_000},
		"a2":      {upstream: "anth", modality: "chat", at: now.Add(-5 * time.Hour), in: 1_000, w1h: 4_000, flagged: true, wantIn: 5_000, cost: 2},
		"a-plain": {upstream: "anth", modality: "chat", at: now.Add(-5 * time.Hour), in: 400, flagged: true, wantIn: 400, cost: 3},
		"oai":     {upstream: "oai", modality: "chat", at: now.Add(-5 * time.Hour), in: 9_000, cached: 4_000, wantIn: 9_000, cost: 4},
		"b1":      {upstream: "bed", modality: "chat", at: now.Add(-2 * time.Hour), in: 20, cached: 80, w5m: 100, flagged: true, wantIn: 200, cost: 5},
	}
	seedTokenRows(t, s, rows)
	flag := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if err := s.exec(ctx, `UPDATE usage_event SET cached_disjoint = 1 WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for id, r := range rows {
		if r.flagged {
			flag(id)
		}
	}
	rollTokenRows(t, s, now)
	if err := s.exec(ctx, `DELETE FROM schema_migration WHERE name = ?`, tokensInAsBilledMigration.name); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	checkTokenRows(t, s, rows)

	// A pod of the previous release writes a fresh-only, flagged row during
	// the rollout, and its roller folds it: the same statements, run again
	// by the release that drops the columns, correct both.
	late := map[string]usageRow{"late": {upstream: "anth", modality: "chat", at: now.Add(-2 * time.Minute), in: 10, cached: 990, flagged: true, wantIn: 1_000, cost: 6}}
	seedTokenRows(t, s, late)
	flag("late")
	rollTokenRows(t, s, now.Add(time.Hour))
	for _, stmt := range tokensInAsBilledStmts {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	rows["late"] = late["late"]
	checkTokenRows(t, s, rows)
}
