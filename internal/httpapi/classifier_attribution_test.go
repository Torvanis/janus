package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// internalEvents waits for at least want gateway-internal classifier rows.
func internalEvents(t *testing.T, h *harness, want int) []*store.UsageEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var out []*store.UsageEvent
	for time.Now().Before(deadline) {
		evs, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 200, IncludeInternal: true})
		if err != nil {
			t.Fatal(err)
		}
		out = out[:0]
		for _, e := range evs {
			if e.Modality == "classification" {
				out = append(out, e)
			}
		}
		if len(out) >= want {
			return out
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d classifier usage events (have %d)", want, len(out))
	return nil
}

// Auto-routing's own classifier calls are Auto-routing cost, not Security
// Gateway cost, and stay tied to the request that caused them.
func TestRoutingClassifierCallsAreAttributedToAutoRouting(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rec := rig.chat(t, "Review this contract clause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ev := rig.h.requestEvent(t)
	for _, c := range internalEvents(t, rig.h, 1) {
		if c.ClientApp != store.InternalRoutingClientApp || c.EndpointPath != "/internal/routing/classify" {
			t.Fatalf("a routing decision call is recorded as %q %q, want %q", c.ClientApp, c.EndpointPath, store.InternalRoutingClientApp)
		}
		if c.RequestID != ev.RequestID {
			t.Fatalf("routing call request_id %q, want %q", c.RequestID, ev.RequestID)
		}
	}
}

// A shadow router decides in the background; its call keeps the request id.
func TestShadowRoutingClassifierCallKeepsRequestID(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusShadow)
	rec := rig.chat(t, "Review this contract clause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rig.decisions(t, 1)
	ev := rig.h.requestEvent(t)
	calls := internalEvents(t, rig.h, 1)
	for _, c := range calls {
		if c.RequestID != ev.RequestID {
			t.Fatalf("the shadow routing call has request_id %q, want %q", c.RequestID, ev.RequestID)
		}
		if c.ClientApp != store.InternalRoutingClientApp {
			t.Fatalf("the shadow routing call is recorded as %q", c.ClientApp)
		}
	}
}

// The served model's second ingress pass counts in the request's
// classifier calls.
func TestRoutedSecondIngressPassCountsClassifierCalls(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	p := &store.SecgwPolicy{Name: "onprem guard", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeObserve, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress,
			ClassifierModelID: rig.classifier.ID, Options: json.RawMessage(`{"timeout_ms":3000}`)},
	}}
	if err := rig.h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeModel, ScopeID: rig.router.Config.Routes[0].ModelID}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "Review this contract clause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ev := rig.h.requestEvent(t)
	guard := 0
	routing := 0
	for _, c := range internalEvents(t, rig.h, 2) {
		switch c.ClientApp {
		case store.InternalClientApp:
			guard++
		case store.InternalRoutingClientApp:
			routing++
		}
	}
	if guard == 0 {
		t.Fatal("the served model's guard never ran")
	}
	// Each question is its own HTTP call to this llama.cpp decision model,
	// so the decider's request count is the number of calls made.
	if made := len(rig.decider.seen()); ev.ClassifierCalls != made {
		t.Fatalf("request records %d classifier calls; %d were made (%d guard, %d routing usage events)", ev.ClassifierCalls, made, guard, routing)
	}
}
