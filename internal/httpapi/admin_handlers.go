package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/authz"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

func (s *Server) mountAdminRoutes(r chi.Router) {
	r.Get("/admin/overview", s.handleAdminOverview)
	r.Get("/admin/system/status", s.handleSystemStatus)
	r.Get("/admin/system/license", s.handleGetLicense)
	r.Put("/admin/system/license", s.handlePutLicense)
	r.Delete("/admin/system/license", s.handleDeleteLicense)
	r.Get("/admin/system/license/sync", s.handleGetLicenseSync)
	r.Put("/admin/system/license/sync", s.handlePutLicenseSync)
	r.Post("/admin/system/license/sync", s.handlePostLicenseSync)
	s.mountAdminLocalAuthRoutes(r)
	// Runtime upstream timeouts (connect / TTFB / total): view, override any
	// hop, or revert to the environment defaults. See admin_system.go.
	r.Get("/admin/system/upstream-timeouts", s.handleGetUpstreamTimeouts)
	r.Patch("/admin/system/upstream-timeouts", s.handlePatchUpstreamTimeouts)
	r.Delete("/admin/system/upstream-timeouts", s.handleDeleteUpstreamTimeouts)
	r.Get("/admin/system/discovery-interval", s.handleGetDiscoveryInterval)
	r.Patch("/admin/system/discovery-interval", s.handlePatchDiscoveryInterval)
	r.Delete("/admin/system/discovery-interval", s.handleDeleteDiscoveryInterval)

	r.Get("/admin/upstreams", s.handleListUpstreams)
	r.Post("/admin/upstreams", s.gateCreate("upstream", s.handleCreateUpstream))
	r.Put("/admin/upstreams/{id}", s.handleUpdateUpstream)
	r.Delete("/admin/upstreams/{id}", s.handleDeleteUpstream)
	r.Get("/admin/upstreams/{id}/dependents", s.handleUpstreamDependents)
	r.Post("/admin/upstreams/{id}/refresh", s.handleRefreshUpstream)

	r.Get("/admin/models", s.handleAdminListModels)
	r.Patch("/admin/models/{id}", s.handlePatchModel)
	r.Get("/admin/ratecards/reference", s.handleReferenceRatecards)
	r.Post("/admin/ratecards/apply", s.handleApplyReferenceRatecards)

	r.Get("/admin/grants", s.handleListGrants)
	r.Post("/admin/grants", s.gateCreate("grant", s.handleCreateGrant))
	r.Delete("/admin/grants/{id}", s.handleDeleteGrant)

	// Service tokens and managed models (see admin_service_tokens.go).
	s.mountServiceTokenRoutes(r)
	s.mountManagedModelRoutes(r)

	r.Get("/admin/users", s.handleAdminListUsers)
	r.Get("/admin/users/{id}", s.handleAdminUserDetail)
	r.Patch("/admin/users/{id}", s.handlePatchUser)
	r.Delete("/admin/users/{id}", s.handleDeleteUser)

	r.Get("/admin/groups", s.handleListGroups)
	r.Post("/admin/groups", s.gateCreate("group", s.handleCreateGroup))
	r.Patch("/admin/groups/{id}", s.handlePatchGroup)
	r.Delete("/admin/groups/{id}", s.handleDeleteGroup)

	// Admin groups: the third source of admin capability, managed here so a
	// group can be added without a redeploy. JANUS_ADMIN_GROUPS remains the
	// environment failsafe and is unioned with these at sign-in.
	r.Get("/admin/admin-groups", s.handleListAdminGroups)
	r.Post("/admin/admin-groups", s.handleAddAdminGroup)
	r.Delete("/admin/admin-groups/{name}", s.handleRemoveAdminGroup)

	r.Post("/admin/teams/import/preview", s.handleTeamImportPreview)
	r.Post("/admin/teams/import", s.gateCreate("team", s.handleTeamImportApply))
	r.Get("/admin/teams", s.handleListTeams)
	r.Post("/admin/teams", s.gateCreate("team", s.handleCreateTeam))
	r.Patch("/admin/teams/{id}", s.handlePatchTeam)
	r.Delete("/admin/teams/{id}", s.handleDeleteTeam)

	r.Get("/admin/quotas", s.handleListQuotas)
	r.Post("/admin/quotas", s.gateCreate("quota", s.handleCreateQuota))
	r.Put("/admin/quotas/{id}", s.handleUpdateQuota)
	r.Delete("/admin/quotas/{id}", s.handleDeleteQuota)

	r.Get("/admin/rate-limits", s.handleListRateLimits)
	r.Post("/admin/rate-limits", s.gateCreate("rate limit", s.handleCreateRateLimit))
	r.Put("/admin/rate-limits/{id}", s.handleUpdateRateLimit)
	r.Delete("/admin/rate-limits/{id}", s.handleDeleteRateLimit)

	r.Get("/admin/rules/blocking", s.handleListRules)
	r.Post("/admin/rules/blocking", s.gateCreate("policy rule", s.handleCreateRule))
	r.Post("/admin/rules/blocking/test", s.handleTestRule)
	r.Put("/admin/rules/blocking/{id}", s.handleUpdateRule)
	r.Delete("/admin/rules/blocking/{id}", s.handleDeleteRule)

	r.Get("/admin/alerts", s.handleListAlerts)
	r.Post("/admin/alerts", s.gateCreate("alert", s.handleCreateAlert))
	r.Put("/admin/alerts/{id}", s.handleUpdateAlert)
	r.Post("/admin/alerts/{id}/test", s.handleTestAlert)
	r.Delete("/admin/alerts/{id}", s.handleDeleteAlert)

	r.Get("/admin/audit", s.handleListAudit)
	r.Get("/admin/audit/export", s.gateFeature("audit_export", s.handleExportAudit))

	// Troubleshooting mode: on-demand capture of request/response payloads
	// for requests matching an admin-defined filter. See troubleshoot.go.
	s.mountSecgwRoutes(r)
	r.Get("/admin/troubleshooting", s.handleGetTroubleshooting)
	r.Put("/admin/troubleshooting", s.handlePutTroubleshooting)
	r.Delete("/admin/troubleshooting", s.handleDeleteTroubleshooting)
	r.Get("/admin/troubleshooting/stats", s.handleGetTroubleshootingStats)
	r.Delete("/admin/troubleshooting/data", s.handlePurgeTroubleshootingData)
	r.Post("/admin/troubleshooting/cleanup", s.handleTroubleshootingCleanup)
	r.Get("/admin/troubleshooting/captures", s.handleListTroubleshootingCaptures)
	r.Get("/admin/troubleshooting/export", s.handleExportTroubleshooting)
	r.Get("/admin/requests/{id}/download", s.handleDownloadRequestCapture)
	r.Get("/admin/requests/{id}/security", s.handleRequestSecurity)
	r.Get("/admin/analytics", s.handleAnalytics)

	// Feature-flag endpoints: GET lists every flag in store.DefaultFeatureFlags
	// (including spend_emphasis, which controls the default dashboard metric);
	// PATCH toggles any subset with validation + audit logging and invalidates
	// the config cache so /api/v1/me reflects the change immediately. See the
	// "Feature flags" section below for the handlers.
	r.Get("/admin/features", s.handleGetFeatures)
	r.Patch("/admin/features", s.handlePatchFeatures)
	r.Post("/admin/docs-feedback/{id}/resolve", s.handleResolveFeedback)
}

// audit records an administrative change. Failure to write the audit trail is
// logged loudly but never hides the change that was made.
func (s *Server) audit(r *http.Request, action, resourceType, resourceID string, oldValue, newValue any) {
	actor := UserFrom(r.Context())
	entry := &store.AuditEntry{Action: action, ResourceType: resourceType, ResourceID: resourceID}
	if actor != nil {
		entry.ActorUserID = actor.ID
		entry.ActorLabel = actor.Email
	}
	entry.OldValue = encodeAuditValue(oldValue)
	entry.NewValue = encodeAuditValue(newValue)
	if err := s.Store.AppendAudit(r.Context(), entry); err != nil {
		s.Logger.ErrorContext(r.Context(), "append audit entry", "error", err.Error(), "action", action)
	}
}

