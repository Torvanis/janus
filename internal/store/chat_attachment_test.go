package store

import (
	"context"
	"testing"
	"time"
)

// TestChatAttachmentRoundTrip covers the attachment lifecycle on whichever
// database newRollupStore provides (SQLite by default, PostgreSQL when
// JANUS_ROLLUP_TEST_DATABASE_URL is set): insert, owner-scoped reads,
// binding to a message, refusing to delete a bound row, listing by
// message and conversation, and the stale-unbound sweep.
func TestChatAttachmentRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()

	old := &ChatAttachment{UserID: "u1", Kind: ChatAttachmentDocument, FilenameEnc: "enc-a", MIME: "text/plain",
		SizeBytes: 12, SHA256: "abc", StorageRef: "database:chat/u1/a/original", TextRef: "database:chat/u1/a/text",
		TextChars: 400, EstTokens: 100, Truncated: true, CreatedAt: time.Now().UTC().Add(-48 * time.Hour)}
	img := &ChatAttachment{UserID: "u1", Kind: ChatAttachmentImage, FilenameEnc: "enc-b", MIME: "image/png",
		SizeBytes: 99, StorageRef: "database:chat/u1/b/original", Width: 640, Height: 480}
	other := &ChatAttachment{UserID: "u2", Kind: ChatAttachmentImage, MIME: "image/png", StorageRef: "database:x"}
	for _, a := range []*ChatAttachment{old, img, other} {
		if err := s.InsertChatAttachment(ctx, a); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := s.ChatAttachment(ctx, "u1", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TextChars != 400 || got.EstTokens != 100 || !got.Truncated || got.Status != ChatAttachmentReady || got.TextRef != old.TextRef {
		t.Fatalf("round trip = %+v", got)
	}
	if _, err := s.ChatAttachment(ctx, "u2", old.ID); err != ErrNotFound {
		t.Fatalf("other user read = %v, want ErrNotFound", err)
	}
	list, err := s.ChatAttachmentsByIDs(ctx, "u1", []string{old.ID, img.ID, other.ID})
	if err != nil || len(list) != 2 {
		t.Fatalf("by ids = %d %v", len(list), err)
	}

	n, err := s.BindChatAttachments(ctx, "u1", "c1", "m1", []string{img.ID, other.ID})
	if err != nil || n != 1 {
		t.Fatalf("bind = %d %v (another user's upload must not bind)", n, err)
	}
	// Bound elsewhere: cannot be re-bound into another conversation.
	if n, _ := s.BindChatAttachments(ctx, "u1", "c2", "m9", []string{img.ID}); n != 0 {
		t.Fatalf("rebind into another conversation = %d", n)
	}
	if ok, err := s.DeleteChatAttachment(ctx, "u1", img.ID); err != nil || ok {
		t.Fatalf("delete bound = %v %v, want refused", ok, err)
	}
	byMsg, err := s.ChatAttachmentsForMessages(ctx, []string{"m1", "nope"})
	if err != nil || len(byMsg) != 1 || byMsg[0].ID != img.ID || byMsg[0].Width != 640 {
		t.Fatalf("for messages = %+v %v", byMsg, err)
	}
	byConv, err := s.ChatAttachmentsForConversation(ctx, "u1", "c1")
	if err != nil || len(byConv) != 1 {
		t.Fatalf("for conversation = %d %v", len(byConv), err)
	}

	stale, err := s.StaleUnboundChatAttachments(ctx, time.Now().UTC().Add(-24*time.Hour), 200)
	if err != nil || len(stale) != 1 || stale[0].ID != old.ID {
		t.Fatalf("stale = %+v %v", stale, err)
	}
	if ok, err := s.DeleteChatAttachment(ctx, "u1", old.ID); err != nil || !ok {
		t.Fatalf("delete unbound = %v %v", ok, err)
	}
	if err := s.DeleteChatAttachmentRows(ctx, []string{img.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChatAttachment(ctx, "u1", img.ID); err != ErrNotFound {
		t.Fatalf("after row delete = %v", err)
	}
}
