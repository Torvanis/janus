package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// --- Service tokens ----------------------------------------------------------
//
// Service tokens are administered here and nowhere else: they are org-level
// credentials, not personal ones, so unlike /api/v1/tokens there is no
// self-service surface. Every mutation is audited.

// mountServiceTokenRoutes registers the admin service-token endpoints.
func (s *Server) mountServiceTokenRoutes(r chi.Router) {
	r.Get("/admin/service-tokens", s.handleListServiceTokens)
	r.Post("/admin/service-tokens", s.gateCreate("service token", s.handleCreateServiceToken))
	r.Get("/admin/service-tokens/{id}", s.handleServiceTokenDetail)
	r.Patch("/admin/service-tokens/{id}", s.handleUpdateServiceToken)
	r.Delete("/admin/service-tokens/{id}", s.handleRevokeServiceToken)
	r.Get("/admin/service-tokens/{id}/usage", s.handleServiceTokenUsage)
}

// handleListServiceTokens returns the admin service-token table.
//
// Rows carry the same 30-day usage aggregate the people table exposes
// (spend_30d_usd, requests_30d, tokens_in_30d, tokens_out_30d, plus an error
// count) because the operational questions are identical: which integration is
// burning budget, which one has gone quiet, which one is failing. A service
// token has no human owner to ask, so the table has to answer on its own.
//
// Query parameters:
//   - status:   active | revoked | expired (empty = all)
//   - search:   name/description substring
//   - sort:     name | created | last_used | expires | spend | requests |
//     tokens | errors | grants, each accepting an explicit _asc/_desc suffix.
//     Default is spend descending — the costliest integration first, which is
//     what an admin opening this page usually wants to see.
func (s *Server) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.Store.ListServiceTokens(r.Context(), store.ServiceTokenFilter{
		Status: r.URL.Query().Get("status"),
		Search: strings.TrimSpace(r.URL.Query().Get("search")),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	grants, err := s.Store.ListGrants(r.Context(), "")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// A token's reachable-model count is its own grants plus every
	// all_service_tokens grant, mirroring the resolver: an admin comparing
	// this column against the models list should see the same number the
	// gateway would enforce.
	perToken := map[string]int{}
	blanket := 0
	for _, g := range grants {
		switch g.GranteeType {
		case store.GranteeServiceToken:
			perToken[g.GranteeID]++
		case store.GranteeAllServiceTokens:
			blanket++
		}
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -30)
	rows := make([]map[string]any, 0, len(tokens))
	for _, tok := range tokens {
		totals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{
			ServiceTokenID: tok.ID, Start: start, End: now,
		})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		models, err := s.Store.BreakdownUsage(r.Context(), store.UsageScope{
			ServiceTokenID: tok.ID, Start: start, End: now,
		}, "model")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		// "Which model is this integration actually using" is the first
		// question after "is it expensive", so the top one rides along in
		// the row rather than requiring the detail page.
		topModel := ""
		if len(models) > 0 {
			topModel = models[0].Key
		}
		rows = append(rows, map[string]any{
			"id": tok.ID, "name": tok.Name, "description": tok.Description,
			"prefix": tok.Prefix, "status": tok.Status(now),
			"created_at": tok.CreatedAt, "last_used_at": tok.LastUsedAt,
			"expires_at": tok.ExpiresAt, "revoked_at": tok.RevokedAt,
			"grant_count":     perToken[tok.ID] + blanket,
			"spend_30d_usd":   usage.USD(totals.CostNano),
			"requests_30d":    totals.Requests,
			"tokens_in_30d":   totals.TokensIn,
			"tokens_out_30d":  totals.TokensOut,
			"errors_30d":      totals.ErrorCount,
			"top_model_30d":   topModel,
			"model_count_30d": len(models),
		})
	}
	sortServiceTokenRows(rows, r.URL.Query().Get("sort"))
	WriteJSON(w, http.StatusOK, map[string]any{"service_tokens": rows, "total_count": len(rows)})
}

