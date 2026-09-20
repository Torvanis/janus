package reporting

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRedactionPreservesOperationalAnalysis(t *testing.T) {
	r := fixture()
	r.Sections = nil
	for _, id := range []string{"portfolio_modality", "portfolio_model_family", "portfolio_provider", "portfolio_hosting", "portfolio_model", "integrations_client_app", "integrations_service_token", "integrations_endpoint", "adoption_users", "executive_users", "executive_models", "reliability_errors"} {
		r.Sections = append(r.Sections, Section{ID: id, Columns: []Column{{"requests", "Requests", "count"}}, Rows: []Row{{Dimensions: map[string]string{"user_label": "Alice"}, Values: map[string]*float64{"requests": ptr(7), "cost_usd": ptr(987.65)}}}})
	}
	r.Sections = append(r.Sections, Section{ID: "adoption", Columns: []Column{{"eligible_users_current", "Eligible", "count"}, {"active_eligible_users", "Active", "count"}, {"adoption_ratio_current_census", "Adoption", "ratio"}}, Rows: []Row{{Values: map[string]*float64{"eligible_users_current": ptr(10), "active_eligible_users": ptr(7), "adoption_ratio_current_census": ptr(.7)}}}})
	got := RedactCosts(r)
	if len(got.Sections) != len(r.Sections) {
		t.Fatalf("operational sections dropped: got %d want %d", len(got.Sections), len(r.Sections))
	}
	for i, s := range got.Sections {
		if i < len(got.Sections)-1 && (s.Rows[0].Values["requests"] == nil || s.Rows[0].Dimensions["user_label"] != "Alice") {
			t.Fatalf("operational data dropped: %+v", s)
		}
		if _, ok := s.Rows[0].Values["cost_usd"]; ok {
			t.Fatal("money leaked")
		}
	}
	if got.Sections[len(got.Sections)-1].Rows[0].Values["adoption_ratio_current_census"] == nil {
		t.Fatal("adoption census dropped")
	}
}

func operationalFixture() Result {
	r := fixture()
	reliable := false
	r.ComparisonReliable = &reliable
	r.ComparisonWarnings = []string{"Previous period: recorded spend $987.65", "Previous period: Some historical model classification is unknown."}
	r.Warnings = []string{"Some quota windows lack complete policy/usage coverage; utilization is null rather than a broad-range/current-limit ratio."}
	r.Sections = []Section{
		{ID: "quota_windows", Columns: []Column{{"metric", "Metric", ""}, {"limit", "USD for cost", "count"}, {"observed", "Observed", "count"}, {"utilization", "Utilization", "ratio"}, {"current", "Current", "count"}, {"consumed", "Consumed", "count"}}, Rows: []Row{
			{Dimensions: map[string]string{"metric": "tokens_in", "quota": "q-tokens", "policy_version": "v1", "window_start": "2026-02-01T00:00:00Z"}, Values: map[string]*float64{"limit": ptr(100), "observed": ptr(40), "utilization": ptr(.4)}},
			{Dimensions: map[string]string{"metric": "cost_usd", "quota": "q-money"}, Values: map[string]*float64{"limit": ptr(987.65), "current": ptr(987.65), "consumed": ptr(987.65), "utilization": ptr(.98765)}},
			{Dimensions: map[string]string{"metric": "unknown"}, Values: map[string]*float64{"limit": ptr(987.65)}},
		}},
		{ID: "data_quality", Columns: []Column{{"unknown_cost_requests", "Unknown costs", "count"}, {"requests", "Money disguised as requests", "EUR"}}, Rows: []Row{{Values: map[string]*float64{"unknown_cost_requests": ptr(3), "requests": ptr(987.65)}}}, Notes: []string{"Coverage marker retention_before: 2026-02-01T00:00:00Z", "Coverage marker retention_before: $987.65", "Coverage marker money: 2026-02-01T00:00:00Z"}},
		{ID: "cost_coverage", Rows: []Row{{Values: map[string]*float64{"priced_requests": ptr(5), "known_free_requests": ptr(1), "unknown_cost_requests": ptr(3)}}}},
		{ID: "quota_history_coverage", Rows: []Row{{Values: map[string]*float64{"recorded_policy_versions": ptr(2)}}}},
		{ID: "efficiency_ratios", Columns: []Column{{"output_input_ratio", "Ratio", "ratio"}}, Rows: []Row{{Values: map[string]*float64{"output_input_ratio": ptr(.5)}}}},
		{ID: "comparison", Rows: []Row{{Values: map[string]*float64{"requests_relative_change": nil, "requests_outlier": nil, "cost_usd_relative_change": ptr(987.65)}}}},
		{ID: "budgets", Rows: []Row{{Values: map[string]*float64{"utilization": ptr(.98765)}}}},
		{ID: "scenario", Rows: []Row{{Values: map[string]*float64{"discount_percent": ptr(98.765)}}}},
		{ID: "portfolio_secret", Rows: []Row{{Values: map[string]*float64{"requests": ptr(987.65)}}}},
	}
	return r
}