func encodeAuditValue(v any) string {
	if v == nil {
		return ""
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// --- Overview & status -------------------------------------------------------

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	start, end, label := rangeBounds(r)
	// The overview is the ORG-WIDE view: the scope carries no principal
	// filter, so totals and time series include service-token traffic
	// alongside user traffic. That is the org-pulse rule — cluster-level
	// numbers count everything the gateway processed.
	scope := store.UsageScope{Start: start, End: end}

	totals, err := s.Store.AggregateUsage(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Top users rank by output-token volume (the default breakdown metric),
	// not by spend — usage is the primary leaderboard signal.
	//
	// This is a PEOPLE-oriented report, so service-token traffic is excluded.
	// BreakdownUsage enforces that for the "user" dimension itself, so the
	// exclusion cannot be lost by editing this call site.
	topSpenders, err := s.Store.BreakdownUsage(r.Context(), scope, "user", store.BreakdownMetricTokensOut)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(topSpenders) > 10 {
		topSpenders = topSpenders[:10]
	}
	// Team scopes use recorded attribution, never the current member roster.
	teams, err := s.Store.ListTeams(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	topTeams := make([]store.Breakdown, 0, len(teams))
	for _, team := range teams {
		teamScope := scope
		teamScope.TeamIDs = []string{team.ID}
		t, err := s.Store.AggregateUsage(r.Context(), teamScope)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		// A zero-output request still represents usage (e.g. embeddings).
		if t.Requests == 0 && t.TokensIn == 0 && t.TokensOut == 0 && t.TokensCached == 0 && t.TokensCacheWrite5m == 0 && t.TokensCacheWrite1h == 0 && t.CostNano == 0 {
			continue
		}
		topTeams = append(topTeams, store.Breakdown{Key: team.ID, Label: team.Name, Totals: t})
	}
	sort.Slice(topTeams, func(i, j int) bool {
		if topTeams[i].Totals.TokensOut != topTeams[j].Totals.TokensOut {
			return topTeams[i].Totals.TokensOut > topTeams[j].Totals.TokensOut
		}
		if topTeams[i].Totals.Requests != topTeams[j].Totals.Requests {
			return topTeams[i].Totals.Requests > topTeams[j].Totals.Requests
		}
		return topTeams[i].Key < topTeams[j].Key
	})
	if len(topTeams) > 10 {
		topTeams = topTeams[:10]
	}
	// Service tokens get their own leaderboard rather than being folded into
	// top users. Their traffic is counted in the org totals above, so without
	// this panel a busy integration would move the headline numbers with no
	// visible explanation anywhere in the console.
	topServiceTokens, err := s.Store.BreakdownUsage(r.Context(), scope, "service_token", store.BreakdownMetricTokensOut)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(topServiceTokens) > 10 {
		topServiceTokens = topServiceTokens[:10]
	}
	// Split totals so an admin can see at a glance how much of the org's
	// consumption is automated versus human.
	serviceTotals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{
		Start: start, End: end, Principal: store.PrincipalServiceTokens,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	serviceTokenCounts, err := s.Store.CountServiceTokensByStatus(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The store filters the model breakdown against the model catalog, so the
	// admin top-models graph never shows invalid model names sent by callers
	// (those are logged by the store for auditing instead).
	topModels, err := s.Store.BreakdownUsage(r.Context(), scope, "model", store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byStatus, err := s.Store.BreakdownUsage(r.Context(), scope, "status", store.BreakdownMetricCost)
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
	modelCounts, err := s.Store.CountModelsByStatus(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	quotas, err := s.Store.ListQuotas(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	nearBreach := []map[string]any{}
	for _, q := range quotas {
		// Local-only mode: USD quotas are inert (never enforced), so listing
		// one as "near breach" would be both a cost surface and a lie.
		if s.Config.LocalOnly && q.Metric == store.MetricCostUSD {
			continue
		}
		status, err := s.Quota.StatusFor(r.Context(), q)
		if err != nil {
			continue
		}
		if status.AtRisk {
			nearBreach = append(nearBreach, quotaStatusPayload(status))
		}
	}
	userCount, err := s.Store.CountUsers(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	activeUsers, err := s.Store.ActiveUserCount(r.Context(), time.Now().UTC().Add(-15*time.Minute))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"range": label, "totals": totals, "series": series,
		"top_teams":    topTeams,
		"top_spenders": topSpenders, "top_models": topModels, "by_status": byStatus,
		"model_counts": modelCounts, "near_breach": nearBreach,
		"user_count": userCount, "active_users_15m": activeUsers,
		// Service-token visibility: their traffic is inside "totals" above,
		// so the console shows what portion it is and which integrations
		// drive it.
		"top_service_tokens":   topServiceTokens,
		"service_token_totals": serviceTotals,
		"service_token_counts": serviceTokenCounts,
	})
}

// handleSystemStatus lives in admin_system.go alongside the database-URL
// sanitizer it depends on.

// --- Upstreams ---------------------------------------------------------------

func (s *Server) handleListUpstreams(w http.ResponseWriter, r *http.Request) {
	upstreams, err := s.Store.ListUpstreams(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"upstreams": upstreams, "adapter_types": adapter.Types()})
}

type upstreamPayload struct {
	Name        string `json:"name"`
	AdapterType string `json:"adapter_type"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	Enabled     *bool  `json:"enabled"`
}

func (s *Server) handleCreateUpstream(w http.ResponseWriter, r *http.Request) {
	var body upstreamPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the upstream a name.").WithParam("name"))
		return
	}
	if _, err := adapter.Get(body.AdapterType); err != nil {
		WriteError(w, r, ErrInvalidRequest("Choose a supported provider type. Available: "+strings.Join(adapter.Types(), ", ")).WithParam("adapter_type"))
		return
	}
	if err := validateBaseURL(body.BaseURL); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("base_url"))
		return
	}
	encrypted, err := s.Cipher.Encrypt(body.APIKey)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	up, err := s.Store.CreateUpstream(r.Context(), strings.TrimSpace(body.Name), body.AdapterType, body.BaseURL, encrypted, crypto.Mask(body.APIKey))
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("That upstream could not be created: "+err.Error()))
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "upstream_created", "upstream", up.ID, nil, map[string]any{"name": up.Name, "adapter_type": up.AdapterType, "base_url": up.BaseURL})

	// Discover immediately so the admin sees models without waiting for the poll.
	go func(u *store.Upstream) {
		// Background context: this discovery run outlives the HTTP request.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.Discovery.RunFor(ctx, u)
	}(up)

	WriteJSON(w, http.StatusCreated, map[string]any{"upstream": up})
}

func validateBaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("a base URL is required, for example https://api.openai.com")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%q is not a valid absolute URL", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("the base URL must use http or https")
	}
	return nil
}

func (s *Server) handleUpdateUpstream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.UpstreamByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That upstream"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body upstreamPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.AdapterType != "" && body.AdapterType != existing.AdapterType {
		WriteError(w, r, ErrInvalidRequest("The provider type cannot be changed after an upstream is created. Create a new upstream instead.").WithParam("adapter_type"))
		return
	}
	if err := validateBaseURL(body.BaseURL); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("base_url"))
		return
	}
	enabled := existing.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	encrypted, mask := "", ""
	if body.APIKey != "" {
		if encrypted, err = s.Cipher.Encrypt(body.APIKey); err != nil {
			WriteError(w, r, err)
			return
		}
		mask = crypto.Mask(body.APIKey)
	}
	// Persist a rename before the rest of the update: the edit drawer sends
	// the (possibly changed) name, and dropping it silently while returning
	// success would lie to the admin.
	name := strings.TrimSpace(body.Name)
	if name != "" && name != existing.Name {
		if err := s.Store.RenameUpstream(r.Context(), id, name); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				WriteError(w, r, ErrInvalidRequest("Another upstream is already named "+name+".").WithParam("name"))
				return
			}
			WriteError(w, r, err)
			return
		}
	} else {
		name = existing.Name
	}
	if err := s.Store.UpdateUpstream(r.Context(), id, body.BaseURL, enabled, encrypted, mask); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "upstream_updated", "upstream", id,
		map[string]any{"name": existing.Name, "base_url": existing.BaseURL, "enabled": existing.Enabled},
		map[string]any{"name": name, "base_url": body.BaseURL, "enabled": enabled, "api_key_rotated": body.APIKey != ""})
	updated, err := s.Store.UpstreamByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"upstream": updated})
}

// upstreamDependents is everything that stops working, or becomes dead
// weight, when an upstream is deleted: the models it hosts (all disabled by
// the delete), the managed models whose target is one of those models (they
// keep resolving by name but can no longer be served), and the direct grants
// on those models (references to models nobody can call any more).
type upstreamDependents struct {
	Models        []upstreamDependentModel        `json:"models"`
	ManagedModels []upstreamDependentManagedModel `json:"managed_models"`
	GrantCount    int                             `json:"grant_count"`
	// BlockingManagedModels counts the enabled aliases — the ones that make
	// the delete a conflict until repointed/disabled or forced.
	BlockingManagedModels int `json:"blocking_managed_models"`
}

type upstreamDependentModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

type upstreamDependentManagedModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	TargetModelName string `json:"target_model_name"`
}

// collectUpstreamDependents enumerates the blast radius of deleting one
// upstream. Disabled aliases are listed too (they are already out of
// service, so they do not block) — the admin should still see them because
// re-enabling one later would silently fail.
func (s *Server) collectUpstreamDependents(ctx context.Context, upstreamID string) (*upstreamDependents, error) {
	models, err := s.Store.ListModels(ctx, store.ModelFilter{UpstreamID: upstreamID, Sort: "name"})
	if err != nil {
		return nil, err
	}
	aliases, err := s.Store.ListManagedModels(ctx, store.ManagedModelFilter{TargetUpstreamID: upstreamID})
	if err != nil {
		return nil, err
	}
	grants, err := s.Store.CountGrantsForUpstreamModels(ctx, upstreamID)
	if err != nil {
		return nil, err
	}
	out := &upstreamDependents{
		Models:        make([]upstreamDependentModel, 0, len(models)),
		ManagedModels: make([]upstreamDependentManagedModel, 0, len(aliases)),
		GrantCount:    grants,
	}
	for _, m := range models {
		out.Models = append(out.Models, upstreamDependentModel{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName, Status: m.Status})
	}
	for _, mm := range aliases {
		out.ManagedModels = append(out.ManagedModels, upstreamDependentManagedModel{
			ID: mm.ID, Name: mm.Name, Status: mm.Status, TargetModelName: mm.TargetPublicName,
		})
		if mm.Status == store.ManagedModelEnabled {
			out.BlockingManagedModels++
		}
	}
	return out, nil
}

// handleUpstreamDependents is the pre-deletion preflight the admin UI calls
// when the delete dialog opens, so the consequences are visible before the
// admin commits rather than discovered as a 409 afterwards.
func (s *Server) handleUpstreamDependents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.Store.UpstreamByID(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That upstream"))
		return
	} else if err != nil {
		WriteError(w, r, err)
		return
	}
	deps, err := s.collectUpstreamDependents(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, deps)
}

// handleDeleteUpstream soft-deletes an upstream: its models are disabled and
// the upstream disappears from listings while history is retained.
//
// Deleting an upstream silently breaks every enabled managed model whose
// target is hosted there, exactly as disabling a single model would — so the
// same rule applies: refuse with 409 and name the affected aliases unless the
// admin has repointed or disabled them, or retries with ?force=true to break
// them deliberately. ?purge_grants=true additionally removes the direct
// grants on the upstream's models, which become dead references otherwise.
func (s *Server) handleDeleteUpstream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.UpstreamByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That upstream"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	deps, err := s.collectUpstreamDependents(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if deps.BlockingManagedModels > 0 && !force {
		names := make([]string, 0, deps.BlockingManagedModels)
		for _, mm := range deps.ManagedModels {
			if mm.Status == store.ManagedModelEnabled {
				names = append(names, mm.Name)
			}
		}
		WriteError(w, r, ErrConflict(fmt.Sprintf(
			"This upstream hosts the target of %d enabled managed model(s): %s. Repoint or disable them first, "+
				"or retry with ?force=true to break them deliberately.",
			len(names), strings.Join(names, ", "))))
		return
	}
	grantsRemoved := 0
	if r.URL.Query().Get("purge_grants") == "true" {
		n, err := s.Store.DeleteGrantsForUpstreamModels(r.Context(), id)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		grantsRemoved = n
	}
	if err := s.Store.SoftDeleteUpstream(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	brokenAliases := make([]string, 0, len(deps.ManagedModels))
	for _, mm := range deps.ManagedModels {
		brokenAliases = append(brokenAliases, mm.Name)
	}
	s.audit(r, "upstream_deleted", "upstream", id, map[string]any{
		"name":                   existing.Name,
		"models_disabled":        len(deps.Models),
		"managed_models_broken":  brokenAliases,
		"forced":                 force && deps.BlockingManagedModels > 0,
		"grants_removed":         grantsRemoved,
		"grants_left_dangling":   deps.GrantCount - grantsRemoved,
		"blocking_managed_model": deps.BlockingManagedModels,
	}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{
		"id":                    id,
		"deleted":               true,
		"models_disabled":       len(deps.Models),
		"managed_models_broken": brokenAliases,
		"grants_removed":        grantsRemoved,
	})
}

func (s *Server) handleRefreshUpstream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	up, err := s.Store.UpstreamByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That upstream"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	result := s.Discovery.RunFor(r.Context(), up)
	// Discovery may have added or re-flagged models; drop cached hot-path config.
	s.InvalidateConfigCache()
	WriteJSON(w, http.StatusOK, map[string]any{"result": result})
}

// --- Models ------------------------------------------------------------------

func (s *Server) handleAdminListModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.Store.ListModels(r.Context(), store.ModelFilter{
		UpstreamID: r.URL.Query().Get("upstream_id"),
		Status:     r.URL.Query().Get("status"),
		Search:     r.URL.Query().Get("search"),
		Sort:       r.URL.Query().Get("sort"),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	counts, err := s.Store.CountModelsByStatus(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"models": models, "counts": counts, "modalities": modalityCatalog()})
}

// maxModelDisplayNameLength bounds admin-set model display names. Native model
// names have no schema-level limit (providers keep them well under 100 bytes
// in practice), so this mirrors that practical bound generously while keeping
// aliases usable in dropdowns, logs, and the `model` field of API calls.
const maxModelDisplayNameLength = 200

func (s *Server) handlePatchModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	model, err := s.Store.ModelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Status           *string                  `json:"status"`
		DisplayName      *string                  `json:"display_name"`
		ContextWindow    modelPatchValue[int64]   `json:"context_window"`
		RateIn           modelPatchValue[float64] `json:"rate_in_usd_per_mtok"`
		RateOut          modelPatchValue[float64] `json:"rate_out_usd_per_mtok"`
		RateCached       modelPatchValue[float64] `json:"rate_cached_usd_per_mtok"`
		RateCacheWrite5m modelPatchValue[float64] `json:"rate_cache_write_5m_usd_per_mtok"`
		RateCacheWrite1h modelPatchValue[float64] `json:"rate_cache_write_1h_usd_per_mtok"`
		EffectiveFrom    string                   `json:"effective_from"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	patch := store.ModelPatch{DisplayName: body.DisplayName, Status: body.Status, Overrides: map[string]*int64{},
		AllowUnpriced: s.Config != nil && s.Config.LocalOnly}
	for _, rate := range []struct {
		name, field string
		value       modelPatchValue[float64]
	}{
		{"rate_in_usd_per_mtok", "rate_in_nanousd", body.RateIn},
		{"rate_out_usd_per_mtok", "rate_out_nanousd", body.RateOut},
		{"rate_cached_usd_per_mtok", "rate_cached_nanousd", body.RateCached},
		{"rate_cache_write_5m_usd_per_mtok", "rate_cache_write_5m_nanousd", body.RateCacheWrite5m},
		{"rate_cache_write_1h_usd_per_mtok", "rate_cache_write_1h_nanousd", body.RateCacheWrite1h},
	} {
		if !rate.value.Set {
			continue
		}
		var nano *int64
		if v := rate.value.Value; v != nil {
			if *v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0) || math.Round(*v*1e9) >= math.Exp2(63) {
				WriteError(w, r, ErrInvalidRequest("Rate must be nonnegative and fit in nano-USD storage.").WithParam(rate.name))
				return
			}
			n := usage.NanoFromUSD(*v)
			nano = &n
		}
		patch.Overrides[rate.field] = nano
	}
	if body.ContextWindow.Set {
		patch.Overrides["context_window"] = body.ContextWindow.Value
	}
	if body.EffectiveFrom != "" {
		at, err := time.Parse(time.RFC3339, body.EffectiveFrom)
		if err != nil {
			WriteError(w, r, ErrInvalidRequest("effective_from must be an RFC 3339 timestamp.").WithParam("effective_from"))
			return
		}
		patch.EffectiveFrom = at.UTC()
	}
	// Taking a model out of service breaks every managed model pointing at
	// it: those aliases keep resolving by name but can no longer be served.
	// Refuse the change and name the affected aliases rather than letting an
	// admin silently break "current-best" for everyone. The admin repoints or
	// disables the aliases first, then retries — an explicit two-step, which
	// is the right shape for a destructive change with invisible blast radius.
	if body.Status != nil && *body.Status != store.ModelEnabled && model.Status == store.ModelEnabled {
		dependents, err := s.Store.ListManagedModels(r.Context(), store.ManagedModelFilter{TargetModelID: id})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		active := []string{}
		for _, mm := range dependents {
			if mm.Status == store.ManagedModelEnabled {
				active = append(active, mm.Name)
			}
		}
		if len(active) > 0 && r.URL.Query().Get("force") != "true" {
			WriteError(w, r, ErrInvalidRequest(fmt.Sprintf(
				"This model is the target of %d enabled managed model(s): %s. Repoint or disable them first, "+
					"or retry with ?force=true to break them deliberately.",
				len(active), strings.Join(active, ", "))).WithParam("status"))
			return
		}
	}

	if err := s.Store.PatchModel(r.Context(), id, patch); err != nil {
		var validation *store.ValidationError
		if errors.As(err, &validation) {
			WriteError(w, r, ErrInvalidRequest(validation.Message).WithParam(validation.Field))
			return
		}
		WriteError(w, r, err)
		return
	}
	updated, err := s.Store.ModelByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	if body.DisplayName != nil && model.DisplayName != updated.DisplayName {
		s.InvalidateModelNameCache(model.DisplayName, model.Name, updated.DisplayName)
		s.audit(r, "model_renamed", "model", id, map[string]any{"display_name": model.DisplayName}, map[string]any{"display_name": updated.DisplayName})
	}
	if body.Status != nil || len(patch.Overrides) > 0 {
		s.audit(r, "model_updated", "model", id, modelMetadataAudit(model), modelMetadataAudit(updated))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"model": updated})
}

// Presence, null and zero are distinct in a metadata PATCH.
type modelPatchValue[T any] struct {
	Set   bool
	Value *T
}

func (v *modelPatchValue[T]) UnmarshalJSON(raw []byte) error {
	v.Set = true
	return json.Unmarshal(raw, &v.Value)
}
func modelMetadataAudit(m *store.Model) map[string]any {
	return map[string]any{
		"status": m.Status, "context_window": m.ContextWindow, "metadata": m.Metadata,
		"rate_in_usd_per_mtok": usage.USD(m.RateInNano), "rate_out_usd_per_mtok": usage.USD(m.RateOutNano), "rate_cached_usd_per_mtok": usage.USD(m.RateCachedNano),
		"rate_cache_write_5m_usd_per_mtok": usage.USD(m.RateCacheWrite5mNano), "rate_cache_write_1h_usd_per_mtok": usage.USD(m.RateCacheWrite1hNano),
	}
}

// --- Grants ------------------------------------------------------------------

func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.Store.ListGrants(r.Context(), r.URL.Query().Get("model_id"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelIDs    []string `json:"model_ids"`
		ModelID     string   `json:"model_id"`
		GranteeType string   `json:"grantee_type"`
		GranteeID   string   `json:"grantee_id"`
		// ModelKind is "model" (default) or "managed"; it says whether the
		// ids above name real catalog models or managed aliases.
		ModelKind string `json:"model_kind"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	models := body.ModelIDs
	if body.ModelID != "" {
		models = append(models, body.ModelID)
	}
	if len(models) == 0 {
		WriteError(w, r, ErrInvalidRequest("Select at least one model to grant.").WithParam("model_ids"))
		return
	}
	// model_kind selects whether the ids name real catalog models or managed
	// aliases. It defaults to "model" so existing API consumers are unchanged.
	modelKind := body.ModelKind
	if modelKind == "" {
		modelKind = store.ModelKindModel
	}
	if modelKind != store.ModelKindModel && modelKind != store.ModelKindManaged {
		WriteError(w, r, ErrInvalidRequest("model_kind must be model or managed.").WithParam("model_kind"))
		return
	}
	switch body.GranteeType {
	case store.GranteeAllUsers, store.GranteeAllServiceTokens, store.GranteeAllTeams:
	case store.GranteeUser, store.GranteeGroup, store.GranteeServiceToken, store.GranteeTeam:
		if body.GranteeID == "" {
			WriteError(w, r, ErrInvalidRequest("Choose who the grant applies to.").WithParam("grantee_id"))
			return
		}
		// Reject a grant aimed at something that no longer exists rather than
		// persisting a dangling row that shows as a raw id in the matrix.
		exists, err := s.Store.GranteeExists(r.Context(), body.GranteeType, body.GranteeID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if !exists {
			WriteError(w, r, ErrInvalidRequest("That grantee no longer exists.").WithParam("grantee_id"))
			return
		}
	default:
		WriteError(w, r, ErrInvalidRequest(
			"grantee_type must be user, group, all_users, service_token, all_service_tokens, team, or all_teams.").WithParam("grantee_type"))
		return
	}
	// Validate every target up front so a partially-applied batch cannot
	// leave some grants created and others rejected.
	for _, modelID := range models {
		var err error
		if modelKind == store.ModelKindManaged {
			_, err = s.Store.ManagedModelByID(r.Context(), modelID)
		} else {
			_, err = s.Store.ModelByID(r.Context(), modelID)
		}
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, r, ErrInvalidRequest("One of the selected models no longer exists.").WithParam("model_ids"))
			return
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
	}
	created := []*store.Grant{}
	for _, modelID := range models {
		g, err := s.Store.CreateGrant(r.Context(), modelID, modelKind, body.GranteeType, body.GranteeID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		created = append(created, g)
		s.audit(r, "grant_created", "grant", g.ID, nil, map[string]any{
			"model_id": modelID, "model_kind": modelKind,
			"grantee_type": body.GranteeType, "grantee_id": body.GranteeID,
		})
	}
	s.InvalidateConfigCache()
	WriteJSON(w, http.StatusCreated, map[string]any{"grants": created})
}

func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteGrant(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "grant_deleted", "grant", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// --- Users, groups, teams ----------------------------------------------------

// handleAdminListUsers serves GET /admin/v1/users. The listing is
// filterable and sortable entirely through query parameters, which the admin
// console's People page constructs from its filter/sort controls:
//
//   - search:   case-insensitive substring match on name or email
//   - role:     user | team_lead | admin
//   - group_id: only members of that group
//   - team_id:  only members of that team
//   - active:   active | inactive (empty = all accounts)
//   - sort:     email | name | created | last_login, each accepting an
//     explicit _asc/_desc suffix (default email ascending)
//   - limit / offset: pagination (default limit 50, offset 0)
//
// Every row still carries the 30-day usage aggregate (spend_30d_usd,
// requests_30d) regardless of the active filter/sort combination.
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	filter := store.UserFilter{
		Search: r.URL.Query().Get("search"), Role: r.URL.Query().Get("role"),
		GroupID: r.URL.Query().Get("group_id"), TeamID: r.URL.Query().Get("team_id"),
		Active: r.URL.Query().Get("active"), Sort: r.URL.Query().Get("sort"),
		Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	}
	users, total, err := s.Store.ListUsers(r.Context(), filter)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	rows := make([]map[string]any, 0, len(users))
	for _, u := range users {
		totals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{UserID: u.ID, Start: now.AddDate(0, 0, -30), End: now})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		groups, err := s.Store.GroupNamesForUser(r.Context(), u.ID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		rows = append(rows, map[string]any{
			"id": u.ID, "email": u.Email, "name": u.Name, "role": u.Role, "is_active": u.IsActive,
			"created_at": u.CreatedAt, "last_login_at": u.LastLoginAt, "groups": groups,
			"spend_30d_usd": usage.USD(totals.CostNano), "requests_30d": totals.Requests,
			"tokens_out_30d": totals.TokensOut,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"users": rows, "total_count": total})
}

func (s *Server) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	user, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That user"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	groupIDs, teamIDs, err := s.principalContext(r.Context(), user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	granted, err := s.Store.GrantedModelIDs(r.Context(), user.ID, groupIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	all, err := s.Store.ListModels(r.Context(), store.ModelFilter{})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	effective := []map[string]any{}
	for _, m := range all {
		if source, ok := granted[m.ID]; ok {
			effective = append(effective, map[string]any{"model": m.Name, "upstream": m.UpstreamName, "source": source, "status": m.Status})
		}
	}
	tokens, err := s.Store.ListTokens(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	quotas, err := s.Quota.Status(r.Context(), user.ID, teamIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	quotaPayload := make([]map[string]any, 0, len(quotas))
	for _, q := range quotas {
		quotaPayload = append(quotaPayload, quotaStatusPayload(q))
	}
	// The drawer's recent-requests table accepts the same token-size
	// thresholds and sort vocabulary as /api/v1/requests (tokens_in_gt,
	// tokens_out_lt, sort=tokens_in…), so an admin can narrow one user's
	// recent traffic to large prompts or long completions without leaving the
	// People page. The 25-row window is applied after the filter.
	recentFilter := store.RequestFilter{UserID: user.ID, Limit: 25, Sort: r.URL.Query().Get("requests_sort")}
	applyTokenSizeParams(r, &recentFilter)
	recent, _, err := s.Store.ListRequests(r.Context(), recentFilter)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	groups, err := s.Store.GroupNamesForUser(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teams, err := s.Store.TeamsForUser(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	auditEntries, _, err := s.Store.ListAudit(r.Context(), store.AuditFilter{Actor: user.ID, Limit: 25})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Per-user usage-over-time for the detail drawer's graph. ?range= mirrors
	// the overview endpoint, but the default is a month of daily buckets so a
	// first open shows a meaningful trend rather than a single day.
	if r.URL.Query().Get("range") == "" {
		q := r.URL.Query()
		q.Set("range", "month")
		r.URL.RawQuery = q.Encode()
	}
	start, end, label := rangeBounds(r)
	usageScope := store.UsageScope{UserID: user.ID, Start: start, End: end}
	bucket, buckets := seriesShape(label)
	series, err := s.Store.UsageSeries(r.Context(), usageScope, bucket, buckets)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	usageTotals, err := s.Store.AggregateUsage(r.Context(), usageScope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"user": user, "groups": groups, "teams": teams, "effective_grants": effective,
		"tokens": tokens, "quotas": quotaPayload, "recent_requests": recent, "audit": auditEntries,
		"usage_range": label, "usage_series": series, "usage_totals": usageTotals,
	})
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := UserFrom(r.Context())
	user, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That user"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Role     *string   `json:"role"`
		IsActive *bool     `json:"is_active"`
		TeamIDs  *[]string `json:"team_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	role, active := user.Role, user.IsActive
	if body.Role != nil {
		switch *body.Role {
		case store.RoleUser, store.RoleTeamLead, store.RoleAdmin:
			role = *body.Role
		default:
			WriteError(w, r, ErrInvalidRequest("Role must be user, team_lead, or admin.").WithParam("role"))
			return
		}
	}
	if body.IsActive != nil {
		active = *body.IsActive
	}
	// Refuse to leave the deployment with no administrator.
	if user.IsAdmin() && (role != store.RoleAdmin || !active) {
		admins, err := s.Store.AdminUserIDs(r.Context())
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if len(admins) <= 1 {
			WriteError(w, r, ErrInvalidRequest("This is the last active administrator. Promote another user before changing this one."))
			return
		}
	}
	if actor != nil && actor.ID == user.ID && !active {
		WriteError(w, r, ErrInvalidRequest("You cannot disable your own account."))
		return
	}
	if err := s.Store.UpdateUserAndTeams(r.Context(), id, role, active, body.TeamIDs); err != nil {
		teamHTTPError(w, r, err)
		return
	}
	if !active {
		// A disabled account must stop proxying at once, not after a cache TTL.
		s.InvalidateTokenCache()
	}
	s.audit(r, "user_updated", "user", id,
		map[string]any{"role": user.Role, "is_active": user.IsActive},
		map[string]any{"role": role, "is_active": active})
	updated, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"user": updated})
}

// handleDeleteUser performs the documented soft delete: the account is marked
// disabled and every credential it holds is revoked at once. Usage
// events and audit history are retained — nothing cascades.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := UserFrom(r.Context())
	user, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That user"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if actor != nil && actor.ID == user.ID {
		WriteError(w, r, ErrInvalidRequest("You cannot delete your own account."))
		return
	}
	// Refuse to leave the deployment with no administrator.
	if user.IsAdmin() {
		admins, err := s.Store.AdminUserIDs(r.Context())
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if len(admins) <= 1 {
			WriteError(w, r, ErrInvalidRequest("This is the last active administrator. Promote another user before deleting this one."))
			return
		}
	}
	if err := s.Store.UpdateUser(r.Context(), id, user.Role, false); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.RevokeTokensForUser(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	// A deleted account must stop proxying at once, not after a cache TTL.
	s.InvalidateTokenCache()
	s.audit(r, "user_deleted", "user", id,
		map[string]any{"email": user.Email, "role": user.Role, "is_active": user.IsActive},
		map[string]any{"is_active": false, "tokens_revoked": true})
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true, "tokens_revoked": true})
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Store.ListGroups(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the group a name.").WithParam("name"))
		return
	}
	group, err := s.Store.CreateGroup(r.Context(), strings.TrimSpace(body.Name))
	if err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("name"))
		return
	}
	s.audit(r, "group_created", "group", group.ID, nil, map[string]any{"name": group.Name})
	WriteJSON(w, http.StatusCreated, map[string]any{"group": group})
}