// sortServiceTokenRows orders the assembled rows in memory. The usage columns
// are computed per row rather than stored, so they cannot be ordered in SQL
// without denormalising; the service-token population is small (an org has
// tens, not millions) so an in-memory sort is the honest trade.
func sortServiceTokenRows(rows []map[string]any, sortParam string) {
	dir := 1
	field := sortParam
	if strings.HasSuffix(sortParam, "_desc") {
		dir, field = -1, strings.TrimSuffix(sortParam, "_desc")
	} else if strings.HasSuffix(sortParam, "_asc") {
		field = strings.TrimSuffix(sortParam, "_asc")
	}
	if field == "" {
		// Default: costliest first. An admin lands here to find the
		// expensive or runaway integration.
		field, dir = "spend", -1
	}
	num := func(row map[string]any, key string) float64 {
		switch v := row[key].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		case int:
			return float64(v)
		}
		return 0
	}
	str := func(row map[string]any, key string) string {
		if v, ok := row[key].(string); ok {
			return strings.ToLower(v)
		}
		return ""
	}
	tim := func(row map[string]any, key string) int64 {
		if v, ok := row[key].(time.Time); ok {
			return v.Unix()
		}
		return 0
	}
	sort.SliceStable(rows, func(i, j int) bool {
		// Missing names/timestamps stay last; equal values retain source order.
		key := map[string]string{"name": "name", "created": "created_at", "last_used": "last_used_at", "expires": "expires_at"}[field]
		blank := func(row map[string]any) bool {
			if key == "" {
				return false
			}
			v := row[key]
			if v == nil || v == "" {
				return true
			}
			if tm, ok := v.(time.Time); ok {
				return tm.IsZero()
			}
			return false
		}
		if blank(rows[i]) {
			return false
		}
		if blank(rows[j]) {
			return true
		}
		if dir < 0 {
			i, j = j, i
		}
		var less bool
		switch field {
		case "name":
			less = str(rows[i], "name") < str(rows[j], "name")
		case "created":
			less = tim(rows[i], "created_at") < tim(rows[j], "created_at")
		case "last_used":
			less = tim(rows[i], "last_used_at") < tim(rows[j], "last_used_at")
		case "expires":
			less = tim(rows[i], "expires_at") < tim(rows[j], "expires_at")
		case "requests":
			less = num(rows[i], "requests_30d") < num(rows[j], "requests_30d")
		case "tokens":
			less = num(rows[i], "tokens_in_30d")+num(rows[i], "tokens_out_30d") <
				num(rows[j], "tokens_in_30d")+num(rows[j], "tokens_out_30d")
		case "errors":
			less = num(rows[i], "errors_30d") < num(rows[j], "errors_30d")
		case "grants":
			less = num(rows[i], "grant_count") < num(rows[j], "grant_count")
		default: // spend
			less = num(rows[i], "spend_30d_usd") < num(rows[j], "spend_30d_usd")
		}
		return less
	})
}

func (s *Server) handleCreateServiceToken(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		// ExpiresAt is optional RFC 3339. Omit or send "" for a credential
		// that never expires.
		ExpiresAt string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	expiresAt, err := parseOptionalTime(body.ExpiresAt)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("expires_at must be an RFC 3339 timestamp, for example 2027-01-31T00:00:00Z.").WithParam("expires_at"))
		return
	}
	token, plaintext, err := s.Store.CreateServiceToken(r.Context(), body.Name, body.Description, actorID(actor), expiresAt)
	if err != nil {
		WriteError(w, r, serviceTokenWriteError(err))
		return
	}
	s.audit(r, "service_token_created", "service_token", token.ID, nil, map[string]any{
		"name": token.Name, "expires_at": body.ExpiresAt,
	})
	WriteJSON(w, http.StatusCreated, map[string]any{
		"service_token": token,
		// Returned exactly once: only the digest is persisted.
		"value":   plaintext,
		"warning": "Copy this value now. Janus stores only a hash and cannot show it again.",
	})
}

func (s *Server) handleServiceTokenDetail(w http.ResponseWriter, r *http.Request) {
	token, err := s.Store.ServiceTokenByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That service token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	grants, err := s.Store.ListGrants(r.Context(), "")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	held := []*store.Grant{}
	for _, g := range grants {
		if g.GranteeType == store.GranteeServiceToken && g.GranteeID == token.ID {
			held = append(held, g)
		}
		if g.GranteeType == store.GranteeAllServiceTokens {
			held = append(held, g)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"service_token": token, "grants": held})
}

