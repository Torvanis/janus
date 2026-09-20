package httpapi

import (
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
	"net/http"
)

// Changing billing affinity is an account-management action, not a capability
// granted to an inference credential (including the key being changed).
func (s *Server) handleChangeTokenTeam(w http.ResponseWriter, r *http.Request) {
	if SessionFrom(r.Context()) == nil || TokenFrom(r.Context()) != nil {
		WriteError(w, r, ErrForbidden("Sign in to Janus to change a token's team affinity."))
		return
	}
	var body struct {
		TeamID      *string `json:"team_id"`
		MoveHistory *bool   `json:"move_history"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.TeamID == nil || body.MoveHistory == nil {
		WriteError(w, r, ErrInvalidRequest("team_id and an explicit move_history choice are required."))
		return
	}
	token, moved, err := s.Store.ChangeTokenTeam(r.Context(), chi.URLParam(r, "id"), UserFrom(r.Context()).ID, *body.TeamID, *body.MoveHistory)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateTokenCache()
	WriteJSON(w, http.StatusOK, map[string]any{"token": token, "reattributed_requests": moved})
}
