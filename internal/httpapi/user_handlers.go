package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// handleMe returns the caller's profile with groups, teams, and effective role.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
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
	sessionCount, err := s.Sessions.Count(r.Context(), user.ID)
	if err != nil {
		sessionCount = 1
	}
	flags, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	activeTeam, err := s.selectedTeamID(r.Context(), user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	leads := []string{}
	for _, t := range teams {
		if t.Role == "leader" {
			leads = append(leads, t.ID)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		// The EFFECTIVE role, not the stored column. A user promoted through
		// an admin group has full admin capability at the API, but their
		// sticky role stays "user" — reporting that here made the SPA hide
		// the admin navigation from someone the gateway was already treating
		// as an administrator. stored_role and admin_via_group travel
		// alongside so the UI can explain WHERE the capability comes from
		// rather than leaving the viewer to guess.
		"id": user.ID, "email": user.Email, "name": user.Name, "role": effectiveRole(user),
		"stored_role":     user.Role,
		"admin_via_group": user.AdminViaGroup,
		"is_active":       user.IsActive, "timezone": user.Timezone, "locale": user.Locale,
		"created_at": user.CreatedAt, "last_login_at": user.LastLoginAt,
		"groups": groups, "teams": teams, "leads_teams": leads, "active_team_id": activeTeam,
		"active_sessions": sessionCount, "feature_flags": flags,
		"endpoint": s.Config.PublicURL + "/v1",
		// Local-only mode (JANUS_LOCAL_ONLY): the SPA hides every
		// cost/spend/pricing surface when true.
		"local_only": s.Config.LocalOnly,
		// License banner state for the shell (edition/status only; the full
		// claims are admin-only under /admin/system/license).
		"license": s.licenseSummary(),
	})
}

// handleUpdatePreferences stores viewer-owned display settings.
func (s *Server) handleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Timezone string `json:"timezone"`
		Locale   string `json:"locale"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(body.Timezone) > 64 || len(body.Locale) > 32 {
		WriteError(w, r, ErrInvalidRequest("Timezone and locale values are too long.").WithParam("timezone"))
		return
	}
	if err := s.Store.UpdateUserPreferences(r.Context(), user.ID, body.Timezone, body.Locale); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"timezone": body.Timezone, "locale": body.Locale})
}

// handleRevokeSessions signs the caller out of every browser.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if err := s.Sessions.DeleteForUser(r.Context(), user.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// --- Tokens ------------------------------------------------------------------

// handleListTokens returns the caller's credentials, each with its 30-day
// usage aggregate. "Which of my tokens is actually doing the work" is the
// question this page exists to answer, and a last-used timestamp alone cannot:
// it distinguishes a token used once from one serving thousands of requests.
//
// Tokens serialize via store.Token.MarshalJSON, which emits revoked_at as ""
// while a token is active — the UI relies on the field being falsy to
// badge/filter tokens as active rather than revoked. The usage fields are
// merged on top of that encoding rather than replacing it.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	tokens, err := s.Store.ListTokens(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -30)
	rows := make([]map[string]any, 0, len(tokens))
	for _, tok := range tokens {
		// Re-encode through the token's own MarshalJSON so the revoked_at ==
		// "" contract above is preserved exactly, then decorate.
		encoded, err := json.Marshal(tok)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		row := map[string]any{}
		if err := json.Unmarshal(encoded, &row); err != nil {
			WriteError(w, r, err)
			return
		}
		totals, err := s.Store.AggregateUsage(r.Context(), store.UsageScope{
			UserID: user.ID, TokenID: tok.ID, Start: start, End: now,
		})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		models, err := s.Store.BreakdownUsage(r.Context(), store.UsageScope{
			UserID: user.ID, TokenID: tok.ID, Start: start, End: now,
		}, "model")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		topModel := ""
		if len(models) > 0 {
			topModel = models[0].Key
		}
		row["requests_30d"] = totals.Requests
		row["tokens_in_30d"] = totals.TokensIn
		row["tokens_out_30d"] = totals.TokensOut
		row["errors_30d"] = totals.ErrorCount
		row["spend_30d_usd"] = usage.USD(totals.CostNano)
		row["top_model_30d"] = topModel
		row["model_count_30d"] = len(models)
		rows = append(rows, row)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"tokens": rows})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		Description string `json:"description"`
		TeamID      string `json:"team_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	body.Description = strings.TrimSpace(body.Description)
	if body.Description == "" {
		WriteError(w, r, ErrInvalidRequest("Give the token a description so you can recognise it later.").WithParam("description"))
		return
	}
	if len(body.Description) > 100 {
		WriteError(w, r, ErrInvalidRequest("Descriptions are limited to 100 characters.").WithParam("description"))
		return
	}
	token, plaintext, err := s.Store.CreateTeamToken(r.Context(), user.ID, body.Description, body.TeamID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"token": token,
		// Returned exactly once: only the digest is persisted.
		"value":   plaintext,
		"warning": "Copy this value now. Janus stores only a hash and cannot show it again.",
	})
}

