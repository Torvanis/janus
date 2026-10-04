package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newRollupStore is newTestStore, or a fresh PostgreSQL schema when
// JANUS_ROLLUP_TEST_DATABASE_URL points at a loopback disposable database:
// the rollup SQL (UNION ALL typing, numeric SUMs, advisory locks, ON
// CONFLICT) must hold on both dialects.
func newRollupStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("JANUS_ROLLUP_TEST_DATABASE_URL")
	if dsn == "" {
		return newTestStore(t)
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("JANUS_ROLLUP_TEST_DATABASE_URL must be a loopback disposable database")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "rollup_" + strings.ReplaceAll(strings.ToLower(NewID()), "-", "")[:16]
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRollupEvents writes n events spread over the last `span` with varied
// dimensions, deterministic per seed.
func seedRollupEvents(t *testing.T, s *Store, now time.Time, span time.Duration, n int, seed int64) {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))
	users := []string{"u1", "u2", "u3"}
	svcs := []string{"", "", "svc1", "svc2"}
	toks := []string{"", "tokA", "tokB", "tokC"}
	teams := []string{"", "team1", "team2", "team1,team2"}
	models := []string{"m-one", "m-two", "ghost-model"}
	aliases := []string{"", "", "alias-x", "alias-y"}
	statuses := []int{200, 200, 200, 429, 500}
	events := make([]*UsageEvent, 0, n)
	for i := 0; i < n; i++ {
		e := &UsageEvent{
			CreatedAt:          now.Add(-time.Duration(rng.Int63n(int64(span)))),
			ModelName:          models[rng.Intn(len(models))],
			RequestedModelName: aliases[rng.Intn(len(aliases))],
			HTTPStatus:         statuses[rng.Intn(len(statuses))],
			TokensIn:           rng.Int63n(1000),
			TokensOut:          rng.Int63n(500),
			TokensCached:       rng.Int63n(50),
			TokensCacheWrite5m: rng.Int63n(20),
			TokensCacheWrite1h: rng.Int63n(10),
			CostNano:           rng.Int63n(1_000_000),
			TeamIDs:            teams[rng.Intn(len(teams))],
		}
		if svc := svcs[rng.Intn(len(svcs))]; svc != "" {
			e.ServiceTokenID = svc
		} else {
			e.UserID = users[rng.Intn(len(users))]
			e.TokenID = toks[rng.Intn(len(toks))]
		}
		events = append(events, e)
	}
	if err := s.InsertUsageEvents(ctx, events, false); err != nil {
		t.Fatal(err)
	}
}

// rawView answers like s but ignores the rollup (no watermark), which is
// exactly how every read worked before rollups existed.
func rawView(t *testing.T, s *Store, fn func()) {
	t.Helper()
	ctx := context.Background()
	var wm string
	if err := s.queryRow(ctx, `SELECT value FROM usage_rollup_state WHERE key='watermark'`).Scan(&wm); err != nil {
		t.Fatalf("rollup has no watermark: %v", err)
	}
	if err := s.exec(ctx, `DELETE FROM usage_rollup_state WHERE key='watermark'`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.exec(ctx, `INSERT INTO usage_rollup_state (key, value) VALUES ('watermark', ?)`, wm); err != nil {
			t.Fatal(err)
		}
	}()
	fn()
}

type rollupReads struct {
	Agg      map[string]Totals
	ByTok    map[string]Totals
	BySvc    map[string]Totals
	ByAlias  map[string]Totals
	ByTeam   map[string]Totals
	Princ    map[string]int64
	Models   map[string][]Breakdown
	Users    []Breakdown
	Status   []Breakdown
	Modality []Breakdown
	Series   map[string][]TimePoint
	Distinct int64
}