func (s *Server) handleUpdateServiceToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ServiceTokenByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That service token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Name == "" {
		body.Name = before.Name
	}
	expiresAt, err := parseOptionalTime(body.ExpiresAt)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("expires_at must be an RFC 3339 timestamp, or empty to remove the expiry.").WithParam("expires_at"))
		return
	}
	if err := s.Store.UpdateServiceToken(r.Context(), id, body.Name, body.Description, expiresAt); err != nil {
		WriteError(w, r, serviceTokenWriteError(err))
		return
	}
	// A rename changes the label usage is reported under, so the credential
	// cache (which carries the name) has to drop.
	s.InvalidateTokenCache()
	s.audit(r, "service_token_updated", "service_token", id,
		map[string]any{"name": before.Name, "expires_at": formatOptional(before.ExpiresAt)},
		map[string]any{"name": body.Name, "expires_at": body.ExpiresAt})
	updated, err := s.Store.ServiceTokenByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"service_token": updated})
}

// handleRevokeServiceToken withdraws a credential. The row is retained so
// historical usage keeps resolving to a name, but every grant it held is
// dropped: a revoked credential must not keep appearing in the access matrix
// as though it still had reach.
func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	token, err := s.Store.ServiceTokenByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That service token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.RevokeServiceToken(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DeleteGrantsForServiceToken(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	// Revocation must be immediate on this replica; others converge within
	// the token cache TTL.
	s.InvalidateTokenCache()
	s.InvalidateConfigCache()
	s.audit(r, "service_token_revoked", "service_token", id,
		map[string]any{"name": token.Name, "revoked": false},
		map[string]any{"name": token.Name, "revoked": true})
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "revoked_at": time.Now().UTC()})
}

// handleServiceTokenUsage reports one credential's own usage detail: totals,
// model and modality breakdowns, a time series, and its recent requests.
//
// This is the surface that makes service-token traffic legible. It is
// deliberately scoped to the single token — org-wide reports already include
// this traffic in their totals, and people-oriented reports exclude it
// entirely, so without this page the usage would be visible only in aggregate.
func (s *Server) handleServiceTokenUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	token, err := s.Store.ServiceTokenByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That service token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	start, end, label := rangeBounds(r)
	scope := store.UsageScope{ServiceTokenID: id, Start: start, End: end}

	totals, err := s.Store.AggregateUsage(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byModel, err := s.Store.BreakdownUsage(r.Context(), scope, "model", store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byModality, err := s.Store.BreakdownUsage(r.Context(), scope, "modality", store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	bucket, buckets := seriesShape(label)
	series, err := s.Store.UsageSeries(r.Context(), scope, bucket, buckets)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	recent, _, err := s.Store.ListRequests(r.Context(), store.RequestFilter{ServiceTokenID: id, Limit: 20})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"service_token": token,
		"range":         label, "start": start, "end": end,
		"totals": totals, "per_model": byModel, "per_modality": byModality,
		"series": series, "recent_requests": recent,
	})
}

// serviceTokenWriteError maps store-level write failures onto the API error
// vocabulary so a name collision reads as a 400 naming the field rather than
// an opaque 500.
func serviceTokenWriteError(err error) error {
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		return ErrInvalidRequest(capitalizeFirst(ve.Message) + ".").WithParam(ve.Field)
	}
	if errors.Is(err, store.ErrServiceTokenNameTaken) {
		return ErrInvalidRequest("A service token with that name already exists. Names appear on usage reports, so they must be unique.").WithParam("name")
	}
	return err
}

// --- Managed models ----------------------------------------------------------

// mountManagedModelRoutes registers the admin managed-model endpoints.
func (s *Server) mountManagedModelRoutes(r chi.Router) {
	r.Get("/admin/managed-models", s.handleListManagedModels)
	r.Post("/admin/managed-models", s.gateCreate("managed model", s.handleCreateManagedModel))
	r.Get("/admin/managed-models/{id}", s.handleManagedModelDetail)
	r.Patch("/admin/managed-models/{id}", s.handlePatchManagedModel)
	r.Delete("/admin/managed-models/{id}", s.handleDeleteManagedModel)
}

