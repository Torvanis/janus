package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// Usage insights classifies what the caller asked, not the instructions
// Child Safety adds to a covered child's prompt.
func TestInsightsClassifiesCallerTextNotChildInstructions(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid11", "kid11@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(),
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": kid.ID}}})
	f := newFakeDecisionServer(t)
	h.enableClassification(t, f, store.ClassificationModeInline, func(s *store.ClassificationSettings) {
		s.IncludeSystemPrompt, s.TextScope = true, store.ClassificationScopeAllUser
	})
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("my landlord kept the deposit", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	sys := h.lastUpstreamSystem(t)
	if strings.TrimSpace(sys) == "" {
		t.Fatal("Child Safety added no instructions; the test proves nothing")
	}
	seen := f.seen()
	if len(seen) == 0 {
		t.Fatal("Usage insights never classified the request")
	}
	probe := sys
	if len(probe) > 40 {
		probe = probe[:40]
	}
	for _, st := range seen {
		if strings.Contains(st, probe) {
			t.Fatalf("Usage insights classified Child Safety's instructions: %q", st)
		}
		if !strings.Contains(st, "landlord") {
			t.Fatalf("Usage insights must classify the caller's text, saw %q", st)
		}
	}
}