func (s *Server) handlePatchGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Name    *string   `json:"name"`
		Members *[]string `json:"member_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	groups, err := s.Store.ListGroups(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for _, g := range groups {
		if g.ID == id && g.FromIDP {
			WriteError(w, r, ErrInvalidRequest("Groups mirrored from the identity provider are read-only. Change membership in the identity provider instead."))
			return
		}
	}
	if body.Name != nil {
		if err := s.Store.RenameGroup(r.Context(), id, strings.TrimSpace(*body.Name)); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if body.Members != nil {
		if err := s.Store.SetGroupMembers(r.Context(), id, *body.Members); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	// Group membership feeds grant resolution on the proxy hot path.
	s.InvalidateConfigCache()
	s.audit(r, "group_updated", "group", id, nil, map[string]any{"name": body.Name, "members_set": body.Members != nil})
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteGroup(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "group_deleted", "group", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.Store.ListTeams(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	rows := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		members, err := s.Store.TeamMemberIDs(r.Context(), t.ID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		totals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{TeamIDs: []string{t.ID}, Start: now.AddDate(0, 0, -30), End: now})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		rows = append(rows, map[string]any{
			"id": t.ID, "name": t.Name, "lead_user_id": t.LeadUserID, "lead_name": t.LeadName,
			"lead_can_edit_quotas": t.LeadCanEditQuotas, "member_count": len(members),
			"member_ids": members, "spend_30d_usd": usage.USD(totals.CostNano), "listed": t.Listed,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"teams": rows})
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		LeadUserID string `json:"lead_user_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the team a name.").WithParam("name"))
		return
	}
	team, err := s.Store.CreateTeam(r.Context(), strings.TrimSpace(body.Name), body.LeadUserID)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("That team could not be created: "+err.Error()))
		return
	}
	s.audit(r, "team_created", "team", team.ID, nil, map[string]any{"name": team.Name, "lead_user_id": team.LeadUserID})
	WriteJSON(w, http.StatusCreated, map[string]any{"team": team})
}

