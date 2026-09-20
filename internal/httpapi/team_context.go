package httpapi

import (
	"context"
	"errors"
	"github.com/torvanis/janus/internal/store"
	"net/http"
)

// selectedTeamID returns the immutable key binding or this browser's selection.
func (s *Server) selectedTeamID(ctx context.Context, user *store.User) (string, error) {
	if token := TokenFrom(ctx); token != nil {
		return token.TeamID, nil
	}
	if session := SessionFrom(ctx); session != nil {
		return s.Store.SessionTeam(ctx, session.ID, user.ID)
	}
	return "", nil
}

func (s *Server) handleSetActiveTeam(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	session := SessionFrom(r.Context())
	if session == nil {
		WriteError(w, r, ErrForbidden("API keys use their own team affinity. Sign in and use Change team on the Tokens page."))
		return
	}
	var body struct {
		TeamID string `json:"team_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.SetSessionTeam(r.Context(), session.ID, user.ID, body.TeamID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "active_team_changed", "team", body.TeamID, nil, nil)
	WriteJSON(w, 200, map[string]any{"active_team_id": body.TeamID})
}

func teamContextError(err error) error {
	var validation *store.ValidationError
	if errors.As(err, &validation) {
		return ErrForbidden(validation.Message)
	}
	return err
}
