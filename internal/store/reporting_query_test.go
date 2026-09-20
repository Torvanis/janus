package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/torvanis/janus/internal/reporting"
	"strings"
	"testing"
	"time"
)

func newReportingTestStore(t *testing.T) *Store { t.Helper(); return newTestStore(t) }
func reportDefinition() reporting.Definition {
	return reporting.Definition{Version: 1, Template: "usage", Scope: "organization", Period: "custom", Start: "2026-01-01T00:00:00Z", End: "2026-02-01T00:00:00Z", Timezone: "UTC", GroupMode: "historical", Dimensions: []string{"model"}, Metrics: []string{"requests", "cost_usd", "errors"}}
}
func TestReportingQueryResolveAuthorization(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "report-user", "report@example.test", "Report", false, false)
	if err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	if _, err = s.ResolveReportScope(ctx, u.ID, d); err == nil {
		t.Fatal("ordinary user got organization scope")
	}
	d.Scope = "self"
	sc, err := s.ResolveReportScope(ctx, u.ID, d)
	if err != nil || sc.UserID != u.ID || sc.Admin {
		t.Fatalf("self: %+v %v", sc, err)
	}
	if err = s.exec(ctx, `UPDATE app_user SET is_active=0 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveReportScope(ctx, u.ID, d); err == nil {
		t.Fatal("disabled user allowed")
	}
}
func reportSnapshot(t *testing.T, s *Store, e UsageEvent, groups []string, costStatus string) {
	t.Helper()
	ctx := context.Background()
	if err := s.insertUsageEvent(ctx, &e); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(groups)
	if err := s.exec(ctx, `INSERT INTO reporting_usage_snapshot(usage_id,recorded_at,group_ids,groups_known,cost_status,classification_known) VALUES(?,?,?,?,?,?) ON CONFLICT(usage_id) DO UPDATE SET group_ids=excluded.group_ids,groups_known=excluded.groups_known,cost_status=excluded.cost_status`, e.ID, FormatTime(e.CreatedAt), string(b), 1, costStatus, 0); err != nil {
		t.Fatal(err)
	}
}
func TestReportingQueryHistoricalGroupsNoCap(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	groups := []string{}
	for i := 0; i < 65; i++ {
		groups = append(groups, fmt.Sprintf("group-%02d", i))
	}
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), UserID: "alice", TeamIDs: "old-team,other-team", ModelName: "deleted", HTTPStatus: 500, CostNano: 1500000001, TokensIn: 100, TokensCached: 25, TokensCacheWrite5m: 100, LatencyMs: 10}, groups, "priced")
	d := reportDefinition()
	d.Dimensions = []string{"group"}
	d.Metrics = []string{"requests", "cost_usd", "errors", "cache_hit_rate"}
	r, err := s.QueryReport(ctx, d, reporting.Scope{TeamIDs: []string{"old-team"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 65 || r.SourceRows != 1 || *r.Totals["requests"] != 1 {
		t.Fatalf("overlap or truncation: rows=%d source=%d", len(r.Rows), r.SourceRows)
	}
	if *r.Totals["cost_usd"] != 1.500000001 || *r.Totals["cache_hit_rate"] != 0.125 || len(r.Warnings) == 0 {
		t.Fatalf("metrics %+v", r.Totals)
	}
	d.Filters = map[string][]string{"group": {"group-64"}}
	r, err = s.QueryReport(ctx, d, reporting.Scope{UserID: "bob"})
	if err != nil || r.SourceRows != 0 {
		t.Fatalf("filter broadened: %+v %v", r, err)
	}
}
func TestReportingQueryLiveTeamAuthorization(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "leader-report", "leader@example.test", "Leader", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "Report Team", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	d.Scope = "team"
	d.TeamID = team.ID
	scope, err := s.ResolveReportScope(ctx, u.ID, d)
	if err != nil || len(scope.TeamIDs) != 1 {
		t.Fatalf("leader denied %+v %v", scope, err)
	}
	if err = s.exec(ctx, `UPDATE team_member SET role='member' WHERE team_id=? AND user_id=?`, team.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveReportScope(ctx, u.ID, d); err == nil {
		t.Fatal("demoted leader retained authority")
	}
	if err = s.exec(ctx, `UPDATE app_user SET role='admin' WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveReportScope(ctx, u.ID, d); err != nil {
		t.Fatal(err)
	}
	if err = s.exec(ctx, `UPDATE team SET archived_at=? WHERE id=?`, FormatTime(time.Now()), team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveReportScope(ctx, u.ID, d); err == nil {
		t.Fatal("archived team authorized")
	}
}
func TestReportingQueryHistoricalVsCurrentCohort(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "cohort-report", "cohort@example.test", "Cohort", false, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, "Today's group")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetGroupMembers(ctx, g.ID, []string{u.ID}); err != nil {
		t.Fatal(err)
	}
	e := UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: u.ID, HTTPStatus: 200}
	if err = s.insertUsageEvent(ctx, &e); err != nil {
		t.Fatal(err)
	}
	if err = s.exec(ctx, `DELETE FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	d.Dimensions = []string{"group"}
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows[0].Dimensions["group"] != reportUnknown {
		t.Fatal("inferred historical membership")
	}
	d.GroupMode = "current"
	r, err = s.QueryReport(ctx, d, reporting.Scope{UserID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows[0].Dimensions["group"] != g.ID {
		t.Fatal("missing explicit current cohort")
	}
}
func TestReportingQueryLocalCalendarAndExactMetrics(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	d := reportDefinition()
	d.Start = "2026-03-08T05:00:00Z"
	d.End = "2026-03-09T04:00:00Z"
	d.Timezone = "America/New_York"
	d.Dimensions = []string{"day", "week", "month"}
	d.Metrics = []string{"requests", "errors", "success_rate", "latency_p50_ms", "latency_p95_ms", "ttfb_p95_ms", "active_users"}
	at, _ := time.Parse(time.RFC3339, d.Start)
	for i, status := range []int{200, 500, 0, 200} {
		e := UsageEvent{ID: NewID(), CreatedAt: at.Add(time.Duration(i) * time.Hour), UserID: "alice", HTTPStatus: status, LatencyMs: (i + 1) * 10, TTFBMs: i + 1}
		reportSnapshot(t, s, e, []string{}, "known_free")
	}
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at.Add(23 * time.Hour), UserID: "alice", HTTPStatus: 500}, []string{}, "known_free")
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.SourceRows != 4 || len(r.Rows) != 1 {
		t.Fatal("half-open DST interval incorrect")
	}
	if r.Rows[0].Dimensions["day"] != "2026-03-08" || r.Rows[0].Dimensions["week"] != "2026-03-02" || r.Rows[0].Dimensions["month"] != "2026-03" {
		t.Fatal("local calendar buckets incorrect")
	}
	for k, want := range map[string]float64{"requests": 4, "errors": 1, "success_rate": float64(2) / 3, "latency_p50_ms": 20, "latency_p95_ms": 40, "ttfb_p95_ms": 4, "active_users": 1} {
		if r.Totals[k] == nil || *r.Totals[k] != want {
			t.Fatalf("%s metric mismatch", k)
		}
	}
	for _, period := range []string{"last_7_days", "last_30_days", "previous_month"} {
		preset := d
		preset.Period = period
		preset.Start = ""
		preset.End = ""
		start, end, err := reporting.ResolvePeriod(preset, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		previous, previousEnd := reportPrevious(preset, start, end)
		if previous.In(start.Location()).Hour() != start.Hour() || !previousEnd.Equal(start) {
			t.Fatalf("DST comparison %s %v %v", period, previous, previousEnd)
		}
	}
}
func TestReportingQueryLocalOnly(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: "alice", HTTPStatus: 200, CostNano: 17000000000}, []string{}, "priced")
	d := reportDefinition()
	d.Template = "efficiency"
	d.ScenarioDiscountPercent = 20
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice", HideCosts: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Totals["cost_usd"]; ok {
		t.Fatal("local-only leaked cost")
	}
	if r.Definition.ScenarioDiscountPercent != 0 {
		t.Fatal("local-only leaked scenario")
	}
}
func TestReportingQueryAccountingAndNames(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "named-report", "named@example.test", "Readable User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: u.ID, HTTPStatus: 200, AccountingMode: "byte_count_fallback", SecgwAction: "stream_cut", LatencyMs: 10, TTFBMs: 1}, []string{}, "known_free")
	d := reportDefinition()
	d.Dimensions = []string{"user"}
	d.Metrics = []string{"estimated_requests", "security_blocks", "success_rate", "latency_p95_ms"}
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if *r.Totals["estimated_requests"] != 1 || *r.Totals["security_blocks"] != 1 {
		t.Fatalf("accounting/security %+v", r.Totals)
	}
	if r.Rows[0].Dimensions["user"] != u.ID || r.Rows[0].Dimensions["user_label"] != "Readable User" {
		t.Fatalf("stable name %+v", r.Rows[0])
	}
}
func TestReportingQueryLegacyRecordedCost(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	e := UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: "alice", HTTPStatus: 200, CostNano: 1234567891}
	if err := s.insertUsageEvent(ctx, &e); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `DELETE FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	r, err := s.QueryReport(ctx, reportDefinition(), reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals["cost_usd"] == nil || *r.Totals["cost_usd"] != 1.234567891 || len(r.Warnings) == 0 {
		t.Fatalf("legacy recorded spend lost: %+v", r.Totals)
	}
	found := false
	for _, sec := range r.Sections {
		if sec.ID == "cost_coverage" {
			found = true
			if *sec.Rows[0].Values["unknown_cost_requests"] != 1 {
				t.Fatal("wrong unknown-cost coverage")
			}
		}
	}
	if !found {
		t.Fatal("missing cost confidence coverage")
	}
}
func TestReportingQueryPreviousConfidencePreservesRecordedCost(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	prior := UsageEvent{ID: NewID(), CreatedAt: time.Date(2025, 12, 20, 0, 0, 0, 0, time.UTC), UserID: "alice", HTTPStatus: 200, CostNano: 1234567891}
	if err := s.insertUsageEvent(ctx, &prior); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `DELETE FROM reporting_usage_snapshot WHERE usage_id=?`, prior.ID); err != nil {
		t.Fatal(err)
	}
	d := reportDefinition()
	d.Compare = true
	r, err := s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.PreviousTotals["cost_usd"] == nil || *r.PreviousTotals["cost_usd"] != 1.234567891 {
		t.Fatal("legacy prior recorded spend lost")
	}
	if r.ComparisonReliable == nil || !*r.ComparisonReliable {
		t.Fatal("missing price confidence must not erase complete recorded coverage")
	}
	for _, text := range []string{"pricing confidence", "classification", "membership"} {
		found := false
		for _, warning := range r.ComparisonWarnings {
			if strings.HasPrefix(warning, "Previous period: ") && strings.Contains(warning, text) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing previous %s warning: %v", text, r.ComparisonWarnings)
		}
	}
	d.Compare = false
	r, err = s.QueryReport(ctx, d, reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "comparison_reliable") || strings.Contains(string(data), "comparison_warnings") {
		t.Fatal("comparison metadata present without comparison")
	}
}