// legacyTeamAuditDetail records effective counts without exposing roster PII.
func legacyTeamAuditDetail(r *http.Request, tx *store.Store, id string) (string, error) {
	members, err := tx.TeamMembers(r.Context(), id)
	if err != nil {
		return "", err
	}
	leaders, manual, directory := 0, 0, 0
	for _, m := range members {
		if m.Role == "leader" {
			leaders++
		}
		for _, source := range m.Sources {
			if source.SourceType == "manual" {
				manual++
			} else if source.SourceType == "group" {
				directory++
			}
		}
	}
	return fmt.Sprintf("Legacy team update; members=%d leaders=%d manual_sources=%d group_sources=%d", len(members), leaders, manual, directory), nil
}

func (s *Server) handlePatchTeam(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := UserFrom(r.Context())
	if actor == nil {
		WriteError(w, r, ErrUnauthenticated())
		return
	}
	var body struct {
		Name       *string   `json:"name"`
		LeadUserID *string   `json:"lead_user_id"`
		Delegate   *bool     `json:"lead_can_edit_quotas"`
		Listed     *bool     `json:"listed"`
		Members    *[]string `json:"member_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	err := s.Store.WithTeamAction(r.Context(), id, actor.ID, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(r.Context(), id, actor.ID)
		if err != nil {
			return err
		}
		if role != "admin" {
			return store.ErrTeamForbidden
		}
		team, err := tx.TeamByID(r.Context(), id)
		if err != nil {
			return err
		}
		name, lead, delegate := team.Name, team.LeadUserID, team.LeadCanEditQuotas
		if body.Name != nil {
			name = strings.TrimSpace(*body.Name)
			if name == "" {
				return ErrInvalidRequest("Give the team a name.").WithParam("name")
			}
		}
		if body.LeadUserID != nil {
			lead = *body.LeadUserID
		}
		if body.Delegate != nil {
			delegate = *body.Delegate
		}
		if err := tx.UpdateTeam(r.Context(), id, name, lead, delegate); err != nil {
			return err
		}
		if err := tx.UpdateTeamProfile(r.Context(), id, nil, body.Listed); err != nil {
			return err
		}
		if body.Members != nil {
			if err := tx.SetTeamMembers(r.Context(), id, *body.Members); err != nil {
				return err
			}
		}
		detail, err := legacyTeamAuditDetail(r, tx, id)
		if err != nil {
			return err
		}
		return tx.TeamActionEvent(r.Context(), id, actor.ID, "team_updated", "", detail)
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteTeam(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "team_deleted", "team", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// --- Quotas ------------------------------------------------------------------

func (s *Server) handleListQuotas(w http.ResponseWriter, r *http.Request) {
	quotas, err := s.Store.ListQuotas(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(quotas))
	for _, q := range quotas {
		status, err := s.Quota.StatusFor(r.Context(), q)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		rows = append(rows, quotaStatusPayload(status))
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"quotas":  rows,
		"metrics": s.quotaMetricOptions(),
		"windows": []map[string]string{
			{"value": store.WindowDaily, "label": "Per day"},
			{"value": store.WindowWeekly, "label": "Per week"},
			{"value": store.WindowMonthly, "label": "Per month"},
			{"value": store.WindowRolling24, "label": "Rolling 24 hours"},
			{"value": store.WindowRolling7, "label": "Rolling 7 days"},
			{"value": store.WindowRolling30, "label": "Rolling 30 days"},
		},
	})
}

// quotaMetricOptions lists the quota metrics offered to quota-creation UIs.
// In local-only mode (JANUS_LOCAL_ONLY) cost tracking is disabled, so the
// Spend (USD) metric is withheld — the create dialogs then never offer it.
func (s *Server) quotaMetricOptions() []map[string]string {
	out := []map[string]string{
		{"value": store.MetricTokensIn, "label": "Input tokens"},
		{"value": store.MetricTokensOut, "label": "Output tokens"},
	}
	if s.Config == nil || !s.Config.LocalOnly {
		out = append(out, map[string]string{"value": store.MetricCostUSD, "label": "Spend (USD)"})
	}
	return append(out, map[string]string{"value": store.MetricRequests, "label": "Requests"})
}

// localOnlyQuotaError rejects USD-metric quota writes while local-only mode is
// on: cost accrues zero there, so such a quota would be a silent no-op.
func (s *Server) localOnlyQuotaError(metric string) *APIError {
	if s.Config == nil || !s.Config.LocalOnly || metric != store.MetricCostUSD {
		return nil
	}
	return ErrInvalidRequest("Spend (USD) quotas are unavailable in local-only mode (JANUS_LOCAL_ONLY): cost tracking is disabled, so the quota would never accrue. Use token or request limits instead.").WithParam("metric")
}

type quotaPayload struct {
	SubjectType    string   `json:"subject_type"`
	SubjectID      string   `json:"subject_id"`
	ModelID        string   `json:"model_id"`
	Metric         string   `json:"metric"`
	Limit          *float64 `json:"limit_value"`
	Window         string   `json:"window"`
	BreachBehavior string   `json:"breach_behavior"`
	// AlertThresholds configures the documented warning percentages for this rule.
	// nil keeps the current value (or the 80/95 defaults on create); an empty
	// list restores the defaults explicitly.
	AlertThresholds *[]int `json:"alert_thresholds"`
}

// quotaWindowRetentionError rejects event-backed windows that outlive usage
// retention. Team quotas use event attribution even for calendar windows so
// history reassignment is reflected; other subjects retain their durable
// calendar ledger. The optional subject preserves callers checking only rolling
// windows. Persisted coverage is checked at enforcement time too: increasing
// retention cannot restore already-purged history.
func (s *Server) quotaWindowRetentionError(window string, subjectType ...string) *APIError {
	days := quota.RollingWindowDays(window)
	if days == 0 && len(subjectType) > 0 && subjectType[0] == "team" {
		switch window {
		case store.WindowDaily:
			days = 1
		case store.WindowWeekly:
			days = 7
		case store.WindowMonthly:
			days = 31
		}
	}
	if days == 0 || s.Config == nil || s.Config.UsageRetentionDays >= days {
		return nil
	}
	return ErrInvalidRequest(fmt.Sprintf(
		"A %s window needs %d days of usage history, but this deployment retains only %d (JANUS_USAGE_RETENTION_DAYS): the quota would silently undercount. Raise the retention or choose a shorter window.",
		quota.WindowLabel(window), days, s.Config.UsageRetentionDays)).WithParam("window")
}

// resolveAlertThresholds validates and normalises a submitted threshold list,
// falling back to the existing value when the field was omitted. Values must be
// whole percentages in 1–100; duplicates collapse; the result is ascending.
func resolveAlertThresholds(submitted *[]int, existing []int) ([]int, *APIError) {
	if submitted == nil {
		return existing, nil
	}
	if len(*submitted) > 10 {
		return nil, ErrInvalidRequest("At most 10 alert thresholds can be configured per quota.").WithParam("alert_thresholds")
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(*submitted))
	for _, t := range *submitted {
		if t < 1 || t > 100 {
			return nil, ErrInvalidRequest("Alert thresholds must be whole percentages between 1 and 100.").WithParam("alert_thresholds")
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Ints(out)
	return out, nil
}

func (s *Server) handleCreateQuota(w http.ResponseWriter, r *http.Request) {
	var body quotaPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	// service_token is a first-class quota subject: bounding an unattended
	// integration is exactly the control an admin needs when a credential is
	// embedded in a website or agent that might loop.
	if body.SubjectType != "user" && body.SubjectType != "team" && body.SubjectType != "service_token" {
		WriteError(w, r, ErrInvalidRequest("subject_type must be user, team, or service_token.").WithParam("subject_type"))
		return
	}
	if body.SubjectID == "" {
		WriteError(w, r, ErrInvalidRequest("Choose who this quota applies to.").WithParam("subject_id"))
		return
	}
	if body.SubjectType == "service_token" {
		if _, err := s.Store.ServiceTokenByID(r.Context(), body.SubjectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, r, ErrInvalidRequest("That service token no longer exists.").WithParam("subject_id"))
				return
			}
			WriteError(w, r, err)
			return
		}
	}
	if !quota.ValidMetric(body.Metric) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported metric.").WithParam("metric"))
		return
	}
	if apiErr := s.localOnlyQuotaError(body.Metric); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if !quota.ValidWindow(body.Window) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported window.").WithParam("window"))
		return
	}
	if apiErr := s.quotaWindowRetentionError(body.Window, body.SubjectType); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if body.Limit == nil || *body.Limit <= 0 {
		WriteError(w, r, ErrInvalidRequest("The limit must be greater than zero.").WithParam("limit_value"))
		return
	}
	behavior := body.BreachBehavior
	if behavior != store.BreachHardKill {
		behavior = store.BreachLetFinish
	}
	limit := int64(*body.Limit)
	if body.Metric == store.MetricCostUSD {
		limit = usage.NanoFromUSD(*body.Limit)
	}
	thresholds, apiErr := resolveAlertThresholds(body.AlertThresholds, nil)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	q := &store.Quota{
		SubjectType: body.SubjectType, SubjectID: body.SubjectID, ModelID: body.ModelID,
		Metric: body.Metric, Limit: limit, Window: body.Window, BreachBehavior: behavior,
		AlertThresholds: thresholds,
	}
	if err := s.Store.CreateQuota(r.Context(), q); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_created", "quota", q.ID, nil, map[string]any{
		"subject_type": q.SubjectType, "subject_id": q.SubjectID, "metric": q.Metric,
		"limit": *body.Limit, "window": q.Window, "breach_behavior": q.BreachBehavior,
		"alert_thresholds": q.AlertThresholds,
	})
	WriteJSON(w, http.StatusCreated, map[string]any{"quota": q})
}

func (s *Server) handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.QuotaByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That quota"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if apiErr := s.localOnlyQuotaError(existing.Metric); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	var body quotaPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Limit == nil || *body.Limit <= 0 {
		WriteError(w, r, ErrInvalidRequest("The limit must be greater than zero.").WithParam("limit_value"))
		return
	}
	window := body.Window
	if window == "" {
		window = existing.Window
	}
	if !quota.ValidWindow(window) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported window.").WithParam("window"))
		return
	}
	if apiErr := s.quotaWindowRetentionError(window, existing.SubjectType); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	behavior := body.BreachBehavior
	if behavior != store.BreachHardKill {
		behavior = store.BreachLetFinish
	}
	limit := int64(*body.Limit)
	if existing.Metric == store.MetricCostUSD {
		limit = usage.NanoFromUSD(*body.Limit)
	}
	thresholds, apiErr := resolveAlertThresholds(body.AlertThresholds, existing.AlertThresholds)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.UpdateQuota(r.Context(), id, limit, window, behavior, thresholds); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_updated", "quota", id,
		map[string]any{"limit": existing.Limit, "window": existing.Window, "breach_behavior": existing.BreachBehavior, "alert_thresholds": existing.AlertThresholds},
		map[string]any{"limit": limit, "window": window, "breach_behavior": behavior, "alert_thresholds": thresholds})
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

func (s *Server) handleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteQuota(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_deleted", "quota", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// --- Blocking rules ----------------------------------------------------------

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Store.ListBlockingRules(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"rules":        rules,
		"clause_types": authz.ClauseTypes(),
		"caveat":       "Policy controls, not security — every signal except the bearer token is user-spoofable.",
	})
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string             `json:"name"`
		Combinator string             `json:"combinator"`
		Reason     string             `json:"reason"`
		Enabled    *bool              `json:"enabled"`
		Clauses    []store.RuleClause `json:"clauses"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the rule a name so it can be recognised in the audit log.").WithParam("name"))
		return
	}
	if len(body.Clauses) == 0 {
		WriteError(w, r, ErrInvalidRequest("Add at least one condition.").WithParam("clauses"))
		return
	}
	for i, clause := range body.Clauses {
		if err := authz.ValidateClause(clause); err != nil {
			WriteError(w, r, ErrInvalidRequest(fmt.Sprintf("Condition %d is invalid: %v", i+1, err)).WithParam(fmt.Sprintf("clauses.%d.pattern", i)))
			return
		}
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	combinator := "and"
	if strings.EqualFold(body.Combinator, "or") {
		combinator = "or"
	}
	rule := &store.BlockingRule{
		Name: strings.TrimSpace(body.Name), Combinator: combinator,
		Reason: body.Reason, Enabled: enabled, Clauses: body.Clauses,
	}
	if err := s.Store.CreateBlockingRule(r.Context(), rule); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rule_created", "blocking_rule", rule.ID, nil, map[string]any{
		"name": rule.Name, "combinator": rule.Combinator, "clauses": rule.Clauses,
	})
	WriteJSON(w, http.StatusCreated, map[string]any{"rule": rule})
}

