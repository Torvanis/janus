package store

import (
	"context"
	"testing"
	"time"
)

func TestGatewayInstanceHeartbeatAndLiveness(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	// Nothing heartbeated yet still counts as one: this process exists.
	if n, err := s.LiveInstanceCount(ctx, t0); err != nil || n != 1 {
		t.Fatalf("empty table: n=%d err=%v, want 1", n, err)
	}

	// Two replicas heartbeat; each sees both.
	if n, err := s.HeartbeatInstance(ctx, "pod-a", t0); err != nil || n != 1 {
		t.Fatalf("first heartbeat: n=%d err=%v", n, err)
	}
	if n, err := s.HeartbeatInstance(ctx, "pod-b", t0); err != nil || n != 2 {
		t.Fatalf("second heartbeat: n=%d err=%v, want 2", n, err)
	}
	// Re-heartbeat is an update, not a new row.
	if n, err := s.HeartbeatInstance(ctx, "pod-a", t0.Add(15*time.Second)); err != nil || n != 2 {
		t.Fatalf("repeat heartbeat: n=%d err=%v, want 2", n, err)
	}

	// pod-b goes quiet. After InstanceStaleAfter it no longer counts —
	// that is what stops a killed pod from inflating everyone's divisor.
	later := t0.Add(15*time.Second + InstanceStaleAfter + time.Second)
	if n, err := s.HeartbeatInstance(ctx, "pod-a", later); err != nil || n != 1 {
		t.Fatalf("after pod-b stale: n=%d err=%v, want 1", n, err)
	}

	// Clean shutdown removes the row immediately.
	if err := s.RemoveInstance(ctx, "pod-a"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LiveInstanceCount(ctx, later); err != nil || n != 1 {
		t.Fatalf("after remove: n=%d err=%v, want floor of 1", n, err)
	}

	// Prune drops rows stale for a day, keeps recent ones.
	if _, err := s.HeartbeatInstance(ctx, "pod-c", later); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneInstances(ctx, later.Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 { // pod-b (stale) and pod-c (now also >24h old)
		t.Errorf("pruned %d rows, want 2", deleted)
	}
}
