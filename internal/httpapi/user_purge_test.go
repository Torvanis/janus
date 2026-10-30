package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/store"
)

// countRows counts rows of a table for one user.
func countRows(t *testing.T, h *harness, table, userID string) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// backdate moves a column of one row into the past.
func backdate(t *testing.T, h *harness, table, column, id string, age time.Duration) {
	t.Helper()
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE `+table+` SET `+column+` = ? WHERE id = ?`,
		store.FormatTime(time.Now().UTC().Add(-age)), id); err != nil {
		t.Fatal(err)
	}
}

// TestUserDeleteHoldsThenPurges: an admin delete keeps the person's
// content (shares stop at once), restoring brings it all back, and once
// the 30-day hold runs out the purge removes conversations, messages,
// files (and their stored objects), memories, prompts, settings, shares,
// subscriptions and notifications.
func TestUserDeleteHoldsThenPurges(t *testing.T) {
	h, admin := chatHarness(t)
	ctx := context.Background()
	other := h.sessionFor(t, "leaver@example.com")
	leaver, err := h.store.UserByAuthID(ctx, "sub-leaver@example.com")
	if err != nil {
		t.Fatal(err)
	}
	conv := chatCreate(t, h, other, map[string]any{"model": "test-model", "title": "Plans"})
	sent := chatSend(t, h, other, conv, map[string]any{"content": "Remember the plans"})
	chatEvents(t, h, other, sent["run_id"].(string), "")
	rec := h.doAsSession(other, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/shares", nil)
	var sh chatShareView
	decodeInto(t, rec, &sh)
	if err := h.store.InsertChatMemory(ctx, &store.ChatMemory{UserID: leaver.ID, ContentEnc: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateChatPrompt(ctx, &store.ChatPrompt{UserID: leaver.ID, Name: "p", Title: "P", BodyEnc: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveChatUserSettings(ctx, &store.ChatUserSetting{UserID: leaver.ID, DefaultModel: "test-model"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateNotification(ctx, &store.Notification{UserID: leaver.ID, Severity: "info", Title: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertSubscription(ctx, store.SubscriptionUpsert{UserID: leaver.ID, Provider: "mistral", AccountSubject: "1", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	bs, err := h.server.blobs().Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Put(ctx, "chat/leaver-file", []byte("secret"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertChatAttachment(ctx, &store.ChatAttachment{UserID: leaver.ID, ConversationID: conv, Kind: "file",
		MIME: "text/plain", StorageRef: blobstore.Ref(bs.Kind(), "chat/leaver-file"), Status: "ready"}); err != nil {
		t.Fatal(err)
	}

	// Delete: held, shares stop at once, content stays.
	rec = h.doAsSession(admin, http.MethodDelete, "/api/v1/admin/users/"+leaver.ID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"held_until"`) {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	viewer := h.sessionFor(t, "viewer@example.com")
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a deleted person's share still opens: %d", rec.Code)
	}
	h.server.PurgeHeldContent(ctx, time.Now())
	if countRows(t, h, "chat_conversation", leaver.ID) != 1 || countRows(t, h, "chat_memory", leaver.ID) != 1 {
		t.Fatal("content purged during the hold")
	}

	// Restore within the hold: everything is back, shares open again.
	rec = h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/users/"+leaver.ID, map[string]any{"is_active": true})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"held_until"`) {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("restored person's share: %d", rec.Code)
	}

	// Delete again and let the hold run out.
	if rec := h.doAsSession(admin, http.MethodDelete, "/api/v1/admin/users/"+leaver.ID, nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	backdate(t, h, "app_user", "deleted_at", leaver.ID, 31*24*time.Hour)
	if n := h.server.PurgeHeldContent(ctx, time.Now()); n < 1 {
		t.Fatalf("purge reported %d", n)
	}
	for _, table := range []string{"chat_conversation", "chat_attachment", "chat_memory", "chat_prompt", "chat_user_setting",
		"chat_share", "subscription_connection", "notification", "chat_search_term"} {
		if n := countRows(t, h, table, leaver.ID); n != 0 {
			t.Errorf("%s: %d rows left after the hold", table, n)
		}
	}
	var msgs int
	_ = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_message WHERE conversation_id = ?`, conv).Scan(&msgs)
	if msgs != 0 {
		t.Errorf("%d messages left after the hold", msgs)
	}
	if _, err := bs.Get(ctx, "chat/leaver-file"); err == nil {
		t.Error("stored file kept after the hold")
	}
	u, _ := h.store.UserByID(ctx, leaver.ID)
	if u.ContentPurgedAt == nil {
		t.Error("purge not recorded on the account")
	}
	// The usage log still names the account (its own retention).
	if _, err := h.store.UserByID(ctx, leaver.ID); err != nil {
		t.Fatal(err)
	}
}

// TestPurgeUserContentNowEndpoint: an admin can delete a deleted person's
// held content at once; an active account is refused.
func TestPurgeUserContentNowEndpoint(t *testing.T) {
	h, admin := chatHarness(t)
	ctx := context.Background()
	other := h.sessionFor(t, "now@example.com")
	u, _ := h.store.UserByAuthID(ctx, "sub-now@example.com")
	chatCreate(t, h, other, map[string]any{"model": "test-model", "title": "x"})
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/users/"+u.ID+"/purge", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge of an active account: %d", rec.Code)
	}
	_ = h.doAsSession(admin, http.MethodDelete, "/api/v1/admin/users/"+u.ID, nil)
	rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/users/"+u.ID+"/purge", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content_purged_at"`) {
		t.Fatalf("purge now: %d %s", rec.Code, rec.Body.String())
	}
	if countRows(t, h, "chat_conversation", u.ID) != 0 {
		t.Fatal("conversation kept")
	}
}

