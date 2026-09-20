package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/torvanis/janus/internal/reporting"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const reportMaxRows = 250000
const reportUnknown = "(unknown)"

type reportFact struct {
	e                                                      *UsageEvent
	groups                                                 []string
	groupsKnown, classificationKnown                       bool
	family, provider, hosting, project, center, costStatus string
}
type reportAccumulator struct {
	facts []*reportFact
	dims  map[string]string
}

// QueryReport consumes a server-resolved scope, never Definition.Scope as authority.
// ResolveReportScope must be called for every run and download.
func (s *Store) QueryReport(ctx context.Context, d reporting.Definition, scope reporting.Scope) (*reporting.Result, error) {
	if err := reporting.Validate(d); err != nil {
		return nil, err
	}
	if !scope.Admin && scope.UserID == "" && len(scope.TeamIDs) == 0 {
		return nil, fmt.Errorf("report scope denied")
	}
	now := time.Now().UTC()
	start, end, err := reporting.ResolvePeriod(d, now)
	if err != nil {
		return nil, err
	}
	queryEnd := end
	if queryEnd.After(now) {
		queryEnd = now
	}
	facts, err := s.reportFacts(ctx, start, queryEnd, scope, d)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, err
	}
	r := &reporting.Result{Version: 1, Definition: d, Start: start, End: end, GeneratedAt: now, DataCutoff: now, SourceRows: int64(len(facts)), Warnings: []string{}, Sections: []reporting.Section{}}
	if end.After(now) {
		reportWarn(r, "Requested period is provisional: event timestamps are limited to data_cutoff; later-arriving records may change totals.")
	}
	r.Columns = reportColumns(d.Dimensions, d.Metrics)
	r.Totals = reportMetrics(facts, d.Metrics)
	r.Rows, err = reportAggregate(facts, d.Dimensions, d.Metrics, loc)
	if err != nil {
		return nil, err
	}
	for _, warning := range reportFactWarnings(facts) {
		reportWarn(r, warning)
	}
	if d.GroupMode == "current" {
		reportWarn(r, "Groups represent a current-member cohort, not historical membership.")
	}
	retained, err := s.reportRetention(ctx)
	if err != nil {
		return nil, err
	}
	if start.Before(retained) {
		reportWarn(r, "Requested period precedes retention_before "+retained.Format(time.RFC3339)+"; retained records cannot establish complete coverage.")
	}
	if d.Compare {
		// Reliability describes interval coverage, not whether recorded spend
		// represents fully priced usage. Pricing confidence is warned separately.
		reliable := !start.Before(retained)
		r.ComparisonReliable = &reliable
		if !reliable {
			reportComparisonWarn(r, "Current period precedes retention_before; comparison coverage is incomplete.")
		}
		if end.After(now) {
			reliable = false
			reportComparisonWarn(r, "Current period is provisional; comparison coverage is incomplete at data_cutoff.")
		}
		ps, pe := reportPrevious(d, start, end)
		if ps.Before(retained) {
			reliable = false
			reportComparisonWarn(r, "Previous period precedes retention_before "+retained.Format(time.RFC3339)+"; retained records cannot establish complete comparison coverage.")
		}
		if pe.After(now) {
			reliable = false
			reportComparisonWarn(r, "Previous period is provisional; event timestamps are limited to data_cutoff.")
			pe = now
		}
		previous, err := s.reportFacts(ctx, ps, pe, scope, d)
		if err != nil {
			return nil, err
		}
		r.PreviousTotals = reportMetrics(previous, d.Metrics)
		for _, warning := range reportFactWarnings(previous) {
			reportComparisonWarn(r, "Previous period: "+warning)
		}
	}
	if err := s.reportAnalysis(ctx, r, facts, scope); err != nil {
		return nil, err
	}
	if err := s.reportLabels(ctx, r); err != nil {
		return nil, err
	}
	if scope.HideCosts {
		redacted := reporting.RedactCosts(*r)
		return &redacted, nil
	}
	return r, nil
}