// handleListManagedModels returns the admin managed-model table.
//
// Rows carry 30-day usage keyed on requested_model_name — the alias the caller
// actually typed — not on the underlying model. This is the one place that
// distinction matters for reporting: model reports must show the real model
// that ran (that is the whole contract), but an admin deciding whether
// "current-best" is worth keeping needs to know how much traffic arrived
// through the alias itself. distinct_principals_30d answers "is anyone
// actually using this?" before a repoint or a delete.
//
// Query parameters: status, search, target_model_id, and sort
// (name | created | spend | requests | tokens | users | grants, with
// _asc/_desc). Default is spend descending.
func (s *Server) handleListManagedModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.Store.ListManagedModels(r.Context(), store.ManagedModelFilter{
		Status:        r.URL.Query().Get("status"),
		Search:        strings.TrimSpace(r.URL.Query().Get("search")),
		TargetModelID: r.URL.Query().Get("target_model_id"),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	grants, err := s.Store.ListGrants(r.Context(), "")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	grantCount := map[string]int{}
	for _, g := range grants {
		if g.ModelKind == "managed" {
			grantCount[g.ModelID]++
		}
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -30)
	rows := make([]map[string]any, 0, len(models))
	for _, m := range models {
		scope := store.UsageScope{RequestedModelName: m.Name, Start: start, End: now}
		totals, err := s.Store.AggregateUsage(r.Context(), scope)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		principals, err := s.Store.CountDistinctPrincipals(r.Context(), scope)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		rows = append(rows, map[string]any{
			"id": m.ID, "name": m.Name, "description": m.Description,
			"status": m.Status, "target_model_id": m.TargetModelID,
			"target_model_name": m.TargetName, "target_public_name": m.TargetPublicName,
			"target_upstream_name": m.TargetUpstreamName, "target_status": m.TargetStatus,
			"broken": m.Broken, "broken_reason": m.BrokenReason,
			"servable": m.Servable, "modalities": m.Modalities,
			"context_window":         m.ContextWindow,
			"fallback_model_id":      m.FallbackModelID,
			"fallback_triggers":      m.FallbackTriggers,
			"fallback_model_name":    m.FallbackName,
			"fallback_public_name":   m.FallbackPublicName,
			"fallback_status":        m.FallbackStatus,
			"fallback_upstream_id":   m.FallbackUpstreamID,
			"fallback_upstream_name": m.FallbackUpstreamName,
			"fallback_broken":        m.FallbackBroken,
			"fallback_broken_reason": m.FallbackBrokenReason,
			"created_at":             m.CreatedAt, "updated_at": m.UpdatedAt,
			"grant_count":             grantCount[m.ID],
			"spend_30d_usd":           usage.USD(totals.CostNano),
			"requests_30d":            totals.Requests,
			"tokens_in_30d":           totals.TokensIn,
			"tokens_out_30d":          totals.TokensOut,
			"errors_30d":              totals.ErrorCount,
			"distinct_principals_30d": principals,
		})
	}
	sortManagedModelRows(rows, r.URL.Query().Get("sort"))
	WriteJSON(w, http.StatusOK, map[string]any{"managed_models": rows, "total_count": len(rows)})
}

// sortManagedModelRows orders assembled alias rows in memory, for the same
// reason sortServiceTokenRows does: the usage columns are computed, not stored.
func sortManagedModelRows(rows []map[string]any, sortParam string) {
	dir := 1
	field := sortParam
	if strings.HasSuffix(sortParam, "_desc") {
		dir, field = -1, strings.TrimSuffix(sortParam, "_desc")
	} else if strings.HasSuffix(sortParam, "_asc") {
		field = strings.TrimSuffix(sortParam, "_asc")
	}
	if field == "" {
		field, dir = "spend", -1
	}
	num := func(row map[string]any, key string) float64 {
		switch v := row[key].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		case int:
			return float64(v)
		}
		return 0
	}
	sort.SliceStable(rows, func(i, j int) bool {
		// Missing names/timestamps stay last; equal values retain source order.
		key := map[string]string{"name": "name", "created": "created_at", "last_used": "last_used_at", "expires": "expires_at"}[field]
		blank := func(row map[string]any) bool {
			if key == "" {
				return false
			}
			v := row[key]
			if v == nil || v == "" {
				return true
			}
			if tm, ok := v.(time.Time); ok {
				return tm.IsZero()
			}
			return false
		}
		if blank(rows[i]) {
			return false
		}
		if blank(rows[j]) {
			return true
		}
		if dir < 0 {
			i, j = j, i
		}
		var less bool
		switch field {
		case "name":
			a, _ := rows[i]["name"].(string)
			b, _ := rows[j]["name"].(string)
			less = strings.ToLower(a) < strings.ToLower(b)
		case "created":
			a, _ := rows[i]["created_at"].(time.Time)
			b, _ := rows[j]["created_at"].(time.Time)
			less = a.Before(b)
		case "requests":
			less = num(rows[i], "requests_30d") < num(rows[j], "requests_30d")
		case "tokens":
			less = num(rows[i], "tokens_in_30d")+num(rows[i], "tokens_out_30d") <
				num(rows[j], "tokens_in_30d")+num(rows[j], "tokens_out_30d")
		case "users":
			less = num(rows[i], "distinct_principals_30d") < num(rows[j], "distinct_principals_30d")
		case "grants":
			less = num(rows[i], "grant_count") < num(rows[j], "grant_count")
		default: // spend
			less = num(rows[i], "spend_30d_usd") < num(rows[j], "spend_30d_usd")
		}
		return less
	})
}

