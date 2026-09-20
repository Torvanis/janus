package store

import (
	"context"
	"testing"
	"time"
)

func TestReportRunFrozenMonthRetainsCalendarComparison(t *testing.T) {
	s, owner, d := reportJobFixture(t)
	d.Period = "previous_month"
	d.Compare = true
	run, err := s.enqueueReportAt(context.Background(), owner, "", d, time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), "", "")
	if err != nil {
		t.Fatal(err)
	}
	start := ParseTime(run.Definition.Start)
	end := ParseTime(run.Definition.End)
	previousStart, previousEnd := reportPrevious(run.Definition, start, end)
	if !previousStart.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || !previousEnd.Equal(start) {
		t.Fatalf("frozen monthly comparison incorrectly became elapsed duration: %s to %s", previousStart, previousEnd)
	}
}
