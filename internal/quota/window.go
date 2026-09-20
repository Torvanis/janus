// Package quota implements usage limits: window arithmetic, the counter ledger,
// and the enforcement decision made ahead of every proxied request.
package quota

import (
	"fmt"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// WindowBounds returns the start and reset instants for a quota window
// evaluated at now.
//
// Calendar windows (daily, weekly, monthly) snap to UTC boundaries so every
// replica agrees without coordination. Rolling windows slide continuously and
// therefore "reset" one window-width from now.
func WindowBounds(window string, now time.Time) (start, reset time.Time, err error) {
	now = now.UTC()
	switch window {
	case store.WindowDaily:
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1), nil
	case store.WindowWeekly:
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7 // ISO weeks start on Monday
		}
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(weekday - 1))
		return start, start.AddDate(0, 0, 7), nil
	case store.WindowMonthly:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), nil
	case store.WindowRolling24:
		return now.Add(-24 * time.Hour), now.Add(24 * time.Hour), nil
	case store.WindowRolling7:
		return now.AddDate(0, 0, -7), now.AddDate(0, 0, 7), nil
	case store.WindowRolling30:
		return now.AddDate(0, 0, -30), now.AddDate(0, 0, 30), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported quota window %q", window)
	}
}

// IsRolling reports whether a window slides continuously. Rolling windows cannot
// use a fixed ledger row and are always evaluated from the event log.
func IsRolling(window string) bool {
	switch window {
	case store.WindowRolling24, store.WindowRolling7, store.WindowRolling30:
		return true
	default:
		return false
	}
}

// RollingWindowDays returns how many days of usage_event history a rolling
// window needs to be evaluated correctly, and 0 for calendar windows (which
// are served from the durable ledger, not the event log). Callers use it to
// refuse quota rules whose window outlives the configured usage retention:
// once the purge job deletes events inside the window, the quota silently
// enforces only ~retention days of consumption.
func RollingWindowDays(window string) int {
	switch window {
	case store.WindowRolling24:
		return 1
	case store.WindowRolling7:
		return 7
	case store.WindowRolling30:
		return 30
	default:
		return 0
	}
}

// WindowLabel renders a window for the UI.
func WindowLabel(window string) string {
	switch window {
	case store.WindowDaily:
		return "per day"
	case store.WindowWeekly:
		return "per week"
	case store.WindowMonthly:
		return "per month"
	case store.WindowRolling24:
		return "rolling 24 hours"
	case store.WindowRolling7:
		return "rolling 7 days"
	case store.WindowRolling30:
		return "rolling 30 days"
	default:
		return window
	}
}

// MetricLabel renders a metric for the UI.
func MetricLabel(metric string) string {
	switch metric {
	case store.MetricTokensIn:
		return "input tokens"
	case store.MetricTokensOut:
		return "output tokens"
	case store.MetricCostUSD:
		return "spend"
	case store.MetricRequests:
		return "requests"
	default:
		return metric
	}
}

// ValidMetric reports whether a metric name is supported.
func ValidMetric(metric string) bool {
	switch metric {
	case store.MetricTokensIn, store.MetricTokensOut, store.MetricCostUSD, store.MetricRequests:
		return true
	}
	return false
}

// ValidWindow reports whether a window name is supported.
func ValidWindow(window string) bool {
	_, _, err := WindowBounds(window, time.Now())
	return err == nil
}