func (s *Server) handleCreateManagedModel(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var body struct {
		Name             string   `json:"name"`
		Description      string   `json:"description"`
		TargetModelID    string   `json:"target_model_id"`
		FallbackModelID  string   `json:"fallback_model_id"`
		FallbackTriggers []string `json:"fallback_triggers"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := s.Store.CreateManagedModel(r.Context(), body.Name, body.Description, body.TargetModelID, actorID(actor))
	if err != nil {
		WriteError(w, r, managedModelWriteError(err))
		return
	}
	if strings.TrimSpace(body.FallbackModelID) != "" {
		if err := s.requireFeature("model_fallbacks"); err != nil {
			WriteError(w, r, err)
			return
		}
		if err := s.Store.SetManagedModelFallback(r.Context(), m.ID, body.FallbackModelID, body.FallbackTriggers); err != nil {
			// The alias was created but the fallback was refused: roll the
			// creation back so the admin's form can be corrected and
			// resubmitted as a whole, rather than leaving a half-configured
			// alias behind.
			_ = s.Store.DeleteManagedModel(r.Context(), m.ID)
			WriteError(w, r, managedModelWriteError(err))
			return
		}
		if m, err = s.Store.ManagedModelByID(r.Context(), m.ID); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	s.InvalidateConfigCache()
	s.InvalidateResolvedModelCache(m.Name)
	s.audit(r, "managed_model_created", "managed_model", m.ID, nil, map[string]any{
		"name": m.Name, "target_model_id": m.TargetModelID, "target_model": m.TargetPublicName,
		"fallback_model_id": m.FallbackModelID, "fallback_model": m.FallbackPublicName, "fallback_triggers": m.FallbackTriggers,
	})
	WriteJSON(w, http.StatusCreated, map[string]any{"managed_model": m})
}

func (s *Server) handleManagedModelDetail(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.ManagedModelByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That managed model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	grants, err := s.Store.ListGrants(r.Context(), m.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"managed_model": m, "grants": grants})
}

// handlePatchManagedModel applies presentation edits, a status change, and/or
// a repoint. The repoint is the operation the whole feature exists for, so it
// is audited as its own event with the old and new target spelled out by name
// — an admin reading the audit log should not have to resolve ids to work out
// what "current-best" meant last Tuesday.
func (s *Server) handlePatchManagedModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ManagedModelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That managed model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Name          *string `json:"name"`
		Description   *string `json:"description"`
		Status        *string `json:"status"`
		TargetModelID *string `json:"target_model_id"`
		// FallbackModelID "" clears the fallback. FallbackTriggers without
		// a FallbackModelID re-configures the failure modes of the current
		// fallback.
		FallbackModelID  *string   `json:"fallback_model_id"`
		FallbackTriggers *[]string `json:"fallback_triggers"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	if body.Name != nil || body.Description != nil {
		name := before.Name
		if body.Name != nil {
			name = *body.Name
		}
		description := before.Description
		if body.Description != nil {
			description = *body.Description
		}
		if err := s.Store.UpdateManagedModel(r.Context(), id, name, description); err != nil {
			WriteError(w, r, managedModelWriteError(err))
			return
		}
		if name != before.Name {
			s.audit(r, "managed_model_renamed", "managed_model", id,
				map[string]any{"name": before.Name}, map[string]any{"name": name})
		}
	}
	if body.Status != nil && *body.Status != before.Status {
		if err := s.Store.SetManagedModelStatus(r.Context(), id, *body.Status); err != nil {
			WriteError(w, r, managedModelWriteError(err))
			return
		}
		s.audit(r, "managed_model_status_changed", "managed_model", id,
			map[string]any{"status": before.Status}, map[string]any{"status": *body.Status})
	}
	if body.TargetModelID != nil && *body.TargetModelID != before.TargetModelID {
		if err := s.Store.SetManagedModelTarget(r.Context(), id, *body.TargetModelID); err != nil {
			WriteError(w, r, managedModelWriteError(err))
			return
		}
		after, err := s.Store.ManagedModelByID(r.Context(), id)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		s.audit(r, "managed_model_repointed", "managed_model", id,
			map[string]any{"target_model_id": before.TargetModelID, "target_model": before.TargetPublicName},
			map[string]any{"target_model_id": after.TargetModelID, "target_model": after.TargetPublicName})
	}
	if body.FallbackModelID != nil && strings.TrimSpace(*body.FallbackModelID) != "" {
		if err := s.requireFeature("model_fallbacks"); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if body.FallbackModelID != nil || body.FallbackTriggers != nil {
		fallbackID := before.FallbackModelID
		if body.FallbackModelID != nil {
			fallbackID = *body.FallbackModelID
		}
		triggers := before.FallbackTriggers
		if body.FallbackTriggers != nil {
			triggers = *body.FallbackTriggers
		}
		if err := s.Store.SetManagedModelFallback(r.Context(), id, fallbackID, triggers); err != nil {
			WriteError(w, r, managedModelWriteError(err))
			return
		}
		after, err := s.Store.ManagedModelByID(r.Context(), id)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if after.FallbackModelID != before.FallbackModelID || !equalStrings(after.FallbackTriggers, before.FallbackTriggers) {
			s.audit(r, "managed_model_fallback_changed", "managed_model", id,
				map[string]any{"fallback_model_id": before.FallbackModelID, "fallback_model": before.FallbackPublicName, "fallback_triggers": before.FallbackTriggers},
				map[string]any{"fallback_model_id": after.FallbackModelID, "fallback_model": after.FallbackPublicName, "fallback_triggers": after.FallbackTriggers})
		}
	}

	// Both the old and new names are flushed: the old so stale lookups miss,
	// the new so an entry cached between the write and this flush cannot pin
	// a pre-change resolution.
	s.InvalidateConfigCache()
	names := []string{before.Name}
	if body.Name != nil {
		names = append(names, *body.Name)
	}
	s.InvalidateResolvedModelCache(names...)

	updated, err := s.Store.ManagedModelByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"managed_model": updated})
}

