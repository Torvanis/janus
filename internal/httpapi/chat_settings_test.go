package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func lastUpstreamJSON(t *testing.T, h *harness) map[string]any {
	t.Helper()
	if len(h.upstreamRequests) == 0 {
		t.Fatal("no upstream request")
	}
	var v map[string]any
	if err := json.Unmarshal(h.upstreamRequests[len(h.upstreamRequests)-1].Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestChatInstructionsAndEffort: the user's own instructions and the
// conversation's instructions go first as one system message, the chosen
// thinking effort goes out as reasoning_effort, and instructions are stored
// encrypted.
func TestChatInstructionsAndEffort(t *testing.T) {
	h, sess := chatHarness(t)
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/chat/settings", map[string]any{"instructions": "Answer in British English.", "default_model": "test-model"}); rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/settings", nil)
	if !strings.Contains(rec.Body.String(), "British English") || !strings.Contains(rec.Body.String(), `"default_model":"test-model"`) {
		t.Fatalf("settings read back: %s", rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/chat/settings", map[string]any{"instructions": strings.Repeat("x", 8001)}); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized instructions: %d", rec.Code)
	}

	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{
		"model": "test-model", "instructions": "You are a patient maths tutor.", "reasoning_effort": "high"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv struct {
		ID              string `json:"id"`
		Instructions    string `json:"instructions"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	decodeInto(t, rec, &conv)
	if conv.Instructions != "You are a patient maths tutor." || conv.ReasoningEffort != "high" {
		t.Fatalf("created = %+v", conv)
	}
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "What is 7 x 8?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	body := lastUpstreamJSON(t, h)
	if body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "Answer in British English.\n\nYou are a patient maths tutor." {
		t.Errorf("system message = %v", first)
	}
	if last := msgs[len(msgs)-1].(map[string]any); last["content"] != "What is 7 x 8?" {
		t.Errorf("last message = %v", last)
	}

	// Stored encrypted, never plaintext.
	c, err := h.store.ChatConversation(context.Background(), h.user.ID, conv.ID)
	if err != nil || c.InstructionsEnc == "" || strings.Contains(c.InstructionsEnc, "tutor") {
		t.Fatalf("instructions stored as %q (err %v)", c.InstructionsEnc, err)
	}

	// Clearing effort and instructions: no system message from the
	// conversation, no reasoning_effort.
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv.ID, map[string]any{"instructions": "", "reasoning_effort": ""}); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/chat/settings", map[string]any{"instructions": ""}); rec.Code != http.StatusOK {
		t.Fatalf("clear settings: %d", rec.Code)
	}
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "And 9 x 9?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	body = lastUpstreamJSON(t, h)
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort still sent: %v", body["reasoning_effort"])
	}
	if first := body["messages"].([]any)[0].(map[string]any); first["role"] == "system" {
		t.Errorf("system message still sent: %v", first)
	}
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv.ID, map[string]any{"reasoning_effort": "turbo"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad effort accepted: %d", rec.Code)
	}
}

// TestChatPinAndTemporary: pinned conversations list first; a temporary
// conversation is never listed or searchable, and the reaper purges it.
func TestChatPinAndTemporary(t *testing.T) {
	h, sess := chatHarness(t)
	ctx := context.Background()
	create := func(body map[string]any) string {
		t.Helper()
		rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		var c struct {
			ID string `json:"id"`
		}
		decodeInto(t, rec, &c)
		return c.ID
	}
	a := create(map[string]any{"model": "test-model", "title": "Older chat"})
	time.Sleep(5 * time.Millisecond)
	b := create(map[string]any{"model": "test-model", "title": "Newer chat"})
	tmp := create(map[string]any{"model": "test-model", "temporary": true})
	sent := chatSend(t, h, sess, tmp, map[string]any{"content": "zebra secrets"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	list := func() []string {
		rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations", nil)
		var out struct {
			Conversations []struct {
				ID     string `json:"id"`
				Pinned bool   `json:"pinned"`
			} `json:"conversations"`
		}
		decodeInto(t, rec, &out)
		ids := []string{}
		for _, c := range out.Conversations {
			ids = append(ids, c.ID)
		}
		return ids
	}
	if got := list(); len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("list = %v, want [newer older] without the temporary chat", got)
	}
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+a, map[string]any{"pinned": true}); rec.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body.String())
	}
	if got := list(); got[0] != a {
		t.Fatalf("pinned chat not first: %v", got)
	}
	// The temporary chat still opens directly (the tab that owns it).
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+tmp, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"temporary":true`) {
		t.Fatalf("open temporary: %d %s", rec.Code, rec.Body.String())
	}
	// Never searchable.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/search?q=zebra", nil)
	if strings.Contains(rec.Body.String(), tmp) {
		t.Fatalf("temporary chat found by search: %s", rec.Body.String())
	}
	var terms int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_search_term WHERE conversation_id = ?`, tmp).Scan(&terms); err != nil || terms != 0 {
		t.Fatalf("temporary chat has %d search terms (err %v)", terms, err)
	}
	// Pinning a temporary chat is ignored.
	h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+tmp, map[string]any{"pinned": true})
	if c, _ := h.store.ChatConversation(ctx, h.user.ID, tmp); c == nil || c.Pinned {
		t.Fatalf("temporary chat pinned: %+v", c)
	}

	// Idle past the TTL: the sweep purges it, messages included.
	if n := h.server.chatSweepTemporary(ctx, time.Now().Add(store.ChatTemporaryTTL+time.Minute)); n != 1 {
		t.Fatalf("swept %d temporary chats, want 1", n)
	}
	if _, err := h.store.ChatConversation(ctx, h.user.ID, tmp); err == nil {
		t.Fatal("temporary chat still there after the sweep")
	}
	var msgs int
	_ = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_message WHERE conversation_id = ?`, tmp).Scan(&msgs)
	if msgs != 0 {
		t.Fatalf("%d messages left after purge", msgs)
	}
	if got := list(); len(got) != 2 {
		t.Fatalf("normal chats touched by the sweep: %v", got)
	}
}

// TestChatReplyNotesShowSecurityOutcome: a reply the Security Gateway
// flagged carries the outcome from the request log (on the done frame and
// when the conversation is reopened); a clean reply carries no notes.
func TestChatReplyNotesShowSecurityOutcome(t *testing.T) {
	h, sess := chatHarness(t)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv struct {
		ID string `json:"id"`
	}
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "plain question"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if _, ok := done["notes"]; ok {
		t.Fatalf("clean reply has notes: %v", done["notes"])
	}

	h.secgwPolicy(t, store.SecgwPolicy{Name: "observe", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeObserve},
	}})
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "my key is " + testAWSKey})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	notes, _ := done["notes"].(map[string]any)
	if notes == nil || notes["secgw_action"] != store.SecgwActionObserved || notes["secgw_violations"].(float64) != 1 {
		t.Fatalf("done notes = %v", done["notes"])
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv.ID, nil)
	if !strings.Contains(rec.Body.String(), `"secgw_action":"observed"`) {
		t.Fatalf("reopened conversation lost the note: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"secgw_action":"checked"`) {
		t.Fatalf("clean replies must not carry a checked note: %s", rec.Body.String())
	}
}
