// Package reporting defines versioned, storage-independent report contracts.
package reporting

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type Definition struct {
	Version                 int                 `json:"version"`
	Name                    string              `json:"name"`
	Template                string              `json:"template"`
	Scope                   string              `json:"scope"`
	TeamID                  string              `json:"team_id"`
	Start                   string              `json:"start"`
	End                     string              `json:"end"`
	Period                  string              `json:"period"`
	SourcePeriod            string              `json:"source_period,omitempty"`
	Timezone                string              `json:"timezone"`
	Compare                 bool                `json:"compare"`
	GroupMode               string              `json:"group_mode"`
	Dimensions              []string            `json:"dimensions"`
	Metrics                 []string            `json:"metrics"`
	Filters                 map[string][]string `json:"filters"`
	Sections                []string            `json:"sections"`
	ScenarioDiscountPercent float64             `json:"scenario_discount_percent"`
}
type Scope struct {
	UserID    string   `json:"user_id"`
	TeamIDs   []string `json:"team_ids"`
	Admin     bool     `json:"admin"`
	HideCosts bool     `json:"-"`
}
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
}
type Row struct {
	Dimensions map[string]string   `json:"dimensions"`
	Values     map[string]*float64 `json:"values"`
}
type Result struct {
	Version            int                 `json:"version"`
	Definition         Definition          `json:"definition"`
	GeneratedAt        time.Time           `json:"generated_at"`
	DataCutoff         time.Time           `json:"data_cutoff"`
	Start              time.Time           `json:"start"`
	End                time.Time           `json:"end"`
	Columns            []Column            `json:"columns"`
	Rows               []Row               `json:"rows"`
	Totals             map[string]*float64 `json:"totals"`
	PreviousTotals     map[string]*float64 `json:"previous_totals"`
	ComparisonReliable *bool               `json:"comparison_reliable,omitempty"`
	ComparisonWarnings []string            `json:"comparison_warnings,omitempty"`
	Warnings           []string            `json:"warnings"`
	Sections           []Section           `json:"sections"`
	SourceRows         int64               `json:"source_rows"`
}
type Section struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
	Notes   []string `json:"notes"`
}
type CatalogItem struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
}
type Catalog struct {
	Version    int           `json:"version"`
	Dimensions []CatalogItem `json:"dimensions"`
	Metrics    []CatalogItem `json:"metrics"`
	Templates  []Definition  `json:"templates"`
}

const dimensionIDs = "user team group model upstream requested_model modality model_family provider hosting client_app project cost_center service_token status error endpoint day week month"
const metricIDs = "requests tokens_in tokens_out tokens_cached cost_usd errors success_rate latency_p50_ms latency_p95_ms ttfb_p95_ms cache_hit_rate active_users quota_denials security_blocks fallbacks estimated_requests unpriced_requests"
const familyIDs = "executive usage adoption portfolio quotas reliability efficiency integrations governance data_quality"
const templateIDs = familyIDs + " team_usage"

func contains(list, id string) bool {
	for _, s := range strings.Fields(list) {
		if s == id {
			return true
		}
	}
	return false
}
func label(id string) string {
	labels := map[string]string{
		"team_usage": "Team usage",
		"executive":  "Executive overview", "usage": "Usage and cost allocation", "adoption": "Adoption and engagement", "portfolio": "Model portfolio", "quotas": "Quotas and budgets", "reliability": "Reliability and performance", "efficiency": "Efficiency and optimization", "integrations": "Applications and integrations", "governance": "Governance and security", "data_quality": "Data quality and reconciliation",
		"tokens_in": "Input tokens", "tokens_out": "Output tokens", "tokens_cached": "Cached input tokens", "cost_usd": "Recorded cost (USD)", "latency_p50_ms": "Median latency", "latency_p95_ms": "95th-percentile latency", "ttfb_p95_ms": "95th-percentile time to first byte", "requested_model": "Requested model or alias", "client_app": "Application", "service_token": "Service credential", "modality": "Request modality", "unpriced_requests": "Requests with incomplete pricing", "estimated_requests": "Requests with estimated usage",
	}
	if text, ok := labels[id]; ok {
		return text
	}
	text := strings.ReplaceAll(id, "_", " ")
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}
func metricUnit(id string) string {
	switch {
	case id == "cost_usd":
		return "USD"
	case strings.HasSuffix(id, "_ms"):
		return "ms"
	case strings.HasSuffix(id, "_rate"):
		return "ratio"
	case strings.HasPrefix(id, "tokens_"):
		return "tokens"
	default:
		return "count"
	}
}

