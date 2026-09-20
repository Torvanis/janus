package store

import (
	"context"
	"fmt"
	"github.com/torvanis/janus/internal/reporting"
	"math"
	"sort"
	"strings"
	"time"
)

func reportPrevious(d reporting.Definition, start, end time.Time) (time.Time, time.Time) {
	loc, _ := time.LoadLocation(d.Timezone)
	s := start.In(loc)
	period := d.Period
	if d.SourcePeriod != "" {
		period = d.SourcePeriod
	}
	switch period {
	case "previous_month":
		return s.AddDate(0, -1, 0), start
	case "last_7_days":
		return s.AddDate(0, 0, -7), start
	case "last_30_days":
		return s.AddDate(0, 0, -30), start
	}
	// Explicit intervals compare equal elapsed time; calendar presets use local calendar arithmetic.
	return start.Add(-end.Sub(start)), start
}
func (s *Store) reportAnalysis(ctx context.Context, r *reporting.Result, facts []*reportFact, scope reporting.Scope) error {
	d := r.Definition
	counts := map[string]*float64{"priced_requests": reportNumber(0), "known_free_requests": reportNumber(0), "unpriced_requests": reportNumber(0), "disabled_cost_requests": reportNumber(0), "unknown_cost_requests": reportNumber(0)}
	for _, f := range facts {
		key := "unknown_cost_requests"
		switch f.costStatus {
		case "priced":
			key = "priced_requests"
		case "known_free":
			key = "known_free_requests"
		case "unpriced":
			key = "unpriced_requests"
		case "disabled":
			key = "disabled_cost_requests"
		}
		*counts[key]++
	}
	r.Sections = append(r.Sections, reportStatSection("cost_coverage", "Recorded cost confidence", counts, "Cost totals sum original integer nanodollar records, including legacy history. Missing price confidence does not erase recorded spend; zero recorded cost is not evidence that a request was free."))
	loc, _ := time.LoadLocation(d.Timezone)
	families := d.Sections
	if len(families) == 0 && d.Template != "" {
		families = []string{d.Template}
	}
	add := func(id, title string, dims, metrics []string, notes ...string) error {
		rows, err := reportAggregate(facts, dims, metrics, loc)
		if err != nil {
			return err
		}
		if len(dims) == 0 && len(rows) == 0 {
			rows = []reporting.Row{{Dimensions: map[string]string{}, Values: reportMetrics(nil, metrics)}}
		}
		if len(rows) == 0 {
			notes = append(notes, "No matching recorded requests in this interval.")
		}
		r.Sections = append(r.Sections, reporting.Section{ID: id, Title: title, Columns: reportColumns(dims, metrics), Rows: rows, Notes: notes})
		return nil
	}
	for _, family := range families {
		var err error
		switch family {
		case "executive":
			err = add("executive", "Executive summary", nil, []string{"requests", "cost_usd", "active_users", "errors", "success_rate"}, "Observed request records only. Costs are original recorded amounts, not repriced at today's rates.")
			if err == nil {
				err = add("executive_users", "User mix", []string{"user"}, []string{"requests", "cost_usd"}, "Stable identity keys; deleted identities remain represented.")
			}
			if err == nil {
				err = add("executive_models", "Model mix", []string{"model"}, []string{"requests", "cost_usd"}, "All observed models, including deleted and unknown catalog entries.")
			}
		case "usage":
			err = add("usage", "Usage over time", []string{"day"}, []string{"requests", "tokens_in", "tokens_out", "cost_usd", "active_users"}, "Half-open interval; calendar buckets use the report timezone.")
		case "adoption":
			err = s.reportAdoption(ctx, r, facts, scope)
			if err == nil {
				err = add("adoption_users", "Observed user activity", []string{"user"}, []string{"requests", "tokens_in", "tokens_out"}, "Activity is historical; the eligible population in the summary is a current active-user census, not a historical roster.")
			}
		case "portfolio":
			for _, dim := range []string{"modality", "model_family", "provider", "hosting", "model"} {
				if err = add("portfolio_"+dim, "Portfolio by "+strings.ReplaceAll(dim, "_", " "), []string{dim}, []string{"requests", "cost_usd", "tokens_in", "tokens_out"}, "Classification comes from recorded snapshots; missing taxonomy stays unknown, never guessed from names."); err != nil {
					break
				}
			}
		case "quotas":
			err = add("quotas", "Observed quota denials", []string{"team"}, []string{"requests", "quota_denials"}, "Observed denials are not a quota utilization ratio. Quota policy history is reported separately.")
			if err == nil {
				err = s.reportQuotas(ctx, r, facts, scope)
			}
			if err == nil {
				err = s.reportBudgets(ctx, r, facts, scope)
			}
		case "reliability":
			err = add("reliability", "Provider reliability", []string{"upstream"}, []string{"requests", "errors", "success_rate", "latency_p50_ms", "latency_p95_ms", "ttfb_p95_ms", "fallbacks"}, "Errors are HTTP status >=400; status 0 is excluded from success-rate denominators. Nearest-rank percentiles use positive per-request timings, not bucket averages.")
			if err == nil {
				err = add("reliability_errors", "Observed error codes", []string{"error"}, []string{"requests", "errors"}, "Error codes are aggregates, not causal diagnoses.")
			}
		case "efficiency":
			err = add("efficiency", "Cache and output efficiency", []string{"model"}, []string{"requests", "tokens_in", "tokens_out", "tokens_cached", "cache_hit_rate", "cost_usd", "errors", "fallbacks"}, "Cache ratio = cached / (input + 5m cache writes + 1h cache writes); cached tokens are a subset of input. Observed failures and fallbacks do not include invisible retry-attempt spend.")
			totals := reportMetrics(facts, []string{"tokens_in", "tokens_out", "tokens_cached", "cost_usd"})
			ratio := reportRatio(*totals["tokens_out"], *totals["tokens_in"])
			r.Sections = append(r.Sections, reportStatSection("efficiency_ratios", "Output ratio", map[string]*float64{"output_input_ratio": ratio}, "Output/input token ratio; null when no input tokens are observed."))
		case "integrations":
			for _, dim := range []string{"client_app", "service_token", "endpoint"} {
				if err = add("integrations_"+dim, "Integration by "+strings.ReplaceAll(dim, "_", " "), []string{dim}, []string{"requests", "errors", "latency_p95_ms", "cost_usd"}, "Only attributed traffic inside the authorized scope; no credentials, request payloads or IP addresses."); err != nil {
					break
				}
			}
		case "governance":
			err = add("governance", "Observed governance signals", []string{"user"}, []string{"requests", "quota_denials", "security_blocks", "errors", "fallbacks"}, "Recorded quota, security-block and error counts only. No payloads, credentials or IP addresses; no causal attribution.")
		case "data_quality":
			err = s.reportQuality(ctx, r, facts)
		default:
			return fmt.Errorf("unsupported report analysis family %q", family)
		}
		if err != nil {
			return err
		}
	}
	if d.ScenarioDiscountPercent > 0 {
		cost := reportMetrics(facts, []string{"cost_usd"})["cost_usd"]
		var hypothetical, savings *float64
		if cost != nil && reportCostsKnown(facts) {
			hypothetical = reportNumber(*cost * (1 - d.ScenarioDiscountPercent/100))
			savings = reportNumber(*cost * d.ScenarioDiscountPercent / 100)
		}
		r.Sections = append(r.Sections, reportStatSection("scenario", "Hypothetical discount scenario", map[string]*float64{"recorded_cost_usd": cost, "discount_percent": reportNumber(d.ScenarioDiscountPercent), "hypothetical_cost_usd": hypothetical, "hypothetical_savings_usd": savings}, "Hypothetical uniform discount on recorded spend, not retroactive repricing, a quote, or an enforcement change. Incomplete cost data makes the scenario unavailable."))
	}
	if d.Compare && r.PreviousTotals != nil {
		complete := r.ComparisonReliable != nil && *r.ComparisonReliable
		if !complete {
			reportWarn(r, "Comparison coverage is incomplete; recorded totals remain visible but changes/outlier flags are unavailable.")
		}
		values := map[string]*float64{}
		for _, m := range d.Metrics {
			a, b := r.Totals[m], r.PreviousTotals[m]
			values[m+"_relative_change"] = nil
			values[m+"_outlier"] = nil
			if complete && a != nil && b != nil && *b > 0 {
				delta := (*a - *b) / *b
				values[m+"_relative_change"] = reportNumber(delta)
				flag := 0.
				if delta >= .5 || delta <= -.5 {
					flag = 1
				}
				values[m+"_outlier"] = reportNumber(flag)
			}
		}
		r.Sections = append(r.Sections, reportStatSection("comparison", "Rule-based comparison", values, "Outlier rule: absolute relative change >=50% against a positive prior value. Zero, unknown or incompletely retained baselines yield null. This is a descriptive threshold, not an AI causal explanation."))
	}
	return nil
}
func reportStatSection(id, title string, values map[string]*float64, notes ...string) reporting.Section {
	keys := []string{}
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cols := []reporting.Column{}
	for _, k := range keys {
		unit := "count"
		switch {
		case strings.HasSuffix(k, "_outlier"):
			unit = "count"
		case strings.Contains(k, "ratio") || strings.Contains(k, "utilization") || strings.Contains(k, "change"):
			unit = "ratio"
		case strings.Contains(k, "percent"):
			unit = "percent"
		case strings.Contains(k, "usd"):
			unit = "USD"
		}
		cols = append(cols, reporting.Column{Key: k, Label: strings.ReplaceAll(k, "_", " "), Unit: unit})
	}
	return reporting.Section{ID: id, Title: title, Columns: cols, Rows: []reporting.Row{{Dimensions: map[string]string{}, Values: values}}, Notes: notes}
}
func (s *Store) reportAdoption(ctx context.Context, r *reporting.Result, facts []*reportFact, scope reporting.Scope) error {
	q := `SELECT u.id FROM app_user u WHERE u.is_active=1`
	args := []any{}
	if scope.UserID != "" {
		q += ` AND u.id=?`
		args = append(args, scope.UserID)
	}
	if len(scope.TeamIDs) > 0 {
		q += ` AND EXISTS(SELECT 1 FROM team_member m JOIN team t ON t.id=m.team_id WHERE m.user_id=u.id AND t.archived_at='' AND m.team_id IN (` + placeholders(len(scope.TeamIDs)) + `))`
		for _, id := range scope.TeamIDs {
			args = append(args, id)
		}
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return err
	}
	eligible := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		eligible[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, f := range facts {
		if eligible[f.e.UserID] {
			active[f.e.UserID] = true
		}
	}
	r.Sections = append(r.Sections, reportStatSection("adoption", "Current-census adoption", map[string]*float64{"eligible_users_current": reportNumber(float64(len(eligible))), "active_eligible_users": reportNumber(float64(len(active))), "adoption_ratio_current_census": reportRatio(float64(len(active)), float64(len(eligible)))}, "Denominator: currently active users in the authorized scope, not a reconstructed historical census. Numerator: those eligible users with matching recorded activity after report filters; filters narrow activity, not census eligibility."))
	return nil
}
func (s *Store) reportQuality(ctx context.Context, r *reporting.Result, facts []*reportFact) error {
	values := map[string]*float64{}
	groups, class, incomplete, disabled, unknown := 0., 0., 0., 0., 0.
	var earliest time.Time
	for _, f := range facts {
		if !f.groupsKnown {
			groups++
		}
		if !f.classificationKnown {
			class++
		}
		if f.e.HTTPStatus == 0 {
			incomplete++
		}
		if f.costStatus == "disabled" {
			disabled++
		}
		if f.costStatus == "unknown" {
			unknown++
		}
		if earliest.IsZero() || f.e.CreatedAt.Before(earliest) {
			earliest = f.e.CreatedAt
		}
	}
	values["unknown_group_requests"] = reportNumber(groups)
	values["unknown_classification_requests"] = reportNumber(class)
	values["incomplete_requests"] = reportNumber(incomplete)
	values["disabled_cost_requests"] = reportNumber(disabled)
	values["unknown_cost_requests"] = reportNumber(unknown)
	for k, v := range reportMetrics(facts, []string{"requests", "estimated_requests", "unpriced_requests"}) {
		values[k] = v
	}
	notes := []string{"Snapshot absence is historical unknown. Current memberships and taxonomy do not backfill facts. Earliest retained event is not proof of complete historical coverage."}
	if earliest.IsZero() {
		notes = append(notes, "No matching retained events; coverage start unavailable.")
	} else {
		notes = append(notes, "Earliest matching retained event: "+earliest.Format(time.RFC3339))
	}
	rows, err := s.query(ctx, `SELECT key,started_at FROM reporting_coverage ORDER BY key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, "Coverage marker "+key+": "+value)
		if key == "retention_before" && r.Start.Before(ParseTime(value)) {
			reportWarn(r, "Requested period precedes retention_before "+value+"; retained records cannot establish complete coverage.")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	r.Sections = append(r.Sections, reportStatSection("data_quality", "Data quality and retained coverage", values, notes...))
	return nil
}

// Policy and budget visibility must be wholly contained in the server scope.
func reportSubjectVisible(kind, id string, scope reporting.Scope) bool {
	if scope.Admin && scope.UserID == "" && len(scope.TeamIDs) == 0 {
		return true
	}
	if scope.UserID != "" {
		return kind == "user" && id == scope.UserID
	}
	return kind == "team" && reportIntersects([]string{id}, scope.TeamIDs)
}
func reportSubjectMatches(f *reportFact, kind, id string) bool {
	switch kind {
	case "organization", "all":
		return true
	case "user":
		return f.e.UserID == id
	case "team":
		return reportIntersects(reportIDs(f.e.TeamIDs), []string{id})
	case "group":
		return f.groupsKnown && reportIntersects(f.groups, []string{id})
	case "service_token":
		return f.e.ServiceTokenID == id
	}
	return false
}
func (s *Store) reportQuotas(ctx context.Context, r *reporting.Result, facts []*reportFact, scope reporting.Scope) error {
	history, err := s.ListReportQuotaHistory(ctx, r.Start, r.End)
	if err != nil {
		return err
	}
	retained, err := s.reportRetention(ctx)
	if err != nil {
		return err
	}
	visible := []ReportQuotaVersion{}
	for _, v := range history {
		if reportSubjectVisible(v.Policy.SubjectType, v.Policy.SubjectID, scope) {
			visible = append(visible, v)
		}
	}
	r.Sections = append(r.Sections, reportStatSection("quota_history_coverage", "Recorded quota history", map[string]*float64{"recorded_policy_versions": reportNumber(float64(len(visible)))}, "Only recorded versions; no current policy is invented for earlier periods. Baselines apply only from their effective timestamp."))
	if len(visible) == 0 {
		reportWarn(r, "No visible quota history for this interval; historical limits and utilization are unavailable.")
		return nil
	}
	section := reporting.Section{ID: "quota_windows", Title: "Historical quota windows", Notes: []string{"Every overlapping UTC calendar window is shown. Rolling windows are evaluated at the policy/report endpoint. Utilization is null unless the complete window is retained, inside the report, unfiltered and covered by the same historical policy. Values are observations, not live enforcement state."}, Columns: []reporting.Column{{Key: "quota", Label: "Quota"}, {Key: "policy_version", Label: "Policy version"}, {Key: "window_start", Label: "Window start"}, {Key: "window_end", Label: "Window end"}, {Key: "metric", Label: "Metric"}, {Key: "limit", Label: "Limit (metric units; USD for cost)"}, {Key: "observed", Label: "Observed window usage"}, {Key: "utilization", Label: "Utilization", Unit: "ratio"}}}
	for _, v := range visible {
		if v.Deleted {
			continue
		}
		q := v.Policy
		from, to := r.Start, r.End
		if v.EffectiveFrom.After(from) {
			from = v.EffectiveFrom
		}
		if !v.EffectiveTo.IsZero() && v.EffectiveTo.Before(to) {
			to = v.EffectiveTo
		}
		if !from.Before(to) {
			continue
		}
		rolling := strings.HasPrefix(q.Window, "rolling_")
		at := from
		if rolling {
			at = to
		}
		ws, we, err := reportQuotaBounds(q.Window, at)
		if err != nil {
			reportWarn(r, err.Error())
			continue
		}
		if rolling {
			we = to
		}
		for ws.Before(to) {
			if len(section.Rows) >= reportMaxRows {
				return fmt.Errorf("too many quota windows; select a shorter period")
			}
			limit := float64(q.Limit)
			if q.Metric == MetricCostUSD {
				limit /= 1e9
			}
			values := map[string]*float64{"limit": reportNumber(limit), "observed": nil, "utilization": nil}
			complete := !ws.Before(r.Start) && !we.After(r.End) && !ws.Before(v.EffectiveFrom) && (v.EffectiveTo.IsZero() || !we.After(v.EffectiveTo)) && !ws.Before(retained) && len(r.Definition.Filters) == 0 && r.Definition.GroupMode != "current"
			if complete {
				matching := []*reportFact{}
				for _, f := range facts {
					if !f.e.CreatedAt.Before(ws) && f.e.CreatedAt.Before(we) && reportSubjectMatches(f, q.SubjectType, q.SubjectID) && (q.ModelID == "" || q.ModelID == f.e.ModelID) {
						matching = append(matching, f)
					}
				}
				value := reportMetrics(matching, []string{q.Metric})[q.Metric]
				values["observed"] = value
				if value != nil {
					values["utilization"] = reportRatio(*value, limit)
				}
			} else {
				reportWarn(r, "Some quota windows lack complete policy/usage coverage; utilization is null rather than a broad-range/current-limit ratio.")
			}
			section.Rows = append(section.Rows, reporting.Row{Dimensions: map[string]string{"quota": v.QuotaID, "policy_version": v.ID, "window_start": ws.Format(time.RFC3339Nano), "window_end": we.Format(time.RFC3339Nano), "metric": q.Metric}, Values: values})
			if rolling {
				break
			}
			ws, we, err = reportQuotaBounds(q.Window, we)
			if err != nil {
				return err
			}
		}
	}
	if len(section.Rows) > 0 {
		r.Sections = append(r.Sections, section)
	}
	return nil
}
func (s *Store) reportRetention(ctx context.Context) (time.Time, error) {
	rows, err := s.query(ctx, `SELECT started_at FROM reporting_coverage WHERE key='retention_before'`)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	var at time.Time
	if rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return at, err
		}
		var err error
		at, err = time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return at, fmt.Errorf("invalid retention coverage marker: %w", err)
		}
	}
	return at, rows.Err()
}

// Kept local to avoid the quota -> store import cycle. Mirrors quota/window.go.
func reportQuotaBounds(window string, now time.Time) (time.Time, time.Time, error) {
	n := now.UTC()
	start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	switch window {
	case WindowDaily:
		return start, start.AddDate(0, 0, 1), nil
	case WindowWeekly:
		start = start.AddDate(0, 0, -((int(n.Weekday()) + 6) % 7))
		return start, start.AddDate(0, 0, 7), nil
	case WindowMonthly:
		start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), nil
	case WindowRolling24:
		return n.Add(-24 * time.Hour), n.Add(24 * time.Hour), nil
	case WindowRolling7:
		return n.AddDate(0, 0, -7), n.AddDate(0, 0, 7), nil
	case WindowRolling30:
		return n.AddDate(0, 0, -30), n.AddDate(0, 0, 30), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("unsupported historical quota window %q", window)
}
func reportBudgetRemaining(amount int64, facts []*reportFact) *float64 {
	remaining := amount
	for _, f := range facts {
		cost := f.e.CostNano
		if (cost < 0 && remaining > math.MaxInt64+cost) || (cost > 0 && remaining < math.MinInt64+cost) {
			return nil
		}
		remaining -= cost
	}
	return reportNumber(float64(remaining) / 1e9)
}
func (s *Store) reportBudgets(ctx context.Context, r *reporting.Result, facts []*reportFact, scope reporting.Scope) error {
	retained, err := s.reportRetention(ctx)
	if err != nil {
		return err
	}
	budgets, err := s.ListReportBudgets(ctx)
	if err != nil {
		return err
	}
	section := reporting.Section{ID: "budgets", Title: "Reporting-only budgets", Columns: []reporting.Column{{Key: "budget", Label: "Budget"}, {Key: "amount_usd", Label: "Budget", Unit: "USD"}, {Key: "spent_usd", Label: "Recorded spend", Unit: "USD"}, {Key: "remaining_usd", Label: "Remaining", Unit: "USD"}, {Key: "utilization", Label: "Utilization", Unit: "ratio"}, {Key: "projected_usd", Label: "Simple elapsed-day projection", Unit: "USD"}}, Notes: []string{"Reporting-only budgets are not enforcement rules. Spend/remaining are available only for complete budget-to-date coverage without narrowing filters. Projection is an elapsed-day run rate on recorded spend, not a prediction or repricing; fewer than one elapsed day or no requests yields null."}}
	for _, b := range budgets {
		if !reportSubjectVisible(b.Scope, b.SubjectID, scope) || !b.Start.Before(r.End) || !b.End.After(r.Start) {
			continue
		}
		at := b.End
		if r.GeneratedAt.Before(at) {
			at = r.GeneratedAt
		}
		amount := float64(b.AmountNano) / 1e9
		values := map[string]*float64{"amount_usd": reportNumber(amount), "spent_usd": nil, "remaining_usd": nil, "utilization": nil, "projected_usd": nil}
		if !b.Start.Before(r.Start) && !b.Start.Before(retained) && at.After(b.Start) && !at.After(r.End) && len(r.Definition.Filters) == 0 {
			matching := []*reportFact{}
			for _, f := range facts {
				if !f.e.CreatedAt.Before(b.Start) && f.e.CreatedAt.Before(at) && reportSubjectMatches(f, b.Scope, b.SubjectID) {
					matching = append(matching, f)
				}
			}
			spent := reportMetrics(matching, []string{"cost_usd"})["cost_usd"]
			values["spent_usd"] = spent
			if !reportCostsKnown(matching) {
				reportWarn(r, "Budget remaining/utilization/projection unavailable: incomplete pricing confidence.")
			}
			if spent != nil && reportCostsKnown(matching) {
				values["remaining_usd"] = reportBudgetRemaining(b.AmountNano, matching)
				values["utilization"] = reportRatio(*spent, amount)
				days := at.Sub(b.Start).Hours() / 24
				if days >= 1 && len(matching) > 0 {
					values["projected_usd"] = reportNumber(*spent * (b.End.Sub(b.Start).Hours() / 24) / days)
				} else {
					reportWarn(r, "Budget projection unavailable: insufficient elapsed history.")
				}
			}
		} else {
			reportWarn(r, "Budget period exceeds complete unfiltered report coverage; spend, remaining and projection are unavailable.")
		}
		section.Rows = append(section.Rows, reporting.Row{Dimensions: map[string]string{"budget": b.ID, "budget_label": b.Name, "start": b.Start.Format(time.RFC3339), "end": b.End.Format(time.RFC3339)}, Values: values})
	}
	if len(section.Rows) > 0 {
		r.Sections = append(r.Sections, section)
	}
	return nil
}