func (s *Server) handleTokenDetail(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	token, err := s.Store.TokenByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if token.UserID != user.ID && !user.IsAdmin() {
		WriteError(w, r, ErrForbidden("That token belongs to another user."))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	id := chi.URLParam(r, "id")
	token, err := s.Store.TokenByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if token.UserID != user.ID && !user.IsAdmin() {
		WriteError(w, r, ErrForbidden("That token belongs to another user."))
		return
	}
	if err := s.Store.RevokeToken(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	// Revocation must be immediate, so the credential cache is dropped at once.
	s.InvalidateTokenCache()
	if err := s.Store.AppendAudit(r.Context(), &store.AuditEntry{
		ActorUserID: user.ID, ActorLabel: user.Email, Action: "token_revoked",
		ResourceType: "token", ResourceID: id, OldValue: `{"revoked":false}`, NewValue: `{"revoked":true}`,
	}); err != nil {
		s.Logger.ErrorContext(r.Context(), "audit token revocation", "error", err.Error())
	}
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "revoked_at": time.Now().UTC()})
}

// --- Models ------------------------------------------------------------------

// handleListModels returns the models the caller may use, filtered by grants.
// Each entry carries both the native upstream name ("name") and the admin-set
// alias ("display_name", empty when never renamed) so the catalog UI can show
// the friendly name while keeping provider provenance visible — and so users
// can correlate a renamed model with the provider's own documentation.
//
// Managed models are returned in a parallel "managed_models" list rather than
// merged into "models": they are a different kind of thing (an alias with a
// visible target) and the UI renders them as their own card so a user can see
// what the alias currently points at.
//
// Each real model also carries live health: the owning upstream's last probe
// result (the same reachable/latency verdict the admin Upstreams page shows)
// and the model's own error rate over the trailing store.ModelHealthWindow, so
// a user can tell "this model is down right now" from "I am doing it wrong"
// before they open a ticket. See attachModelHealth.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	catalog, err := s.grantedCatalog(r.Context(), user, nil)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.attachModelHealth(r.Context(), catalog.Models); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"models":         catalog.Models,
		"managed_models": catalog.Managed,
	})
}

// attachModelHealth fills the health fields of every model in place: upstream
// probe state joined from the upstream table, and the 10-minute error rollup
// from usage_event (two queries total, whatever the catalog size). It is
// deliberately NOT part of grantedCatalog, which also backs the hot
// OpenAI-compatible GET /v1/models listing where SDKs neither need nor
// understand these fields.
func (s *Server) attachModelHealth(ctx context.Context, models []*store.Model) error {
	if len(models) == 0 {
		return nil
	}
	upstreams, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]*store.Upstream, len(upstreams))
	for _, up := range upstreams {
		byID[up.ID] = up
	}
	stats, err := s.Store.ModelHealthStats(ctx, time.Now().Add(-store.ModelHealthWindow))
	if err != nil {
		return err
	}
	for _, m := range models {
		m.SetHealth(byID[m.UpstreamID], stats[m.ID])
	}
	return nil
}