// GetCatalog returns fresh mutable copies. Template defaults are explicit;
// callers must apply their own defaults to ad-hoc definitions before Validate.
func GetCatalog() Catalog {
	c := Catalog{Version: 1}
	for _, id := range strings.Fields(dimensionIDs) {
		c.Dimensions = append(c.Dimensions, CatalogItem{id, label(id), "", "Group or filter by " + label(id)})
	}
	for _, id := range strings.Fields(metricIDs) {
		c.Metrics = append(c.Metrics, CatalogItem{id, label(id), metricUnit(id), "Observed " + label(id) + "; unavailable values are null"})
	}
	metrics := [][]string{{"requests", "cost_usd", "active_users"}, {"requests", "tokens_in", "tokens_out"}, {"active_users", "requests"}, {"requests", "cost_usd"}, {"quota_denials", "requests"}, {"errors", "success_rate", "latency_p95_ms"}, {"cache_hit_rate", "cost_usd"}, {"requests", "errors"}, {"security_blocks", "quota_denials"}, {"estimated_requests", "unpriced_requests"}, {"requests", "tokens_in", "tokens_out", "cost_usd"}}
	dims := [][]string{{"day"}, {"model"}, {"user"}, {"model_family"}, {"team"}, {"upstream"}, {"model"}, {"client_app"}, {"user"}, {"model"}, {"team", "model"}}
	for i, id := range strings.Fields(templateIDs) {
		section := id
		if id == "team_usage" {
			// The primary table groups by team and model; reuse the existing
			// usage-over-time analysis rather than requiring a new engine family.
			section = "usage"
		}
		c.Templates = append(c.Templates, Definition{Version: 1, Name: label(id), Template: id, Scope: "self", Period: "last_30_days", Timezone: "UTC", GroupMode: "historical", Dimensions: dims[i], Metrics: metrics[i], Sections: []string{section}, Filters: map[string][]string{}})
	}
	return c
}

