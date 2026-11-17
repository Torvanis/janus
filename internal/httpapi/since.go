package httpapi

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// errSince is the message every admin list that filters by ?since= returns
// for a value it cannot read.
var errSince = errors.New("since must be an RFC 3339 timestamp, a duration like 24h, or a number of days like 7d")

// parseSince reads a ?since= filter: an RFC 3339 timestamp, a Go duration
// ("90m", "24h") or whole days ("7d", "30d"), the last two counted back
// from now. The admin UI's time filters send hours and days.
func parseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		// Bounded so the subtraction cannot overflow; ten years is far
		// beyond any retention window.
		if err != nil || n <= 0 || n > 3660 {
			return time.Time{}, errSince
		}
		return now.UTC().AddDate(0, 0, -n), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.UTC().Add(-d), nil
	}
	return time.Time{}, errSince
}
