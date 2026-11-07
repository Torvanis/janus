package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// Saved prompts for Janus Chat (the user's own library, "/" commands).

const chatMaxPromptChars = 20000

// chatPromptName is the slash command: lowercase letters, digits and
// dashes, 1–40 characters, starting with a letter or digit.
var chatPromptName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type chatPromptView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
}

type chatPromptBody struct {
	Name  *string `json:"name"`
	Title *string `json:"title"`
	Body  *string `json:"body"`
}

func (s *Server) chatPromptView(p *store.ChatPrompt) chatPromptView {
	return chatPromptView{ID: p.ID, Name: p.Name, Title: p.Title, Body: s.chatDecrypt(p.BodyEnc), UpdatedAt: p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")}
}

func (s *Server) applyChatPromptBody(p *store.ChatPrompt, body chatPromptBody) *APIError {
	if body.Name != nil {
		n := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(*body.Name), "/")))
		if !chatPromptName.MatchString(n) {
			return ErrInvalidRequest("A command name is 1–40 lowercase letters, digits or dashes, like summarize or email-reply").WithParam("name")
		}
		p.Name = n
	}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		if len([]rune(t)) > 120 {
			return ErrInvalidRequest("A title can be at most 120 characters").WithParam("title")
		}
		p.Title = t
	}
	if body.Body != nil {
		v := strings.TrimSpace(*body.Body)
		if v == "" {
			return ErrInvalidRequest("Write the prompt text first").WithParam("body")
		}
		if len(v) > chatMaxPromptChars {
			return ErrInvalidRequest("A prompt can be at most 20,000 characters").WithParam("body")
		}
		enc, err := s.chatEncrypt(v)
		if err != nil {
			return ErrInternal()
		}
		p.BodyEnc = enc
	}
	return nil
}

func chatPromptStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrChatPromptNameTaken):
		return ErrConflict("You already have a prompt with that command name").WithParam("name")
	case errors.Is(err, store.ErrChatPromptLimit):
		return ErrInvalidRequest("Your prompt library is full (200 prompts). Delete one to add another.")
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFoundf("prompt")
	}
	return err
}

func (s *Server) handleChatPromptList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	list, err := s.Store.ListChatPrompts(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := make([]chatPromptView, 0, len(list))
	for _, p := range list {
		out = append(out, s.chatPromptView(p))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"prompts": out})
}

func (s *Server) handleChatPromptCreate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body chatPromptBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Name == nil || body.Body == nil {
		WriteError(w, r, ErrInvalidRequest("A prompt needs a command name and text"))
		return
	}
	p := &store.ChatPrompt{UserID: user.ID}
	if apiErr := s.applyChatPromptBody(p, body); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.CreateChatPrompt(r.Context(), p); err != nil {
		WriteError(w, r, chatPromptStoreError(err))
		return
	}
	WriteJSON(w, http.StatusCreated, s.chatPromptView(p))
}

func (s *Server) handleChatPromptUpdate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	p, err := s.Store.ChatPrompt(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatPromptStoreError(err))
		return
	}
	var body chatPromptBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if apiErr := s.applyChatPromptBody(p, body); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.UpdateChatPrompt(r.Context(), p); err != nil {
		WriteError(w, r, chatPromptStoreError(err))
		return
	}
	WriteJSON(w, http.StatusOK, s.chatPromptView(p))
}

func (s *Server) handleChatPromptDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if _, err := s.Store.ChatPrompt(r.Context(), user.ID, chi.URLParam(r, "id")); err != nil {
		WriteError(w, r, chatPromptStoreError(err))
		return
	}
	if err := s.Store.DeleteChatPrompt(r.Context(), user.ID, chi.URLParam(r, "id")); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
