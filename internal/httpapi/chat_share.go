package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// Janus Chat share links. A link is a read-only snapshot of the current
// branch, taken when it is made: later messages are not shared, and
// revoking or deleting the conversation ends it. Viewing needs a signed-in
// session on this Janus (sharing stays inside the instance; links outside
// it are Business, not built here). Cost and token details are left out
// of the snapshot: they say what the owner pays, not what was said.

const chatMaxSharesPerConversation = 20

type chatShareView struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	MessageCount int    `json:"message_count"`
	Views        int    `json:"views"`
	CreatedAt    string `json:"created_at"`
}

func (s *Server) chatShareView(sh *store.ChatShare) chatShareView {
	return chatShareView{ID: sh.ID, URL: "/chat/shared/" + sh.ID, Title: sh.Title, MessageCount: sh.MessageCount,
		Views: sh.Views, CreatedAt: sh.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")}
}

func newShareToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) handleChatShareCreate(w http.ResponseWriter, r *http.Request) {
	if !s.chatAdmin(r.Context()).SharesEnabled {
		WriteError(w, r, ErrForbidden("Share links are turned off for Janus Chat on this instance."))
		return
	}
	ctx := r.Context()
	user := UserFrom(ctx)
	c, err := s.Store.ChatConversation(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	if c.Temporary {
		WriteError(w, r, ErrInvalidRequest("Temporary chats cannot be shared"))
		return
	}
	existing, err := s.Store.ListChatShares(ctx, user.ID, c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(existing) >= chatMaxSharesPerConversation {
		WriteError(w, r, ErrInvalidRequest("This chat already has 20 links. Revoke one to make another."))
		return
	}
	snap, err := s.chatBuildExport(ctx, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(snap.Messages) == 0 {
		WriteError(w, r, ErrInvalidRequest("There is nothing to share yet"))
		return
	}
	snap.Instructions = "" // private to the owner
	snap.CostUSD = 0
	for i := range snap.Messages {
		snap.Messages[i].TokensIn, snap.Messages[i].TokensOut, snap.Messages[i].CostUSD = 0, 0, 0
	}
	raw, _ := json.Marshal(snap)
	enc, err := s.chatEncrypt(string(raw))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := newShareToken()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	sh := &store.ChatShare{ID: id, UserID: user.ID, ConversationID: c.ID, Title: snap.Title, SnapshotEnc: enc, MessageCount: len(snap.Messages)}
	if err := s.Store.CreateChatShare(ctx, sh); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "chat_share_created", "chat_share", sh.ID, nil, map[string]any{"conversation_id": c.ID, "messages": sh.MessageCount})
	WriteJSON(w, http.StatusCreated, s.chatShareView(sh))
}

func (s *Server) handleChatShareList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	c, err := s.Store.ChatConversation(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	list, err := s.Store.ListChatShares(ctx, user.ID, c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := make([]chatShareView, 0, len(list))
	for _, sh := range list {
		out = append(out, s.chatShareView(sh))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"shares": out})
}

func (s *Server) handleChatShareRevoke(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	ok, err := s.Store.RevokeChatShare(r.Context(), user.ID, chi.URLParam(r, "share"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !ok {
		WriteError(w, r, ErrNotFoundf("share link"))
		return
	}
	s.audit(r, "chat_share_revoked", "chat_share", chi.URLParam(r, "share"), nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleChatShareGet serves a snapshot to any signed-in user. The owner
// is named by display name only.
func (s *Server) handleChatShareGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.Store.ChatShareByID(ctx, chi.URLParam(r, "share"))
	if err != nil {
		WriteError(w, r, ErrNotFoundf("shared chat"))
		return
	}
	var snap chatExport
	if json.Unmarshal([]byte(s.chatDecrypt(sh.SnapshotEnc)), &snap) != nil {
		WriteError(w, r, ErrNotFoundf("shared chat"))
		return
	}
	// A disabled or deleted person's links stop working at once (and work
	// again if the account is restored).
	u, err := s.Store.UserByID(ctx, sh.UserID)
	if err != nil || !u.IsActive {
		WriteError(w, r, ErrNotFoundf("shared chat"))
		return
	}
	owner := u.Name
	viewer := UserFrom(ctx)
	if viewer == nil || viewer.ID != sh.UserID {
		_ = s.Store.CountChatShareView(ctx, sh.ID)
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, map[string]any{
		"title": snap.Title, "shared_by": owner, "shared_at": sh.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"messages": snap.Messages, "own": viewer != nil && viewer.ID == sh.UserID,
	})
}

// Share routes that stay open to a person outside the Chat access
// restriction (Administration → Chat → Access): reading a link someone
// shared is a read-only view, not using Chat, and an owner who lost Chat
// access must still be able to list and revoke the links they made.
// Making a new link still needs Chat access.
var (
	chatSharedViewPath = regexp.MustCompile(`/chat/shared/[^/]+/?$`)
	chatOwnSharesPath  = regexp.MustCompile(`/chat/conversations/[^/]+/shares(/[^/]+)?/?$`)
)

func chatShareRouteOpen(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return chatSharedViewPath.MatchString(r.URL.Path) || chatOwnSharesPath.MatchString(r.URL.Path)
	case http.MethodDelete:
		return chatOwnSharesPath.MatchString(r.URL.Path)
	}
	return false
}
