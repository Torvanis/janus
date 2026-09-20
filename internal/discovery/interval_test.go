package discovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// The scheduler's effective interval is the stored override when one exists
// and the environment default otherwise; Reschedule never blocks even when no
// loop is draining it.
func TestDiscoveryEffectiveIntervalFollowsOverride(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if got := h.svc.DefaultInterval(); got != time.Hour {
		t.Fatalf("default interval = %v, want 1h", got)
	}
	if got := h.svc.EffectiveInterval(ctx); got != time.Hour {
		t.Fatalf("effective interval without override = %v, want 1h", got)
	}
	if err := h.store.SetDiscoveryIntervalOverride(ctx, store.DiscoveryIntervalOverride{Minutes: 2}); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if got := h.svc.EffectiveInterval(ctx); got != 2*time.Minute {
		t.Fatalf("effective interval with override = %v, want 2m", got)
	}
	if err := h.store.ClearDiscoveryIntervalOverride(ctx); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if got := h.svc.EffectiveInterval(ctx); got != time.Hour {
		t.Fatalf("effective interval after clear = %v, want 1h", got)
	}

	done := make(chan struct{})
	go func() {
		h.svc.Reschedule()
		h.svc.Reschedule()
		h.svc.Reschedule()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Reschedule must never block")
	}

	if err := (store.DiscoveryIntervalOverride{Minutes: store.MaxDiscoveryIntervalMinutes + 1}).Validate(); err == nil {
		t.Fatal("interval past the ceiling must be rejected")
	}
	if err := (store.DiscoveryIntervalOverride{Minutes: -1}).Validate(); err == nil {
		t.Fatal("negative interval must be rejected")
	}
}
