package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const usageColumns = `id, created_at, user_id, service_token_id, token_id, team_ids, upstream_id, model_id, model_name,
	requested_model_name, endpoint_path,
	http_method, modality, streaming, request_bytes, response_bytes, attachment_count, tokens_in, tokens_out,
	tokens_cached, tokens_cache_write_5m, tokens_cache_write_1h, token_accounting_method, cost_nanousd,
	finish_reason, http_status, latency_ms, ttfb_ms,
	upstream_latency_ms, client_user_agent, client_ip, x_forwarded_for, referer, client_app, error_code, quota_violated,
	blocking_rule_id, request_id, tokens_in_per_second, tokens_out_per_second, throughput_source, fallback_reason,
	secgw_action, secgw_violations`

// InsertUsageEvent appends a metering record, preserving explicit team snapshots.
// Empty snapshots may be attributed by a recorded zero-to-one transition after
// admission; the current roster is never used to infer request context.
// CostNano is stored unchanged, including in local-only mode.
func (s *Store) InsertUsageEvent(ctx context.Context, e *UsageEvent) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = nowUTC()
	}
	if e.TeamIDs == "" && e.UserID != "" {
		return s.modelTx(ctx, func(s *Store) error {
			if err := s.lockTeamAttributionUser(ctx, e.UserID); err != nil {
				return err
			}
			teamID, err := s.lateUsageTeam(ctx, e.UserID, e.CreatedAt)
			if err != nil {
				return err
			}
			// Do not mutate the caller's snapshot, including on transaction failure.
			record := *e
			record.TeamIDs = teamID
			return s.insertUsageEvent(ctx, &record)
		})
	}
	return s.insertUsageEvent(ctx, e)
}

func (s *Store) insertUsageEvent(ctx context.Context, e *UsageEvent) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.exec(ctx, `INSERT INTO usage_event (`+usageColumns+`) VALUES (`+placeholders(44)+`)`,
			e.ID, FormatTime(e.CreatedAt), e.UserID, e.ServiceTokenID, e.TokenID, e.TeamIDs, e.UpstreamID, e.ModelID, e.ModelName,
			e.RequestedModelName, e.EndpointPath, e.HTTPMethod, e.Modality, boolInt(e.Streaming), e.RequestBytes, e.ResponseBytes,
			e.AttachmentCount, e.TokensIn, e.TokensOut, e.TokensCached, e.TokensCacheWrite5m, e.TokensCacheWrite1h,
			e.AccountingMode, e.CostNano, e.FinishReason,
			e.HTTPStatus, e.LatencyMs, e.TTFBMs, e.UpstreamMs, e.UserAgent, e.ClientIP, e.XForwardedFor, e.Referer,
			e.ClientApp, e.ErrorCode, boolInt(e.QuotaViolated), e.BlockingRuleID, e.RequestID,
			e.TokensInPerSecond, e.TokensOutPerSecond, e.ThroughputSource, e.FallbackReason,
			e.SecgwAction, e.SecgwViolations); err != nil {
			return err
		}
		return s.insertReportingFact(ctx, e)
	})
}

func scanUsage(scan func(...any) error) (*UsageEvent, error) {
	var e UsageEvent
	var created string
	var streaming, violated int
	if err := scan(&e.ID, &created, &e.UserID, &e.ServiceTokenID, &e.TokenID, &e.TeamIDs, &e.UpstreamID, &e.ModelID, &e.ModelName,
		&e.RequestedModelName, &e.EndpointPath, &e.HTTPMethod, &e.Modality, &streaming, &e.RequestBytes, &e.ResponseBytes,
		&e.AttachmentCount, &e.TokensIn, &e.TokensOut, &e.TokensCached, &e.TokensCacheWrite5m, &e.TokensCacheWrite1h,
		&e.AccountingMode, &e.CostNano,
		&e.FinishReason, &e.HTTPStatus, &e.LatencyMs, &e.TTFBMs, &e.UpstreamMs, &e.UserAgent, &e.ClientIP,
		&e.XForwardedFor, &e.Referer, &e.ClientApp, &e.ErrorCode, &violated, &e.BlockingRuleID, &e.RequestID,
		&e.TokensInPerSecond, &e.TokensOutPerSecond, &e.ThroughputSource, &e.FallbackReason,
		&e.SecgwAction, &e.SecgwViolations); err != nil {
		return nil, err
	}
	e.CreatedAt = ParseTime(created)
	e.Streaming = streaming == 1
	e.QuotaViolated = violated == 1
	return &e, nil
}

