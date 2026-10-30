package store

import (
	"context"
	"testing"
)

// Migration 0055 and the blind search index work on both databases (run
// with JANUS_ROLLUP_TEST_DATABASE_URL for Postgres). The store only ever sees
// opaque hashes; tests use readable stand-ins.
func TestChatSearchIndex(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	a, _, err := s.UpsertUserFromIdentity(ctx, "sub-sa", "sa@example.com", "A", true, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.UpsertUserFromIdentity(ctx, "sub-sb", "sb@example.com", "B", true, false)
	if err != nil {
		t.Fatal(err)
	}
	conv := func(userID, title string) *ChatConversation {
		c := &ChatConversation{UserID: userID, ModelName: "m", Title: title}
		if err := s.CreateChatConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	msg := func(c *ChatConversation, hashes ...string) *ChatMessage {
		m := &ChatMessage{ConversationID: c.ID, Role: ChatRoleUser, Status: ChatStatusComplete, ContextDropped: 3}
		if err := s.InsertChatMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := s.IndexChatMessage(ctx, c.UserID, c.ID, m.ID, hashes); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Per-user salting means real hashes differ per user; here user B's
	// index deliberately reuses A's hash strings to prove the store scopes
	// by user and not just by hash.
	c1 := conv(a.ID, "one")
	m1 := msg(c1, "hRED", "hFOX")
	msg(c1, "hRED")
	c2 := conv(a.ID, "two")
	msg(c2, "hRED", "hDOG", "hDOG")
	cb := conv(b.ID, "b")
	msg(cb, "hRED", "hFOX")

	if got, _ := s.ChatMessage(ctx, m1.ID); got.ContextDropped != 3 {
		t.Fatalf("context_dropped round trip = %d", got.ContextDropped)
	}

	hits, err := s.SearchChat(ctx, a.ID, []string{"hRED"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ConversationID != c1.ID || hits[0].Matches != 2 || hits[1].ConversationID != c2.ID {
		t.Fatalf("single-term ranking = %+v", hits)
	}
	// AND: both terms must appear in the conversation.
	hits, _ = s.SearchChat(ctx, a.ID, []string{"hRED", "hFOX"}, 10)
	if len(hits) != 1 || hits[0].ConversationID != c1.ID || hits[0].BestMessageID != m1.ID {
		t.Fatalf("AND search = %+v", hits)
	}
	if hits, _ := s.SearchChat(ctx, a.ID, []string{"hFOX", "hDOG"}, 10); len(hits) != 0 {
		t.Fatalf("terms in different conversations matched: %+v", hits)
	}
	// User isolation: B's identical hashes never surface A's chats and
	// vice versa.
	hits, _ = s.SearchChat(ctx, b.ID, []string{"hRED", "hFOX"}, 10)
	if len(hits) != 1 || hits[0].ConversationID != cb.ID {
		t.Fatalf("user B search = %+v", hits)
	}
	if hits, _ := s.SearchChat(ctx, b.ID, []string{"hDOG"}, 10); len(hits) != 0 {
		t.Fatalf("user B found user A's message: %+v", hits)
	}

	// Titles are indexed under a title: row and count toward AND.
	if err := s.IndexChatMessage(ctx, a.ID, c2.ID, ChatTitleMessageID(c2.ID), []string{"hTITLE"}); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.SearchChat(ctx, a.ID, []string{"hTITLE", "hDOG"}, 10)
	if len(hits) != 1 || hits[0].ConversationID != c2.ID || hits[0].Matches != 1 || IsChatTitleMessageID(hits[0].BestMessageID) {
		t.Fatalf("title AND body = %+v", hits)
	}
	// Re-writing a message replaces its terms.
	if err := s.IndexChatMessage(ctx, a.ID, c1.ID, m1.ID, []string{"hCAT"}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.SearchChat(ctx, a.ID, []string{"hFOX"}, 10); len(hits) != 0 {
		t.Fatalf("stale terms after rewrite: %+v", hits)
	}
	// Large term lists insert in batches.
	many := make([]string, 1200)
	for i := range many {
		many[i] = "h" + NewID()
	}
	if err := s.IndexChatMessage(ctx, a.ID, c1.ID, m1.ID, many); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.SearchChat(ctx, a.ID, []string{many[1100]}, 10); len(hits) != 1 {
		t.Fatalf("batched insert lost rows: %+v", hits)
	}

	// Deleting a conversation removes its hits and its rows.
	if err := s.DeleteChatConversation(ctx, a.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.SearchChat(ctx, a.ID, []string{"hRED"}, 10)
	if len(hits) != 1 || hits[0].ConversationID != c2.ID {
		t.Fatalf("deleted conversation still found: %+v", hits)
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM chat_search_term WHERE conversation_id = ?`, c1.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deleted conversation keeps %d rows (%v)", n, err)
	}
	if pruned, err := s.PruneChatSearchIndex(ctx); err != nil || pruned != 0 {
		t.Fatalf("prune = %d %v", pruned, err)
	}

	// Reindex paging sees only live conversations.
	msgs, err := s.ChatMessagesForReindex(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range msgs {
		if it.Message.ConversationID == c1.ID {
			t.Fatal("reindex listed a deleted conversation's message")
		}
		if it.UserID == "" {
			t.Fatal("reindex row without owner")
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("reindex messages = %d, want 2", len(msgs))
	}
	convs, err := s.ChatConversationsForReindex(ctx, "", 500)
	if err != nil || len(convs) != 2 {
		t.Fatalf("reindex conversations = %d %v", len(convs), err)
	}
}

// 0055's down drops what its up added, so rollback then migrate works.
func TestChatSearchMigrationRollbackIsReapplicable(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	if err := s.RollbackMigration(ctx, "0055_chat_search"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-apply after rollback must work: %v", err)
	}
}
