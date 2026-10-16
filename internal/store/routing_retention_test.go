package store

import (
	"context"
	"testing"
	"time"
)

// Auto-routing retention: expired sticky conversations go, decisions and
// verdicts past "Keep decisions" go, and a decision kept longer than the
// usage log loses the caller's ids. Runs on SQLite, and on PostgreSQL when
// JANUS_ROLLUP_TEST_DATABASE_URL is set.
func TestPruneRoutingRetention(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := 24 * time.Hour

	decision := func(age time.Duration, user, svc string) string {
		t.Helper()
		d := &RoutingDecision{RequestID: NewID(), RouterID: "r1", Mode: RouterModeLive, Reason: RouteReasonMatched,
			UserID: user, ServiceTokenID: svc, CreatedAt: now.Add(-age)}
		if err := s.InsertRoutingDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d.RequestID
	}
	oldest := decision(100*day, "u-old", "")       // past the 90-day history window
	middle := decision(40*day, "u-mid", "")        // kept, but past the 30-day usage window
	middleSvc := decision(35*day, "", "svc-mid")   // same, service token
	fresh := decision(2*day, "u-new", "svc-fresh") // kept with its ids
	for _, age := range []time.Duration{100 * day, 2 * day} {
		if err := s.InsertRoutingEval(ctx, &RoutingEvalResult{RouterID: "r1", RouteID: "legal", Status: "ok", CreatedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	for key, exp := range map[string]time.Time{"gone": now.Add(-time.Minute), "live": now.Add(time.Hour)} {
		if err := s.PutStickyRoute(ctx, StickyRoute{Key: key, RouterID: "r1", RouteID: "legal", ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.PruneRouting(ctx, now, now.AddDate(0, 0, -90), now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if n.Sticky != 1 || n.Decisions != 1 || n.Evals != 1 || n.Anonymised != 2 {
		t.Fatalf("pruned %+v, want 1 sticky, 1 decision, 1 evaluation, 2 anonymised", n)
	}

	byRequest := map[string]RoutingDecision{}
	ds, err := s.ListRoutingDecisions(ctx, RoutingDecisionFilter{RouterID: "r1", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		byRequest[d.RequestID] = d
	}
	if _, ok := byRequest[oldest]; ok {
		t.Fatal("a decision older than Keep decisions was kept")
	}
	for _, id := range []string{middle, middleSvc} {
		d, ok := byRequest[id]
		if !ok {
			t.Fatal("a decision inside Keep decisions was deleted")
		}
		if d.UserID != "" || d.ServiceTokenID != "" {
			t.Fatalf("a decision older than the usage log still names its caller: user=%q svc=%q", d.UserID, d.ServiceTokenID)
		}
	}
	if d := byRequest[fresh]; d.UserID != "u-new" || d.ServiceTokenID != "svc-fresh" {
		t.Fatalf("a recent decision lost its caller: %+v", d)
	}
	if evals, _ := s.ListRoutingEvals(ctx, "r1", "legal", 10); len(evals) != 1 {
		t.Fatalf("%d evaluations remain, want the recent one", len(evals))
	}
	if _, err := s.LookupStickyRoute(ctx, "live", now); err != nil {
		t.Fatalf("a live sticky conversation was removed: %v", err)
	}
	var sticky int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM routing_sticky`).Scan(&sticky); err != nil || sticky != 1 {
		t.Fatalf("routing_sticky rows = %d (%v), want only the live one", sticky, err)
	}

	// Keep decisions = 0 and no usage window: only expired sticky rows go.
	if _, err := s.PruneRouting(ctx, now.Add(2*time.Hour), time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.ListRoutingDecisions(ctx, RoutingDecisionFilter{RouterID: "r1", Limit: 500}); len(ds) != 3 {
		t.Fatalf("zero retention must keep decisions, %d left", len(ds))
	}
}

// A quota on an alias that a router route served through counts that
// traffic when the ledger is rebuilt from the event log.
func TestSumMetricSinceCountsTrafficServedThroughAnAlias(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, served := range []string{"alias-1", "alias-1", ""} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{UserID: "u1", ModelID: "m1", ModelName: "m", ManagedModelID: "router-1",
			ServedManagedModelID: served, EndpointPath: "/v1/chat/completions", HTTPMethod: "POST", HTTPStatus: 200,
			RequestID: NewID(), CreatedAt: now.Add(-time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SumMetricSince(ctx, "user", "u1", "alias-1", MetricRequests, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("alias quota counts %d requests, want the 2 served through it", n)
	}
	if n, _ := s.SumMetricSince(ctx, "user", "u1", "router-1", MetricRequests, now.Add(-time.Hour)); n != 3 {
		t.Fatalf("router quota counts %d, want 3", n)
	}
}
