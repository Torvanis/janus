package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// Deleting (archiving) a team switches everything still bound to it back to
// personal context so API keys, the browser selection and Chat keep working.
func TestTeamArchiveSwitchesBoundContextsToPersonal(t *testing.T) {
	h, sess := chatHarness(t)
	promote(t, h)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Doomed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTeamMembers(ctx, team.ID, []string{h.user.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeTeam, team.ID); err != nil {
		t.Fatal(err)
	}
	h.server.configCache.invalidate()
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/me/active-team", map[string]any{"team_id": team.ID}); rec.Code != http.StatusOK {
		t.Fatalf("set active team: %d %s", rec.Code, rec.Body.String())
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hi"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	// A second, team-bound API key the integration uses.
	tok, secret, err := h.store.CreateToken(ctx, h.user.ID, "integration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`UPDATE api_token SET team_id=? WHERE id=?`, team.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateTokenCache()

	rec = h.do(http.MethodDelete, "/api/v1/admin/teams/"+team.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete team: %d %s", rec.Code, rec.Body.String())
	}
	moved, _ := decodeBody(t, rec)["switched_to_personal"].(map[string]any)
	if moved["tokens"] != 1.0 || moved["sessions"] != 1.0 || moved["conversations"] != 1.0 {
		t.Errorf("switched_to_personal = %v, want 1 token, 1 session, 1 conversation", moved)
	}

	// The team-bound API key keeps working (personal grants apply).
	h.token = secret
	if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]any{{"role": "user", "content": "hi"}}}); rec.Code != http.StatusOK {
		t.Fatalf("team-bound key after archive: %d %s", rec.Code, rec.Body.String())
	}
	// The browser selection fell back to personal and Chat sends again.
	if team, err := h.store.SessionTeam(ctx, sess.ID, h.user.ID); err != nil || team != "" {
		t.Fatalf("session team = %q, %v; want personal", team, err)
	}
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "after"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["team_id"] != "" {
		t.Errorf("chat after archive billed to %v, want personal", done["team_id"])
	}
	// The member is told.
	notes, _, err := h.store.ListNotifications(ctx, h.user.ID, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n.Body, "Doomed") && strings.Contains(n.Body, "personal") {
			found = true
		}
	}
	if !found {
		t.Error("no notification told the member their team context switched to personal")
	}
}