func TestRedactionMixedQuotaSchemasAndCoverage(t *testing.T) {
	r := operationalFixture()
	before, _ := json.Marshal(r)
	got := RedactCosts(r)
	if len(got.Sections) != 6 {
		t.Fatalf("unexpected sections: %+v", got.Sections)
	}
	quota := got.Sections[0]
	if len(quota.Rows) != 1 || *quota.Rows[0].Values["limit"] != 100 || *quota.Rows[0].Values["observed"] != 40 || quota.Rows[0].Dimensions["policy_version"] != "v1" {
		t.Fatalf("quota schema lost: %+v", quota)
	}
	if got.Sections[1].Rows[0].Values["unknown_cost_requests"] == nil || got.Totals["requests"] == nil {
		t.Fatal("count or unrelated table removed")
	}
	if _, ok := got.Sections[1].Rows[0].Values["requests"]; ok {
		t.Fatal("currency override ignored")
	}
	if got.Sections[1].Notes[0] != "Coverage marker retention_before: 2026-02-01T00:00:00Z" {
		t.Fatalf("safe coverage notice lost: %v", got.Sections[1].Notes)
	}
	raw, _ := json.Marshal(got)
	for _, secret := range []string{"987.65", "98765", "98.765", "q-money", "cost_usd", "Coverage marker money"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if got.ComparisonReliable == nil || *got.ComparisonReliable || !strings.Contains(strings.Join(got.ComparisonWarnings, " "), "Comparison unavailable") {
		t.Fatal("comparison confidence lost")
	}
	if !strings.Contains(strings.Join(got.ComparisonWarnings, " "), "Previous period: Some historical model classification is unknown.") || !strings.Contains(strings.Join(got.Warnings, " "), "Some quota windows") {
		t.Fatal("safe operational warnings lost")
	}
	*got.ComparisonReliable = true
	got.ComparisonWarnings[0] = "changed"
	*got.Sections[0].Rows[0].Values["limit"] = 1
	got.Sections[0].Rows[0].Dimensions["quota"] = "changed"
	got.Sections[1].Notes[0] = "changed"
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("redaction mutated frozen source")
	}
}

func TestValidationRejectsInvalidDefinitions(t *testing.T) {
	cases := map[string]func(*Definition){
		"version": func(d *Definition) { d.Version = 2 }, "scope": func(d *Definition) { d.Scope = "all" }, "team": func(d *Definition) { d.Scope = "team" }, "stray_team": func(d *Definition) { d.TeamID = "t" }, "timezone": func(d *Definition) { d.Timezone = "Mars/Base" }, "local": func(d *Definition) { d.Timezone = "Local" }, "period": func(d *Definition) { d.Period = "forever" }, "group": func(d *Definition) { d.GroupMode = "future" }, "dims": func(d *Definition) { d.Dimensions = []string{"user", "team", "model", "day"} }, "duplicate": func(d *Definition) { d.Metrics = []string{"requests", "requests"} }, "metric": func(d *Definition) { d.Metrics = []string{"sum(secret)"} }, "empty": func(d *Definition) { d.Metrics = nil }, "filter": func(d *Definition) { d.Filters = map[string][]string{"1=1": {"x"}} }, "discount": func(d *Definition) { d.ScenarioDiscountPercent = math.NaN() }, "discount_high": func(d *Definition) { d.ScenarioDiscountPercent = 101 }, "section": func(d *Definition) { d.Sections = []string{"fake"} }, "template": func(d *Definition) { d.Template = "fake" }, "custom": func(d *Definition) { d.Period = "custom" }, "mixed": func(d *Definition) { d.Start = "2026-01-01T00:00:00Z" }, "wide": func(d *Definition) {
			d.Period = "custom"
			d.Start = "2020-01-01T00:00:00Z"
			d.End = "2026-01-01T00:00:00Z"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			d := GetCatalog().Templates[0]
			change(&d)
			if Validate(d) == nil {
				t.Fatal("accepted invalid definition")
			}
		})
	}
}
func TestRollingPeriodsIncludeCurrentHour(t *testing.T) {
	for _, period := range []string{"last_7_days", "last_30_days"} {
		d := GetCatalog().Templates[0]
		d.Period = period
		d.Timezone = "America/New_York"
		now := time.Date(2026, 3, 10, 16, 37, 5, 0, time.UTC)
		s, e, err := ResolvePeriod(d, now)
		if err != nil {
			t.Fatal(err)
		}
		if !e.Equal(now) || !e.After(now.Add(-time.Minute)) {
			t.Fatalf("%s excludes current hour: %v", period, e)
		}
		if s.Hour() != 12 || s.Minute() != 37 || s.Second() != 5 {
			t.Fatal("local clock not preserved", s)
		}
	}
}

