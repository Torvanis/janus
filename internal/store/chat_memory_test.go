package store

import (
	"context"
	"strings"
	"testing"
)

// Chat reads a reply's metering row by request id on every reply: that
// lookup must use an index, not scan usage_event.
func TestUsageEventByRequestIDUsesAnIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.QueryContext(context.Background(),
		`EXPLAIN QUERY PLAN SELECT `+usageColumns+` FROM usage_event WHERE request_id = ? AND client_app <> 'janus-security-gateway' ORDER BY created_at DESC LIMIT 1`, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "idx_usage_request") {
		t.Fatalf("plan does not use idx_usage_request:\n%s", plan.String())
	}
}