// RedactCosts returns an independent snapshot containing only explicit known
// nonmonetary schemas. Titles and free-form financial commentary are withheld.
// Entity display labels are identifiers, not financial metrics.
func RedactCosts(r Result) Result {
	out := r
	redact := func(id string, cols []Column, rows []Row) ([]Column, []Row, func(map[string]*float64) map[string]*float64) {
		metrics := ""
		for _, k := range strings.Fields(metricIDs) {
			if k != "cost_usd" {
				metrics += " " + k
			}
		}
		dimensions := dimensionIDs
		for _, k := range strings.Fields(dimensionIDs) {
			dimensions += " " + k + "_label"
		}
		switch id {
		case "adoption":
			metrics += " eligible_users_current active_eligible_users adoption_ratio_current_census"
		case "efficiency_ratios":
			metrics += " output_input_ratio"
		case "cost_coverage":
			metrics += " priced_requests known_free_requests disabled_cost_requests unknown_cost_requests"
		case "data_quality":
			metrics += " unknown_group_requests unknown_classification_requests incomplete_requests disabled_cost_requests unknown_cost_requests"
		case "quota_history_coverage":
			metrics += " recorded_policy_versions"
		case "quota_windows":
			metrics += " limit observed utilization"
			dimensions += " quota policy_version window_start window_end metric"
		case "comparison":
			for _, k := range strings.Fields(metrics) {
				metrics += " " + k + "_relative_change " + k + "_outlier"
			}
		}
		// Unit declarations only narrow the schema, and apply to this table only.
		blocked := map[string]bool{}
		for _, c := range cols {
			if !contains("count tokens ms ratio percent", strings.ToLower(c.Unit)) && c.Unit != "" {
				blocked[c.Key] = true
			}
		}
		values := func(in map[string]*float64) map[string]*float64 {
			if in == nil {
				return nil
			}
			m := map[string]*float64{}
			for k, v := range in {
				if contains(metrics, k) && !blocked[k] {
					if v == nil {
						m[k] = nil
					} else {
						x := *v
						m[k] = &x
					}
				}
			}
			return m
		}
		var cc []Column
		if cols != nil {
			cc = []Column{}
		}
		for _, c := range cols {
			if !blocked[c.Key] && (contains(metrics, c.Key) || contains(dimensions, c.Key)) {
				unit := ""
				if contains(metrics, c.Key) {
					unit = metricUnit(c.Key)
					if c.Unit != "" {
						unit = strings.ToLower(c.Unit)
					}
					if strings.Contains(c.Key, "ratio") || strings.HasSuffix(c.Key, "_relative_change") || c.Key == "utilization" {
						unit = "ratio"
					}
				}
				cc = append(cc, Column{c.Key, label(c.Key), unit})
			}
		}
		var rr []Row
		if rows != nil {
			rr = []Row{}
		}
		for _, row := range rows {
			// Quota quantities inherit the row's metric, not the generic column unit.
			// Unknown/missing metrics fail closed, including legacy snapshots.
			if id == "quota_windows" && !contains("requests tokens_in tokens_out", row.Dimensions["metric"]) {
				continue
			}
			var dd map[string]string
			if row.Dimensions != nil {
				dd = map[string]string{}
				for k, v := range row.Dimensions {
					if contains(dimensions, k) && !blocked[k] {
						dd[k] = v
					}
				}
			}
			rr = append(rr, Row{Dimensions: dd, Values: values(row.Values)})
		}
		return cc, rr, values
	}
	var values func(map[string]*float64) map[string]*float64
	out.Columns, out.Rows, values = redact("", r.Columns, r.Rows)
	out.Totals = values(r.Totals)
	out.PreviousTotals = values(r.PreviousTotals)
	out.Definition.Name = "Usage report (costs disabled)"
	// Only exact built-in titles (including their already-redacted form) are
	// safe to retain. Never pass through arbitrary saved report names.
	if contains(templateIDs, r.Definition.Template) {
		canonical := label(r.Definition.Template)
		safe := canonical
		switch r.Definition.Template {
		case "usage":
			safe = "Usage and allocation"
		case "quotas":
			safe = "Quotas"
		}
		if r.Definition.Name == canonical || r.Definition.Name == safe {
			out.Definition.Name = safe
		}
	}
	out.Definition.ScenarioDiscountPercent = 0
	out.Definition.Dimensions = append([]string(nil), r.Definition.Dimensions...)
	out.Definition.Sections = append([]string(nil), r.Definition.Sections...)
	out.Definition.Metrics = []string{}
	for _, k := range r.Definition.Metrics {
		if _, ok := values(map[string]*float64{k: nil})[k]; ok {
			out.Definition.Metrics = append(out.Definition.Metrics, k)
		}
	}
	if r.Definition.Filters != nil {
		out.Definition.Filters = map[string][]string{}
		for k, v := range r.Definition.Filters {
			if contains(dimensionIDs, k) {
				out.Definition.Filters[k] = append([]string(nil), v...)
			}
		}
	}
	out.Sections = nil
	// Enumerated producer outputs, not arbitrary family-prefix passthrough.
	const analyses = "executive_users executive_models adoption_users portfolio_modality portfolio_model_family portfolio_provider portfolio_hosting portfolio_model reliability_errors efficiency_ratios integrations_client_app integrations_service_token integrations_endpoint cost_coverage quota_history_coverage quota_windows comparison"
	for _, s := range r.Sections {
		if !contains(familyIDs, s.ID) && !contains(analyses, s.ID) {
			continue
		}
		cc, rr, _ := redact(s.ID, s.Columns, s.Rows)
		out.Sections = append(out.Sections, Section{ID: s.ID, Title: label(s.ID), Columns: cc, Rows: rr, Notes: safeOperationalNotices(s.Notes)})
	}
	out.ComparisonWarnings = nil
	if r.ComparisonReliable != nil {
		reliable := *r.ComparisonReliable
		out.ComparisonReliable = &reliable
		if !reliable {
			out.ComparisonWarnings = []string{"Comparison unavailable: coverage or baseline confidence is insufficient; changes and outlier flags are unavailable."}
		}
	}
	if len(r.ComparisonWarnings) > 0 && len(out.ComparisonWarnings) == 0 {
		out.ComparisonWarnings = []string{"Comparison caveats apply; original free-text details withheld in cost-disabled mode."}
	}
	out.ComparisonWarnings = append(out.ComparisonWarnings, safeOperationalNotices(r.ComparisonWarnings)...)
	out.Warnings = append(safeOperationalNotices(r.Warnings), "Original free-text warnings and notes withheld in cost-disabled mode; data-quality caveats may apply", "Cost reporting disabled for this deployment")
	return out
}