func readAll(t *testing.T, s *Store, now time.Time) rollupReads {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	out := rollupReads{Agg: map[string]Totals{}, Series: map[string][]TimePoint{}}
	scopes := map[string]UsageScope{
		"all":          {},
		"30d":          {Start: now.AddDate(0, 0, -30), End: now},
		"ragged":       {Start: now.Add(-50*time.Hour - 17*time.Minute), End: now.Add(-3*time.Hour - 41*time.Minute)},
		"sub-hour":     {Start: now.Add(-37 * time.Minute), End: now.Add(-12 * time.Minute)},
		"open-end":     {Start: now.Add(-20*time.Hour - 5*time.Minute)},
		"user":         {UserID: "u1", Start: now.AddDate(0, 0, -30), End: now},
		"token":        {UserID: "u2", TokenID: "tokB", Start: now.AddDate(0, 0, -30), End: now},
		"svc":          {ServiceTokenID: "svc1", Start: now.AddDate(0, 0, -7), End: now},
		"alias":        {RequestedModelName: "alias-x", Start: now.AddDate(0, 0, -30), End: now},
		"team":         {TeamIDs: []string{"team2"}, Start: now.AddDate(0, 0, -30), End: now},
		"people":       {Principal: PrincipalUsers, Start: now.AddDate(0, 0, -30), End: now},
		"integrations": {Principal: PrincipalServiceTokens, Start: now.AddDate(0, 0, -30), End: now},
		"users-in":     {UserIDs: []string{"u1", "u3"}, Start: now.AddDate(0, 0, -30), End: now},
	}
	for name, sc := range scopes {
		tot, err := s.AggregateUsage(ctx, sc)
		must(err)
		out.Agg[name] = tot
	}
	win := UsageScope{Start: now.AddDate(0, 0, -30), End: now}
	var err error
	out.ByTok, err = s.AggregateUsageBy(ctx, UsageScope{UserID: "u2", Start: win.Start, End: win.End}, "token")
	must(err)
	out.BySvc, err = s.AggregateUsageBy(ctx, UsageScope{Principal: PrincipalServiceTokens, Start: win.Start, End: win.End}, "service_token")
	must(err)
	out.ByAlias, err = s.AggregateUsageBy(ctx, win, "requested_model")
	must(err)
	out.ByTeam, err = s.AggregateUsageBy(ctx, win, "team_snapshot")
	must(err)
	out.Princ, err = s.CountDistinctPrincipalsBy(ctx, win, "requested_model")
	must(err)
	out.Models, err = s.BreakdownUsageBy(ctx, win, "service_token", "model")
	must(err)
	out.Users, err = s.BreakdownUsage(ctx, win, "user")
	must(err)
	out.Status, err = s.BreakdownUsage(ctx, win, "status", BreakdownMetricCost)
	must(err)
	out.Modality, err = s.BreakdownUsage(ctx, win, "modality")
	must(err)
	out.Distinct, err = s.CountDistinctPrincipals(ctx, UsageScope{RequestedModelName: "alias-y", Start: win.Start, End: win.End})
	must(err)
	for _, shape := range []struct {
		name    string
		bucket  time.Duration
		buckets int
	}{{"day", 30 * time.Minute, 48}, {"week", 4 * time.Hour, 42}, {"month", 24 * time.Hour, 30}} {
		series, err := s.UsageSeries(ctx, UsageScope{Start: now.Add(-shape.bucket * time.Duration(shape.buckets)), End: now}, shape.bucket, shape.buckets)
		must(err)
		out.Series[shape.name] = series
	}
	return out
}

