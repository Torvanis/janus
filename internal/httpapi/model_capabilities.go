package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// Model capabilities for administrators: what is known about one model,
// which source said it, and the override.

func (s *Server) mountModelCapabilityRoutes(r chi.Router) {
	r.Get("/admin/models/{id}/capabilities", s.handleGetModelCapabilities)
	r.Put("/admin/models/{id}/capabilities", s.handlePutModelCapabilities)
}

// modelCapabilityView is one model's capabilities as the editor needs them:
// every key (so unknown ones are listed too), the value in force, and what
// each source said.
func modelCapabilityView(m *store.Model) map[string]any {
	state := m.CapabilityState()
	return map[string]any{
		"model_id": m.ID,
		"keys":     adapter.CapabilityKeys(),
		"resolved": state.Resolve(),
		"provider": emptyFacts(state.Provider),
		"probe":    emptyFacts(state.Probe),
		"admin":    emptyFacts(state.Admin),
		"probed":   state.Probed,
	}
}

func emptyFacts(f capability.Facts) capability.Facts {
	if f == nil {
		return capability.Facts{}
	}
	return f
}

func (s *Server) handleGetModelCapabilities(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.ModelByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, modelCapabilityView(m))
}

// handlePutModelCapabilities sets or clears overrides: {"overrides":
// {"input.image": true, "thinking.levels": ["low", "high"],
// "request.tools": null}}. A value overrides what the provider said and what
// a probe measured; null removes the override.
func (s *Server) handlePutModelCapabilities(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ModelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Overrides map[string]any `json:"overrides"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(body.Overrides) == 0 {
		WriteError(w, r, ErrInvalidRequest("overrides is required: capability keys with a value to set, or null to clear.").WithParam("overrides"))
		return
	}
	var invalid error
	after, err := s.Store.UpdateModelCapabilities(r.Context(), id, func(state *capability.State) (bool, error) {
		if invalid = state.SetAdmin(body.Overrides, time.Now().UTC()); invalid != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if invalid != nil {
		WriteError(w, r, ErrInvalidRequest(invalid.Error()).WithParam("overrides"))
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "model_capabilities_updated", "model", id,
		map[string]any{"model": before.Name, "overrides": adminValues(before.CapabilityState())},
		map[string]any{"model": after.Name, "overrides": adminValues(after.CapabilityState())})
	WriteJSON(w, http.StatusOK, modelCapabilityView(after))
}

func adminValues(state capability.State) map[string]any {
	out := map[string]any{}
	for k, f := range state.Admin {
		out[k] = f.Value
	}
	return out
}