func TestReportingQueryProvisionalCutoff(t *testing.T) {
	for _, futureOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(futureOnly), func(t *testing.T) {
			s := newReportingTestStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			d := reportDefinition()
			start, end := now.Add(-24*time.Hour), now.Add(24*time.Hour)
			if futureOnly {
				start, end = now.Add(24*time.Hour), now.Add(72*time.Hour)
			}
			d.Start, d.End, d.Compare = start.Format(time.RFC3339), end.Format(time.RFC3339), true
			for _, at := range []time.Time{now.Add(-time.Hour), now.Add(time.Hour), now.Add(48 * time.Hour)} {
				reportSnapshot(t, s, UsageEvent{ID: NewID(), CreatedAt: at, UserID: "alice", HTTPStatus: 200, CostNano: 1000000000}, nil, "priced")
			}
			r, err := s.QueryReport(context.Background(), d, reporting.Scope{UserID: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			wantCurrent, wantPrevious := 1., 0.
			if futureOnly {
				wantCurrent, wantPrevious = 0, 1
			}
			if *r.Totals["requests"] != wantCurrent || *r.PreviousTotals["requests"] != wantPrevious {
				t.Fatalf("future events leaked beyond cutoff: current=%v previous=%v", *r.Totals["requests"], *r.PreviousTotals["requests"])
			}
			if !r.Start.Equal(start) || !r.End.Equal(end) {
				t.Fatal("requested interval mutated")
			}
			if !r.DataCutoff.Before(end) || !strings.Contains(strings.Join(r.Warnings, " "), "provisional") {
				t.Fatal("missing provisional cutoff warning")
			}
			if r.ComparisonReliable == nil || *r.ComparisonReliable {
				t.Fatal("provisional comparison advertised as complete")
			}
			for _, sec := range r.Sections {
				if sec.ID == "comparison" {
					for _, v := range sec.Rows[0].Values {
						if v != nil {
							t.Fatal("provisional comparison produced delta/outlier")
						}
					}
				}
			}
		})
	}
}

func TestReportingQueryScopeIsolation(t *testing.T) {
	s := newReportingTestStore(t)
	ctx := context.Background()
	for _, u := range []string{"alice", "bob", ""} {
		if err := s.insertUsageEvent(ctx, &UsageEvent{ID: NewID(), CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), UserID: u, ModelName: "deleted-model", HTTPStatus: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.QueryReport(ctx, reportDefinition(), reporting.Scope{}); err == nil {
		t.Fatal("empty scope allowed")
	}
	r, err := s.QueryReport(ctx, reportDefinition(), reporting.Scope{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if r.SourceRows != 1 || *r.Totals["requests"] != 1 || len(r.Rows) != 1 {
		t.Fatalf("isolated result: %+v", r)
	}
	if r.Totals["cost_usd"] == nil || *r.Totals["cost_usd"] != 0 || len(r.Warnings) == 0 {
		t.Fatal("recorded zero cost must remain visible with an incomplete-pricing warning")
	}
}
