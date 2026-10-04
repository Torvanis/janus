package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

func TestDecidePerformanceModeGatesOnBusiness(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name      string
		requested bool
		st        license.State
		active    bool
		reason    bool
	}{
		{"not requested", false, license.State{Edition: license.EditionBusiness, Status: license.StatusValid}, false, false},
		{"community", true, license.State{Edition: license.EditionCommunity, Status: license.StatusValid}, false, true},
		{"business", true, license.State{Edition: license.EditionBusiness, Status: license.StatusValid, ExpiresAt: &future}, true, false},
		{"enterprise", true, license.State{Edition: license.EditionEnterprise, Status: license.StatusValid}, true, false},
		{"business in grace", true, license.State{Edition: license.EditionBusiness, Status: license.StatusGrace}, true, false},
		{"business expired", true, license.State{Edition: license.EditionBusiness, Status: license.StatusExpired}, false, true},
		{"invalid key", true, license.State{Edition: license.EditionBusiness, Status: license.StatusInvalid}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := DecidePerformanceMode(tc.requested, tc.st)
			if m.Requested != tc.requested || m.Active != tc.active || (m.Reason != "") != tc.reason {
				t.Fatalf("got %+v", m)
			}
		})
	}
}

func chatOK(t *testing.T, h *harness) {
	t.Helper()
	rec := h.do("POST", "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat completion = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// Performance mode batches usage writes, but Drain must still not return
// until every queued event and its follow-up work (quota) is durable.
func TestPerformanceModeDrainFlushesBatch(t *testing.T) {
	h := newHarness(t)
	h.server.StartPerformanceMode(PerformanceMode{Requested: true, Active: true})
	t.Cleanup(h.server.StopPerformanceMode)
	for i := 0; i < 5; i++ {
		chatOK(t, h)
	}
	rec := h.do("POST", "/v1/chat/completions", map[string]any{"model": "no-such-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code == http.StatusOK {
		t.Fatal("unknown model unexpectedly succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	events, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("usage events after Drain = %d, want 6", len(events))
	}
	var snapshots int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM reporting_usage_snapshot`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 6 {
		t.Fatalf("reporting snapshots = %d, want 6", snapshots)
	}
}

// Stop flushes a partial batch immediately rather than waiting for a tick,
// and an event arriving after Stop is still written (directly).
func TestPerformanceModeStopFlushesAndLateWritesStillLand(t *testing.T) {
	h := newHarness(t)
	h.server.StartPerformanceMode(PerformanceMode{Requested: true, Active: true})
	chatOK(t, h)
	h.server.StopPerformanceMode()
	chatOK(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	events, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("usage events = %d, want 2", len(events))
	}
}

// Many events share batches; nothing is lost or duplicated. Requests are sent
// back to back (the harness's fake upstream is not concurrency-safe); the
// writes still overlap because they are queued, not written inline.
func TestPerformanceModeManyWritesShareBatches(t *testing.T) {
	h := newHarness(t)
	h.server.StartPerformanceMode(PerformanceMode{Requested: true, Active: true})
	t.Cleanup(h.server.StopPerformanceMode)
	const n = 40
	for i := 0; i < n; i++ {
		chatOK(t, h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	events, total, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if total != n || len(events) != n {
		t.Fatalf("usage events = %d (total %d), want %d", len(events), total, n)
	}
}

func TestShouldTouchThrottlesPerCredential(t *testing.T) {
	s := &Server{}
	if !s.shouldTouch("tok|a") {
		t.Fatal("first use must touch")
	}
	if s.shouldTouch("tok|a") {
		t.Fatal("second use within the interval must not touch")
	}
	if !s.shouldTouch("tok|b") {
		t.Fatal("another credential must touch")
	}
	s.touched.Store("tok|a", time.Now().Add(-touchInterval-time.Second))
	if !s.shouldTouch("tok|a") {
		t.Fatal("use after the interval must touch")
	}
}

// Concurrent enqueue from many goroutines races a stop; every event is
// written exactly once and done is called exactly once per event.
func TestUsageBatcherConcurrentEnqueueAndStop(t *testing.T) {
	h := newHarness(t)
	b := newUsageBatcher(h.store, h.server.Logger)
	const n = 300
	var done sync.WaitGroup
	done.Add(n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.enqueue(usageItem{ctx: context.Background(), event: &store.UsageEvent{HTTPStatus: 200}, done: done.Done})
		}()
		if i == n/2 {
			go b.close()
		}
	}
	wg.Wait()
	b.close()
	done.Wait()
	_, total, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Fatalf("written = %d, want %d", total, n)
	}
}
