package store

import (
	"context"
	"testing"
	"time"
)

func TestReportScheduleRevocationAndOwnership(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	def := &ReportDefinition{OwnerUserID: owner, Definition: d}
	if err := s.SaveReportDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	v := &ReportSchedule{ReportID: def.ID, OwnerUserID: "other", Frequency: "daily", Timezone: "UTC", At: "10:00", Enabled: true}
	if err := s.SaveReportSchedule(ctx, v); err == nil {
		t.Fatal("foreign schedule accepted")
	}
	v.OwnerUserID = owner
	if err := s.SaveReportSchedule(ctx, v); err != nil {
		t.Fatal(err)
	}
	now := v.NextRunAt.Add(time.Minute)
	if err := s.exec(ctx, `UPDATE app_user SET is_active=0 WHERE id=?`, owner); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		n, err := s.EnqueueDueReports(ctx, now)
		if err != nil || n != 0 {
			t.Fatalf("unauthorized enqueue %d %v", n, err)
		}
	}
	list, err := s.ListReportSchedules(ctx)
	if err != nil || len(list) != 1 || list[0].Enabled {
		t.Fatalf("not disabled %+v %v", list, err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification WHERE user_id=?`, owner).Scan(&count); err != nil || count != 1 {
		t.Fatalf("disable notifications %d %v", count, err)
	}
	if err := s.DeleteReportSchedule(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
}
func TestReportScheduleCalendar(t *testing.T) {
	for _, tc := range []struct{ name, at, after, want string }{
		{"spring gap skips nonexistent clock", "02:30", "2026-03-08T06:00:00Z", "2026-03-09T06:30:00Z"},
		{"fall first occurrence", "01:30", "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z"},
		{"fall no repeated local day", "01:30", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ReportSchedule{Frequency: "daily", Timezone: "America/New_York", At: tc.at}
			after, _ := time.Parse(time.RFC3339, tc.after)
			got, err := NextReportSchedule(s, after)
			if err != nil || got.Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v %v want %s", got, err, tc.want)
			}
		})
	}
	s := ReportSchedule{Frequency: "monthly", Timezone: "UTC", At: "09:05", Monthday: 28}
	after := time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC)
	got, err := NextReportSchedule(s, after)
	if err != nil || got.Day() != 28 || got.Month() != time.February {
		t.Fatalf("monthly %v %v", got, err)
	}
	s.Monthday = 31
	if _, err := NextReportSchedule(s, after); err == nil {
		t.Fatal("unsupported monthday accepted")
	}
	s = ReportSchedule{Frequency: "weekly", Timezone: "UTC", At: "09:05", Weekday: 1}
	got, err = NextReportSchedule(s, after)
	if err != nil || got.Weekday() != time.Monday {
		t.Fatalf("weekly %v %v", got, err)
	}
}
func TestReportScheduleIdempotentCatchUp(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	ctx := context.Background()
	def := &ReportDefinition{OwnerUserID: owner, Definition: d}
	if err := s.SaveReportDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	schedule := &ReportSchedule{ReportID: def.ID, OwnerUserID: owner, Frequency: "daily", Timezone: "UTC", At: "09:00", Enabled: true}
	if err := s.SaveReportSchedule(ctx, schedule); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := s.exec(ctx, `UPDATE report_schedule SET next_run_at=? WHERE id=?`, FormatTime(now.AddDate(-2, 0, 0)), schedule.ID); err != nil {
		t.Fatal(err)
	}
	ch := make(chan int, 5)
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { n, e := s.EnqueueDueReports(ctx, now); ch <- n; errs <- e }()
	}
	total := 0
	for i := 0; i < 5; i++ {
		total += <-ch
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	if total != 1 {
		t.Fatalf("enqueues %d", total)
	}
	runs, err := s.ListReportRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	// Rolling windows include the scheduled day's usage up to the occurrence,
	// not midnight and not the later catch-up worker's wall clock.
	occurrence := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	if runs[0].Definition.End != occurrence.Format(time.RFC3339) || runs[0].Definition.Start != occurrence.AddDate(0, 0, -30).Format(time.RFC3339) || runs[0].Definition.SourcePeriod != "last_30_days" {
		t.Fatalf("scheduled period %+v", runs[0].Definition)
	}
	list, err := s.ListReportSchedules(ctx)
	if err != nil || len(list) != 1 || !list[0].NextRunAt.After(now) {
		t.Fatalf("next %+v %v", list, err)
	}
	if err := s.DeleteReportDefinition(ctx, def.ID); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListReportSchedules(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal("orphan schedule")
	}
	if _, err := s.ReportRunByID(ctx, runs[0].ID); err != nil {
		t.Fatal("run deleted with definition")
	}
}
