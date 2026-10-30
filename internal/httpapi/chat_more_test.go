package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

var _ = store.ChatRoleUser

func chatCreate(t *testing.T, h *harness, sess *auth.Session, body map[string]any) string {
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

// TestChatExport: Markdown and JSON exports of the current branch, with
// instructions, model and cost; another user's chat is not exportable.
func TestChatExport(t *testing.T) {
	h, sess := chatHarness(t)
	id := chatCreate(t, h, sess, map[string]any{"model": "test-model", "title": "Trip / plan: v2!", "instructions": "Be brief."})
	sent := chatSend(t, h, sess, id, map[string]any{"content": "Where should we camp?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+id+"/export?format=markdown", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="Trip-plan-v2.md"` {
		t.Errorf("filename = %s", cd)
	}
	md := rec.Body.String()
	for _, want := range []string{"# Trip / plan: v2!", "> Be brief.", "### You", "Where should we camp?", "### test-model"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}

	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+id+"/export?format=json", nil)
	var ex chatExport
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.Format != "janus-chat-v1" || len(ex.Messages) != 2 || ex.Messages[0].Role != "user" || ex.Messages[1].Content == "" {
		t.Fatalf("json export = %+v", ex)
	}
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+id+"/export?format=pdf", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad format: %d", rec.Code)
	}

	other := h.sessionFor(t, "other@example.com")
	if rec := h.doAsSession(other, http.MethodGet, "/api/v1/chat/conversations/"+id+"/export", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user exported the chat: %d", rec.Code)
	}
}

// TestChatPrompts: create, list, validate names, unique per user,
// encrypted bodies, update, delete; another user cannot see them.
func TestChatPrompts(t *testing.T) {
	h, sess := chatHarness(t)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/prompts", map[string]any{"name": "/Summarise", "title": "Summarise", "body": "Summarise this for {{audience}}:\n\n{{text}}"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var p chatPromptView
	decodeInto(t, rec, &p)
	if p.Name != "summarise" || !strings.Contains(p.Body, "{{audience}}") {
		t.Fatalf("created = %+v", p)
	}
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/prompts", map[string]any{"name": "summarise", "body": "x"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name: %d", rec.Code)
	}
	for _, bad := range []string{"", "has space", "-lead", strings.Repeat("a", 41), "ünï"} {
		if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/prompts", map[string]any{"name": bad, "body": "x"}); rec.Code != http.StatusBadRequest {
			t.Errorf("name %q accepted: %d", bad, rec.Code)
		}
	}
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/prompts", map[string]any{"name": "empty", "body": "  "}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty body accepted: %d", rec.Code)
	}
	stored, err := h.store.ChatPrompt(context.Background(), h.user.ID, p.ID)
	if err != nil || strings.Contains(stored.BodyEnc, "Summarise this") {
		t.Fatalf("body stored as %q (err %v)", stored.BodyEnc, err)
	}

	rec = h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/prompts/"+p.ID, map[string]any{"name": "tldr", "body": "TL;DR: {{text}}"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"tldr"`) {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	other := h.sessionFor(t, "other@example.com")
	rec = h.doAsSession(other, http.MethodGet, "/api/v1/chat/prompts", nil)
	if strings.Contains(rec.Body.String(), "tldr") {
		t.Fatalf("other user sees the prompt: %s", rec.Body.String())
	}
	if rec := h.doAsSession(other, http.MethodDelete, "/api/v1/chat/prompts/"+p.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("other user deleted the prompt: %d", rec.Code)
	}
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/prompts/"+p.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/prompts", nil)
	if strings.Contains(rec.Body.String(), p.ID) {
		t.Fatal("deleted prompt still listed")
	}
}

// TestChatShareLinks: a link is a snapshot (later messages are not in it),
// opens for another signed-in user without cost or instructions, counts
// views, and ends when revoked or when the chat is deleted. Temporary
// chats cannot be shared; signed-out viewers are refused.
func TestChatShareLinks(t *testing.T) {
	h, sess := chatHarness(t)
	id := chatCreate(t, h, sess, map[string]any{"model": "test-model", "title": "Recipe", "instructions": "Secret house style."})
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+id+"/shares", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("empty chat shared: %d", rec.Code)
	}
	sent := chatSend(t, h, sess, id, map[string]any{"content": "Pancakes for four?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+id+"/shares", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", rec.Code, rec.Body.String())
	}
	var sh chatShareView
	decodeInto(t, rec, &sh)
	if len(sh.ID) < 30 || sh.URL != "/chat/shared/"+sh.ID || sh.MessageCount != 2 {
		t.Fatalf("share = %+v", sh)
	}
	// A later message is not in the snapshot.
	sent = chatSend(t, h, sess, id, map[string]any{"content": "And for eight?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	other := h.sessionFor(t, "other@example.com")
	rec = h.doAsSession(other, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("view: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Pancakes for four?") || strings.Contains(body, "And for eight?") {
		t.Fatalf("snapshot wrong: %s", body)
	}
	if strings.Contains(body, "Secret house style") || strings.Contains(body, "cost_usd") || strings.Contains(body, "tokens_in") {
		t.Fatalf("snapshot leaks private details: %s", body)
	}
	if !strings.Contains(body, `"own":false`) {
		t.Errorf("own flag: %s", body)
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+id+"/shares", nil)
	if !strings.Contains(rec.Body.String(), `"views":1`) {
		t.Errorf("views not counted: %s", rec.Body.String())
	}
	// Signed out: refused.
	if rec := h.do(http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code == http.StatusOK {
		t.Fatalf("signed-out viewer got the share: %d", rec.Code)
	}

	// Revoke ends it and drops the snapshot.
	if rec := h.doAsSession(other, http.MethodDelete, "/api/v1/chat/conversations/"+id+"/shares/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("non-owner revoked: %d", rec.Code)
	}
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+id+"/shares/"+sh.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := h.doAsSession(other, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked share still opens: %d", rec.Code)
	}
	var snap string
	_ = h.store.DB().QueryRowContext(context.Background(), `SELECT snapshot_enc FROM chat_share WHERE id = ?`, sh.ID).Scan(&snap)
	if snap != "" {
		t.Errorf("revoked snapshot kept")
	}

	// Deleting the chat ends its other links.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+id+"/shares", nil)
	decodeInto(t, rec, &sh)
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete chat: %d", rec.Code)
	}
	if rec := h.doAsSession(other, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("share of a deleted chat still opens: %d", rec.Code)
	}

	tmp := chatCreate(t, h, sess, map[string]any{"model": "test-model", "temporary": true})
	sent = chatSend(t, h, sess, tmp, map[string]any{"content": "hi"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+tmp+"/shares", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("temporary chat shared: %d", rec.Code)
	}
	var audits int
	_ = h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_log WHERE action IN ('chat_share_created','chat_share_revoked')`).Scan(&audits)
	if audits < 3 {
		t.Errorf("share audit rows = %d", audits)
	}
}

// sessionFor signs in a second, non-admin user.
func (h *harness) sessionFor(t *testing.T, email string) *auth.Session {
	t.Helper()
	ctx := context.Background()
	u, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-"+email, email, "Other User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := h.server.Sessions.Create(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}
