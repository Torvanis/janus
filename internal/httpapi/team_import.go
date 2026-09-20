package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/torvanis/janus/internal/store"
)

// readTeamImportRequest enforces authorization even when called without route
// middleware. Bound the entire JSON body, including trailing whitespace, before
// using the shared decoder (which intentionally tolerates trailing input).
func readTeamImportRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	if !UserFrom(r.Context()).IsAdmin() {
		return "", ErrForbidden("This action requires the administrator role.")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxTeamImportBytes))
	if err != nil {
		return "", ErrInvalidRequest("The import request must be no larger than 2 MiB.")
	}
	if !json.Valid(body) {
		return "", ErrInvalidRequest("The request body must contain one valid JSON object.")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var request struct {
		CSV string `json:"csv"`
	}
	if err := decodeJSON(r, &request); err != nil {
		return "", err
	}
	return request.CSV, nil
}

func (s *Server) handleTeamImportPreview(w http.ResponseWriter, r *http.Request) {
	text, err := readTeamImportRequest(w, r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	preview, err := s.Store.PreviewTeamImport(r.Context(), text)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("Could not preview the CSV: "+err.Error()))
		return
	}
	WriteJSON(w, http.StatusOK, preview)
}

func (s *Server) handleTeamImportApply(w http.ResponseWriter, r *http.Request) {
	text, err := readTeamImportRequest(w, r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teams, err := s.Store.ImportTeamsCSV(r.Context(), text)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("Could not import teams. Preview the CSV and correct any errors before retrying."))
		return
	}
	// Audit only the count and created resource IDs, never the CSV or emails.
	ids := make([]string, 0, len(teams))
	for _, team := range teams {
		ids = append(ids, team.ID)
	}
	s.audit(r, "teams_imported", "team", "", nil, map[string]any{"created": len(teams), "team_ids": ids})
	WriteJSON(w, http.StatusCreated, map[string]any{"teams": teams, "created": len(teams)})
}
