package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression tests for the auto-routing store fixes of the release review
// (area D).

// 0049's down drops what its up added, so rollback then migrate works.
func TestRoutingMigrationRollbackIsReapplicable(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	if err := s.RollbackMigration(ctx, "0050_decision_models"); err != nil {
		t.Fatalf("rollback 0050: %v", err)
	}
	if err := s.RollbackMigration(ctx, "0049_auto_routing"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-apply after rollback must work: %v", err)
	}
}

// Rationale and error are cut on character boundaries (Postgres refuses
// invalid UTF-8, and the lost row would also lose its spend).
func TestRoutingEvalTextIsTruncatedOnRuneBoundaries(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	r := "a" + strings.Repeat("é", 400)
	e := &RoutingEvalResult{RouterID: "r", RouteID: "x", Rationale: r, Error: r, Status: "ok", EvalCostNano: 7}
	if err := s.InsertRoutingEval(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !utf8.ValidString(e.Rationale) || !utf8.ValidString(e.Error) || len(e.Rationale) > 600 {
		t.Fatalf("stored text is not valid UTF-8 (len %d)", len(e.Rationale))
	}
	n, spent, err := s.RouteEvalToday(ctx, "r", "x", time.Now().UTC())
	if err != nil || n != 1 || spent != 7 {
		t.Fatalf("the row and its spend must be counted: n=%d spent=%d err=%v", n, spent, err)
	}
	d := &RoutingDecision{RouterID: "r", Error: r}
	if err := s.InsertRoutingDecision(ctx, d); err != nil || !utf8.ValidString(d.Error) {
		t.Fatalf("decision error must be stored as valid UTF-8: %v", err)
	}
}

// Reservations are atomic: concurrent jobs cannot all pass the daily count
// or budget, and an in-flight reservation counts at its estimate.
func TestReserveRouteEvalIsAtomic(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ReserveRouteEval(ctx, &RoutingEvalResult{RouterID: "r", RouteID: "x"}, 3, 1_000_000, 10, now)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 3 {
		t.Fatalf("max_per_day=3 but %d reservations were admitted", admitted)
	}
	// Budget: an estimate that would exceed the budget is refused even
	// though nothing has been spent yet.
	if ok, _ := s.ReserveRouteEval(ctx, &RoutingEvalResult{RouterID: "r", RouteID: "budget"}, 100, 100, 101, now); ok {
		t.Fatal("an estimate above the budget must not be reserved")
	}
	first := &RoutingEvalResult{RouterID: "r", RouteID: "budget"}
	if ok, _ := s.ReserveRouteEval(ctx, first, 100, 100, 60, now); !ok {
		t.Fatal("first reservation within budget refused")
	}
	if ok, _ := s.ReserveRouteEval(ctx, &RoutingEvalResult{RouterID: "r", RouteID: "budget"}, 100, 100, 60, now); ok {
		t.Fatal("an in-flight reservation must count against the budget at its estimate")
	}
	// Finishing replaces the estimate with the actual cost.
	first.Status, first.EvalCostNano = "ok", 20
	if err := s.FinishRoutingEval(ctx, first); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ReserveRouteEval(ctx, &RoutingEvalResult{RouterID: "r", RouteID: "budget"}, 100, 100, 60, now); !ok {
		t.Fatal("after finishing at a lower cost the budget must have room again")
	}
	// Running reservations are not listed as results.
	evs, err := s.ListRoutingEvals(ctx, "r", "budget", 10)
	if err != nil || len(evs) != 1 || evs[0].Status != "ok" {
		t.Fatalf("only finished evaluations are listed: %+v %v", evs, err)
	}
}

// Tallies are per pairing (routed model, default, judge), most recent first.
func TestRoutingEvalSummariesSplitByPairing(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	add := func(routed, verdict, status string, at time.Time) {
		if err := s.InsertRoutingEval(ctx, &RoutingEvalResult{RouterID: "r", RouteID: "x", RoutedModelID: routed, DefaultModelID: "def",
			JudgeModelID: "j", Verdict: verdict, Status: status, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		add("old", EvalVerdictWorse, "ok", now.Add(-time.Duration(48+i)*time.Hour))
	}
	add("new", EvalVerdictBetter, "ok", now.Add(-time.Hour))
	add("new", "", EvalStatusSkipped, now.Add(-time.Minute))
	add("new", "", EvalStatusRunning, now)
	sums, err := s.RoutingEvalSummaries(ctx, now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("want two pairings, got %+v", sums)
	}
	if sums[0].RoutedModelID != "new" || sums[0].Evaluated != 1 || sums[0].Worse != 0 || sums[0].Skipped != 1 {
		t.Fatalf("most recent pairing first, without the old verdicts: %+v", sums[0])
	}
	if sums[1].RoutedModelID != "old" || sums[1].Verdict != EvalRouteLosing {
		t.Fatalf("old pairing stays its own history: %+v", sums[1])
	}
}

// The verdict thresholds as documented.
func TestEvalVerdictThresholds(t *testing.T) {
	lo, hi := wilson(5, 10)
	if EvalVerdictFor(10, lo, hi) != EvalRouteNeedsData {
		t.Fatalf("10 ties: %v %v", lo, hi)
	}
	lo, hi = wilson(0, 10)
	if EvalVerdictFor(10, lo, hi) != EvalRouteLosing {
		t.Fatalf("all worse: %v %v", lo, hi)
	}
}
