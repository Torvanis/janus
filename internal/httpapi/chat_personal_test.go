package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// TestChatUsesPersonalSubscriptions: the chat model picker reads
// /api/v1/models, which now lists the caller's selected personal-plan models
// (my/<provider>/<model>) next to the org catalog, and a chat message sent to
// one goes through the owner's plan, is metered as personal (no org cost),
// and works whatever "Working under" says.
func TestChatUsesPersonalSubscriptions(t *testing.T) {
	h, sess := chatHarness(t)
	fake := newFakeXAI(t)
	ctx := context.Background()

	listPersonal := func() []map[string]any {
		t.Helper()
		rec := h.doAsSession(sess, http.MethodGet, "/api/v1/models", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/v1/models: %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Personal []map[string]any `json:"personal_models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Personal == nil {
			t.Fatalf("personal_models missing (must be [] when empty): %s", rec.Body.String())
		}
		return out.Personal
	}
	if got := listPersonal(); len(got) != 0 {
		t.Fatalf("personal models before connecting: %v", got)
	}

	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if got := listPersonal(); len(got) != 0 {
		t.Fatalf("unselected models listed: %v", got)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	got := listPersonal()
	if len(got) != 1 || got[0]["id"] != "my/xai/grok-test" {
		t.Fatalf("personal models = %v", got)
	}

	// Working under a team the user belongs to changes nothing for a personal plan.
	team, err := h.store.CreateTeam(ctx, "Household", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, "member", "manual", ""); err != nil && !strings.Contains(err.Error(), "already") {
		t.Fatal(err)
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/me/active-team", map[string]any{"team_id": team.ID}); rec.Code != http.StatusOK {
		t.Fatalf("active team: %d %s", rec.Code, rec.Body.String())
	}
	if got := listPersonal(); len(got) != 1 {
		t.Fatalf("personal models under a team: %v", got)
	}

	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "my/xai/grok-test"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hello plan"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete || done["content"] != "hi" {
		t.Fatalf("done = %v", done)
	}

	fake.mu.Lock()
	n := len(fake.inferenceBody)
	var last string
	if n > 0 {
		last = fake.inferenceBody[n-1]
	}
	fake.mu.Unlock()
	if n == 0 || !strings.Contains(last, "hello plan") || !strings.Contains(last, `"grok-test"`) {
		t.Fatalf("the plan never saw the chat (n=%d): %s", n, last)
	}

	var ev *store.UsageEvent
	for _, e := range h.waitForUsageEvents(1) {
		if e.ClientApp == chatClientApp && e.SubscriptionID == connID {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no chat usage event attributed to the subscription")
	}
	if ev.CostNano != 0 {
		t.Errorf("personal plan chat cost the organization %v", ev.CostNano)
	}
}

// TestChatReadsAStreamLabelledTextPlain: the ChatGPT plan backend sent its
// event stream as "text/plain; charset=utf-8"; Chat stored an empty reply
// (tokens metered, no text) because it decided how to read the body from
// Content-Type alone. The reply text must arrive whatever the label says.
func TestChatReadsAStreamLabelledTextPlain(t *testing.T) {
	h, sess := chatHarness(t)
	fake := newFakeXAI(t)
	fake.streamType = "text/plain; charset=utf-8"
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "my/xai/grok-test"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hello?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete || done["content"] != "hi" {
		t.Fatalf("reply lost under text/plain: %v", done)
	}
}

// TestChatEmptyReplyIsNamed: a reply that "completes" with no text is shown
// as an error that says so, never as silence.
func TestChatEmptyReplyIsNamed(t *testing.T) {
	h, sess := chatHarness(t)
	fake := newFakeXAI(t)
	fake.emptyStream = true
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "my/xai/grok-test"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hello?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusError || done["error_code"] != "upstream.empty_reply" {
		t.Fatalf("empty reply not named: %v", done)
	}
}