// handleUpdateRule edits a blocking rule in place. The rule keeps its
// id, hit counter, and audit continuity — fixing a typo must not require
// delete + recreate.
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.BlockingRuleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That blocking rule"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Name       string             `json:"name"`
		Combinator string             `json:"combinator"`
		Reason     string             `json:"reason"`
		Enabled    *bool              `json:"enabled"`
		Clauses    []store.RuleClause `json:"clauses"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the rule a name so it can be recognised in the audit log.").WithParam("name"))
		return
	}
	if len(body.Clauses) == 0 {
		WriteError(w, r, ErrInvalidRequest("Add at least one condition.").WithParam("clauses"))
		return
	}
	for i, clause := range body.Clauses {
		if err := authz.ValidateClause(clause); err != nil {
			WriteError(w, r, ErrInvalidRequest(fmt.Sprintf("Condition %d is invalid: %v", i+1, err)).WithParam(fmt.Sprintf("clauses.%d.pattern", i)))
			return
		}
	}
	enabled := existing.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	combinator := "and"
	if strings.EqualFold(body.Combinator, "or") {
		combinator = "or"
	}
	updated := &store.BlockingRule{
		ID: existing.ID, Name: strings.TrimSpace(body.Name), Combinator: combinator,
		Reason: body.Reason, Enabled: enabled, Clauses: body.Clauses,
		HitCount: existing.HitCount, LastHitAt: existing.LastHitAt, CreatedAt: existing.CreatedAt,
	}
	if err := s.Store.UpdateBlockingRule(r.Context(), updated); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rule_updated", "blocking_rule", existing.ID,
		map[string]any{"name": existing.Name, "combinator": existing.Combinator, "reason": existing.Reason, "enabled": existing.Enabled, "clauses": existing.Clauses},
		map[string]any{"name": updated.Name, "combinator": updated.Combinator, "reason": updated.Reason, "enabled": updated.Enabled, "clauses": updated.Clauses})
	WriteJSON(w, http.StatusOK, map[string]any{"rule": updated})
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteBlockingRule(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "rule_deleted", "blocking_rule", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// handleTestRule evaluates the stored rule set against a sample request so an
// admin can confirm behaviour before real traffic is affected.
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserAgent    string            `json:"user_agent"`
		SourceIP     string            `json:"source_ip"`
		ForwardedFor string            `json:"x_forwarded_for"`
		Path         string            `json:"path"`
		Method       string            `json:"method"`
		Headers      map[string]string `json:"headers"`
		TokenPrefix  string            `json:"token_prefix"`
		Model        string            `json:"model"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	rules, err := s.Store.ListBlockingRules(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	header := http.Header{}
	for k, v := range body.Headers {
		header.Set(k, v)
	}
	if body.Method == "" {
		body.Method = http.MethodPost
	}
	if body.Path == "" {
		body.Path = "/v1/chat/completions"
	}
	sig := authz.RequestSignals{
		UserAgent: body.UserAgent, SourceIP: body.SourceIP, ForwardedFor: body.ForwardedFor,
		Path: body.Path, Method: body.Method, Header: header,
		TokenPrefix: body.TokenPrefix, Model: body.Model,
	}
	rule, clause := authz.Match(rules, sig)
	if rule == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"matched": false, "detail": "No rule blocks this request."})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"matched": true, "rule_id": rule.ID, "rule_name": rule.Name,
		"clause": clause, "reason": rule.Reason,
	})
}

