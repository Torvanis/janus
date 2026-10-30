package store

import (
	"context"
	"testing"
	"time"
)

// A routing-evaluation row (and a Security Gateway row) shares the reply's
// request id; Chat's footer and notes must never read it.
func TestChatUsageIgnoresInternalRowsWithTheSameRequestID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	reply := &UsageEvent{RequestID: "req-1", ClientApp: "janus-chat", ModelName: "router-served", RouteReason: "matched:coding",
		RequestedModelName: "auto", TokensIn: 10, TokensOut: 5, CostNano: 100, ChatMessageID: "msg-1", CreatedAt: now}
	judge := &UsageEvent{RequestID: "req-1", ClientApp: InternalRoutingClientApp, ModelName: "judge-model", TokensIn: 900, CostNano: 9000,
		ChatMessageID: "msg-1", CreatedAt: now.Add(time.Second)}
	guard := &UsageEvent{RequestID: "req-1", ClientApp: InternalClientApp, ModelName: "guard", TokensIn: 50, ChatMessageID: "msg-1", CreatedAt: now.Add(2 * time.Second)}
	for _, e := range []*UsageEvent{reply, judge, guard} {
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UsageEventByRequestID(ctx, "req-1")
	if err != nil || got.ModelName != "router-served" {
		t.Fatalf("footer row = %+v (%v), want the reply's own", got, err)
	}
	notes, err := s.ChatRequestNotes(ctx, []string{"req-1"})
	if err != nil || notes["req-1"].ServedModel != "router-served" || notes["req-1"].RouteReason != "matched:coding" {
		t.Fatalf("notes = %+v (%v), want the reply's route", notes["req-1"], err)
	}
	tot, err := s.ChatMessageUsage(ctx, "msg-1")
	if err != nil || tot.Events != 1 || tot.TokensIn != 10 || tot.CostNano != 100 {
		t.Fatalf("totals = %+v (%v)", tot, err)
	}
}

// Request-count quotas count a Chat reply once: its sub-calls carry tokens
// and cost but are not requests.
func TestRequestQuotaSkipsChatSubcalls(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i, sub := range []bool{false, true, true} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{UserID: "u1", TeamIDs: "t1", RequestID: "r", TokensIn: 10, ChatSubcall: sub,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.SumMetricSince(ctx, "user", "u1", "", MetricRequests, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("user requests = %d (%v), want 1", n, err)
	}
	if n, err := s.SumMetricSince(ctx, "user", "u1", "", MetricTokensIn, time.Now().Add(-time.Hour)); err != nil || n != 30 {
		t.Fatalf("user tokens = %d (%v), want 30", n, err)
	}
	if n, err := s.SumTeamMetricSince(ctx, "t1", "", MetricRequests, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("team requests = %d (%v), want 1", n, err)
	}
}
