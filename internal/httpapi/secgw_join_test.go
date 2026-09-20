package httpapi

import (
	"context"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
)

func TestSecgwViolationJoinsUsageEvent(t *testing.T) {
	h := newHarness(t)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "o", Enabled: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock}}})
	h.do(http.MethodPost, "/v1/chat/completions", chatBody("key "+testAWSKey, false))
	ev := h.waitForUsageEvents(1)[0]
	vs := h.waitForViolations(t, 1)
	if vs[0].UsageEventID != ev.ID || vs[0].RequestID != ev.RequestID {
		t.Fatalf("join broken: violation=%+v event.ID=%s", vs[0], ev.ID)
	}
	// Display labels are resolved at read time so the admin UI never has to
	// show a UUID for a person or a policy.
	if vs[0].UserLabel == "" || vs[0].PolicyName != "o" {
		t.Fatalf("labels not resolved: user=%q policy=%q", vs[0].UserLabel, vs[0].PolicyName)
	}
	full, _ := h.store.SecgwViolationByID(context.Background(), vs[0].ID, h.server.Cipher)
	if full.PolicyID == "" || full.BindingID == "" || full.ModelName != "test-model" {
		t.Fatalf("attribution: %+v", full)
	}
}
