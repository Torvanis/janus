package store

import (
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/reporting"
	"strings"
	"testing"
	"time"
)

func TestReportingAnalysisQuotaWindowsRetention(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := Quota{ID: "q", SubjectType: "user", SubjectID: "alice", Metric: MetricRequests, Limit: 10, Window: WindowDaily}
	b, _ := json.Marshal(q)
	if err := s.exec(ctx, `INSERT INTO report_quota_history(id,quota_id,effective_from,policy_json,deleted) VALUES(?,?,?,?,0)`, "version", "q", reportPolicyTime(at), string(b)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at.AddDate(0, 0, i).Add(time.Hour), UserID: "alice", HTTPStatus: 200}, []string{}, "known_free")
	}
	d := reportDefinition()
	d.Template = "quotas"
	d.End = "2026-01-03T00:00:00Z"
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var windows []reporting.Row
	for _, sec := range r.Sections {
		if sec.ID == "quota_windows" {
			windows = sec.Rows
		}
	}
	if len(windows) != 2 {
		t.Fatalf("expected every calendar window, got %d", len(windows))
	}
	for _, row := range windows {
		if row.Values["utilization"] == nil || *row.Values["utilization"] != .1 {
			t.Fatalf("quota ratio %+v", row)
		}
	}
	if err = s.exec(ctx, `INSERT INTO reporting_coverage(key,started_at) VALUES('retention_before',?)`, FormatTime(at.AddDate(0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	r, err = s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "retention_before") {
		t.Fatal("missing retained coverage warning")
	}
	for _, sec := range r.Sections {
		if sec.ID == "quota_windows" && sec.Rows[0].Values["utilization"] != nil {
			t.Fatal("partial retained window has ratio")
		}
	}
}
func TestReportingAnalysisBudgetRetainedCoverage(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := ReportBudget{Name: "Monthly", OwnerUserID: "alice", Scope: "user", SubjectID: "alice", AmountNano: 10000000000, Start: at, End: at.AddDate(0, 1, 0)}
	if err := s.SaveReportBudget(ctx, &b); err != nil {
		t.Fatal(err)
	}
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at.Add(time.Hour), UserID: "alice", HTTPStatus: 200, CostNano: 2000000000}, []string{}, "priced")
	d := reportDefinition()
	d.Template = "quotas"
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(r *reporting.Result) map[string]*float64 {
		for _, sec := range r.Sections {
			if sec.ID == "budgets" {
				return sec.Rows[0].Values
			}
		}
		t.Fatal("missing budget")
		return nil
	}
	v := get(r)
	if v["remaining_usd"] == nil || *v["remaining_usd"] != 8 || *v["utilization"] != .2 || *v["projected_usd"] != 2 {
		t.Fatalf("budget values %+v", v)
	}
	if err = s.exec(ctx, `INSERT INTO reporting_coverage(key,started_at) VALUES('retention_before',?)`, FormatTime(at.AddDate(0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	r, err = s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if get(r)["remaining_usd"] != nil {
		t.Fatal("retention gap represented as complete budget")
	}
}
func TestReportingAnalysisBudgetNanoPrecision(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := ReportBudget{Name: "Small", OwnerUserID: "alice", Scope: "user", SubjectID: "alice", AmountNano: 3, Start: at, End: at.AddDate(0, 1, 0)}
	if err := s.SaveReportBudget(ctx, &b); err != nil {
		t.Fatal(err)
	}
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at.Add(time.Hour), UserID: "alice", HTTPStatus: 200, CostNano: 1}, []string{}, "priced")
	d := reportDefinition()
	d.Template = "quotas"
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range r.Sections {
		if sec.ID == "budgets" {
			if *sec.Rows[0].Values["remaining_usd"] != float64(2)/1e9 {
				t.Fatalf("subtracted floating currency %.20g", *sec.Rows[0].Values["remaining_usd"])
			}
			return
		}
	}
	t.Fatal("no budget")
}
func TestReportingAnalysisComparisonRetention(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	for _, day := range []string{"2025-12-03T00:00:00Z", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z"} {
		at, _ := time.Parse(time.RFC3339, day)
		reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at, UserID: "alice", HTTPStatus: 200}, []string{}, "known_free")
	}
	if err := s.exec(ctx, `INSERT INTO reporting_coverage(key,started_at) VALUES('retention_before',?)`, "2026-01-01T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	d.Compare = true
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range r.Sections {
		if sec.ID == "comparison" {
			if sec.Rows[0].Values["requests_outlier"] != nil {
				t.Fatal("retention gap created a false comparative outlier")
			}
			return
		}
	}
	t.Fatal("missing comparison")
}
func TestReportingAnalysisComparisonPriorPartialCoverage(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	for _, day := range []string{"2025-12-20T00:00:00Z", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z"} {
		at, _ := time.Parse(time.RFC3339, day)
		reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at, UserID: "alice", HTTPStatus: 200, CostNano: 1234567891}, []string{}, "priced")
	}
	if err := s.exec(ctx, `INSERT INTO reporting_coverage(key,started_at) VALUES('retention_before',?)`, "2025-12-15T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	d.Compare = true
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ComparisonReliable == nil || *r.ComparisonReliable {
		t.Fatal("partially retained previous period must explicitly disable comparisons")
	}
	if !strings.Contains(strings.Join(r.ComparisonWarnings, " "), "Previous period") || !strings.Contains(strings.Join(r.ComparisonWarnings, " "), "retention_before") {
		t.Fatalf("missing prior coverage: %v", r.ComparisonWarnings)
	}
	if strings.Contains(strings.Join(r.Warnings, " "), "Requested period precedes") {
		t.Fatal("current period is fully retained")
	}
	if r.PreviousTotals["cost_usd"] == nil || *r.PreviousTotals["cost_usd"] != 1.234567891 {
		t.Fatal("recorded prior spend erased")
	}
	for _, sec := range r.Sections {
		if sec.ID == "comparison" {
			for _, value := range sec.Rows[0].Values {
				if value != nil {
					t.Fatal("incomplete comparison produced a delta/outlier")
				}
			}
			return
		}
	}
	t.Fatal("missing comparison")
}