// safeOperationalNotices recognizes fixed producer messages and typed timestamps,
// never a keyword-based free-text pass. Unknown messages remain withheld.
func safeOperationalNotices(in []string) []string {
	out := []string{}
	for _, text := range in {
		prefix := ""
		body := text
		if strings.HasPrefix(body, "Previous period: ") {
			prefix = "Previous period: "
			body = strings.TrimPrefix(body, prefix)
		}
		safe := ""
		switch body {
		case "No matching recorded requests in this interval.",
			"No matching retained events; coverage start unavailable.",
			"Requested period is provisional: event timestamps are limited to data_cutoff; later-arriving records may change totals.",
			"Groups represent a current-member cohort, not historical membership.",
			"Group/team rows overlap and are nonadditive; totals count each request once.",
			"Historical group membership is unknown for some requests; current membership was not inferred.",
			"Some historical model classification is unknown.",
			"Status 0 is incomplete, not a successful response or an HTTP error.",
			"Current period precedes retention_before; comparison coverage is incomplete.",
			"Current period is provisional; comparison coverage is incomplete at data_cutoff.",
			"Comparison coverage is incomplete; recorded totals remain visible but changes/outlier flags are unavailable.",
			"No visible quota history for this interval; historical limits and utilization are unavailable.",
			"Some quota windows lack complete policy/usage coverage; utilization is null rather than a broad-range/current-limit ratio.",
			"Only recorded versions; no current policy is invented for earlier periods. Baselines apply only from their effective timestamp.",
			"Snapshot absence is historical unknown. Current memberships and taxonomy do not backfill facts. Earliest retained event is not proof of complete historical coverage.",
			"Denominator: currently active users in the authorized scope, not a reconstructed historical census. Numerator: those eligible users with matching recorded activity after report filters; filters narrow activity, not census eligibility.",
			"Every overlapping UTC calendar window is shown. Rolling windows are evaluated at the policy/report endpoint. Utilization is null unless the complete window is retained, inside the report, unfiltered and covered by the same historical policy. Values are observations, not live enforcement state.",
			"Output/input token ratio; null when no input tokens are observed.":
			safe = body
		}
		for _, pattern := range [][2]string{
			{"Coverage marker retention_before: ", ""}, {"Coverage marker usage_snapshots: ", ""},
			{"Earliest matching retained event: ", ""},
			{"Requested period precedes retention_before ", "; retained records cannot establish complete coverage."},
			{"Previous period precedes retention_before ", "; retained records cannot establish complete comparison coverage."},
		} {
			if strings.HasPrefix(body, pattern[0]) && strings.HasSuffix(body, pattern[1]) {
				stamp := strings.TrimSuffix(strings.TrimPrefix(body, pattern[0]), pattern[1])
				if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
					safe = pattern[0] + at.Format(time.RFC3339Nano) + pattern[1]
				}
			}
		}
		if safe != "" {
			out = append(out, prefix+safe)
		}
	}
	return out
}

