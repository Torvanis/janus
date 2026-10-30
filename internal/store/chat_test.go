package store

import (
	"context"
	"testing"
	"time"
)

// Migration 0054 and the chat store work on both databases (run with
// JANUS_ROLLUP_TEST_DATABASE_URL for Postgres): hidden chat tokens are per
// (user, team) and never listed, conversations are owner-scoped, and the
// reaper interrupts only stale streaming runs.
func TestChatStoreRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "sub-chat", "chat@example.com", "Chat", true, false)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other", true, false)
	if err != nil {
		t.Fatal(err)
	}

	personal, err := s.ChatToken(ctx, u.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ChatToken(ctx, u.ID, "")
	if err != nil || again.ID != personal.ID {
		t.Fatalf("chat token not reused: %v %v", again, err)
	}
	if personal.Kind != TokenKindChat {
		t.Fatalf("kind = %q", personal.Kind)
	}
	team, err := s.CreateTeam(ctx, "Family", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChatToken(ctx, u.ID, team.ID); err == nil {
		t.Fatal("chat token minted for a team the user is not on")
	}
	if err := s.SetTeamMembers(ctx, team.ID, []string{u.ID}); err != nil {
		t.Fatal(err)
	}
	teamTok, err := s.ChatToken(ctx, u.ID, team.ID)
	if err != nil || teamTok.ID == personal.ID || teamTok.TeamID != team.ID {
		t.Fatalf("team chat token = %+v err %v", teamTok, err)
	}
	if list, _ := s.ListTokens(ctx, u.ID); len(list) != 0 {
		t.Fatalf("hidden chat tokens listed: %d", len(list))
	}
	// Disabling a user revokes everything; the next chat mints a fresh one.
	if err := s.RevokeTokensForUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ChatToken(ctx, u.ID, "")
	if err != nil || fresh.ID == personal.ID {
		t.Fatalf("revoked chat token revived: %v %v", fresh, err)
	}

	c := &ChatConversation{UserID: u.ID, ModelName: "m", Title: "t"}
	if err := s.CreateChatConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChatConversation(ctx, other.ID, c.ID); err != ErrNotFound {
		t.Fatalf("another user read the conversation: %v", err)
	}
	m := &ChatMessage{ConversationID: c.ID, Role: ChatRoleAssistant, Status: ChatStatusStreaming, TokensOutPerSecond: 0.35}
	if err := s.InsertChatMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChatMessage(ctx, m.ID)
	if err != nil || got.TokensOutPerSecond != 0.35 {
		t.Fatalf("message round trip: %+v %v", got, err)
	}
	live := &ChatRun{MessageID: m.ID, ConversationID: c.ID, UserID: u.ID, Status: ChatStatusStreaming}
	if err := s.InsertChatRun(ctx, live); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReapChatRuns(ctx, time.Now().Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("reaped a live run: %d %v", n, err)
	}
	if err := s.RequestChatRunCancel(ctx, other.ID, live.ID); err != nil {
		t.Fatal(err)
	}
	if stop, _ := s.HeartbeatChatRun(ctx, live.ID, 1); stop {
		t.Fatal("another user's stop request landed")
	}
	if err := s.RequestChatRunCancel(ctx, u.ID, live.ID); err != nil {
		t.Fatal(err)
	}
	if stop, _ := s.HeartbeatChatRun(ctx, live.ID, 2); !stop {
		t.Fatal("owner's stop request lost")
	}
	if n, err := s.ReapChatRuns(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("stale run not reaped: %d %v", n, err)
	}
	if got, _ := s.ChatMessage(ctx, m.ID); got.Status != ChatStatusInterrupted {
		t.Fatalf("message status %q", got.Status)
	}
	if err := s.DeleteChatConversation(ctx, u.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListChatConversations(ctx, u.ID, 0); len(list) != 0 {
		t.Fatalf("deleted conversation listed")
	}
}
