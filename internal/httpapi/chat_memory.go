package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// Janus Chat memory: what the assistant remembers about a person across
// chats. Owner-only (admins cannot read it, like chats). The person can
// turn memory off, list, add, edit and delete memories, or clear them all.

type chatMemoryView struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	FromChat    string `json:"from_conversation_id,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	FromChatTtl string `json:"from_conversation_title,omitempty"`
}

func (s *Server) chatMemoryDoc(r *http.Request, userID string) (map[string]any, error) {
	ctx := r.Context()
	off, err := s.Store.ChatMemoryOff(ctx, userID)
	if err != nil {
		return nil, err
	}
	mems, err := s.Store.ChatMemories(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]chatMemoryView, 0, len(mems))
	titles := map[string]string{}
	for _, m := range mems {
		v := chatMemoryView{ID: m.ID, Content: s.chatDecrypt(m.ContentEnc), FromChat: m.SourceConversationID,
			CreatedAt: store.FormatTime(m.CreatedAt), UpdatedAt: store.FormatTime(m.UpdatedAt)}
		if id := m.SourceConversationID; id != "" {
			if _, seen := titles[id]; !seen {
				titles[id] = ""
				if c, err := s.Store.ChatConversation(ctx, userID, id); err == nil {
					titles[id] = c.Title
				}
			}
			v.FromChatTtl = titles[id]
		}
		out = append(out, v)
	}
	adminOff := false
	for _, t := range s.chatAdmin(ctx).ToolsOff {
		if t == store.ChatToolMemory {
			adminOff = true
		}
	}
	return map[string]any{"enabled": !off, "available": !adminOff, "memories": out, "limit": store.ChatMaxMemories}, nil
}

func (s *Server) handleChatMemoryList(w http.ResponseWriter, r *http.Request) {
	doc, err := s.chatMemoryDoc(r, UserFrom(r.Context()).ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// handleChatMemorySettings turns memory on or off for the person.
func (s *Server) handleChatMemorySettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Enabled == nil {
		WriteError(w, r, ErrInvalidRequest("Say whether memory is on (enabled).").WithParam("enabled"))
		return
	}
	user := UserFrom(r.Context())
	if err := s.Store.SetChatMemoryOff(r.Context(), user.ID, !*body.Enabled); err != nil {
		WriteError(w, r, err)
		return
	}
	s.handleChatMemoryList(w, r)
}

func chatMemoryText(v string) (string, *APIError) {
	v = strings.TrimSpace(strings.Join(strings.Fields(v), " "))
	if v == "" {
		return "", ErrInvalidRequest("Write what to remember.").WithParam("content")
	}
	if len(v) > store.ChatMaxMemoryChars {
		return "", ErrInvalidRequest("A memory can be at most 500 characters.").WithParam("content")
	}
	return v, nil
}

func (s *Server) handleChatMemoryCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	text, apiErr := chatMemoryText(body.Content)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	enc, err := s.chatEncrypt(text)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	user := UserFrom(r.Context())
	m := &store.ChatMemory{UserID: user.ID, ContentEnc: enc}
	if err := s.Store.InsertChatMemory(r.Context(), m); err != nil {
		if errors.Is(err, store.ErrChatMemoryFull) {
			WriteError(w, r, ErrInvalidRequest("Memory is full (200). Delete some memories first."))
			return
		}
		WriteError(w, r, err)
		return
	}
	s.handleChatMemoryList(w, r)
}

func (s *Server) handleChatMemoryUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	text, apiErr := chatMemoryText(body.Content)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	enc, err := s.chatEncrypt(text)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	user := UserFrom(r.Context())
	if err := s.Store.UpdateChatMemory(r.Context(), user.ID, chi.URLParam(r, "id"), enc); err != nil {
		WriteError(w, r, chatMemoryNotFound(err))
		return
	}
	s.handleChatMemoryList(w, r)
}

func (s *Server) handleChatMemoryDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if err := s.Store.DeleteChatMemory(r.Context(), user.ID, chi.URLParam(r, "id")); err != nil {
		WriteError(w, r, chatMemoryNotFound(err))
		return
	}
	s.handleChatMemoryList(w, r)
}

func (s *Server) handleChatMemoryDeleteAll(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if err := s.Store.DeleteAllChatMemories(r.Context(), user.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.handleChatMemoryList(w, r)
}

func chatMemoryNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFoundf("memory")
	}
	return err
}
