package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
)

// Mount under RequireUser; every request also refreshes administrator status.
func (s *Server) mountReportClassificationRoutes(r chi.Router) {
	r.Get("/reports/classifications", s.reportClassifications)
	r.Put("/reports/classifications/{id}", s.reportClassifications)
}

func (s *Server) reportClassifications(w http.ResponseWriter, r *http.Request) {
	actor := s.reportActor(w, r)
	if actor == nil {
		return
	}
	if !actor.IsAdmin() {
		WriteError(w, r, ErrForbidden("Only administrators may manage model reporting classifications."))
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodGet {
		values, err := s.Store.ListReportModelClassifications(ctx)
		if err != nil {
			reportStoreError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"classifications": values})
		return
	}
	var body *struct {
		Family   string `json:"family"`
		Provider string `json:"provider"`
		Hosting  string `json:"hosting"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body == nil {
		WriteError(w, r, ErrInvalidRequest("A classification object is required."))
		return
	}
	id := chi.URLParam(r, "id")
	before, err := s.Store.GetReportModelClassification(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		reportStoreError(w, err)
		return
	}
	v := store.ReportModelClassification{ModelID: id, Family: body.Family, Provider: body.Provider, Hosting: body.Hosting}
	if err := s.Store.SaveReportModelClassification(ctx, &v); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidReportClassification):
			WriteError(w, r, ErrInvalidRequest(err.Error()))
		case errors.Is(err, store.ErrNotFound):
			reportHTTPError(w, http.StatusNotFound, "Model not found")
		default:
			reportStoreError(w, err)
		}
		return
	}
	s.audit(r, "report.classification.save", "model", id, before, v)
	WriteJSON(w, http.StatusOK, map[string]any{"classification": v})
}
