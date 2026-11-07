package httpapi

import (
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/headerpolicy"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Header policy administration. Every write flushes the config cache so the
// proxy applies it at once on this replica; others converge within the TTL.

func (s *Server) mountHeaderPolicyRoutes(r chi.Router) {
	r.Get("/admin/header-policies", s.handleListHeaderPolicies)
	r.Post("/admin/header-policies", s.gateCreate("header policy", s.handleCreateHeaderPolicy))
	r.Get("/admin/header-policies/{id}", s.handleGetHeaderPolicy)
	r.Put("/admin/header-policies/{id}", s.handleUpdateHeaderPolicy)
	r.Delete("/admin/header-policies/{id}", s.handleDeleteHeaderPolicy)
	r.Put("/admin/header-policies/assignments", s.handleSetHeaderAssignment)
	r.Get("/admin/header-policies/settings", s.handleGetHeaderLogSettings)
	r.Put("/admin/header-policies/settings", s.handlePutHeaderLogSettings)
	r.Get("/admin/requests/{id}/headers", s.handleRequestHeaders)
}

// headerTarget is one thing a policy can be assigned to.
type headerTarget struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	// PolicyID is the target's own assignment ("" = inherits the default).
	PolicyID string `json:"policy_id"`
	// EffectivePolicyID is what applies today, inheritance resolved.
	EffectivePolicyID string `json:"effective_policy_id"`
}

func (s *Server) handleListHeaderPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	policies, err := s.Store.ListHeaderPolicies(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	assigns, err := s.Store.ListHeaderAssignments(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	own := map[string]string{}
	for _, a := range assigns {
		own[a.TargetType+"\x00"+a.TargetID] = a.PolicyID
	}
	def := own[store.HeaderTargetDefault+"\x00"]
	eff := func(t, id string) (string, string) {
		if p, ok := own[t+"\x00"+id]; ok {
			return p, p
		}
		return "", def
	}
	targets := []headerTarget{{TargetType: store.HeaderTargetDefault, Name: "Default (all upstreams)", PolicyID: def, EffectivePolicyID: def}}
	ups, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].Name < ups[j].Name })
	for _, u := range ups {
		p, e := eff(store.HeaderTargetUpstream, u.ID)
		targets = append(targets, headerTarget{TargetType: store.HeaderTargetUpstream, TargetID: u.ID, Name: u.Name, Detail: u.BaseURL, PolicyID: p, EffectivePolicyID: e})
	}
	for _, prov := range subscription.All() {
		p, e := eff(store.HeaderTargetPersonal, prov.ID())
		targets = append(targets, headerTarget{TargetType: store.HeaderTargetPersonal, TargetID: prov.ID(), Name: prov.DisplayName(), Detail: "Personal subscription", PolicyID: p, EffectivePolicyID: e})
	}
	settings, err := s.Store.HeaderLogSettings(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"policies":     policies,
		"targets":      targets,
		"settings":     settings,
		"edge_headers": headerpolicy.EdgeHeaders,
	})
}

func (s *Server) handleGetHeaderPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.HeaderPolicyByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
}

func ruleSummary(p *store.HeaderPolicy) map[string]any {
	return map[string]any{"name": p.Name, "rules": len(p.Rules), "allow_only": p.AllowOnly}
}

func (s *Server) handleCreateHeaderPolicy(w http.ResponseWriter, r *http.Request) {
	var p store.HeaderPolicy
	if err := decodeJSON(r, &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if u := UserFrom(r.Context()); u != nil {
		p.CreatedBy = u.ID
	}
	if err := s.Store.CreateHeaderPolicy(r.Context(), &p, s.headerCipher()); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "header_policy_created", "header_policy", p.ID, nil, ruleSummary(&p))
	out, err := s.Store.HeaderPolicyByID(r.Context(), p.ID)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"policy": out})
}

func (s *Server) handleUpdateHeaderPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.HeaderPolicyByID(r.Context(), id)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	var p store.HeaderPolicy
	if err := decodeJSON(r, &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	p.ID = id
	if err := s.Store.UpdateHeaderPolicy(r.Context(), &p, s.headerCipher()); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "header_policy_updated", "header_policy", id, ruleSummary(existing), ruleSummary(&p))
	out, err := s.Store.HeaderPolicyByID(r.Context(), id)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"policy": out})
}

func (s *Server) handleDeleteHeaderPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.HeaderPolicyByID(r.Context(), id)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.DeleteHeaderPolicy(r.Context(), id); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "header_policy_deleted", "header_policy", id, ruleSummary(existing), nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleSetHeaderAssignment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		PolicyID   string `json:"policy_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if body.TargetType == store.HeaderTargetPersonal {
		if _, ok := subscription.Get(body.TargetID); !ok {
			writeSecgwError(w, r, &store.ValidationError{Field: "target_id", Message: "Unknown personal-subscription provider."})
			return
		}
	}
	by := ""
	if u := UserFrom(r.Context()); u != nil {
		by = u.ID
	}
	if err := s.Store.SetHeaderAssignment(r.Context(), body.TargetType, body.TargetID, body.PolicyID, by); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "header_policy_assigned", "header_policy_assignment", body.TargetType+":"+body.TargetID, nil, map[string]any{"policy_id": body.PolicyID})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGetHeaderLogSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.HeaderLogSettings(r.Context())
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"settings": v})
}

func (s *Server) handlePutHeaderLogSettings(w http.ResponseWriter, r *http.Request) {
	old, _ := s.Store.HeaderLogSettings(r.Context())
	var v store.HeaderLogSettings
	if err := decodeJSON(r, &v); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.SetHeaderLogSettings(r.Context(), v); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "header_logging_updated", "system_setting", store.SettingHeaderLogging, old, v)
	WriteJSON(w, http.StatusOK, map[string]any{"settings": v})
}

// handleRequestHeaders serves GET /admin/requests/{id}/headers.
func (s *Server) handleRequestHeaders(w http.ResponseWriter, r *http.Request) {
	l, err := s.Store.RequestHeaderLogByUsageID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteJSON(w, http.StatusOK, map[string]any{"headers": nil})
		return
	}
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"headers": l})
}

// headerCipher is the encryption used for secret header values (nil when the
// instance has no JANUS_ENCRYPTION_KEY; secret values are then refused).
func (s *Server) headerCipher() store.SecgwCipher {
	if s.Cipher == nil {
		return nil
	}
	return s.Cipher
}