func (s *Server) handleDeleteManagedModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ManagedModelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That managed model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DeleteManagedModel(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.InvalidateResolvedModelCache(before.Name)
	s.audit(r, "managed_model_deleted", "managed_model", id,
		map[string]any{"name": before.Name, "target_model": before.TargetPublicName}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// managedModelWriteError maps store-level write failures onto the API error
// vocabulary, so a name collision or an invalid target reads as a 400 naming
// the offending field.
func managedModelWriteError(err error) error {
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		return ErrInvalidRequest(capitalizeFirst(ve.Message) + ".").WithParam(ve.Field)
	}
	if errors.Is(err, store.ErrManagedModelNameTaken) {
		return ErrInvalidRequest("That name is already used by another model or managed model. Model names must be unique so requests route unambiguously.").WithParam("name")
	}
	return err
}

// --- shared helpers ----------------------------------------------------------

func actorID(u *store.User) string {
	if u == nil {
		return ""
	}
	return u.ID
}

// parseOptionalTime accepts an empty string (meaning "unset") or an RFC 3339
// timestamp. It exists so "no expiry" and "bad expiry" stay distinguishable.
func parseOptionalTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func formatOptional(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// capitalizeFirst upper-cases the first letter so a store-level validation
// message reads as a sentence in the API response.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// equalStrings reports whether two string slices hold the same elements in
// the same order (nil and empty compare equal).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
