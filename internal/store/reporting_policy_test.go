package store

import (
	"context"
	"testing"
	"time"
)

func reportingPolicyStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	for _, stmt := range reportingPolicyMigration.stmt {
		if err := s.exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestReportBudgetPersistenceAndValidation(t *testing.T) {
	s := reportingPolicyStore(t)
	ctx := context.Background()
	b := ReportBudget{Name: "Monthly", OwnerUserID: "owner", Scope: "organization", Start: time.Now().UTC(), End: time.Now().UTC().Add(time.Hour)}
	if err := s.SaveReportBudget(ctx, &b); err != nil {
		t.Fatal(err)
	}
	created := b.CreatedAt
	b.AmountNano = 42
	if err := s.SaveReportBudget(ctx, &b); err != nil {
		t.Fatal(err)
	}
	b.AmountNano = 0
	if err := s.SaveReportBudget(ctx, &b); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListReportBudgets(ctx)
	if err != nil || len(got) != 1 || got[0].AmountNano != 0 || !got[0].CreatedAt.Equal(created) {
		t.Fatalf("budgets %+v: %v", got, err)
	}
	for _, mutate := range []func(*ReportBudget){func(b *ReportBudget) { b.AmountNano = -1 }, func(b *ReportBudget) { b.Name = " " }, func(b *ReportBudget) { b.Scope = "" }, func(b *ReportBudget) { b.Scope = "enforcing" }, func(b *ReportBudget) { b.Scope = "user"; b.SubjectID = "" }, func(b *ReportBudget) { b.Scope = "team"; b.SubjectID = "" }, func(b *ReportBudget) { b.End = b.Start }, func(b *ReportBudget) { b.Start = time.Time{} }} {
		invalid := b
		mutate(&invalid)
		if err := s.SaveReportBudget(ctx, &invalid); err == nil {
			t.Fatalf("accepted invalid budget %+v", invalid)
		}
	}
	if quotas, err := s.ListQuotas(ctx); err != nil || len(quotas) != 0 {
		t.Fatalf("budgets affected quotas: %+v %v", quotas, err)
	}
	if err := s.DeleteReportBudget(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListReportBudgets(ctx); err != nil || len(got) != 0 {
		t.Fatalf("delete: %+v %v", got, err)
	}
}

func TestReportingPolicyBaselineAndAtomicRollback(t *testing.T) {
	s := reportingPolicyStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-24 * time.Hour)
	if err := s.exec(ctx, `INSERT INTO quota (`+quotaColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, "legacy", "user", "u", "", "requests", int64(5), "daily", "block", "", FormatTime(old)); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if err := s.EnsureReportQuotaHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureReportQuotaHistory(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListReportQuotaHistory(ctx, old, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].Baseline || history[0].EffectiveFrom.Before(before) {
		t.Fatalf("bad baseline %+v", history)
	}
	historic, err := s.ListReportQuotaHistory(ctx, old, before)
	if err != nil || len(historic) != 0 {
		t.Fatalf("claimed past history %+v %v", historic, err)
	}
	if err := s.exec(ctx, `CREATE TRIGGER fail_report_history BEFORE INSERT ON report_quota_history BEGIN SELECT RAISE(ABORT, 'history unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLedger(ctx, "legacy", old, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateQuota(ctx, "legacy", 9, "daily", "block", nil); err == nil {
		t.Fatal("update should fail")
	}
	if err := s.DeleteQuota(ctx, "legacy"); err == nil {
		t.Fatal("delete should fail")
	}
	if err := s.CreateQuota(ctx, &Quota{SubjectType: "user", SubjectID: "u", Metric: "requests", Limit: 1, Window: "daily", BreachBehavior: "block"}); err == nil {
		t.Fatal("create should fail")
	}
	quotas, err := s.ListQuotas(ctx)
	if err != nil || len(quotas) != 1 || quotas[0].Limit != 5 {
		t.Fatalf("partial mutation %+v %v", quotas, err)
	}
	if n, err := s.LedgerValue(ctx, "legacy", old); err != nil || n != 3 {
		t.Fatalf("ledger lost: %d %v", n, err)
	}
	history, err = s.ListReportQuotaHistory(ctx, old, time.Now().UTC().Add(time.Second))
	if err != nil || len(history) != 1 || !history[0].EffectiveTo.IsZero() {
		t.Fatalf("partial history %+v %v", history, err)
	}
}

func TestReportingPolicyMutationSeedsBeforeChange(t *testing.T) {
	s := reportingPolicyStore(t)
	ctx := context.Background()
	if err := s.exec(ctx, `INSERT INTO quota (`+quotaColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, "legacy", "user", "u", "", "requests", int64(5), "daily", "block", "", FormatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Second)
	if err := s.UpdateQuota(ctx, "legacy", 9, "daily", "block", nil); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListReportQuotaHistory(ctx, start, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !history[0].Baseline || history[0].Policy.Limit != 5 || history[1].Policy.Limit != 9 {
		t.Fatalf("lost old policy %+v", history)
	}
	// An exact boundary excludes the preceding policy, using half-open intervals.
	at := history[1].EffectiveFrom
	history, err = s.ListReportQuotaHistory(ctx, at, at.Add(time.Second))
	if err != nil || len(history) != 1 || history[0].Baseline {
		t.Fatalf("boundary %+v %v", history, err)
	}
	if err := s.UpdateQuota(ctx, "missing", 1, "daily", "block", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteQuota(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListReportQuotaHistory(ctx, at, at); err == nil {
		t.Fatal("accepted empty range")
	}
}

func TestReportingPolicyLifecycle(t *testing.T) {
	s := reportingPolicyStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Second)
	q := &Quota{SubjectType: "user", SubjectID: "u", Metric: "requests", Limit: 10, Window: "daily", BreachBehavior: "block"}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.UpdateQuota(ctx, q.ID, 20, "daily", "block", []int{95, 80}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteQuota(ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	versions, err := s.ListReportQuotaHistory(ctx, start, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 4 {
		t.Fatalf("want 4 versions, got %+v", versions)
	}
	for i, v := range versions {
		if v.QuotaID != q.ID || v.Baseline {
			t.Fatalf("unexpected version %+v", v)
		}
		if i < 3 && (!v.EffectiveTo.Equal(versions[i+1].EffectiveFrom) || !v.EffectiveTo.After(v.EffectiveFrom)) {
			t.Fatalf("invalid closure %+v", versions)
		}
	}
	if versions[0].Policy.Limit != 10 || versions[1].Policy.Limit != 20 || !versions[3].Deleted || !versions[3].EffectiveTo.IsZero() {
		t.Fatalf("wrong policies %+v", versions)
	}
	if qs, err := s.ListQuotas(ctx); err != nil || len(qs) != 0 {
		t.Fatalf("quotas %v: %v", qs, err)
	}
}