// --- Alerts ------------------------------------------------------------------

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Store.ListAlertRules(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"alerts": rules, "email_enabled": s.Alerts.EmailEnabled(),
		"email_detail": smtpDetail(s.Alerts.EmailEnabled()),
		"triggers": []map[string]string{
			{"value": "quota_80", "label": "Quota warning below 95% (default 80%)"},
			{"value": "quota_95", "label": "Quota warning at 95% or above"},
			{"value": "quota_breach", "label": "Quota breached"},
			{"value": "upstream_down", "label": "Upstream unreachable"},
			{"value": "system", "label": "System events"},
			{"value": "all", "label": "Everything"},
		},
	})
}

func (s *Server) handleCreateAlert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Trigger    string   `json:"trigger"`
		Severity   string   `json:"severity"`
		Channels   []string `json:"channels"`
		WebhookURL string   `json:"webhook_url"`
		Enabled    *bool    `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Trigger == "" {
		WriteError(w, r, ErrInvalidRequest("Choose what this alert responds to.").WithParam("trigger"))
		return
	}
	if len(body.Channels) == 0 {
		body.Channels = []string{"in_app"}
	}
	if err := s.requireEmailChannelLicensed(body.Channels); err != nil {
		WriteError(w, r, err)
		return
	}
	for _, c := range body.Channels {
		if c == "webhook" && strings.TrimSpace(body.WebhookURL) == "" {
			WriteError(w, r, ErrInvalidRequest("A webhook URL is required when the webhook channel is selected.").WithParam("webhook_url"))
			return
		}
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	severity := body.Severity
	if severity == "" {
		severity = "warning"
	}
	rule := &store.AlertRule{
		Trigger: body.Trigger, Severity: severity, Channels: body.Channels,
		WebhookURL: strings.TrimSpace(body.WebhookURL), Enabled: enabled,
	}
	if err := s.Store.CreateAlertRule(r.Context(), rule); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "alert_created", "alert_rule", rule.ID, nil, map[string]any{"trigger": rule.Trigger, "channels": rule.Channels})
	WriteJSON(w, http.StatusCreated, map[string]any{"alert": rule})
}

// handleUpdateAlert edits an alert delivery rule in place, keeping
// its id and audit continuity.
func (s *Server) handleUpdateAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.AlertRuleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That alert rule"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Trigger    string   `json:"trigger"`
		Severity   string   `json:"severity"`
		Channels   []string `json:"channels"`
		WebhookURL string   `json:"webhook_url"`
		Enabled    *bool    `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Trigger == "" {
		WriteError(w, r, ErrInvalidRequest("Choose what this alert responds to.").WithParam("trigger"))
		return
	}
	if len(body.Channels) == 0 {
		body.Channels = []string{"in_app"}
	}
	if err := s.requireEmailChannelLicensed(body.Channels); err != nil {
		WriteError(w, r, err)
		return
	}
	for _, c := range body.Channels {
		if c == "webhook" && strings.TrimSpace(body.WebhookURL) == "" {
			WriteError(w, r, ErrInvalidRequest("A webhook URL is required when the webhook channel is selected.").WithParam("webhook_url"))
			return
		}
	}
	enabled := existing.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	severity := body.Severity
	if severity == "" {
		severity = "warning"
	}
	updated := &store.AlertRule{
		ID: existing.ID, Trigger: body.Trigger, Severity: severity, Channels: body.Channels,
		WebhookURL: strings.TrimSpace(body.WebhookURL), Enabled: enabled, CreatedAt: existing.CreatedAt,
	}
	if err := s.Store.UpdateAlertRule(r.Context(), updated); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "alert_updated", "alert_rule", existing.ID,
		map[string]any{"trigger": existing.Trigger, "severity": existing.Severity, "channels": existing.Channels, "webhook_url": existing.WebhookURL, "enabled": existing.Enabled},
		map[string]any{"trigger": updated.Trigger, "severity": updated.Severity, "channels": updated.Channels, "webhook_url": updated.WebhookURL, "enabled": updated.Enabled})
	WriteJSON(w, http.StatusOK, map[string]any{"alert": updated})
}

