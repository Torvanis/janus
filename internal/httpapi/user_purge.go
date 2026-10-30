package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// userPurgeBatch bounds one pass of the deleted-content purge.
const userPurgeBatch = 200

// PurgeHeldContent is the retention purge's content step (run by the daily
// purge job): deleted accounts whose 30-day hold has ended lose their
// content, and conversations people deleted themselves more than 30 days
// ago are removed for good. Returns the number of accounts plus
// conversations purged.
func (s *Server) PurgeHeldContent(ctx context.Context, now time.Time) int64 {
	var n int64
	ids, err := s.Store.HeldUsersDue(ctx, now, userPurgeBatch)
	if err != nil {
		s.Logger.WarnContext(ctx, "list deleted accounts to purge", "error", err.Error())
	}
	for _, id := range ids {
		if err := s.purgeUserContent(ctx, id); err != nil {
			s.Logger.WarnContext(ctx, "purge deleted account content", "error", err.Error(), "user_id", id)
			continue
		}
		n++
	}
	n += int64(s.chatPurgeDeleted(ctx, now))
	return n
}

// purgeUserContent removes a deleted person's files from storage, revokes
// their personal subscriptions at the vendor, then deletes the rows.
func (s *Server) purgeUserContent(ctx context.Context, userID string) error {
	u, err := s.Store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.IsActive {
		return nil
	}
	if atts, err := s.Store.ChatAttachmentsForUser(ctx, userID); err == nil && len(atts) > 0 {
		s.chatDeleteAttachments(ctx, atts)
	}
	if conns, err := s.Store.ListUserSubscriptions(ctx, userID); err == nil {
		for _, c := range conns {
			full, err := s.Store.SubscriptionByID(ctx, c.ID)
			if err != nil {
				continue
			}
			s.revokeSubscriptionAtVendor(ctx, full)
		}
	}
	return s.Store.PurgeUserContent(ctx, userID)
}

// revokeSubscriptionAtVendor revokes a connection's refresh token at the
// vendor (best effort, bounded); the caller deletes the row.
func (s *Server) revokeSubscriptionAtVendor(ctx context.Context, conn *store.SubscriptionConnection) {
	provider, ok := subscription.Get(conn.Provider)
	if !ok || s.Cipher == nil {
		return
	}
	refresh, err := s.Cipher.Decrypt(conn.EncryptedRefreshToken())
	if err != nil || refresh == "" {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := provider.Revoke(rctx, s.upstreamHTTPClient(ctx), refresh); err != nil {
		s.Logger.WarnContext(ctx, "revoke subscription at vendor", "subscription_id", conn.ID, "provider", conn.Provider, "error", err.Error())
	}
}

// chatPurgeDeleted removes conversations their owner deleted more than
// store.ChatDeletePurgeAfter ago (files first, then rows).
func (s *Server) chatPurgeDeleted(ctx context.Context, now time.Time) int {
	stale, err := s.Store.StaleDeletedChats(ctx, now.Add(-store.ChatDeletePurgeAfter), chatSweepBatch)
	if err != nil {
		s.Logger.WarnContext(ctx, "list deleted chats to purge", "error", err.Error())
		return 0
	}
	n := 0
	for _, c := range stale {
		if atts, err := s.Store.ChatAttachmentsForConversation(ctx, c.UserID, c.ID); err == nil && len(atts) > 0 {
			s.chatDeleteAttachments(ctx, atts)
		}
		if err := s.Store.PurgeChatConversation(ctx, c.UserID, c.ID); err != nil {
			s.Logger.WarnContext(ctx, "purge deleted chat", "error", err.Error(), "conversation_id", c.ID)
			continue
		}
		n++
	}
	return n
}

// handlePurgeUserContent deletes a deleted person's held content now,
// instead of waiting for the end of the hold.
func (s *Server) handlePurgeUserContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	user, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That user"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if user.IsActive || user.DeletedAt == nil {
		WriteError(w, r, ErrInvalidRequest("Only a deleted person’s content can be deleted. Delete the person first."))
		return
	}
	if err := s.purgeUserContent(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "user_content_purged", "user", id, map[string]any{"email": user.Email}, nil)
	updated, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"user": updated})
}