// PrincipalFilter selects which kind of principal's traffic a query covers.
// It is the single vocabulary every report uses to say whether service-token
// usage belongs in its answer.
type PrincipalFilter string

const (
	// PrincipalAny counts every request regardless of who made it. This is
	// the org-wide view: admin overview, org pulse, analytics, totals.
	PrincipalAny PrincipalFilter = ""
	// PrincipalUsers counts only human-attributed traffic. This is what
	// people-oriented reports (top users, per-user leaderboards) use, and it
	// is what excludes service tokens from them.
	PrincipalUsers PrincipalFilter = "users"
	// PrincipalServiceTokens counts only service-token traffic, for the
	// service-token detail and leaderboard surfaces.
	PrincipalServiceTokens PrincipalFilter = "service_tokens"
)

// clause renders the principal restriction. Service-token events carry an
// empty user_id and a non-empty service_token_id, and user events the reverse,
// so the split is a simple structural test rather than a join.
func (p PrincipalFilter) clause() string {
	switch p {
	case PrincipalUsers:
		return "service_token_id = ''"
	case PrincipalServiceTokens:
		return "service_token_id <> ''"
	default:
		return ""
	}
}

// RequestFilter narrows a request-log query.
type RequestFilter struct {
	UserID   string
	TokenID  string
	TeamIDs  []string
	Model    string
	Modality string
	Status   string // "success" | "error" | exact code
	// Accounting narrows to one token_accounting_method (usage.Accounting*),
	// e.g. "unmetered_modality" to list the requests behind a metering gap.
	Accounting string
	Start      time.Time
	End        time.Time
	Sort       string
	Limit      int
	Offset     int
	// ServiceTokenID narrows to one integration credential's traffic.
	ServiceTokenID string
	// Principal restricts the log to one kind of principal. The default
	// (PrincipalAny) shows everything the other filters allow.
	Principal PrincipalFilter
	// Token-size thresholds. Each is a strict comparison applied only when
	// non-nil, so "greater than 1,000" and "smaller than 1,000" are both
	// expressible and 0 remains a meaningful bound ("smaller than 0" matches
	// nothing; "greater than 0" excludes untokenised rows).
	TokensInGT  *int64
	TokensInLT  *int64
	TokensOutGT *int64
	TokensOutLT *int64
	// IncludeInternal keeps the gateway's own platform-overhead calls in the
	// list. Classifier calls are written as usage events sharing the caller's
	// request_id so cost is attributable, but they are not requests anyone
	// made: listing them flat makes one chat look like two or three rows and
	// buries the user's own traffic. Default false hides them; the Requests
	// page exposes a "show gateway internals" toggle rather than hiding them
	// outright, and a request's drawer always shows its own classifier runs.
	IncludeInternal bool
}

// InternalClientApp marks usage events the gateway generated for itself
// (security classifier calls today). Metered as platform overhead, never
// against the caller's quota.
const InternalClientApp = "janus-security-gateway"

// requestSortOrder maps the request-log sort vocabulary to an ORDER BY
// expression. Every key has a `<key>_asc` ascending twin except "time", whose
// ascending form keeps the historical "oldest" name.
func requestSortOrder(sort string) string {
	switch sort {
	case "latency":
		return "latency_ms DESC"
	case "latency_asc":
		return "latency_ms ASC"
	case "cost":
		return "cost_nanousd DESC"
	case "cost_asc":
		return "cost_nanousd ASC"
	case "tokens_in":
		return "tokens_in DESC"
	case "tokens_in_asc":
		return "tokens_in ASC"
	case "tokens_out":
		return "tokens_out DESC"
	case "tokens_out_asc":
		return "tokens_out ASC"
	case "oldest":
		return "created_at ASC"
	default:
		return "created_at DESC"
	}
}