func (s *Server) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteAlertRule(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "alert_deleted", "alert_rule", id, map[string]any{"id": id}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleTestAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rule, err := s.Store.AlertRuleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That alert rule"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Alerts.SendTest(r.Context(), rule); err != nil {
		WriteError(w, r, ErrInvalidRequest("The test alert could not be delivered — "+err.Error()))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"sent": true})
}

// --- Audit & analytics -------------------------------------------------------

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	start, end := time.Time{}, time.Time{}
	if v := r.URL.Query().Get("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			start = t
		}
	}
	if v := r.URL.Query().Get("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			end = t
		}
	}
	entries, total, err := s.Store.ListAudit(r.Context(), store.AuditFilter{
		Actor: r.URL.Query().Get("actor"), Action: r.URL.Query().Get("action"),
		ResourceType: r.URL.Query().Get("resource_type"), Start: start, End: end,
		Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "total_count": total,
		"note": "Audit entries are append-only and cannot be edited or deleted.",
	})
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	start, end, label := rangeBounds(r)
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "model"
	}
	// Analytics keeps the historical cost-first ordering; only leaderboard
	// surfaces (top users) rank by token volume.
	series, err := s.Store.BreakdownUsage(r.Context(), store.UsageScope{Start: start, End: end}, groupBy, store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("group_by must be one of model, modality, user, upstream, token, status, endpoint.").WithParam("group_by"))
		return
	}
	totals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{Start: start, End: end})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"group_by": groupBy, "range": label, "series": series, "totals": totals})
}