// TestSelfDeletedChatPurgedAfter30Days: a conversation the person deleted
// is removed for good (messages, files) 30 days later, not before.
func TestSelfDeletedChatPurgedAfter30Days(t *testing.T) {
	h, sess := chatHarness(t)
	ctx := context.Background()
	conv := chatCreate(t, h, sess, map[string]any{"model": "test-model"})
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "forget me"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+conv, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete chat: %d", rec.Code)
	}
	msgs := func() int {
		var n int
		_ = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_message WHERE conversation_id = ?`, conv).Scan(&n)
		return n
	}
	h.server.PurgeHeldContent(ctx, time.Now())
	if msgs() == 0 {
		t.Fatal("deleted chat purged before 30 days")
	}
	backdate(t, h, "chat_conversation", "deleted_at", conv, 31*24*time.Hour)
	h.server.PurgeHeldContent(ctx, time.Now())
	if n := msgs(); n != 0 {
		t.Fatalf("%d messages of a chat deleted 31 days ago remain", n)
	}
	var convs int
	_ = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_conversation WHERE id = ?`, conv).Scan(&convs)
	if convs != 0 {
		t.Fatal("conversation row kept")
	}
}

// TestSubscriptionCheckSkipsDisabledAccounts: the background check never
// renews a disabled or deleted person's vendor tokens.
func TestSubscriptionCheckSkipsDisabledAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-gone", "gone@example.com", "Gone", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertSubscription(ctx, store.SubscriptionUpsert{UserID: u.ID, Provider: "mistral", AccountSubject: "1", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkUserDeleted(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	conns, err := h.store.ListCheckableSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.UserID == u.ID {
			t.Fatal("a deleted person's subscription is still checked and renewed")
		}
	}
}

// The People list shows a deleted person's hold end.
func TestAdminUsersListShowsHold(t *testing.T) {
	h, admin := chatHarness(t)
	_ = h.sessionFor(t, "listed@example.com")
	u, _ := h.store.UserByAuthID(context.Background(), "sub-listed@example.com")
	_ = h.doAsSession(admin, http.MethodDelete, "/api/v1/admin/users/"+u.ID, nil)
	rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/users?search=listed", nil)
	if !strings.Contains(rec.Body.String(), `"held_until"`) {
		t.Fatalf("list lacks held_until: %s", rec.Body.String())
	}
}