func usageTeamClause(teamIDs []string) (string, []any) {
	if len(teamIDs) == 0 {
		return "", nil
	}
	parts := []string{}
	args := []any{}
	escape := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	for _, id := range teamIDs {
		if id == "" || strings.Contains(id, ",") {
			continue
		}
		parts = append(parts, "(',' || team_ids || ',') LIKE ? ESCAPE '!'")
		args = append(args, "%,"+escape.Replace(id)+",%")
	}
	if len(parts) == 0 {
		return "1=0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func (f RequestFilter) clause() (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if clause, teamArgs := usageTeamClause(f.TeamIDs); clause != "" {
		where = append(where, clause)
		args = append(args, teamArgs...)
	}
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.ServiceTokenID != "" {
		where = append(where, "service_token_id = ?")
		args = append(args, f.ServiceTokenID)
	}
	if p := f.Principal.clause(); p != "" {
		where = append(where, p)
	}
	if f.TokenID != "" {
		where = append(where, "token_id = ?")
		args = append(args, f.TokenID)
	}
	if f.Model != "" {
		where = append(where, "model_name = ?")
		args = append(args, f.Model)
	}
	if f.Modality != "" {
		where = append(where, "modality = ?")
		args = append(args, f.Modality)
	}
	if !f.IncludeInternal {
		where = append(where, "(client_app IS NULL OR client_app <> ?)")
		args = append(args, InternalClientApp)
	}
	if f.Accounting != "" {
		where = append(where, "token_accounting_method = ?")
		args = append(args, f.Accounting)
	}
	switch f.Status {
	case "success":
		where = append(where, "http_status >= 200 AND http_status < 300")
	case "error":
		where = append(where, "http_status >= 400")
	case "":
	default:
		where = append(where, "http_status = ?")
		args = append(args, f.Status)
	}
	if !f.Start.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, FormatTime(f.Start))
	}
	if !f.End.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, FormatTime(f.End))
	}
	if f.TokensInGT != nil {
		where = append(where, "tokens_in > ?")
		args = append(args, *f.TokensInGT)
	}
	if f.TokensInLT != nil {
		where = append(where, "tokens_in < ?")
		args = append(args, *f.TokensInLT)
	}
	if f.TokensOutGT != nil {
		where = append(where, "tokens_out > ?")
		args = append(args, *f.TokensOutGT)
	}
	if f.TokensOutLT != nil {
		where = append(where, "tokens_out < ?")
		args = append(args, *f.TokensOutLT)
	}
	return strings.Join(where, " AND "), args
}

// ListRequests returns a filtered page of usage events plus the total count.
func (s *Store) ListRequests(ctx context.Context, f RequestFilter) ([]*UsageEvent, int, error) {
	clause, args := f.clause()
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM usage_event WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count requests: %w", err)
	}
	order := requestSortOrder(f.Sort)
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT `+usageColumns+` FROM usage_event WHERE `+clause+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []*UsageEvent{}
	for rows.Next() {
		e, err := scanUsage(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("scan usage event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	if err := s.attachUsageTeamNames(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// UsageEventByID loads one usage event, or ErrNotFound.
func (s *Store) UsageEventByID(ctx context.Context, id string) (*UsageEvent, error) {
	e, err := scanUsage(s.queryRow(ctx, `SELECT `+usageColumns+` FROM usage_event WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("usage event by id: %w", err)
	}
	if err := s.attachUsageTeamNames(ctx, []*UsageEvent{e}); err != nil {
		return nil, err
	}
	return e, nil
}

// Totals is an aggregate over a set of usage events.
//
// The cache-write sums exist so every consumer can compute the one canonical
// cache-hit rate: tokens_cached / (tokens_in + tokens_cache_write_5m +
// tokens_cache_write_1h), undefined when the denominator is zero. Events
// recorded before migration 0013 contribute zero to both cache-write sums.
type Totals struct {
	TokensIn           int64 `json:"tokens_in"`
	TokensOut          int64 `json:"tokens_out"`
	TokensCached       int64 `json:"tokens_cached"`
	TokensCacheWrite5m int64 `json:"tokens_cache_write_5m"`
	TokensCacheWrite1h int64 `json:"tokens_cache_write_1h"`
	CostNano           int64 `json:"cost_nanousd"`
	Requests           int64 `json:"request_count"`
	ErrorCount         int64 `json:"error_count"`
}

