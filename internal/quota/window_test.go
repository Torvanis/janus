package quota

import (
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse fixture time %q: %v", value, err)
	}
	return parsed.UTC()
}

func TestWindowBoundsCalendarWindows(t *testing.T) {
	now := mustTime(t, "2026-08-14T13:45:12Z") // a Friday

	tests := []struct {
		window    string
		wantStart string
		wantReset string
	}{
		{store.WindowDaily, "2026-08-14T00:00:00Z", "2026-08-15T00:00:00Z"},
		{store.WindowWeekly, "2026-08-10T00:00:00Z", "2026-08-17T00:00:00Z"}, // ISO week starts Monday
		{store.WindowMonthly, "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.window, func(t *testing.T) {
			start, reset, err := WindowBounds(tc.window, now)
			if err != nil {
				t.Fatalf("WindowBounds(%q): %v", tc.window, err)
			}
			if !start.Equal(mustTime(t, tc.wantStart)) {
				t.Errorf("start = %s, want %s", start.Format(time.RFC3339), tc.wantStart)
			}
			if !reset.Equal(mustTime(t, tc.wantReset)) {
				t.Errorf("reset = %s, want %s", reset.Format(time.RFC3339), tc.wantReset)
			}
		})
	}
}

func TestWeeklyWindowOnSunday(t *testing.T) {
	// Sunday must belong to the week that began the preceding Monday.
	sunday := mustTime(t, "2026-08-16T09:00:00Z")
	start, _, err := WindowBounds(store.WindowWeekly, sunday)
	if err != nil {
		t.Fatalf("WindowBounds: %v", err)
	}
	if want := mustTime(t, "2026-08-10T00:00:00Z"); !start.Equal(want) {
		t.Fatalf("Sunday week start = %s, want %s", start.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestRollingWindows(t *testing.T) {
	now := mustTime(t, "2026-08-14T13:45:12Z")
	for _, window := range []string{store.WindowRolling24, store.WindowRolling7, store.WindowRolling30} {
		if !IsRolling(window) {
			t.Fatalf("%q should be reported as a rolling window", window)
		}
		start, _, err := WindowBounds(window, now)
		if err != nil {
			t.Fatalf("WindowBounds(%q): %v", window, err)
		}
		if !start.Before(now) {
			t.Fatalf("%q start %s should precede now", window, start)
		}
	}
	if IsRolling(store.WindowDaily) {
		t.Fatal("daily is a calendar window, not a rolling one")
	}
}

func TestWindowBoundsRejectsUnknown(t *testing.T) {
	if _, _, err := WindowBounds("fortnightly", time.Now()); err == nil {
		t.Fatal("expected an error for an unsupported window")
	}
	if ValidWindow("fortnightly") {
		t.Fatal("ValidWindow should reject an unsupported window")
	}
	if !ValidMetric(store.MetricCostUSD) {
		t.Fatal("cost_usd should be a valid metric")
	}
	if ValidMetric("vibes") {
		t.Fatal("unknown metrics must be rejected")
	}
}

func TestDeltaFor(t *testing.T) {
	d := Delta{TokensIn: 10, TokensOut: 20, CostNano: 30, Requests: 1}
	cases := map[string]int64{
		store.MetricTokensIn:  10,
		store.MetricTokensOut: 20,
		store.MetricCostUSD:   30,
		store.MetricRequests:  1,
		"unknown":             0,
	}
	for metric, want := range cases {
		if got := d.For(metric); got != want {
			t.Errorf("Delta.For(%q) = %d, want %d", metric, got, want)
		}
	}
}