// Validate requires explicit version, scope, period, timezone and group mode.
// Custom intervals are limited to 366 elapsed days; exceeding the bound errors,
// never truncates. Scope authorization is the caller's responsibility.
func Validate(d Definition) error {
	if d.Version != 1 {
		return fmt.Errorf("unsupported definition version %d", d.Version)
	}
	if !contains("self team organization", d.Scope) {
		return fmt.Errorf("invalid scope")
	}
	if (d.Scope == "team") != (d.TeamID != "") {
		return fmt.Errorf("team_id is required only for team scope")
	}
	if d.Timezone == "" || d.Timezone == "Local" {
		return fmt.Errorf("explicit IANA timezone required")
	}
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if !contains("historical current", d.GroupMode) {
		return fmt.Errorf("invalid group_mode")
	}
	if d.Template != "" && !contains(templateIDs, d.Template) {
		return fmt.Errorf("invalid template")
	}
	if len(d.Dimensions) > 3 {
		return fmt.Errorf("at most three dimensions")
	}
	if len(d.Metrics) == 0 {
		return fmt.Errorf("at least one metric required")
	}
	check := func(values []string, allowed string) error {
		seen := map[string]bool{}
		for _, v := range values {
			if !contains(allowed, v) || seen[v] {
				return fmt.Errorf("unknown or duplicate field %q", v)
			}
			seen[v] = true
		}
		return nil
	}
	if err := check(d.Dimensions, dimensionIDs); err != nil {
		return err
	}
	if err := check(d.Metrics, metricIDs); err != nil {
		return err
	}
	if err := check(d.Sections, familyIDs); err != nil {
		return err
	}
	for k, v := range d.Filters {
		if !contains(dimensionIDs, k) {
			return fmt.Errorf("unknown filter %q", k)
		}
		if len(v) == 0 {
			return fmt.Errorf("empty filter %q", k)
		}
		for _, s := range v {
			if strings.ContainsRune(s, 0) {
				return fmt.Errorf("NUL in filter")
			}
		}
	}
	if math.IsNaN(d.ScenarioDiscountPercent) || math.IsInf(d.ScenarioDiscountPercent, 0) || d.ScenarioDiscountPercent < 0 || d.ScenarioDiscountPercent > 100 {
		return fmt.Errorf("discount must be finite and within 0..100")
	}
	if !contains("custom last_7_days last_30_days previous_month", d.Period) {
		return fmt.Errorf("invalid period")
	}
	if d.SourcePeriod != "" && !contains("custom last_7_days last_30_days previous_month", d.SourcePeriod) {
		return fmt.Errorf("invalid source period")
	}
	if d.Period == "custom" {
		_, _, err := customPeriod(d)
		return err
	}
	if d.Start != "" || d.End != "" {
		return fmt.Errorf("start/end only permitted for custom period")
	}
	return nil
}
func customPeriod(d Definition) (time.Time, time.Time, error) {
	s, err := time.Parse(time.RFC3339, d.Start)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	e, err := time.Parse(time.RFC3339, d.End)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	if !s.Before(e) || e.Sub(s) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("interval must be positive and at most 366 days")
	}
	return s, e, nil
}

// ResolvePeriod returns [start,end). Rolling periods end at now and subtract
// local calendar days, preserving wall-clock time across DST (not fixed hours).
func ResolvePeriod(d Definition, now time.Time) (start, end time.Time, err error) {
	if err = Validate(d); err != nil {
		return
	}
	if d.Period == "custom" {
		return customPeriod(d)
	}
	loc, _ := time.LoadLocation(d.Timezone)
	n := now.In(loc)
	end = n
	switch d.Period {
	case "last_7_days":
		start = end.AddDate(0, 0, -7)
	case "last_30_days":
		start = end.AddDate(0, 0, -30)
	case "previous_month":
		end = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
		start = end.AddDate(0, -1, 0)
	}
	return
}