func TestResolveCalendarPeriods(t *testing.T) {
	d := GetCatalog().Templates[0]
	d.Timezone = "America/New_York"
	d.Period = "last_7_days"
	now := time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC)
	s, e, err := ResolvePeriod(d, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Format(time.RFC3339) != "2026-03-03T12:00:00-05:00" || e.Format(time.RFC3339) != "2026-03-10T12:00:00-04:00" {
		t.Fatalf("%v %v", s, e)
	}
	d.Period = "previous_month"
	s, e, err = ResolvePeriod(d, now)
	if err != nil || s.Day() != 1 || s.Month() != time.February || e.Month() != time.March || e.Day() != 1 {
		t.Fatalf("%v %v %v", s, e, err)
	}
	d.Period = "custom"
	d.Start = "2026-01-01T00:00:00Z"
	d.End = d.Start
	if _, _, err = ResolvePeriod(d, now); err == nil {
		t.Fatal("empty interval accepted")
	}
	d.End = "2026-01-02T00:00:00Z"
	s, e, err = ResolvePeriod(d, now)
	if err != nil || e.Sub(s) != 24*time.Hour {
		t.Fatal(s, e, err)
	}
}

func TestTeamUsageCatalog(t *testing.T) {
	for _, d := range GetCatalog().Templates {
		if d.Template != "team_usage" {
			continue
		}
		if d.Name != "Team usage" || d.Scope != "self" || strings.Join(d.Dimensions, ",") != "team,model" || strings.Join(d.Metrics, ",") != "requests,tokens_in,tokens_out,cost_usd" {
			t.Fatalf("unexpected team usage defaults: %+v", d)
		}
		if strings.Join(d.Sections, ",") != "usage" {
			t.Fatalf("team usage must reuse the supported usage analysis: %v", d.Sections)
		}
		for _, scope := range []string{"self", "team", "organization"} {
			d.Scope, d.TeamID = scope, ""
			if scope == "team" {
				d.TeamID = "team-a"
			}
			if err := Validate(d); err != nil {
				t.Fatalf("%s: %v", scope, err)
			}
		}
		redacted := RedactCosts(Result{Definition: d})
		if redacted.Definition.Name != "Team usage" || strings.Contains(strings.Join(redacted.Definition.Metrics, ","), "cost_usd") {
			t.Fatalf("unsafe or unrecognizable redacted template: %+v", redacted.Definition)
		}
		return
	}
	t.Fatal("team_usage missing from catalog")
}

func TestCatalogTemplatesAreUsable(t *testing.T) {
	c := GetCatalog()
	if c.Version != 1 || len(c.Templates) != 11 || len(c.Dimensions) != 20 || len(c.Metrics) < 17 {
		t.Fatalf("incomplete catalog: %+v", c)
	}
	for _, d := range c.Templates {
		if err := Validate(d); err != nil {
			t.Errorf("%s: %v", d.Template, err)
		}
	}
	b, _ := json.Marshal(c)
	var v map[string]any
	json.Unmarshal(b, &v)
	if v["version"] != float64(1) {
		t.Fatal(string(b))
	}
	c.Templates[0].Metrics[0] = "corrupted"
	if GetCatalog().Templates[0].Metrics[0] == "corrupted" {
		t.Fatal("catalog aliases mutable state")
	}
}
