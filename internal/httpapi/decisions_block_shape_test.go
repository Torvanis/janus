package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// A policy's "synthetic refusal" is a chat completion; a decisions caller
// gets the decisions API's own refusal: the 403 error object.
func TestDecisionsBlockIgnoresSyntheticRefusalShape(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "synthetic", Enabled: true, SyntheticRefusal: true, RefusalText: "Declined by policy.",
		Checks: []store.SecgwCheck{{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress}}})
	body := strings.Replace(sprintf(s1Body, jev.Name), "I was charged twice.", "I was charged twice, my key is AKIAIOSFODNN7EXAMPLE", 1)
	rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a blocked decision must answer 403, not %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "chat.completion") || !strings.Contains(rec.Body.String(), CodeSecurityBlocked) {
		t.Fatalf("a blocked decision must carry the error object, not a chat completion: %s", rec.Body.String())
	}
	ev := h.waitForUsageEvents(1)[0]
	if ev.FinishReason == "content_filter" {
		t.Fatalf("a decision is not a chat completion: finish_reason %q", ev.FinishReason)
	}
}
