package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRouteEvalJSONEndDate(t *testing.T) {
	for in, want := range map[string]time.Time{
		`""`:                     {},
		`"0001-01-01T00:00:00Z"`: {},
		`"2026-11-30"`:           time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC),
		`"2026-11-30T12:00:00Z"`: time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC),
	} {
		var e RouteEval
		if err := json.Unmarshal([]byte(`{"enabled":true,"until":`+in+`}`), &e); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !e.Until.Equal(want) || !e.Enabled {
			t.Fatalf("%s: got %v", in, e.Until)
		}
	}
	var bad RouteEval
	if err := json.Unmarshal([]byte(`{"until":"next week"}`), &bad); err == nil {
		t.Fatal("a non-date must be refused")
	}
	out, _ := json.Marshal(RouteEval{Enabled: true})
	if !strings.Contains(string(out), `"until":""`) {
		t.Fatalf("an unset end date must be written as empty: %s", out)
	}
}
