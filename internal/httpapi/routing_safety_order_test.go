package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func (f *routeDecider) seenStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.states...)
}

// A blocking ingress policy decides before the routing classifier is asked:
// the text it blocks never reaches the decision model.
func TestRoutingClassifierNeverSeesBlockedText(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.h.secgwPolicy(t, store.SecgwPolicy{Name: "org secrets block", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}})
	rec := rig.chat(t, "Review this contract, my key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected a block: %d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(300 * time.Millisecond) // a call started in the background would land by now
	for _, st := range rig.decider.seenStates() {
		if strings.Contains(st, "AKIA") {
			t.Fatalf("the routing classifier received the blocked text: %q", st)
		}
	}
	if n := len(rig.decider.seen()); n != 0 {
		t.Fatalf("the routing classifier was called %d times for a blocked request", n)
	}
	// An admitted request still routes.
	rec = rig.chat(t, "Review this contract clause", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("admitted request must still route: %d %s", rec.Code, rec.Body.String())
	}
}

// A shadow router's background decision reads the forwarded (redacted)
// text, never the caller's original.
func TestShadowRoutingClassifierSeesRedactedText(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusShadow)
	rig.h.secgwPolicy(t, store.SecgwPolicy{Name: "org secrets redact", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeRedact, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}})
	rec := rig.chat(t, "Review this contract, my key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ds := rig.decisions(t, 1); ds[0].RouteID != "legal" {
		t.Fatalf("shadow decision: %+v", ds[0])
	}
	states := rig.decider.seenStates()
	if len(states) == 0 {
		t.Fatal("the shadow router never asked the classifier")
	}
	for _, st := range states {
		if strings.Contains(st, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("the shadow routing classifier received the unredacted secret: %q", st)
		}
	}
}
