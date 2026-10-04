package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The batched writer must produce exactly what per-event inserts produce:
// every usage row plus its reporting snapshot, across multiple INSERT chunks.
func TestInsertUsageEventsBatch(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	n := usageBatchRows*2 + 7
	events := make([]*UsageEvent, n)
	for i := range events {
		events[i] = &UsageEvent{CreatedAt: at.Add(time.Duration(i) * time.Millisecond), UserID: fmt.Sprintf("u%d", i%3),
			ModelName: "m", TokensIn: int64(i), TokensOut: 2, HTTPStatus: 200, GroupIDs: []string{"g"}, CostStatus: "priced"}
	}
	events[0].ID = "fixed-id"
	if err := s.InsertUsageEvents(ctx, events, true); err != nil {
		t.Fatal(err)
	}
	var rows, snaps, sumIn int64
	if err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(tokens_in),0) FROM usage_event`).Scan(&rows, &sumIn); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM reporting_usage_snapshot WHERE groups_known=1 AND cost_status='priced'`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if rows != int64(n) || snaps != int64(n) || sumIn != int64(n*(n-1)/2) {
		t.Fatalf("rows=%d snaps=%d sum=%d want %d/%d/%d", rows, snaps, sumIn, n, n, n*(n-1)/2)
	}
	if _, err := s.UsageEventByID(ctx, "fixed-id"); err != nil {
		t.Fatalf("explicit id lost: %v", err)
	}
	for _, e := range events {
		if e.ID == "" {
			t.Fatal("generated id not written back to the event")
		}
	}
}

// All-or-nothing: one bad row rolls the whole batch back, so the caller can
// retry row by row without duplicating the good ones.
func TestInsertUsageEventsBatchAtomic(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	if err := s.InsertUsageEvent(ctx, &UsageEvent{ID: "dup"}); err != nil {
		t.Fatal(err)
	}
	err := s.InsertUsageEvents(ctx, []*UsageEvent{{ID: "fresh"}, {ID: "dup"}}, false)
	if err == nil {
		t.Fatal("expected duplicate-key failure")
	}
	if _, err := s.UsageEventByID(ctx, "fresh"); err == nil {
		t.Fatal("batch partially committed")
	}
}

// Metering writes must not bump the model revision: that flushed every
// revision-tagged proxy cache (model resolution, rate cards) per request.
func TestUsageWritesKeepModelRevision(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	before := s.ModelRevision()
	if err := s.InsertUsageEvent(ctx, &UsageEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageEvents(ctx, []*UsageEvent{{}, {}}, true); err != nil {
		t.Fatal(err)
	}
	if after := s.ModelRevision(); after != before {
		t.Fatalf("model revision moved %d -> %d on usage writes", before, after)
	}
}
