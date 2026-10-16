package store

// request-log classification filters on a large
// PostgreSQL table. Runs only with JANUS_ROLLUP_TEST_DATABASE_URL and
// JANUS_REVIEW_SCALE set (it seeds a few hundred thousand rows).

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestClassifiedFilterScalesOnPostgres(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("JANUS_REVIEW_SCALE"))
	if os.Getenv("JANUS_ROLLUP_TEST_DATABASE_URL") == "" || n == 0 {
		t.Skip("JANUS_ROLLUP_TEST_DATABASE_URL / JANUS_REVIEW_SCALE not set")
	}
	s := newRollupStore(t)
	ctx := context.Background()
	if err := s.exec(ctx, `INSERT INTO usage_event (id, created_at, request_id, user_id, model_name, http_status)
		SELECT 'ev'||g, to_char(now() at time zone 'utc' - (g || ' seconds')::interval, 'YYYY-MM-DD"T"HH24:MI:SS.US000"Z"'), 'rq'||g, 'u1', 'gpt', 200
		FROM generate_series(1, `+strconv.Itoa(n)+`) g`); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `INSERT INTO classification_result (id, created_at, request_id, category, status)
		SELECT 'cr'||g, to_char(now() at time zone 'utc' - (g || ' seconds')::interval, 'YYYY-MM-DD"T"HH24:MI:SS.US000"Z"'), 'rq'||g, CASE WHEN g % 3 = 0 THEN 'coding' ELSE 'legal' END, 'ok'
		FROM generate_series(1, `+strconv.Itoa(n)+`, 2) g`); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"", "*", "-", "coding"} {
		start := time.Now()
		_, total, err := s.ListRequests(ctx, RequestFilter{Classified: f, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("classified=%q total=%d took %v", f, total, time.Since(start))
	}
}