// Ranking metrics accepted by BreakdownUsage. Token volume (tokens_out) is
// the default so leaderboards emphasise usage rather than spend.
const (
	BreakdownMetricTokensOut = "tokens_out"
	BreakdownMetricCost      = "cost"
	BreakdownMetricRequests  = "requests"
)

// Breakdown is one labelled slice of an aggregate.
type Breakdown struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Totals Totals `json:"totals"`
}

// UsageScope selects which events an aggregate covers.
type UsageScope struct {
	// TeamIDs matches recorded snapshots, never the current membership roster.
	TeamIDs []string
	UserID  string
	UserIDs []string
	TokenID string
	Start   time.Time
	End     time.Time
	// ServiceTokenID narrows an aggregate to one integration credential,
	// which is what the per-service-token detail page reports on.
	ServiceTokenID string
	// RequestedModelName narrows an aggregate to the alias a caller actually
	// asked for. Managed-model reporting needs this because the event's
	// model_name deliberately records the underlying model that ran, so
	// "usage of current-best" is only answerable from the requested name.
	RequestedModelName string
	// Principal decides whether service-token traffic is in scope. Leave it
	// at the PrincipalAny default for org-wide reporting (admin overview,
	// org pulse, analytics); set PrincipalUsers for people-oriented reports
	// so integration traffic never appears in a leaderboard of humans.
	Principal PrincipalFilter
}

func (u UsageScope) clause() (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if clause, teamArgs := usageTeamClause(u.TeamIDs); clause != "" {
		where = append(where, clause)
		args = append(args, teamArgs...)
	}
	if u.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, u.UserID)
	}
	if u.ServiceTokenID != "" {
		where = append(where, "service_token_id = ?")
		args = append(args, u.ServiceTokenID)
	}
	if u.RequestedModelName != "" {
		where = append(where, "requested_model_name = ?")
		args = append(args, u.RequestedModelName)
	}
	if p := u.Principal.clause(); p != "" {
		where = append(where, p)
	}
	if u.TokenID != "" {
		where = append(where, "token_id = ?")
		args = append(args, u.TokenID)
	}
	if len(u.UserIDs) > 0 {
		where = append(where, "user_id IN ("+placeholders(len(u.UserIDs))+")")
		args = append(args, toArgs(u.UserIDs)...)
	}
	if !u.Start.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, FormatTime(u.Start))
	}
	if !u.End.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, FormatTime(u.End))
	}
	return strings.Join(where, " AND "), args
}

const totalsExpr = `COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(tokens_cached),0),
	COALESCE(SUM(tokens_cache_write_5m),0), COALESCE(SUM(tokens_cache_write_1h),0),
	COALESCE(SUM(cost_nanousd),0), COUNT(*), COALESCE(SUM(CASE WHEN http_status >= 400 THEN 1 ELSE 0 END),0)`

// CountDistinctPrincipals reports how many distinct principals generated
// traffic in a scope. A service-token event carries an empty user_id and a
// non-empty service_token_id (and vice versa), so counting distinct non-empty
// values of each column and summing them yields "how many separate callers",
// whether they are people, integrations, or both.
func (s *Store) CountDistinctPrincipals(ctx context.Context, scope UsageScope) (int64, error) {
	clause, args := scope.clause()
	var users, tokens int64
	err := s.queryRow(ctx, `SELECT
		COUNT(DISTINCT CASE WHEN user_id <> '' THEN user_id END),
		COUNT(DISTINCT CASE WHEN service_token_id <> '' THEN service_token_id END)
		FROM usage_event WHERE `+clause, args...).Scan(&users, &tokens)
	if err != nil {
		return 0, fmt.Errorf("count distinct principals: %w", err)
	}
	return users + tokens, nil
}

