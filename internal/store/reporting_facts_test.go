package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestReportingFactAtomicFailure(t *testing.T) {
	for _, table := range []string{"reporting_usage_snapshot", "reporting_coverage"} {
		for _, team := range []string{"", "selected-team"} {
			t.Run(table+team, func(t *testing.T) {
				s := newReportingFactStore(t)
				ctx := context.Background()
				u, _, err := s.UpsertUserFromIdentity(ctx, "auth", "u@example.com", "User", false, false)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.exec(ctx, `CREATE TRIGGER reject_fact BEFORE INSERT ON `+table+` BEGIN SELECT RAISE(ABORT, 'forced snapshot failure'); END`); err != nil {
					t.Fatal(err)
				}
				e := &UsageEvent{UserID: u.ID, TeamIDs: team}
				if err := s.InsertUsageEvent(ctx, e); err == nil {
					t.Fatal("expected snapshot write failure")
				}
				for _, target := range []string{"usage_event", "reporting_usage_snapshot", "reporting_coverage"} {
					var n int
					if err := s.queryRow(ctx, `SELECT COUNT(*) FROM `+target).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatalf("%s retained %d rows after rollback", target, n)
					}
				}
			})
		}
	}
}

func TestReportingFactPurgeFailureRollsBack(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	e := &UsageEvent{}
	if err := s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `CREATE TRIGGER reject_purge BEFORE DELETE ON reporting_usage_snapshot BEGIN SELECT RAISE(ABORT, 'forced purge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeUsageEvents(ctx, e.CreatedAt.Add(time.Hour)); err == nil || n != 0 {
		t.Fatalf("purge=%d err=%v", n, err)
	}
	if _, err := s.UsageEventByID(ctx, e.ID); err != nil {
		t.Fatalf("metering not restored: %v", err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM reporting_coverage WHERE key='retention_before'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed purge advanced coverage")
	}
}

func TestReportingFactGroupsRemainHistorical(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "auth", "u@example.com", "User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceIDPGroupsForUser(ctx, u.ID, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.GroupIDsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(groups)
	e := &UsageEvent{UserID: u.ID, GroupIDs: groups}
	// Identity changes before delayed metering must not affect admission facts.
	if err := s.ReplaceIDPGroupsForUser(ctx, u.ID, []string{"new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceIDPGroupsForUser(ctx, u.ID, nil); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err := s.queryRow(ctx, `SELECT group_ids FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("groups drifted: %s -> %s", expected, actual)
	}
}

func TestReportingFactLegacyMissingRemainsMissing(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	e := &UsageEvent{}
	if err := s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration record, which has metering but no reporting row.
	if err := s.exec(ctx, `DROP TABLE reporting_usage_snapshot`); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `DROP TABLE reporting_coverage`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range reportingFactsMigration.stmt {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("legacy usage was retroactively classified")
	}
}

func TestReportingFactCoverage(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{early.Add(time.Hour), early, early.Add(2 * time.Hour)} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	var start string
	if err := s.queryRow(ctx, `SELECT started_at FROM reporting_coverage WHERE key='usage_snapshots'`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if start != FormatTime(early) {
		t.Fatalf("coverage=%s", start)
	}
}

func TestReportingFactInvalidCostStatus(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	e := &UsageEvent{CostStatus: "invented"}
	if err := s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.queryRow(ctx, `SELECT cost_status FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Fatalf("status=%s", status)
	}
}

func TestReportingFactAdmissionClassification(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	var e UsageEvent
	if err := json.Unmarshal([]byte(`{"model_family":"family-at-admission","model_provider":"provider-at-admission","model_hosting":"local","project":"project","cost_center":"center","cost_status":"known_free"}`), &e); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageEvent(ctx, &e); err != nil {
		t.Fatal(err)
	}
	var family, provider, hosting, project, center, status string
	var known int
	if err := s.queryRow(ctx, `SELECT model_family,provider,hosting,project,cost_center,cost_status,classification_known FROM reporting_usage_snapshot WHERE usage_id=?`, e.ID).Scan(&family, &provider, &hosting, &project, &center, &status, &known); err != nil {
		t.Fatal(err)
	}
	if family != "family-at-admission" || provider != "provider-at-admission" || hosting != "local" || project != "project" || center != "center" || status != "known_free" || known != 1 {
		t.Fatalf("classification=%q %q %q %q %q %q %d", family, provider, hosting, project, center, status, known)
	}
}

func TestReportingFactRetention(t *testing.T) {
	s := newReportingFactStore(t)
	ctx := context.Background()
	before := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, e := range []*UsageEvent{{ID: "old", CreatedAt: before.Add(-time.Hour)}, {ID: "keep", CreatedAt: before}} {
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgeUsageEvents(ctx, before)
	if err != nil || n != 1 {
		t.Fatalf("purge=%d err=%v", n, err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM reporting_usage_snapshot`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("orphan snapshot retained: %d", count)
	}
	var cutoff string
	if err := s.queryRow(ctx, `SELECT started_at FROM reporting_coverage WHERE key='retention_before'`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if cutoff != FormatTime(before) {
		t.Fatalf("cutoff=%s", cutoff)
	}
	if _, err := s.PurgeUsageEvents(ctx, before.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT started_at FROM reporting_coverage WHERE key='retention_before'`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if cutoff != FormatTime(before) {
		t.Fatal("retention watermark moved backwards")
	}
}

// Applying the idempotent DDL also permits isolated development before the
// parent registers the migration; production registration is tested separately.
func newReportingFactStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	for _, stmt := range reportingFactsMigration.stmt {
		if err := s.exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestReportingFactGroupKnowledge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  []string
		service string
		known   int
		want    string
	}{
		{"unknown", nil, "", 0, "[]"},
		{"empty", []string{}, "", 1, "[]"},
		{"groups", []string{"group-at-admission"}, "", 1, `["group-at-admission"]`},
		{"service", nil, "service", 1, "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newReportingFactStore(t)
			ctx := context.Background()
			e := &UsageEvent{TeamIDs: "team", GroupIDs: tc.groups, ServiceTokenID: tc.service}
			if err := s.InsertUsageEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
			var groups, status string
			var known int
			if err := s.queryRow(ctx, `SELECT group_ids, groups_known, cost_status FROM reporting_usage_snapshot WHERE usage_id = ?`, e.ID).Scan(&groups, &known, &status); err != nil {
				t.Fatal(err)
			}
			if groups != tc.want || known != tc.known || status != "unknown" {
				t.Fatalf("got groups=%s known=%d cost=%s", groups, known, status)
			}
		})
	}
}