// GrantedCatalog is everything a principal may address: real models and
// managed aliases, each already labelled with how access was obtained.
type GrantedCatalog struct {
	Models  []*store.Model
	Managed []*store.ManagedModel
}

// grantedCatalog resolves the full addressable catalog for either principal
// kind. It is the single source of truth for "what may this caller use",
// shared by GET /api/v1/models and GET /v1/models so the web catalog and the
// OpenAI-compatible listing can never disagree.
//
// A managed alias is included only when it is servable: a disabled alias, or
// one whose underlying model has gone missing or been disabled, is withheld
// rather than advertised as usable. Broken aliases are an admin's problem to
// see (the admin console badges them), not something to offer to callers who
// would only get an error.
func (s *Server) grantedCatalog(ctx context.Context, user *store.User, serviceToken *store.ServiceToken) (GrantedCatalog, error) {
	subject := store.GrantSubject{}
	if serviceToken != nil {
		subject.ServiceTokenID = serviceToken.ID
	} else if user != nil {
		groupIDs, teamIDs, err := s.principalContext(ctx, user)
		if err != nil {
			return GrantedCatalog{}, err
		}
		subject.UserID = user.ID
		subject.GroupIDs = groupIDs
		if len(teamIDs) > 0 {
			subject.TeamID = teamIDs[0]
		}
	}
	granted, err := s.Store.GrantedIDsFor(ctx, subject)
	if err != nil {
		return GrantedCatalog{}, err
	}

	out := GrantedCatalog{Models: []*store.Model{}, Managed: []*store.ManagedModel{}}
	models, err := s.Store.ListModels(ctx, store.ModelFilter{Status: store.ModelEnabled})
	if err != nil {
		return GrantedCatalog{}, err
	}
	for _, m := range models {
		if source, ok := granted[m.ID]; ok {
			m.GrantSource = source
			out.Models = append(out.Models, m)
		}
	}
	managed, err := s.Store.ListManagedModels(ctx, store.ManagedModelFilter{Status: store.ManagedModelEnabled})
	if err != nil {
		return GrantedCatalog{}, err
	}
	for _, mm := range managed {
		source, ok := granted[mm.ID]
		if !ok || !mm.Servable {
			continue
		}
		mm.GrantSource = source
		out.Managed = append(out.Managed, mm)
	}
	return out, nil
}

func (s *Server) grantedModels(r *http.Request, user *store.User) ([]*store.Model, error) {
	catalog, err := s.grantedCatalog(r.Context(), user, nil)
	if err != nil {
		return nil, err
	}
	return catalog.Models, nil
}

// --- Request log -------------------------------------------------------------