func TestReportingAnalysisFamilies(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	e := UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: "alice", ModelName: "old-model", Modality: "chat", HTTPStatus: 500, TokensIn: 100, TokensOut: 50, TokensCached: 25, CostNano: 2000000000, ClientApp: "sdk", QuotaViolated: true, SecgwAction: "block", FallbackReason: "provider_error"}
	reportSnapshot(t, s, e, []string{"g"}, "priced")
	for _, family := range []string{"executive", "usage", "adoption", "portfolio", "quotas", "reliability", "efficiency", "integrations", "governance", "data_quality"} {
		t.Run(family, func(t *testing.T) {
			d := reportDefinition()
			d.Template = family
			d.Sections = []string{family}
			r, err := s.QueryReport(ctx, d, reporting.Scope{Admin: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Sections) == 0 {
				t.Fatal("missing derived sections")
			}
			for _, sec := range r.Sections {
				if len(sec.Rows) == 0 || len(sec.Columns) == 0 || len(sec.Notes) == 0 {
					t.Fatalf("placeholder section %+v", sec)
				}
			}
		})
	}
}
func TestReportingAnalysisComparisonAndScenario(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	for _, at := range []time.Time{time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2025, 12, 3, 0, 0, 0, 0, time.UTC)} {
		reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at, UserID: "alice", HTTPStatus: 200, CostNano: 1000000000}, []string{}, "priced")
	}
	d := reportDefinition()
	d.Template = "efficiency"
	d.Sections = []string{"efficiency"}
	d.Compare = true
	d.ScenarioDiscountPercent = 20
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ComparisonReliable == nil || !*r.ComparisonReliable {
		t.Fatal("complete prior coverage should permit comparison")
	}
	for _, sec := range r.Sections {
		if sec.ID == "comparison" {
			if v := sec.Rows[0].Values["requests_relative_change"]; v == nil || *v != 0 {
				t.Fatal("complete equal periods should report zero change")
			}
		}
	}
	if r.PreviousTotals["requests"] == nil || *r.PreviousTotals["requests"] != 1 {
		t.Fatalf("previous totals %+v", r.PreviousTotals)
	}
	found := false
	for _, sec := range r.Sections {
		if sec.ID == "scenario" {
			found = true
			if *sec.Rows[0].Values["hypothetical_cost_usd"] != .8 {
				t.Fatal("wrong scenario")
			}
		}
	}
	if !found {
		t.Fatal("missing hypothetical scenario")
	}
}
