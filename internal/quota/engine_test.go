package quota

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestTeamAffinityQuotaReadsAuthoritativeEvents(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "affinity", "affinity@example.com", "Affinity", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "A", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "B", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateTeamToken(ctx, user.ID, "key", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, event := range []*store.UsageEvent{
		{UserID: user.ID, TokenID: token.ID, TeamIDs: a.ID, ModelID: "m", TokensIn: 20, TokensOut: 30, CostNano: 40, CreatedAt: now},
		{UserID: user.ID, TeamIDs: a.ID, ModelID: "other", TokensIn: 200, TokensOut: 300, CostNano: 400, CreatedAt: now},
	} {
		if err := s.InsertUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	engines := []*Engine{NewEngine(s), NewEngine(s)}
	var quotas []*store.Quota
	for _, window := range []string{store.WindowDaily, store.WindowMonthly, "rolling_7d"} {
		for _, metric := range []string{store.MetricTokensIn, store.MetricTokensOut, store.MetricCostUSD, store.MetricRequests} {
			for _, team := range []string{a.ID, b.ID} {
				q := &store.Quota{SubjectType: "team", SubjectID: team, ModelID: "m", Metric: metric, Window: window, Limit: 1, BreachBehavior: store.BreachLetFinish}
				if err := s.CreateQuota(ctx, q); err != nil {
					t.Fatal(err)
				}
				quotas = append(quotas, q)
				start, _, err := WindowBounds(window, now)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetLedger(ctx, q.ID, start, 999); err != nil {
					t.Fatal(err)
				}
				for _, e := range engines {
					e.cache[q.ID] = &cacheEntry{value: 999, windowStart: start, loadedAt: now}
				}
			}
		}
	}
	assert := func(moved bool) {
		t.Helper()
		for _, e := range engines {
			for _, q := range quotas {
				var want int64
				if (q.SubjectID == a.ID) != moved {
					want = map[string]int64{store.MetricTokensIn: 20, store.MetricTokensOut: 30, store.MetricCostUSD: 40, store.MetricRequests: 1}[q.Metric]
				}
				status, err := e.StatusFor(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if status.Current != want {
					t.Fatalf("%s %s team=%s moved=%v current=%d want=%d", q.Window, q.Metric, q.SubjectID, moved, status.Current, want)
				}
			}
		}
	}
	assert(false)
	if _, n, err := s.ChangeTokenTeam(ctx, token.ID, user.ID, b.ID, true); err != nil || n != 1 {
		t.Fatalf("move=%d %v", n, err)
	}
	assert(true)
	// Record observes the already-inserted row and must not add it a second time.
	for _, e := range engines {
		if err := e.Record(ctx, UserSubject(user.ID, []string{b.ID}), "m", Delta{TokensIn: 20, TokensOut: 30, CostNano: 40, Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	assert(true)
	for _, e := range engines {
		if err := e.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	assert(true)
	for _, e := range engines {
		for _, team := range []string{a.ID, b.ID} {
			decision, err := e.Check(ctx, UserSubject(user.ID, []string{team}), "m")
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allowed != (team == a.ID) {
				t.Fatalf("team %s allowed=%v", team, decision.Allowed)
			}
		}
	}
}

func newEngineStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestEngineReconcileRebuildsLostLedgers verifies database-backed resilience:
// calendar-window counters must be
// fully re-derivable from the immutable usage-event log. It simulates losing
// BOTH the durable ledger table and the in-process cache, then asserts that
// Reconcile — and, independently, the lazy currentValue rebuild — restore the
// exact pre-loss values and that enforcement decisions are unchanged.
func TestEngineReconcileRebuildsLostLedgers(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()

	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-1", "casey@example.com", "Casey", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	q := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricTokensIn, Limit: 1000,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	e := NewEngine(s)
	now := time.Now().UTC()
	windowStart, _, err := WindowBounds(q.Window, now)
	if err != nil {
		t.Fatalf("window bounds: %v", err)
	}

	// Two metered requests: each lands in the immutable event log (source of
	// truth) and in the ledger fast path via Record.
	for i := 0; i < 2; i++ {
		if err := s.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now, UserID: user.ID, ModelName: "gpt-4o-mini", Modality: "chat",
			TokensIn: 200, TokensOut: 50, CostNano: 1000, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
		if err := e.Record(ctx, UserSubject(user.ID, nil), "", Delta{TokensIn: 200, TokensOut: 50, CostNano: 1000, Requests: 1}); err != nil {
			t.Fatalf("record usage: %v", err)
		}
	}

	before, err := e.StatusFor(ctx, q)
	if err != nil {
		t.Fatalf("status before loss: %v", err)
	}
	if before.Current != 400 {
		t.Fatalf("current before loss = %d, want 400", before.Current)
	}
	if v, err := s.LedgerValue(ctx, q.ID, windowStart); err != nil || v != 400 {
		t.Fatalf("ledger before loss = %d (err %v), want 400", v, err)
	}

	wipe := func() {
		t.Helper()
		if _, err := s.DB().ExecContext(ctx, "DELETE FROM quota_ledger"); err != nil {
			t.Fatalf("wipe ledger table: %v", err)
		}
		e.mu.Lock()
		e.cache = map[string]*cacheEntry{}
		e.mu.Unlock()
	}

	// --- Disaster 1: ledger + cache lost, explicit Reconcile runs (the
	// scheduled checkpoint / post-purge path). -------------------------------
	wipe()
	if err := e.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if v, err := s.LedgerValue(ctx, q.ID, windowStart); err != nil || v != 400 {
		t.Fatalf("ledger after reconcile = %d (err %v), want 400", v, err)
	}
	after, err := e.StatusFor(ctx, q)
	if err != nil {
		t.Fatalf("status after reconcile: %v", err)
	}
	if after.Current != before.Current || after.Limit != before.Limit || !after.ResetAt.Equal(before.ResetAt) {
		t.Errorf("status diverged across reconcile: before=%+v after=%+v", before, after)
	}
	if d, err := e.Check(ctx, UserSubject(user.ID, nil), ""); err != nil || !d.Allowed {
		t.Errorf("check after reconcile: allowed=%v err=%v, want allowed under a 1000 limit at 400", d.Allowed, err)
	}

	// --- Disaster 2: ledger + cache lost, NO reconcile — the lazy
	// currentValue path must rebuild from the event log and backfill the
	// ledger row on first read. ----------------------------------------------
	wipe()
	lazy, err := e.StatusFor(ctx, q)
	if err != nil {
		t.Fatalf("status via lazy rebuild: %v", err)
	}
	if lazy.Current != 400 {
		t.Errorf("lazy rebuilt current = %d, want 400", lazy.Current)
	}
	if v, err := s.LedgerValue(ctx, q.ID, windowStart); err != nil || v != 400 {
		t.Errorf("ledger not backfilled by lazy rebuild: %d (err %v), want 400", v, err)
	}

	// --- Enforcement correctness after rebuild: a tighter quota created over
	// the same history must refuse immediately, from log-derived values. ------
	tight := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricTokensIn, Limit: 300,
		Window: store.WindowDaily, BreachBehavior: store.BreachHardKill,
	}
	if err := s.CreateQuota(ctx, tight); err != nil {
		t.Fatalf("create tight quota: %v", err)
	}
	wipe()
	if err := e.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile with tight quota: %v", err)
	}
	d, err := e.Check(ctx, UserSubject(user.ID, nil), "")
	if err != nil {
		t.Fatalf("check tight quota: %v", err)
	}
	if d.Allowed || d.Breached == nil || d.Breached.Current != 400 || !d.HardKill {
		t.Errorf("tight quota after rebuild: %+v (breached=%+v), want refused hard-kill at 400/300", d, d.Breached)
	}
}

// TestEngineCustomAlertThresholds is the documented configurability story: a rule
// carrying admin-configured thresholds must alert at exactly those percentages
// (not the 80/95 defaults), still raise the non-configurable 100% breach, and
// rules without configuration must keep the 80/95 defaults.
func TestEngineCustomAlertThresholds(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()

	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-3", "thresholds@example.com", "Thresh", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	q := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricRequests, Limit: 100,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
		AlertThresholds: []int{90, 50}, // deliberately unsorted
	}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	// The configured thresholds must survive the storage round-trip, sorted.
	loaded, err := s.QuotaByID(ctx, q.ID)
	if err != nil {
		t.Fatalf("reload quota: %v", err)
	}
	if len(loaded.AlertThresholds) != 2 || loaded.AlertThresholds[0] != 50 || loaded.AlertThresholds[1] != 90 {
		t.Fatalf("alert thresholds round-trip = %v, want [50 90]", loaded.AlertThresholds)
	}

	e := NewEngine(s)
	var events []ThresholdEvent
	e.SetNotifier(func(_ context.Context, ev ThresholdEvent) { events = append(events, ev) })

	record := func(n int64) {
		t.Helper()
		if err := e.Record(ctx, UserSubject(user.ID, nil), "", Delta{Requests: n}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	record(50) // 50% — custom threshold fires
	if len(events) != 1 || events[0].Threshold != 50 {
		t.Fatalf("after 50%%: events = %+v, want one event at threshold 50", events)
	}
	record(30) // 80% — the DEFAULT tier must NOT fire on a customised rule
	if len(events) != 1 {
		t.Fatalf("after 80%%: events = %+v, want no new event (80 is not configured)", events)
	}
	record(10) // 90% — second custom threshold fires
	if len(events) != 2 || events[1].Threshold != 90 {
		t.Fatalf("after 90%%: events = %+v, want second event at threshold 90", events)
	}
	record(10) // 100% — breach fires even though 100 is not in the custom list
	if len(events) != 3 || events[2].Threshold != 100 {
		t.Fatalf("after 100%%: events = %+v, want breach event at threshold 100", events)
	}

	// A rule without configuration keeps the 80/95 defaults, and only the
	// highest crossed threshold alerts per window.
	other, _, err := s.UpsertUserFromIdentity(ctx, "sub-4", "defaults@example.com", "Default", false, false)
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	dq := &store.Quota{
		SubjectType: "user", SubjectID: other.ID,
		Metric: store.MetricRequests, Limit: 100,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}
	if err := s.CreateQuota(ctx, dq); err != nil {
		t.Fatalf("create default quota: %v", err)
	}
	events = nil
	if err := e.Record(ctx, UserSubject(other.ID, nil), "", Delta{Requests: 96}); err != nil {
		t.Fatalf("record default: %v", err)
	}
	if len(events) != 1 || events[0].Threshold != 95 {
		t.Fatalf("default rule at 96%%: events = %+v, want one event at threshold 95", events)
	}
}

// TestEngineRollingWindowAlertsDeduplicated is the documented regression for
// rolling windows: their windowStart is unique on every request (now - width),
// so a naive dedup key would re-alert on every metered call while usage sits
// above a threshold. The dedup key must be quantized so a threshold alerts at
// most once per window width, and repeated requests must not grow
// quota_alert_state unboundedly.
func TestEngineRollingWindowAlertsDeduplicated(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()

	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-5", "rolling-alerts@example.com", "RollAlert", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	q := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricRequests, Limit: 10,
		Window: store.WindowRolling24, BreachBehavior: store.BreachLetFinish,
	}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	e := NewEngine(s)
	var events []ThresholdEvent
	e.SetNotifier(func(_ context.Context, ev ThresholdEvent) { events = append(events, ev) })

	// Push usage to 80% of the rolling limit (8/10 requests), then keep
	// metering. Rolling windows derive from the event log, so every Record
	// needs a matching event.
	meter := func() {
		t.Helper()
		if err := s.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: time.Now().UTC(), UserID: user.ID, ModelName: "gpt-4o-mini", Modality: "chat",
			TokensIn: 1, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
		if err := e.Record(ctx, UserSubject(user.ID, nil), "", Delta{Requests: 1}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	for i := 0; i < 8; i++ {
		meter()
	}
	if len(events) != 1 || events[0].Threshold != 80 {
		t.Fatalf("after reaching 80%%: events = %+v, want exactly one event at threshold 80", events)
	}

	// More requests while still >= 80%: the 80 alert must NOT repeat.
	meter() // 9/10 — still in the 80 tier
	if len(events) != 1 {
		t.Fatalf("repeated requests above 80%% re-alerted: events = %+v", events)
	}
	meter() // 10/10 — crosses breach; the higher tier fires exactly once
	if len(events) != 2 || events[1].Threshold != 100 {
		t.Fatalf("breach on rolling window: events = %+v, want a single new event at 100", events)
	}
	meter() // still breached — no further alert
	if len(events) != 2 {
		t.Fatalf("repeated requests above 100%% re-alerted: events = %+v", events)
	}

	// The dedup table must hold one row per fired threshold, not per request.
	var stateRows int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM quota_alert_state WHERE quota_id = ?", q.ID).Scan(&stateRows); err != nil {
		t.Fatalf("count alert state rows: %v", err)
	}
	if stateRows != 2 {
		t.Errorf("quota_alert_state rows = %d, want 2 (one per fired threshold)", stateRows)
	}

	// And the retention purge must clear rows once they age out.
	if _, err := s.DB().ExecContext(ctx, "UPDATE quota_alert_state SET notified_at = ?", store.FormatTime(time.Now().UTC().AddDate(0, 0, -60))); err != nil {
		t.Fatalf("age alert state rows: %v", err)
	}
	n, err := s.PurgeQuotaAlertState(ctx, time.Now().UTC().AddDate(0, 0, -45))
	if err != nil {
		t.Fatalf("purge quota alert state: %v", err)
	}
	if n != 2 {
		t.Errorf("purged %d alert-state rows, want 2", n)
	}
}

// TestEngineReconcileSkipsRollingWindows documents that rolling windows have no
// ledger to rebuild — they are always computed straight from the event log.
func TestEngineReconcileSkipsRollingWindows(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()

	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-2", "rolling@example.com", "Rolling", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	q := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricRequests, Limit: 10,
		Window: store.WindowRolling24, BreachBehavior: store.BreachLetFinish,
	}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota: %v", err)
	}
	if err := s.InsertUsageEvent(ctx, &store.UsageEvent{
		CreatedAt: time.Now().UTC(), UserID: user.ID, ModelName: "gpt-4o-mini", Modality: "chat",
		TokensIn: 10, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	e := NewEngine(s)
	if err := e.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	windowStart, _, err := WindowBounds(q.Window, time.Now().UTC())
	if err != nil {
		t.Fatalf("window bounds: %v", err)
	}
	// No ledger row is created for a rolling window…
	if v, err := s.LedgerValue(ctx, q.ID, windowStart); err != nil || v != 0 {
		t.Errorf("rolling window grew a ledger row: %d (err %v), want 0", v, err)
	}
	// …yet its status still reflects the event log.
	st, err := e.StatusFor(ctx, q)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Current != 1 {
		t.Errorf("rolling current = %d, want 1 request", st.Current)
	}
}