// requestFilter builds the store.RequestFilter for the request-log endpoints.
// It is user-scoped by default (UserID = caller); admins may widen it with
// scope=all or retarget it with user_id=<id> (see the override notes below).
func (s *Server) requestFilter(r *http.Request, user *store.User) store.RequestFilter {
	start, end, _ := rangeBounds(r)
	f := store.RequestFilter{
		UserID:   user.ID,
		TokenID:  r.URL.Query().Get("token_id"), // Narrows the authorized user scope; never widens it.
		Model:    r.URL.Query().Get("model"),
		Modality: r.URL.Query().Get("modality"),
		Status:   r.URL.Query().Get("status"),
		Sort:     r.URL.Query().Get("sort"),
		Limit:    queryInt(r, "limit", 50),
		Offset:   queryInt(r, "offset", 0),
		// accounting=<token_accounting_method> lists the requests behind
		// one metering outcome (the System page links here for gaps).
		Accounting: r.URL.Query().Get("accounting"),
	}
	if r.URL.Query().Get("range") != "" {
		f.Start, f.End = start, end
	}
	// The gateway's own classifier calls are hidden unless asked for: they
	// share the caller's request_id so their cost stays attributable, but
	// they are platform overhead, not requests a person made. A request's
	// detail drawer shows its own classifier runs regardless.
	if user.IsAdmin() && r.URL.Query().Get("include_internal") == "true" {
		f.IncludeInternal = true
	}
	applyTokenSizeParams(r, &f)
	// A user's own request log is human traffic by definition: service-token
	// requests belong to no user, so they can never appear here.
	//
	// Admin overrides: scope=all widens to every user; user_id narrows to one
	// specific user and wins over scope=all so the admin log viewer can keep
	// scope=all in the URL while filtering by person.
	if user.IsAdmin() && r.URL.Query().Get("scope") == "all" {
		f.UserID = ""
		// scope=all means "everything the gateway served", including
		// integration traffic — an admin investigating an incident must be
		// able to see service-token requests in the same timeline.
		f.Principal = store.PrincipalAny
	}
	if user.IsAdmin() && r.URL.Query().Get("user_id") != "" {
		f.UserID = r.URL.Query().Get("user_id")
		f.Principal = store.PrincipalUsers
	}
	// Admins may narrow the log to one integration credential, which is what
	// the service-token detail page links into.
	if user.IsAdmin() && r.URL.Query().Get("service_token_id") != "" {
		f.UserID = ""
		f.ServiceTokenID = r.URL.Query().Get("service_token_id")
		f.Principal = store.PrincipalAny
	}
	// principal=service_tokens shows only integration traffic across the org.
	if user.IsAdmin() {
		switch r.URL.Query().Get("principal") {
		case "service_tokens":
			f.UserID = ""
			f.Principal = store.PrincipalServiceTokens
		case "users":
			f.Principal = store.PrincipalUsers
		}
	}
	return f
}

// applyTokenSizeParams reads the token-size threshold query parameters
// (tokens_in_gt, tokens_in_lt, tokens_out_gt, tokens_out_lt) onto a request
// filter. Each is a strict comparison; a missing or non-numeric value leaves
// that bound unset rather than failing the whole request, because these are
// optional narrowing filters on a read-only listing.
func applyTokenSizeParams(r *http.Request, f *store.RequestFilter) {
	parse := func(key string) *int64 {
		raw := strings.TrimSpace(r.URL.Query().Get(key))
		if raw == "" {
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	f.TokensInGT = parse("tokens_in_gt")
	f.TokensInLT = parse("tokens_in_lt")
	f.TokensOutGT = parse("tokens_out_gt")
	f.TokensOutLT = parse("tokens_out_lt")
}

// isAdminCrossUserRequestQuery reports whether the caller is an admin using
// one of the cross-user request-log overrides (scope=all or user_id). Only
// such responses carry the owning user's identity per event.
func isAdminCrossUserRequestQuery(r *http.Request, user *store.User) bool {
	if !user.IsAdmin() {
		return false
	}
	q := r.URL.Query()
	return q.Get("scope") == "all" || q.Get("user_id") != "" ||
		q.Get("service_token_id") != "" || q.Get("principal") != ""
}

// attachRequestOwners resolves each event's owning user into the transient
// UserEmail/UserName fields so admins can tell whose request a row is.
// Lookups are memoised per call; a deleted/unknown owner simply stays blank.
func (s *Server) attachRequestOwners(ctx context.Context, events []*store.UsageEvent) {
	cache := map[string]*store.User{}
	// Service-token names are resolved once for the whole page: a log of 500
	// integration requests should not issue 500 lookups.
	var serviceNames map[string]string
	for _, e := range events {
		if e.ServiceTokenID != "" {
			if serviceNames == nil {
				names, err := s.Store.ServiceTokenNames(ctx)
				if err != nil {
					names = map[string]string{}
				}
				serviceNames = names
			}
			if name, ok := serviceNames[e.ServiceTokenID]; ok {
				e.ServiceTokenName = name
			} else {
				e.ServiceTokenName = "Deleted service token"
			}
			continue
		}
		owner, seen := cache[e.UserID]
		if !seen {
			u, err := s.Store.UserByID(ctx, e.UserID)
			if err != nil {
				u = nil
			}
			owner = u
			cache[e.UserID] = owner
		}
		if owner != nil {
			e.UserEmail = owner.Email
			e.UserName = owner.Name
		}
	}
}

// handleListRequests serves GET /api/v1/requests. Responses are user-scoped
// unless an admin uses the scope=all / user_id overrides, in which case each
// event is enriched with the owning user's identity for the admin log viewer.
func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	events, total, err := s.Store.ListRequests(r.Context(), s.requestFilter(r, user))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if isAdminCrossUserRequestQuery(r, user) {
		s.attachRequestOwners(r.Context(), events)
		s.attachCaptureMarkers(r.Context(), events)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"requests": events, "total_count": total})
}