func (s *Store) reportFacts(ctx context.Context, start, end time.Time, scope reporting.Scope, d reporting.Definition) ([]*reportFact, error) {
	// SQL identifiers below are constants. Client filters are applied as values only.
	cols := strings.Split(usageColumns, ",")
	for i := range cols {
		cols[i] = "u." + strings.TrimSpace(cols[i])
	}
	q := `SELECT ` + strings.Join(cols, ",") + `,COALESCE(f.group_ids,'[]'),COALESCE(f.groups_known,0),COALESCE(f.classification_known,0),COALESCE(f.model_family,''),COALESCE(f.provider,''),COALESCE(f.hosting,''),COALESCE(f.project,''),COALESCE(f.cost_center,''),COALESCE(f.cost_status,'unknown') FROM usage_event u LEFT JOIN reporting_usage_snapshot f ON f.usage_id=u.id WHERE u.created_at>=? AND u.created_at<?`
	args := []any{FormatTime(start), FormatTime(end)}
	if scope.UserID != "" {
		q += ` AND u.user_id=?`
		args = append(args, scope.UserID)
	}
	if clause, teamArgs := usageTeamClause(scope.TeamIDs); clause != "" {
		q += " AND " + clause
		args = append(args, teamArgs...)
	}
	q += ` ORDER BY u.created_at,u.id`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read report facts (reporting migration required): %w", err)
	}
	out := []*reportFact{}
	seen := 0
	for rows.Next() {
		seen++
		if seen > reportMaxRows {
			rows.Close()
			return nil, fmt.Errorf("report exceeds %d source rows; select a shorter period or narrower user scope", reportMaxRows)
		}
		f := &reportFact{}
		var groups string
		var g, c int
		e, err := scanUsage(func(dest ...any) error {
			return rows.Scan(append(dest, &groups, &g, &c, &f.family, &f.provider, &f.hosting, &f.project, &f.center, &f.costStatus)...)
		})
		if err != nil {
			rows.Close()
			return nil, err
		}
		f.e = e
		f.groupsKnown = g != 0
		f.classificationKnown = c != 0
		if err = json.Unmarshal([]byte(groups), &f.groups); err != nil {
			f.groupsKnown = false
			f.groups = nil
		}
		if !f.groupsKnown {
			f.groups = nil
		}
		if len(scope.TeamIDs) > 0 && !reportIntersects(reportIDs(e.TeamIDs), scope.TeamIDs) {
			continue
		}
		out = append(out, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if d.GroupMode == "current" {
		memberships := map[string][]string{}
		rows, err := s.query(ctx, `SELECT user_id,group_id FROM group_member ORDER BY user_id,group_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var u, g string
			if err := rows.Scan(&u, &g); err != nil {
				rows.Close()
				return nil, err
			}
			memberships[u] = append(memberships[u], g)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, f := range out {
			f.groups = memberships[f.e.UserID]
			f.groupsKnown = true
		}
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, err
	}
	filtered := make([]*reportFact, 0, len(out))
	for _, f := range out {
		ok := true
		for dim, wanted := range d.Filters {
			if len(wanted) > 0 && !reportIntersects(reportDimension(f, dim, loc), wanted) {
				ok = false
				break
			}
		}
		if ok {
			filtered = append(filtered, f)
		}
	}
	return filtered, nil
}
func reportIDs(v string) []string { return reportUnique(strings.Split(v, ",")) }
func reportUnique(v []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, x := range v {
		x = strings.TrimSpace(x)
		if x != "" && !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
func reportDimension(f *reportFact, dim string, loc *time.Location) []string {
	e := f.e
	v := ""
	switch dim {
	case "user":
		v = e.UserID
	case "team":
		if ids := reportIDs(e.TeamIDs); len(ids) > 0 {
			return ids
		}
	case "group":
		if !f.groupsKnown {
			return []string{reportUnknown}
		}
		if ids := reportUnique(f.groups); len(ids) > 0 {
			return ids
		}
		return []string{"(none)"}
	case "model":
		v = e.ModelID
		if v == "" {
			v = e.ModelName
		}
	case "upstream":
		v = e.UpstreamID
	case "requested_model":
		v = e.RequestedModelName
	case "modality":
		v = e.Modality
	case "model_family":
		v = f.family
	case "provider":
		v = f.provider
	case "hosting":
		v = f.hosting
	case "project":
		v = f.project
	case "cost_center":
		v = f.center
	case "client_app":
		v = e.ClientApp
	case "service_token":
		v = e.ServiceTokenID
	case "status":
		v = strconv.Itoa(e.HTTPStatus)
	case "error":
		v = e.ErrorCode
	case "endpoint":
		v = e.EndpointPath
	case "day", "week", "month":
		t := e.CreatedAt.In(loc)
		if dim == "week" {
			t = t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
		}
		if dim == "month" {
			v = t.Format("2006-01")
		} else {
			v = t.Format("2006-01-02")
		}
	}
	if v == "" {
		v = reportUnknown
	}
	return []string{v}
}
func reportAggregate(facts []*reportFact, dims, metrics []string, loc *time.Location) ([]reporting.Row, error) {
	buckets := map[string]*reportAccumulator{}
	expanded := 0
	for _, f := range facts {
		combinations := [][]string{{}}
		for _, dim := range dims {
			next := [][]string{}
			for _, prefix := range combinations {
				for _, v := range reportDimension(f, dim, loc) {
					row := append(append([]string{}, prefix...), v)
					next = append(next, row)
					expanded++
					if expanded > reportMaxRows*10 {
						return nil, fmt.Errorf("report grouping expands beyond resource limit; select fewer dimensions or shorter period")
					}
				}
			}
			combinations = next
		}
		for _, values := range combinations {
			keyBytes, _ := json.Marshal(values)
			key := string(keyBytes)
			a := buckets[key]
			if a == nil {
				if len(buckets) >= reportMaxRows {
					return nil, fmt.Errorf("too many report groups; narrow period or dimensions")
				}
				a = &reportAccumulator{dims: map[string]string{}}
				for i, dim := range dims {
					a.dims[dim] = values[i]
				}
				buckets[key] = a
			}
			a.facts = append(a.facts, f)
		}
	}
	keys := []string{}
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []reporting.Row{}
	for _, k := range keys {
		a := buckets[k]
		out = append(out, reporting.Row{Dimensions: a.dims, Values: reportMetrics(a.facts, metrics)})
	}
	return out, nil
}
func reportMetrics(facts []*reportFact, metrics []string) map[string]*float64 {
	vals := map[string]*float64{}
	var nano, in, out, cached, writes int64
	var errors, complete, success, quota, security, fallback, estimated, unpriced int
	known := true
	users := map[string]bool{}
	lat, ttfb := []float64{}, []float64{}
	for _, f := range facts {
		e := f.e
		if (e.CostNano > 0 && nano > math.MaxInt64-e.CostNano) || (e.CostNano < 0 && nano < math.MinInt64-e.CostNano) {
			known = false
		} else {
			nano += e.CostNano
		}
		in += e.TokensIn
		out += e.TokensOut
		cached += e.TokensCached
		writes += e.TokensCacheWrite5m + e.TokensCacheWrite1h
		if e.HTTPStatus >= 400 {
			errors++
		}
		if e.HTTPStatus > 0 {
			complete++
			if e.HTTPStatus < 400 {
				success++
			}
		}
		if e.UserID != "" {
			users[e.UserID] = true
		}
		if e.LatencyMs > 0 {
			lat = append(lat, float64(e.LatencyMs))
		}
		if e.TTFBMs > 0 {
			ttfb = append(ttfb, float64(e.TTFBMs))
		}
		if e.QuotaViolated {
			quota++
		}
		if e.SecgwAction == "block" || e.SecgwAction == "blocked" || e.SecgwAction == "stream_cut" {
			security++
		}
		if e.FallbackReason != "" {
			fallback++
		}
		if e.AccountingMode == "byte_count_fallback" || strings.Contains(e.AccountingMode, "estimat") {
			estimated++
		}
		if f.costStatus == "unpriced" {
			unpriced++
		}
	}
	for _, m := range metrics {
		var v *float64
		switch m {
		case "requests":
			v = reportNumber(float64(len(facts)))
		case "tokens_in":
			v = reportNumber(float64(in))
		case "tokens_out":
			v = reportNumber(float64(out))
		case "tokens_cached":
			v = reportNumber(float64(cached))
		case "cost_usd":
			if known {
				v = reportNumber(float64(nano) / 1e9)
			}
		case "errors":
			v = reportNumber(float64(errors))
		case "success_rate":
			v = reportRatio(float64(success), float64(complete))
		case "cache_hit_rate":
			v = reportRatio(float64(cached), float64(in+writes))
		case "active_users":
			v = reportNumber(float64(len(users)))
		case "latency_p50_ms":
			v = reportPercentile(lat, .5)
		case "latency_p95_ms":
			v = reportPercentile(lat, .95)
		case "ttfb_p95_ms":
			v = reportPercentile(ttfb, .95)
		case "quota_denials":
			v = reportNumber(float64(quota))
		case "security_blocks":
			v = reportNumber(float64(security))
		case "fallbacks":
			v = reportNumber(float64(fallback))
		case "estimated_requests":
			v = reportNumber(float64(estimated))
		case "unpriced_requests":
			v = reportNumber(float64(unpriced))
		}
		vals[m] = v
	}
	return vals
}
func reportNumber(v float64) *float64 { return &v }
func reportRatio(n, d float64) *float64 {
	if d <= 0 {
		return nil
	}
	return reportNumber(n / d)
}

// Nearest-rank percentiles of observed positive timings, never averages of percentiles.
func reportPercentile(v []float64, p float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	return reportNumber(v[int(math.Ceil(p*float64(len(v))))-1])
}
func reportColumns(dims, metrics []string) []reporting.Column {
	out := []reporting.Column{}
	for _, d := range dims {
		out = append(out, reporting.Column{Key: d, Label: strings.ReplaceAll(d, "_", " ")})
	}
	catalog := reporting.GetCatalog()
	for _, m := range metrics {
		for _, c := range catalog.Metrics {
			if c.ID == m {
				out = append(out, reporting.Column{Key: m, Label: reportMetricLabel(m, c.Label), Unit: c.Unit})
			}
		}
	}
	return out
}
func reportMetricLabel(key, fallback string) string {
	if key == "cost_usd" {
		return "Recorded cost (USD)"
	}
	return fallback
}
func reportCostsKnown(facts []*reportFact) bool {
	for _, f := range facts {
		if f.costStatus != "priced" && f.costStatus != "known_free" {
			return false
		}
	}
	return true
}
func reportFactWarnings(facts []*reportFact) []string {
	r := &reporting.Result{}
	for _, f := range facts {
		if len(f.groups) > 1 || len(reportIDs(f.e.TeamIDs)) > 1 {
			reportWarn(r, "Group/team rows overlap and are nonadditive; totals count each request once.")
		}
		if !f.groupsKnown {
			reportWarn(r, "Historical group membership is unknown for some requests; current membership was not inferred.")
		}
		if !f.classificationKnown {
			reportWarn(r, "Some historical model classification is unknown.")
		}
		if f.costStatus != "priced" && f.costStatus != "known_free" {
			reportWarn(r, "Recorded cost includes historical amounts, but pricing confidence is incomplete (unknown, disabled or unpriced requests). Recorded zero does not prove free usage.")
		}
		if f.e.HTTPStatus == 0 {
			reportWarn(r, "Status 0 is incomplete, not a successful response or an HTTP error.")
		}
	}
	return r.Warnings
}

func reportComparisonWarn(r *reporting.Result, w string) {
	for _, existing := range r.ComparisonWarnings {
		if existing == w {
			return
		}
	}
	r.ComparisonWarnings = append(r.ComparisonWarnings, w)
	reportWarn(r, w)
}

func reportWarn(r *reporting.Result, w string) {
	for _, v := range r.Warnings {
		if v == w {
			return
		}
	}
	r.Warnings = append(r.Warnings, w)
}

// ResolveReportScope checks live authorization; saved definitions convey no grant.
func (s *Store) ResolveReportScope(ctx context.Context, userID string, d reporting.Definition) (reporting.Scope, error) {
	deny := fmt.Errorf("report scope denied")
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		return reporting.Scope{}, err
	}
	if !u.IsActive {
		return reporting.Scope{}, deny
	}
	switch d.Scope {
	case "self":
		return reporting.Scope{UserID: u.ID}, nil
	case "organization":
		if u.IsAdmin() {
			return reporting.Scope{Admin: true}, nil
		}
	case "team":
		team, err := s.TeamByID(ctx, d.TeamID)
		if err != nil {
			return reporting.Scope{}, err
		}
		if !team.ArchivedAt.IsZero() {
			return reporting.Scope{}, deny
		}
		if u.IsAdmin() {
			return reporting.Scope{Admin: true, TeamIDs: []string{team.ID}}, nil
		}
		role, err := s.TeamRole(ctx, team.ID, u.ID)
		if err != nil {
			return reporting.Scope{}, deny
		}
		if role == "leader" {
			return reporting.Scope{TeamIDs: []string{team.ID}}, nil
		}
	}
	return reporting.Scope{}, deny
}

// Labels decorate stable keys; neither joins nor missing current entities remove facts.
func (s *Store) reportLabels(ctx context.Context, r *reporting.Result) error {
	tables := map[string]string{"user": "app_user", "team": "team", "group": "user_group", "model": "model", "upstream": "upstream", "service_token": "service_token"}
	rowsets := [][]reporting.Row{r.Rows}
	for _, sec := range r.Sections {
		rowsets = append(rowsets, sec.Rows)
	}
	names := map[string]map[string]string{}
	dims := []string{"user", "team", "group", "model", "upstream", "service_token"}
	for _, dim := range dims {
		ids := []string{}
		for _, rows := range rowsets {
			for _, row := range rows {
				if id := row.Dimensions[dim]; id != "" && id != reportUnknown && id != "(none)" {
					ids = append(ids, id)
				}
			}
		}
		ids = reportUnique(ids)
		names[dim] = map[string]string{}
		for begin := 0; begin < len(ids); begin += 500 {
			end := begin + 500
			if end > len(ids) {
				end = len(ids)
			}
			args := []any{}
			for _, id := range ids[begin:end] {
				args = append(args, id)
			}
			rows, err := s.query(ctx, `SELECT id,name FROM `+tables[dim]+` WHERE id IN (`+placeholders(len(args))+`)`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, name string
				if err := rows.Scan(&id, &name); err != nil {
					rows.Close()
					return err
				}
				names[dim][id] = name
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
	}
	for _, rows := range rowsets {
		for _, row := range rows {
			for _, dim := range dims {
				if id, ok := row.Dimensions[dim]; ok {
					label := names[dim][id]
					if label == "" {
						if id == reportUnknown || id == "(none)" {
							label = id
						} else {
							label = "Unknown/deleted: " + id
						}
					}
					row.Dimensions[dim+"_label"] = label
				}
			}
		}
	}
	decorate := func(cols []reporting.Column) []reporting.Column {
		out := []reporting.Column{}
		for _, col := range cols {
			out = append(out, col)
			if _, ok := tables[col.Key]; ok {
				out = append(out, reporting.Column{Key: col.Key + "_label", Label: col.Label + " name (current)"})
			}
		}
		return out
	}
	r.Columns = decorate(r.Columns)
	for i := range r.Sections {
		r.Sections[i].Columns = decorate(r.Sections[i].Columns)
	}
	return nil
}
func reportIntersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x != "" && x == y {
				return true
			}
		}
	}
	return false
}