func rollAll(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		done, err := s.RollupUsage(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
	t.Fatal("rollup did not finish")
}

func assertSameReads(t *testing.T, s *Store, now time.Time, when string) {
	t.Helper()
	rolled := readAll(t, s, now)
	var raw rollupReads
	rawView(t, s, func() { raw = readAll(t, s, now) })
	if !reflect.DeepEqual(rolled, raw) {
		rv, wv := reflect.ValueOf(rolled), reflect.ValueOf(raw)
		for i := 0; i < rv.NumField(); i++ {
			if !reflect.DeepEqual(rv.Field(i).Interface(), wv.Field(i).Interface()) {
				t.Errorf("%s: %s differs\n rollup: %+v\n raw:    %+v", when, rv.Type().Field(i).Name, rv.Field(i).Interface(), wv.Field(i).Interface())
			}
		}
	}
}

// Every rolled-up read must equal the raw-log answer exactly, for windows
// that start and end mid-hour, sub-hour windows, open-ended windows, every
// scope filter, grouped reads and series. Run with the watermark both mid
// way through an hour and right at "now - grace".
func TestUsageRollupMatchesRawLog(t *testing.T) {
	s := newRollupStore(t)
	seedCatalogModels(t, s, "m-one", "m-two")
	now := time.Now().UTC()
	seedRollupEvents(t, s, now, 40*24*time.Hour, 500, 1)
	// Dense recent traffic so the raw tail and ragged hours are non-trivial.
	seedRollupEvents(t, s, now, 3*time.Hour, 150, 2)

	rollAll(t, s, now)
	wm, ok, err := s.rollupWatermark(context.Background())
	if err != nil || !ok {
		t.Fatalf("watermark: %v %v", wm, err)
	}
	if want := now.Add(-rollupGrace); !wm.Equal(want) {
		t.Fatalf("watermark = %s, want now-grace %s", wm, want)
	}
	var rows int
	if err := s.queryRow(context.Background(), `SELECT COUNT(*) FROM usage_rollup`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("rollup rows = %d (%v)", rows, err)
	}
	assertSameReads(t, s, now, "after rollup")

	// Prove reads come from the rollup, not a silent raw fallback: inflate
	// one settled hour's rollup rows and the 30-day total must move.
	ctx := context.Background()
	base, _ := s.AggregateUsage(ctx, UsageScope{Start: now.AddDate(0, 0, -30), End: now})
	// Pick a settled hour that has rows (inside the 30-day window).
	var hour string
	if err := s.queryRow(ctx, `SELECT MAX(bucket) FROM usage_rollup WHERE bucket < ? AND bucket > ?`,
		FormatTime(floorHour(now.Add(-2*time.Hour))), FormatTime(now.AddDate(0, 0, -29))).Scan(&hour); err != nil || hour == "" {
		t.Fatalf("no settled rollup hour: %v", err)
	}
	if err := s.exec(ctx, `UPDATE usage_rollup SET request_count = request_count + 1000 WHERE bucket = ?`, hour); err != nil {
		t.Fatal(err)
	}
	if bumped, _ := s.AggregateUsage(ctx, UsageScope{Start: now.AddDate(0, 0, -30), End: now}); bumped.Requests <= base.Requests {
		t.Fatalf("30-day total ignored the rollup (%d -> %d)", base.Requests, bumped.Requests)
	}
	if err := s.exec(ctx, `UPDATE usage_rollup SET request_count = request_count - 1000 WHERE bucket = ?`, hour); err != nil {
		t.Fatal(err)
	}

	// New traffic after the watermark is read from the raw tail.
	seedRollupEvents(t, s, now, rollupGrace, 50, 3)
	assertSameReads(t, s, now, "with unrolled tail")

	// The roller catches up later; still identical.
	later := now.Add(17 * time.Minute)
	rollAll(t, s, later)
	assertSameReads(t, s, now, "after a later roll")
}

// Re-attributing a token's history (Change team with move history) rewrites
// raw rows; the rollup must follow in the same transaction.
func TestUsageRollupFollowsTokenHistoryMove(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "owner", "owner@example.com", "Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "Rollup team", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreateToken(ctx, user.ID, "moving token")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []*UsageEvent{}
	for i := 0; i < 40; i++ {
		events = append(events, &UsageEvent{UserID: user.ID, TokenID: tok.ID, CreatedAt: now.Add(-time.Duration(i) * 3 * time.Hour), TokensOut: 7, HTTPStatus: 200})
	}
	if err := s.InsertUsageEvents(ctx, events, false); err != nil {
		t.Fatal(err)
	}
	rollAll(t, s, now)
	if _, moved, err := s.ChangeTokenTeam(ctx, tok.ID, user.ID, team.ID, true); err != nil || moved != 40 {
		t.Fatalf("change team: moved=%d err=%v", moved, err)
	}
	tot, err := s.AggregateUsage(ctx, UsageScope{TeamIDs: []string{team.ID}, Start: now.AddDate(0, 0, -30), End: now})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 40 || tot.TokensOut != 280 {
		t.Fatalf("team totals after move = %+v, want 40 requests / 280 tokens", tot)
	}
	assertSameReads(t, s, now, "after history move")
}

// Purging old usage removes it from the rollup too, including the partial
// hour the cutoff falls in.
func TestUsageRollupFollowsPurge(t *testing.T) {
	s := newRollupStore(t)
	seedCatalogModels(t, s, "m-one", "m-two")
	now := time.Now().UTC()
	seedRollupEvents(t, s, now, 40*24*time.Hour, 300, 4)
	rollAll(t, s, now)
	cutoff := now.AddDate(0, 0, -10).Add(-23 * time.Minute)
	if _, err := s.PurgeUsageEvents(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	all, err := s.AggregateUsage(context.Background(), UsageScope{})
	if err != nil {
		t.Fatal(err)
	}
	var raw int64
	if err := s.queryRow(context.Background(), `SELECT COUNT(*) FROM usage_event`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if all.Requests != raw {
		t.Fatalf("all-time requests after purge = %d, raw rows = %d", all.Requests, raw)
	}
	assertSameReads(t, s, now, "after purge")
}

// An event committed behind the watermark (a write retried after an outage
// longer than the grace) is caught by reconcile.
func TestUsageRollupReconcileCatchesLateWrites(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedRollupEvents(t, s, now, 6*time.Hour, 100, 5)
	rollAll(t, s, now)
	late := &UsageEvent{UserID: "late", CreatedAt: now.Add(-2 * time.Hour), TokensOut: 9, HTTPStatus: 200}
	if err := s.InsertUsageEvent(ctx, late); err != nil {
		t.Fatal(err)
	}
	before, _ := s.AggregateUsage(ctx, UsageScope{UserID: "late"})
	if before.Requests != 0 {
		t.Fatalf("late event visible before reconcile (%d); the test no longer exercises the gap", before.Requests)
	}
	n, err := s.ReconcileUsageRollup(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reconcile rebuilt %d hours (err %v), want 1", n, err)
	}
	after, _ := s.AggregateUsage(ctx, UsageScope{UserID: "late"})
	if after.Requests != 1 || after.TokensOut != 9 {
		t.Fatalf("after reconcile: %+v", after)
	}
	if n, _ := s.ReconcileUsageRollup(ctx); n != 0 {
		t.Fatalf("second reconcile rebuilt %d hours, want 0", n)
	}
	assertSameReads(t, s, now, "after reconcile")
}

// Before the roller has ever run there is no watermark and reads use the raw
// log; an empty log still gets a watermark so later reads use the rollup.
func TestUsageRollupFreshInstall(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	if _, ok, _ := s.rollupWatermark(ctx); ok {
		t.Fatal("fresh store has a watermark")
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{UserID: "x", TokensOut: 3}); err != nil {
		t.Fatal(err)
	}
	if tot, _ := s.AggregateUsage(ctx, UsageScope{}); tot.Requests != 1 {
		t.Fatalf("raw read before first roll = %+v", tot)
	}
	empty := newRollupStore(t)
	done, err := empty.RollupUsage(ctx, time.Now())
	if err != nil || !done {
		t.Fatalf("empty roll: done=%v err=%v", done, err)
	}
	if _, ok, _ := empty.rollupWatermark(ctx); !ok {
		t.Fatal("empty log did not get a watermark")
	}
}

// The first roll of a long history proceeds in bounded chunks.
func TestUsageRollupBackfillsInChunks(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedRollupEvents(t, s, now, 3*24*time.Hour, 200, 6)
	steps := 0
	for {
		done, err := s.RollupUsage(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		steps++
		if done {
			break
		}
	}
	if want := int(3*24*time.Hour/rollupChunk) - 1; steps < want {
		t.Fatalf("backfill took %d steps, want at least %d chunked steps", steps, want)
	}
	_ = fmt.Sprint(steps)
}
