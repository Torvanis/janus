package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"2026-11-01T00:00:00Z", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"1h", now.Add(-time.Hour)},
		{"24h", now.Add(-24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"7d", now.AddDate(0, 0, -7)},
		{"30d", now.AddDate(0, 0, -30)},
		{" 7d ", now.AddDate(0, 0, -7)},
	} {
		got, err := parseSince(c.in, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "7", "d", "7days", "-7d", "0d", "1.5d", "99999d", "-1h", "yesterday", "2026-11-01"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}

// The violations list's time filter takes the values the admin UI sends
// (1h, 24h, 7d, 30d): days used to be refused with a 400.
func TestAdminSecgwViolationsSinceDays(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()
	now := time.Now().UTC()
	ages := map[string]time.Duration{
		"v-2h":  2 * time.Hour,
		"v-3d":  3 * 24 * time.Hour,
		"v-20d": 20 * 24 * time.Hour,
		"v-40d": 40 * 24 * time.Hour,
	}
	var vs []*store.SecgwViolation
	for id, age := range ages {
		vs = append(vs, &store.SecgwViolation{ID: id, RequestID: "req-" + id, Kind: "pii", RuleID: "email", Severity: "low", Direction: "ingress", Action: "observed", CreatedAt: now.Add(-age)})
	}
	if err := h.store.InsertSecgwViolations(ctx, vs); err != nil {
		t.Fatal(err)
	}
	list := func(since string) []string {
		t.Helper()
		rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations?since="+since, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("since=%s: %d %s", since, rec.Code, rec.Body.String())
		}
		var out struct {
			Violations []store.SecgwViolation `json:"violations"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, v := range out.Violations {
			ids = append(ids, v.ID)
		}
		sort.Strings(ids)
		return ids
	}
	for since, want := range map[string]string{
		"1h":  "",
		"24h": "v-2h",
		"7d":  "v-2h,v-3d",
		"30d": "v-20d,v-2h,v-3d",
		now.Add(-21 * 24 * time.Hour).Format(time.RFC3339): "v-20d,v-2h,v-3d",
	} {
		if got := strings.Join(list(since), ","); got != want {
			t.Errorf("since=%s: %s, want %s", since, got, want)
		}
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations?since=lastweek", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a number of days like 7d") || !strings.Contains(rec.Body.String(), `"since"`) {
		t.Fatalf("invalid since: %d %s", rec.Code, rec.Body.String())
	}
}
