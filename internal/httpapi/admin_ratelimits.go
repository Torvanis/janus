package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// Rate-limit rule administration. Rules cap requests per minute per person and
// endpoint; configuration is always available, enforcement is gated by the
// per_user_rate_limits_enabled feature flag. Feature flags themselves
// are defined in store.DefaultFeatureFlags and managed by the handlers in
// admin_handlers.go.

func (s *Server) handleListRateLimits(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Store.ListRateLimitRules(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	flags, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"rules":               rules,
		"enforcement_enabled": flags["per_user_rate_limits_enabled"],
	})
}

type rateLimitPayload struct {
	SubjectID         string `json:"subject_id"`
	Endpoint          string `json:"endpoint"`
	RequestsPerMinute int    `json:"requests_per_minute"`
}

func (s *Server) handleCreateRateLimit(w http.ResponseWriter, r *http.Request) {
	var body rateLimitPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.RequestsPerMinute < 1 || body.RequestsPerMinute > 1_000_000 {
		WriteError(w, r, ErrInvalidRequest("requests_per_minute must be between 1 and 1000000.").WithParam("requests_per_minute"))
		return
	}
	endpoint := strings.TrimSpace(body.Endpoint)
	if endpoint == "" {
		endpoint = "*"
	}
	if endpoint != "*" && !strings.HasPrefix(endpoint, "/v1/") {
		WriteError(w, r, ErrInvalidRequest("endpoint must be * or a proxied path starting with /v1/, for example /v1/chat/completions or /v1/audio/*.").WithParam("endpoint"))
		return
	}
	if body.SubjectID != "" {
		if _, err := s.Store.UserByID(r.Context(), body.SubjectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, r, ErrInvalidRequest("Choose an existing person, or leave the person empty to apply the limit to everyone.").WithParam("subject_id"))
				return
			}
			WriteError(w, r, err)
			return
		}
	}
	rule := &store.RateLimitRule{
		SubjectType: "user", SubjectID: body.SubjectID,
		Endpoint: endpoint, RequestsPerMinute: body.RequestsPerMinute,
	}
	if err := s.Store.CreateRateLimitRule(r.Context(), rule); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rate_limit_created", "rate_limit_rule", rule.ID, nil, map[string]any{
		"subject_id": rule.SubjectID, "endpoint": rule.Endpoint, "requests_per_minute": rule.RequestsPerMinute,
	})
	WriteJSON(w, http.StatusCreated, map[string]any{"rule": rule})
}

// handleUpdateRateLimit changes the cap on an existing rule. Only the
// requests-per-minute value is editable — subject and endpoint identify the
// rule; changing those is a different rule, so it stays create+delete.
func (s *Server) handleUpdateRateLimit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.RateLimitRuleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("The rate limit rule"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body rateLimitPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.RequestsPerMinute < 1 || body.RequestsPerMinute > 1_000_000 {
		WriteError(w, r, ErrInvalidRequest("requests_per_minute must be between 1 and 1000000.").WithParam("requests_per_minute"))
		return
	}
	if err := s.Store.UpdateRateLimitRule(r.Context(), id, body.RequestsPerMinute); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rate_limit_updated", "rate_limit_rule", id,
		map[string]any{"subject_id": existing.SubjectID, "endpoint": existing.Endpoint, "requests_per_minute": existing.RequestsPerMinute},
		map[string]any{"subject_id": existing.SubjectID, "endpoint": existing.Endpoint, "requests_per_minute": body.RequestsPerMinute})
	existing.RequestsPerMinute = body.RequestsPerMinute
	WriteJSON(w, http.StatusOK, map[string]any{"rule": existing})
}

func (s *Server) handleDeleteRateLimit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rule, err := s.Store.RateLimitRuleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("The rate limit rule"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DeleteRateLimitRule(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rate_limit_deleted", "rate_limit_rule", id, map[string]any{
		"subject_id": rule.SubjectID, "endpoint": rule.Endpoint, "requests_per_minute": rule.RequestsPerMinute,
	}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