// AggregateUsage computes headline totals for a scope.
func (s *Store) AggregateUsage(ctx context.Context, scope UsageScope) (Totals, error) {
	clause, args := scope.clause()
	var t Totals
	err := s.queryRow(ctx, `SELECT `+totalsExpr+` FROM usage_event WHERE `+clause, args...).
		Scan(&t.TokensIn, &t.TokensOut, &t.TokensCached, &t.TokensCacheWrite5m, &t.TokensCacheWrite1h,
			&t.CostNano, &t.Requests, &t.ErrorCount)
	if err != nil {
		return t, fmt.Errorf("aggregate usage: %w", err)
	}
	return t, nil
}

// BreakdownUsage groups a scope by one dimension: model, modality, token,
// upstream, or user. The optional metric parameter controls the ranking:
// "tokens_out" (the default when omitted or empty), "cost", or "requests".
func (s *Store) BreakdownUsage(ctx context.Context, scope UsageScope, dimension string, metric ...string) ([]Breakdown, error) {
	column := map[string]string{
		"model":         "model_name",
		"modality":      "modality",
		"token":         "token_id",
		"upstream":      "upstream_id",
		"user":          "user_id",
		"status":        "http_status",
		"endpoint":      "endpoint_path",
		"service_token": "service_token_id",
		// requested_model reports which managed-model alias callers asked
		// for. It is deliberately separate from the "model" dimension, which
		// keeps reporting the underlying models that actually ran.
		"requested_model": "requested_model_name",
	}[dimension]
	if column == "" {
		return nil, fmt.Errorf("unsupported breakdown dimension %q", dimension)
	}
	// A breakdown BY user is a people-oriented report by definition, so it
	// excludes service-token traffic unless the caller has deliberately asked
	// for another principal scope. This is the single place the "top users
	// never includes service tokens" rule is enforced, so no caller can
	// forget it. Conversely a breakdown by service_token implies that scope.
	if dimension == "user" && scope.Principal == PrincipalAny {
		scope.Principal = PrincipalUsers
	}
	if dimension == "service_token" && scope.Principal == PrincipalAny {
		scope.Principal = PrincipalServiceTokens
	}
	// Token volume is the default ranking metric for breakdowns; "cost" and
	// "requests" remain available for surfaces that emphasise spend or volume
	// of calls.
	rank := BreakdownMetricTokensOut
	if len(metric) > 0 && metric[0] != "" {
		rank = metric[0]
	}
	orderExpr := map[string]string{
		BreakdownMetricTokensOut: "SUM(tokens_out)",
		BreakdownMetricCost:      "SUM(cost_nanousd)",
		BreakdownMetricRequests:  "COUNT(*)",
	}[rank]
	if orderExpr == "" {
		return nil, fmt.Errorf("unsupported breakdown metric %q", rank)
	}
	clause, args := scope.clause()
	rows, err := s.query(ctx, `SELECT `+column+`, `+totalsExpr+` FROM usage_event WHERE `+clause+
		` GROUP BY `+column+` ORDER BY `+orderExpr+` DESC, COUNT(*) DESC LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Breakdown{}
	for rows.Next() {
		var b Breakdown
		var key any
		if err := rows.Scan(&key, &b.Totals.TokensIn, &b.Totals.TokensOut, &b.Totals.TokensCached,
			&b.Totals.TokensCacheWrite5m, &b.Totals.TokensCacheWrite1h,
			&b.Totals.CostNano, &b.Totals.Requests, &b.Totals.ErrorCount); err != nil {
			return nil, fmt.Errorf("scan breakdown: %w", err)
		}
		b.Key = fmt.Sprintf("%v", key)
		b.Label = b.Key
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.labelBreakdown(ctx, dimension, scope, out), nil
}

// labelBreakdown resolves human-readable labels for a breakdown. For the model
// dimension it additionally filters the rows against the model catalog: usage
// events keep whatever model name the caller sent (including invalid ones),
// and those must never surface on usage graphs — they are logged instead.
func (s *Store) labelBreakdown(ctx context.Context, dimension string, scope UsageScope, items []Breakdown) []Breakdown {
	if dimension == "model" {
		return s.filterModelBreakdown(ctx, scope, items)
	}
	// Service-token names are resolved in one query rather than per row: a
	// leaderboard of 50 integrations should not issue 50 lookups.
	var serviceTokenNames map[string]string
	if dimension == "service_token" {
		if names, err := s.ServiceTokenNames(ctx); err == nil {
			serviceTokenNames = names
		}
	}
	filtered := items[:0]
	for i := range items {
		switch dimension {
		case "user":
			if u, err := s.UserByID(ctx, items[i].Key); err == nil {
				items[i].Label = displayName(u)
			} else {
				items[i].Label = "Unknown user"
			}
		case "service_token":
			if name, ok := serviceTokenNames[items[i].Key]; ok {
				items[i].Label = name
			} else {
				// A token deleted from the table (should not happen — they
				// are revoked, never deleted) still has history; label it
				// honestly rather than showing a raw id.
				items[i].Label = "Deleted service token"
			}
		case "requested_model":
			// Only alias-addressed requests populate this column; the empty
			// key means "addressed the model directly" and is not a row.
			if items[i].Key == "" {
				continue
			}
		case "token":
			if t, err := s.TokenByID(ctx, items[i].Key); err == nil && t.Description != "" {
				items[i].Label = t.Description
			} else if items[i].Key == "" {
				items[i].Label = "Browser session"
			}
		case "upstream":
			if u, err := s.UpstreamByID(ctx, items[i].Key); err == nil {
				items[i].Label = u.Name
			}
		}
		filtered = append(filtered, items[i])
	}
	return filtered
}

// catalogModelNames returns every name a catalogued model answers to — its
// native upstream name plus its display-name alias when set — across all
// curation statuses, plus every managed-model alias name. Models on deleted
// upstreams are no longer part of the catalog. Usage events record
// PublicName() (the display name when set, the native name otherwise), so this
// set covers every value a legitimate request can have written to
// usage_event.model_name.
//
// Managed-model names are included because a caller may address an alias and,
// while the resulting event records the UNDERLYING model's name, an alias name
// that also matches a real model's name must not be treated as uncatalogued.
func (s *Store) catalogModelNames(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.query(ctx, `SELECT m.name, m.display_name FROM model m
		JOIN upstream u ON u.id = m.upstream_id WHERE u.deleted_at = ''`)
	if err != nil {
		return nil, fmt.Errorf("load model catalog names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names := map[string]struct{}{}
	for rows.Next() {
		var name, display string
		if err := rows.Scan(&name, &display); err != nil {
			return nil, fmt.Errorf("scan model catalog name: %w", err)
		}
		names[name] = struct{}{}
		if display != "" {
			names[display] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	managed, err := s.ManagedModelNames(ctx)
	if err != nil {
		return nil, err
	}
	for name := range managed {
		names[name] = struct{}{}
	}
	return names, nil
}

// filterModelBreakdown keeps only breakdown rows whose model name exists in
// the model catalog. Requests made with invalid model identifiers are stored
// in usage_event verbatim for auditing, but they must never appear on usage
// graphs — each dropped name is logged at warn level with its scope context
// instead. When the catalog itself cannot be read the rows pass through
// unfiltered: a transient catalog error must not blank every dashboard.
func (s *Store) filterModelBreakdown(ctx context.Context, scope UsageScope, items []Breakdown) []Breakdown {
	valid, err := s.catalogModelNames(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "model breakdown: could not load the model catalog; returning unfiltered results",
			"error", err.Error())
		return items
	}
	out := items[:0]
	for _, item := range items {
		if _, ok := valid[item.Key]; ok {
			out = append(out, item)
			continue
		}
		s.logger().WarnContext(ctx, "model breakdown: dropped model name not present in the model catalog",
			"model_name", item.Key,
			"request_count", item.Totals.Requests,
			"user_id", scope.UserID,
			"token_id", scope.TokenID,
			"window_start", formatOptionalTime(scope.Start),
			"window_end", formatOptionalTime(scope.End),
		)
	}
	return out
}

// formatOptionalTime renders a scope bound, keeping zero values readable.
func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return FormatTime(t)
}

func displayName(u *User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// TimePoint is one bucket in a time series.
type TimePoint struct {
	Bucket string `json:"bucket"`
	Totals Totals `json:"totals"`
}

// UsageSeries buckets a scope into fixed-width time slices. Bucketing is done in
// Go so the SQL stays identical across both supported engines.
func (s *Store) UsageSeries(ctx context.Context, scope UsageScope, bucket time.Duration, buckets int) ([]TimePoint, error) {
	if bucket <= 0 || buckets <= 0 {
		return nil, fmt.Errorf("usage series requires a positive bucket size and count")
	}
	end := scope.End
	if end.IsZero() {
		end = nowUTC()
	}
	start := end.Add(-bucket * time.Duration(buckets))
	if scope.Start.After(start) {
		start = scope.Start
	}
	sc := scope
	sc.Start, sc.End = start, end
	clause, args := sc.clause()
	rows, err := s.query(ctx, `SELECT created_at, tokens_in, tokens_out, tokens_cached,
		tokens_cache_write_5m, tokens_cache_write_1h, cost_nanousd, http_status
		FROM usage_event WHERE `+clause+` ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	series := make([]TimePoint, buckets)
	for i := range series {
		series[i].Bucket = FormatTime(end.Add(-bucket * time.Duration(buckets-i-1)).Truncate(time.Second))
	}
	for rows.Next() {
		var created string
		var in, out, cached, cacheWrite5m, cacheWrite1h, cost int64
		var status int
		if err := rows.Scan(&created, &in, &out, &cached, &cacheWrite5m, &cacheWrite1h, &cost, &status); err != nil {
			return nil, fmt.Errorf("scan series row: %w", err)
		}
		ts := ParseTime(created)
		idx := buckets - 1 - int(end.Sub(ts)/bucket)
		if idx < 0 || idx >= buckets {
			continue
		}
		p := &series[idx].Totals
		p.TokensIn += in
		p.TokensOut += out
		p.TokensCached += cached
		p.TokensCacheWrite5m += cacheWrite5m
		p.TokensCacheWrite1h += cacheWrite1h
		p.CostNano += cost
		p.Requests++
		if status >= 400 {
			p.ErrorCount++
		}
	}
	return series, rows.Err()
}

// SumMetricSince computes the value of a quota metric over a window. It is the
// durable fallback used when the in-memory ledger is unavailable.
func (s *Store) SumMetricSince(ctx context.Context, subjectType, subjectID, modelID, metric string, since time.Time) (int64, error) {
	expr := map[string]string{
		MetricTokensIn:  "COALESCE(SUM(tokens_in),0)",
		MetricTokensOut: "COALESCE(SUM(tokens_out),0)",
		MetricCostUSD:   "COALESCE(SUM(cost_nanousd),0)",
		MetricRequests:  "COUNT(*)",
	}[metric]
	if expr == "" {
		return 0, fmt.Errorf("unsupported quota metric %q", metric)
	}
	where := []string{"created_at >= ?"}
	args := []any{FormatTime(since)}
	switch subjectType {
	case "user":
		where = append(where, "user_id = ?")
		args = append(args, subjectID)
	case "service_token":
		// A service-token quota bounds one integration credential. This is
		// the control that stops a runaway unattended agent, so it is scoped
		// exactly like a user quota but on the other principal column.
		where = append(where, "service_token_id = ?")
		args = append(args, subjectID)
	case "team":
		members, err := s.TeamMemberIDs(ctx, subjectID)
		if err != nil {
			return 0, err
		}
		if len(members) == 0 {
			return 0, nil
		}
		where = append(where, "user_id IN ("+placeholders(len(members))+")")
		args = append(args, toArgs(members)...)
	}
	if modelID != "" {
		where = append(where, "model_id = ?")
		args = append(args, modelID)
	}
	var total int64
	if err := s.queryRow(ctx, `SELECT `+expr+` FROM usage_event WHERE `+strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("sum quota metric: %w", err)
	}
	return total, nil
}

// PurgeUsageEvents deletes metering records older than the retention window.
func (s *Store) PurgeUsageEvents(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := s.modelTx(ctx, func(s *Store) error {
		res, err := s.tx.ExecContext(ctx, s.rebind(`DELETE FROM usage_event WHERE created_at < ?`), FormatTime(before))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		if err := s.exec(ctx, `DELETE FROM reporting_usage_snapshot WHERE NOT EXISTS (SELECT 1 FROM usage_event WHERE usage_event.id = reporting_usage_snapshot.usage_id)`); err != nil {
			return err
		}
		return s.exec(ctx, `INSERT INTO reporting_coverage (key, started_at) VALUES ('retention_before', ?)
   ON CONFLICT (key) DO UPDATE SET started_at = excluded.started_at
   WHERE reporting_coverage.started_at < excluded.started_at`, FormatTime(before))
	})
	if err != nil {
		return 0, fmt.Errorf("purge usage events: %w", err)
	}
	return n, nil
}

// AccountingGap is one model whose successful media responses were recorded
// unmetered (usage.AccountingUnmetered) within the window: the upstream
// returned no usage the adapter could read, so the requests carry zero tokens
// and zero cost. Each row is a configuration gap for the operator to close.
type AccountingGap struct {
	ModelName    string    `json:"model_name"`
	UpstreamID   string    `json:"upstream_id"`
	UpstreamName string    `json:"upstream_name"`
	Modality     string    `json:"modality"`
	Requests     int64     `json:"requests"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// AccountingSummary rolls up how requests since a point in time were metered,
// for the System page's metering health tile.
type AccountingSummary struct {
	UpstreamReported     int64 `json:"upstream_reported"`
	UpstreamReportedCost int64 `json:"upstream_reported_cost"`
	ByteEstimated        int64 `json:"byte_estimated"`
	Unmetered            int64 `json:"unmetered"`
	// Gaps lists the unmetered models, most requests first.
	Gaps []AccountingGap `json:"gaps"`
}

// AccountingSummarySince aggregates token_accounting_method over successful
// requests since the given time.
func (s *Store) AccountingSummarySince(ctx context.Context, since time.Time) (AccountingSummary, error) {
	var out AccountingSummary
	rows, err := s.query(ctx, `SELECT token_accounting_method, COUNT(*) FROM usage_event
		WHERE created_at >= ? AND http_status < 300 GROUP BY token_accounting_method`, FormatTime(since))
	if err != nil {
		return out, fmt.Errorf("accounting summary: %w", err)
	}
	for rows.Next() {
		var method string
		var n int64
		if err := rows.Scan(&method, &n); err != nil {
			rows.Close()
			return out, err
		}
		switch method {
		case "upstream_reported":
			out.UpstreamReported = n
		case "upstream_reported_cost":
			out.UpstreamReportedCost = n
		case "byte_count_fallback":
			out.ByteEstimated = n
		case "unmetered_modality":
			out.Unmetered = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Gaps = []AccountingGap{}
	if out.Unmetered == 0 {
		return out, nil
	}
	gaps, err := s.query(ctx, `SELECT e.model_name, e.upstream_id, COALESCE(u.name, ''), e.modality, COUNT(*), MAX(e.created_at)
		FROM usage_event e LEFT JOIN upstream u ON u.id = e.upstream_id
		WHERE e.created_at >= ? AND e.http_status < 300 AND e.token_accounting_method = 'unmetered_modality'
		GROUP BY e.model_name, e.upstream_id, u.name, e.modality
		ORDER BY COUNT(*) DESC, e.model_name ASC LIMIT 20`, FormatTime(since))
	if err != nil {
		return out, fmt.Errorf("accounting gaps: %w", err)
	}
	defer gaps.Close()
	for gaps.Next() {
		var g AccountingGap
		var last string
		if err := gaps.Scan(&g.ModelName, &g.UpstreamID, &g.UpstreamName, &g.Modality, &g.Requests, &last); err != nil {
			return out, err
		}
		g.LastSeenAt = ParseTime(last)
		out.Gaps = append(out.Gaps, g)
	}
	return out, gaps.Err()
}
