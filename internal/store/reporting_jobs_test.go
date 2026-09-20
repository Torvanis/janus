package store

import (
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/reporting"
	"math"
	"strings"
	"testing"
	"time"
)

func reportJobFixture(t *testing.T) (*Store, string, reporting.Definition) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "report-owner", "report@example.com", "Report Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	return s, u.ID, reporting.GetCatalog().Templates[0]
}
func TestReportRunLifecycle(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	r, err := s.EnqueueReport(ctx, owner, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if r.Definition.Period != "custom" || r.Definition.Start == "" {
		t.Fatal("relative dates not frozen")
	}
	ch := make(chan *ReportRun, 8)
	for i := 0; i < 8; i++ {
		go func() { v, _ := s.ClaimReportRun(ctx, time.Minute); ch <- v }()
	}
	var claimed *ReportRun
	n := 0
	for i := 0; i < 8; i++ {
		if v := <-ch; v != nil {
			n++
			claimed = v
		}
	}
	if n != 1 {
		t.Fatalf("claims=%d", n)
	}
	b, _ := json.Marshal(claimed)
	if strings.Contains(string(b), claimed.LeaseToken) {
		t.Fatal("lease leaked")
	}
	if err := s.FinishReportRun(ctx, r.ID, "wrong", &reporting.Result{Version: 1}, ""); err == nil {
		t.Fatal("wrong lease accepted")
	}
	result := &reporting.Result{Version: 1, Warnings: []string{"original"}}
	if err := s.FinishReportRun(ctx, r.ID, claimed.LeaseToken, result, ""); err != nil {
		t.Fatal(err)
	}
	result.Warnings[0] = "mutated"
	if err := s.FinishReportRun(ctx, r.ID, claimed.LeaseToken, result, ""); err == nil {
		t.Fatal("repeat finish accepted")
	}
	got, err := s.ReportRunByID(ctx, r.ID)
	if err != nil || got.Result.Warnings[0] != "original" {
		t.Fatalf("immutable result: %+v %v", got, err)
	}
	list, err := s.ListReportRuns(ctx)
	if err != nil || len(list) != 1 || list[0].Result != nil {
		t.Fatalf("metadata: %+v %v", list, err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification WHERE user_id=?`, owner).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notifications %d %v", count, err)
	}
	if err := s.ExpireReportRuns(ctx, time.Now().Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReportRunByID(ctx, r.ID)
	if err != nil || got.Status != "expired" || got.Result != nil {
		t.Fatal("result did not expire")
	}
}
func TestReportRunLeaseCancellationAndRetry(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	r, err := s.EnqueueReport(ctx, owner, "", d)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimReportRun(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE report_run SET lease_until=? WHERE id=?`, FormatTime(time.Now().Add(-time.Second)), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReportRun(ctx, r.ID, first.LeaseToken, &reporting.Result{}, ""); err == nil {
		t.Fatal("expired lease accepted")
	}
	second, err := s.ClaimReportRun(ctx, time.Minute)
	if err != nil || second.Attempts != 2 || second.LeaseToken == first.LeaseToken {
		t.Fatalf("retry %+v %v", second, err)
	}
	if err := s.FinishReportRun(ctx, r.ID, first.LeaseToken, &reporting.Result{}, ""); err == nil {
		t.Fatal("stale worker accepted")
	}
	if err := s.CancelReportRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReportRun(ctx, r.ID, second.LeaseToken, &reporting.Result{}, ""); err == nil {
		t.Fatal("cancel undone")
	}
	if _, err := s.ClaimReportRun(ctx, time.Minute); err != ErrNotFound {
		t.Fatalf("cancel claimed: %v", err)
	}
}
func TestReportRunAuthorizationRetryBoundAndFiniteResult(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	r, err := s.EnqueueReport(ctx, owner, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		v, err := s.ClaimReportRun(ctx, time.Minute)
		if err != nil || v.Attempts != attempt {
			t.Fatalf("attempt %+v %v", v, err)
		}
		if err := s.exec(ctx, `UPDATE report_run SET lease_until=? WHERE id=?`, FormatTime(time.Now().Add(-time.Second)), r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimReportRun(ctx, time.Minute); err != ErrNotFound {
		t.Fatalf("fourth attempt: %v", err)
	}
	r, err = s.ReportRunByID(ctx, r.ID)
	if err != nil || r.Status != "failed" || r.Error == "" {
		t.Fatalf("not loud failure %+v %v", r, err)
	}
	var count int
	s.queryRow(ctx, `SELECT COUNT(*) FROM notification WHERE user_id=?`, owner).Scan(&count)
	if count != 1 {
		t.Fatalf("retry notifications %d", count)
	}
	r, err = s.EnqueueReport(ctx, owner, "", d)
	if err != nil {
		t.Fatal(err)
	}
	enqueuedID := r.ID
	r, err = s.ClaimReportRun(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != enqueuedID {
		t.Fatalf("claimed %s, expected %s", r.ID, enqueuedID)
	}
	nan := math.NaN()
	if err := s.FinishReportRun(ctx, r.ID, r.LeaseToken, &reporting.Result{Totals: map[string]*float64{"requests": &nan}}, ""); err == nil {
		t.Fatal("NaN accepted")
	}
	if err := s.exec(ctx, `UPDATE app_user SET is_active=0 WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReportRun(ctx, r.ID, r.LeaseToken, &reporting.Result{Version: 1}, ""); err != nil {
		t.Fatal(err)
	}
	r, err = s.ReportRunByID(ctx, r.ID)
	if err != nil || r.Status != "failed" || r.Result != nil {
		t.Fatalf("unauthorized publication %+v %v", r, err)
	}
	if _, err := s.EnqueueReport(ctx, owner, "", d); err == nil {
		t.Fatal("inactive enqueue")
	}
}
func TestReportRunFrozenDefinitionAndPersistence(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	v := &ReportDefinition{OwnerUserID: owner, Definition: d}
	if err := s.SaveReportDefinition(ctx, v); err != nil {
		t.Fatal(err)
	}
	r, err := s.EnqueueReport(ctx, owner, v.ID, v.Definition)
	if err != nil {
		t.Fatal(err)
	}
	v.Definition.Name = "new version"
	if err := s.SaveReportDefinition(ctx, v); err != nil {
		t.Fatal(err)
	}
	var file string
	var seq int
	var dbname string
	if err := s.queryRow(ctx, `PRAGMA database_list`).Scan(&seq, &dbname, &file); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ReportRunByID(ctx, r.ID)
	if err != nil || got.Definition.Name != d.Name || got.Definition.Start != r.Definition.Start {
		t.Fatalf("snapshot changed %+v %v", got, err)
	}
}
func TestReportDefinitionRevision(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	v := &ReportDefinition{OwnerUserID: owner, Definition: d}
	if err := s.SaveReportDefinition(ctx, v); err != nil {
		t.Fatal(err)
	}
	stale := *v
	v.Definition.Name = "updated"
	if err := s.SaveReportDefinition(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReportDefinition(ctx, &stale); err == nil {
		t.Fatal("stale revision accepted")
	}
	got, err := s.ReportDefinitionByID(ctx, v.ID)
	if err != nil || got.Revision != 2 || got.Definition.Name != "updated" {
		t.Fatalf("read: %+v %v", got, err)
	}
}