// handleExportRequests streams the current filter as CSV. Under an admin
// cross-user query (scope=all / user_id) the header gains user identity
// columns; the user-scoped export keeps its historical column set.
func (s *Server) handleExportRequests(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	filter := s.requestFilter(r, user)
	// Export up to 5000 rows. The store clamps any page larger than 500, so
	// page through it rather than asking for one oversized page (a single
	// Limit:5000 call would be silently clamped to the 50-row default).
	const exportCap = 5000
	const pageSize = 500
	var events []*store.UsageEvent
	total := 0
	for len(events) < exportCap {
		filter.Limit = pageSize
		filter.Offset = len(events)
		page, t, err := s.Store.ListRequests(r.Context(), filter)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		total = t
		events = append(events, page...)
		if len(page) < pageSize {
			break
		}
	}
	if len(events) > exportCap {
		events = events[:exportCap]
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-requests.csv"`)
	truncated := total > len(events)
	if truncated {
		// Machine-readable signal for API consumers; the marker row below is
		// the human-visible one (the browser download can't surface headers).
		w.Header().Set("X-Janus-Truncated", "true")
		w.Header().Set("X-Janus-Total-Rows", strconv.Itoa(total))
	}
	// Admin cross-user exports carry the owning user's identity per row; the
	// user-scoped export keeps its historical column set unchanged.
	crossUser := isAdminCrossUserRequestQuery(r, user)
	if crossUser {
		s.attachRequestOwners(r.Context(), events)
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()
	// The cache-write columns sit next to tokens_cached so the canonical cache
	// hit rate — tokens_cached / (tokens_in + tokens_cache_write_5m +
	// tokens_cache_write_1h) — is reproducible from the exported rows alone.
	// requested_model records the managed-model alias the caller addressed,
	// when one was used. It sits beside "model" (which always carries the
	// underlying model that actually ran) so an export can answer both
	// "what did we run" and "what did people ask for".
	header := []string{"timestamp", "model", "requested_model", "modality", "status", "latency_ms", "tokens_in", "tokens_out",
		"tokens_cached", "tokens_cache_write_5m", "tokens_cache_write_1h", "cost_usd", "error_code", "request_id", "charged_team_ids", "charged_team"}
	if crossUser {
		header = append([]string{"timestamp", "user_email", "user_name", "service_token"}, header[1:]...)
	}
	_ = cw.Write(header)
	for _, e := range events {
		row := []string{
			e.CreatedAt.Format(time.RFC3339), e.ModelName, e.RequestedModelName, e.Modality, strconv.Itoa(e.HTTPStatus),
			strconv.Itoa(e.LatencyMs), strconv.FormatInt(e.TokensIn, 10), strconv.FormatInt(e.TokensOut, 10),
			strconv.FormatInt(e.TokensCached, 10), strconv.FormatInt(e.TokensCacheWrite5m, 10),
			strconv.FormatInt(e.TokensCacheWrite1h, 10), fmt.Sprintf("%.6f", usage.USD(e.CostNano)), e.ErrorCode, e.RequestID, e.TeamIDs, requestChargedTeamLabel(e),
		}
		if crossUser {
			row = append([]string{row[0], e.UserEmail, e.UserName, e.ServiceTokenName}, row[1:]...)
		}
		_ = cw.Write(row)
	}
	if truncated {
		_ = cw.Write([]string{fmt.Sprintf(
			"# TRUNCATED: %d of %d matching requests exported (5000-row cap) — narrow the time range or filters to export the rest",
			len(events), total)})
	}
}

// --- Dashboards --------------------------------------------------------------

func (s *Server) handlePersonalDashboard(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	start, end, label := rangeBounds(r)
	// Scoping by UserID already excludes service-token traffic (those events
	// carry no user id); PrincipalUsers states the intent explicitly so the
	// exclusion survives any future change to how the scope is built.
	scope := store.UsageScope{UserID: user.ID, Start: start, End: end, Principal: store.PrincipalUsers}

	totals, err := s.Store.AggregateUsage(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Model breakdowns are catalog-filtered by the store: names recorded from
	// requests with invalid model identifiers are logged there, never graphed.
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
	byToken, err := s.Store.BreakdownUsage(r.Context(), scope, "token", store.BreakdownMetricCost)
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
	recent, _, err := s.Store.ListRequests(r.Context(), store.RequestFilter{UserID: user.ID, Limit: 5})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"range": label, "start": start, "end": end,
		"totals": totals, "per_model": byModel, "per_modality": byModality,
		"per_token": byToken, "series": series, "recent_requests": recent,
	})
}

func seriesShape(label string) (time.Duration, int) {
	switch label {
	case "week":
		return time.Hour * 4, 42
	case "month":
		return time.Hour * 24, 30
	case "quarter":
		return time.Hour * 24, 90
	default:
		return time.Minute * 30, 48
	}
}

func (s *Server) handleQuotaDashboard(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	_, teamIDs, err := s.principalContext(r.Context(), user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	statuses, err := s.Quota.Status(r.Context(), user.ID, teamIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(statuses))
	for _, st := range statuses {
		// Local-only mode: USD quotas are inert (never enforced, accrue zero),
		// so the user-facing quota view omits them — a dormant dollar limit is
		// a cost surface, not information. Admin/lead management lists keep
		// showing them so the rules can still be deleted.
		if s.Config.LocalOnly && st.Quota.Metric == store.MetricCostUSD {
			continue
		}
		out = append(out, quotaStatusPayload(st))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"quotas": out})
}

func quotaStatusPayload(st *quota.Status) map[string]any {
	payload := map[string]any{
		"id": st.Quota.ID, "metric": st.Quota.Metric, "metric_label": quota.MetricLabel(st.Quota.Metric),
		"window": st.Quota.Window, "window_label": quota.WindowLabel(st.Quota.Window),
		"subject_type": st.Quota.SubjectType, "subject_name": st.Quota.SubjectName,
		"model_name": st.Quota.ModelName, "breach_behavior": st.Quota.BreachBehavior,
		"limit_value": st.Limit, "current_value": st.Current,
		"percent": st.Percent, "at_risk": st.AtRisk, "breached": st.Breached, "reset_at": st.ResetAt,
		"alert_thresholds":        quota.EffectiveWarningThresholds(st.Quota),
		"alert_thresholds_custom": len(st.Quota.AlertThresholds) > 0,
	}
	if st.Quota.Metric == store.MetricCostUSD {
		payload["limit_display"] = usage.USD(st.Limit)
		payload["current_display"] = usage.USD(st.Current)
		payload["unit"] = "usd"
	} else {
		payload["limit_display"] = st.Limit
		payload["current_display"] = st.Current
		payload["unit"] = "count"
	}
	return payload
}

func (s *Server) handleTeamDashboard(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	teamID := r.URL.Query().Get("team_id")
	teams, err := s.Store.TeamsForUser(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if teamID == "" {
		if len(teams) == 0 {
			WriteJSON(w, http.StatusOK, map[string]any{"team": nil, "teams": teams})
			return
		}
		teamID = teams[0].ID
	}
	isMember := false
	for _, t := range teams {
		if t.ID == teamID {
			isMember = true
		}
	}
	if !isMember && !user.IsAdmin() {
		WriteError(w, r, ErrForbidden("You are not a member of this team."))
		return
	}
	team, err := s.Store.TeamByID(r.Context(), teamID)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That team"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	members, err := s.Store.TeamMemberIDs(r.Context(), teamID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	start, end, label := rangeBounds(r)
	// Team accounting follows the call's stored attribution, not today's roster.
	scope := store.UsageScope{TeamIDs: []string{teamID}, Start: start, End: end, Principal: store.PrincipalUsers}
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
	perMember, err := s.Store.BreakdownUsage(r.Context(), scope, "user", store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Members see contribution shares; only the lead (or an admin) sees who is who.
	teamRole, _ := s.Store.TeamRole(r.Context(), teamID, user.ID)
	canSeeMembers := user.IsAdmin() || teamRole == "leader" || teamRole == "moderator"
	if !canSeeMembers {
		for i := range perMember {
			if perMember[i].Key != user.ID {
				perMember[i].Label = "Team member"
			}
			perMember[i].Key = ""
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"team": team, "teams": teams, "range": label, "totals": totals,
		"per_model": byModel, "per_member": perMember,
		"can_see_member_detail": canSeeMembers, "member_count": len(members),
	})
}

func (s *Server) handleGlobalDashboard(w http.ResponseWriter, r *http.Request) {
	flags, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	scope := store.UsageScope{Start: now.AddDate(0, 0, -30), End: now}

	topModels, err := s.Store.BreakdownUsage(r.Context(), scope, "model", store.BreakdownMetricCost)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	totals, err := s.Store.AggregateUsage(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	activeUsers, err := s.Store.ActiveUserCount(r.Context(), now.Add(-15*time.Minute))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	series, err := s.Store.UsageSeries(r.Context(), store.UsageScope{Start: now.Add(-2 * time.Hour), End: now}, time.Minute*5, 24)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	newModels, err := s.Store.ListModels(r.Context(), store.ModelFilter{Status: store.ModelEnabled, Sort: "discovered"})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(newModels) > 5 {
		newModels = newModels[:5]
	}
	payload := map[string]any{
		"top_models": topModels, "totals_30d": totals, "active_users_15m": activeUsers,
		"series": series, "new_models": newModels, "leaderboards_enabled": flags["leaderboards_enabled"],
	}
	// Recent jobs drive the ambient layer's shooting stars: one star per real
	// completed request, sized by that request's output tokens. Only the two
	// fields the animation needs are returned — no model, user, or token
	// identity — so the decorative layer never becomes an information leak on
	// a shared screen. The window matches the series above.
	recent, _, err := s.Store.ListRequests(r.Context(), store.RequestFilter{
		Start: now.Add(-2 * time.Hour), End: now, Limit: 60,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	jobs := make([]map[string]any, 0, len(recent))
	for _, ev := range recent {
		jobs = append(jobs, map[string]any{
			"at":         ev.CreatedAt,
			"tokens_out": ev.TokensOut,
			"error":      ev.HTTPStatus >= 400,
		})
	}
	payload["recent_jobs"] = jobs
	// Leaderboards are opt-in; when the flag is off the data is not returned at
	// all rather than hidden client-side.
	if flags["leaderboards_enabled"] {
		// The top-users leaderboard ranks by output-token volume, not spend.
		users, err := s.Store.BreakdownUsage(r.Context(), scope, "user", store.BreakdownMetricTokensOut)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if len(users) > 5 {
			users = users[:5]
		}
		payload["top_users"] = users
		payload["top_teams"] = s.teamLeaderboard(r, scope)
	}
	WriteJSON(w, http.StatusOK, payload)
}

func (s *Server) teamLeaderboard(r *http.Request, scope store.UsageScope) []store.Breakdown {
	teams, err := s.Store.ListTeams(r.Context())
	if err != nil {
		return nil
	}
	out := []store.Breakdown{}
	for _, t := range teams {
		members, err := s.Store.TeamMemberIDs(r.Context(), t.ID)
		if err != nil || len(members) == 0 {
			continue
		}
		teamScope := scope
		teamScope.UserIDs = members
		totals, err := s.Store.AggregateUsage(r.Context(), teamScope)
		if err != nil {
			continue
		}
		out = append(out, store.Breakdown{Key: t.ID, Label: t.Name, Totals: totals})
	}
	// Teams rank by output-token volume, matching the top-users leaderboard.
	sort.Slice(out, func(i, j int) bool { return out[i].Totals.TokensOut > out[j].Totals.TokensOut })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func (s *Server) handleTokenUsage(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	tokenID := chi.URLParam(r, "id")
	token, err := s.Store.TokenByID(r.Context(), tokenID)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That token"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if token.UserID != user.ID && !user.IsAdmin() {
		WriteError(w, r, ErrForbidden("That token belongs to another user."))
		return
	}
	start, end, label := rangeBounds(r)
	// Scope everything to this token: totals must cover every event in the
	// range (not just the recent page), and per_model must not include the
	// user's other tokens.
	scope := store.UsageScope{TokenID: tokenID, Start: start, End: end}
	events, _, err := s.Store.ListRequests(r.Context(), store.RequestFilter{TokenID: tokenID, Start: start, End: end, Limit: 25})
	if err != nil {
		WriteError(w, r, err)
		return
	}
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
	WriteJSON(w, http.StatusOK, map[string]any{
		"token": token, "range": label, "totals": totals, "per_model": byModel, "recent_requests": events,
	})
}

// --- Notifications -----------------------------------------------------------

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	unreadOnly := r.URL.Query().Get("unread") == "true"
	limit := queryInt(r, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := max(0, queryInt(r, "offset", 0))
	items, unread, err := s.Store.ListNotifications(r.Context(), user.ID, unreadOnly, limit, offset)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next, _, err := s.Store.ListNotifications(r.Context(), user.ID, unreadOnly, 1, offset+limit)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"notifications": items, "unread_count": unread, "has_more": len(next) > 0, "offset": offset, "limit": limit})
}

func (s *Server) handleMarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body struct {
		ID string `json:"id"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if err := s.Store.MarkNotificationsRead(r.Context(), user.ID, body.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Docs feedback -----------------------------------------------------------

func (s *Server) handleDocsFeedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Page    string `json:"page"`
		Helpful bool   `json:"helpful"`
		Note    string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(body.Page) == "" {
		WriteError(w, r, ErrInvalidRequest("A page identifier is required.").WithParam("page"))
		return
	}
	if len(body.Note) > 1000 {
		body.Note = body.Note[:1000]
	}
	// Feedback is deliberately anonymous: no user id is recorded.
	if err := s.Store.AddDocsFeedback(r.Context(), body.Page, body.Helpful, body.Note); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"recorded": true})
}

// --- Help --------------------------------------------------------------------

// handleHelpTopic returns contextual help with code snippets pre-filled with the
// caller's endpoint and selected token.
func (s *Server) handleHelpTopic(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	topic := chi.URLParam(r, "topic")

	tokenLabel := "$JANUS_API_KEY"
	tokens, err := s.Store.ListTokens(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	activeTokens := []*store.Token{}
	for _, t := range tokens {
		if !t.Revoked() {
			activeTokens = append(activeTokens, t)
		}
	}
	if len(activeTokens) > 0 {
		// Token values are never recoverable, so snippets show the prefix and
		// tell the caller to paste the value they saved.
		tokenLabel = activeTokens[0].Prefix + "…"
	}
	model := "gpt-4o-mini"
	if models, err := s.grantedModels(r, user); err == nil && len(models) > 0 {
		// Pre-fill snippets with the catalog (public) name — the routing name
		// downstream callers should use — not the native upstream identifier.
		model = models[0].PublicName()
	}
	content, ok := helpTopic(topic, s.Config.PublicURL, tokenLabel, model)
	if !ok {
		WriteError(w, r, ErrNotFoundf("That help topic"))
		return
	}
	content["has_active_token"] = len(activeTokens) > 0
	content["topics"] = helpTopics()
	WriteJSON(w, http.StatusOK, content)
}

// modalityCatalog exposes the canonical modality enum to the UI filters.
func modalityCatalog() []map[string]string {
	out := make([]map[string]string, 0, len(adapter.AllModalities))
	for _, m := range adapter.AllModalities {
		out = append(out, map[string]string{"value": m, "label": usage.ModalityLabel(m)})
	}
	return out
}