// --- Feature flags -----------------------------------------------------------
//
// These handlers are flag-name-agnostic: the set of valid flags is defined by
// store.DefaultFeatureFlags (e.g. spend_emphasis, which flips the default
// dashboard-graph metric between Spend and Tokens), validation happens in
// Store.SetFeatureFlag, and every successful PATCH is audit-logged with the
// full before/after flag maps and invalidates the config cache so /api/v1/me
// serves the new values immediately. Adding a flag therefore requires no
// change here: register it (with its shipped default — spend_emphasis ships
// off, i.e. usage emphasis) in DefaultFeatureFlags and these endpoints,
// /api/v1/me, and the admin system status all pick it up automatically.
// The web client reads the flag from /api/v1/me feature_flags via
// useDefaultMetric() (web/src/app/session.tsx) to choose Spend vs Tokens as
// the opening metric on the dashboard routes.

func (s *Server) handleGetFeatures(w http.ResponseWriter, r *http.Request) {
	flags, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"features": flags})
}

func (s *Server) handlePatchFeatures(w http.ResponseWriter, r *http.Request) {
	var body map[string]bool
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	before, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for name, enabled := range body {
		if err := s.Store.SetFeatureFlag(r.Context(), name, enabled); err != nil {
			WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam(name))
			return
		}
	}
	after, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "feature_flag_changed", "feature_flag", "", before, after)
	WriteJSON(w, http.StatusOK, map[string]any{"features": after})
}

func (s *Server) handleResolveFeedback(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.ResolveDocsFeedback(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "resolved": true})
}

// requireEmailChannelLicensed: the email channel needs email_alerts;
// in-app and webhook are Community.
func (s *Server) requireEmailChannelLicensed(channels []string) error {
	for _, c := range channels {
		if c == "email" {
			return s.requireFeature("email_alerts")
		}
	}
	return nil
}

// handleExportAudit streams the filtered audit log as CSV (Business:
// audit_export). Same filters as the list; no paging — the whole range.
func (s *Server) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	start, end := time.Time{}, time.Time{}
	if v := r.URL.Query().Get("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			start = t
		}
	}
	if v := r.URL.Query().Get("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			end = t
		}
	}
	filter := store.AuditFilter{
		Actor: r.URL.Query().Get("actor"), Action: r.URL.Query().Get("action"),
		ResourceType: r.URL.Query().Get("resource_type"), Start: start, End: end, Limit: 1000,
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"janus-audit-"+time.Now().UTC().Format("20060102-150405")+".csv\"")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"created_at", "actor", "actor_user_id", "action", "resource_type", "resource_id", "old_value", "new_value"})
	for {
		entries, _, err := s.Store.ListAudit(r.Context(), filter)
		if err != nil {
			s.Logger.WarnContext(r.Context(), "audit export", "error", err.Error())
			return
		}
		for _, e := range entries {
			_ = cw.Write([]string{e.CreatedAt.UTC().Format(time.RFC3339), e.ActorLabel, e.ActorUserID, e.Action, e.ResourceType, e.ResourceID, e.OldValue, e.NewValue})
		}
		if len(entries) < filter.Limit {
			break
		}
		filter.Offset += filter.Limit
	}
	cw.Flush()
	s.audit(r, "audit.export", "audit", "csv", nil, map[string]any{"action": filter.Action, "resource_type": filter.ResourceType})
}
