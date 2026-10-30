package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
)

// Notifications are kept 90 days; violations and Usage insights results
// never outlive the usage log (JANUS_USAGE_RETENTION_DAYS); the content
// purge hook runs with the job.
func TestPurgeNotificationsAndCapsAtUsageRetention(t *testing.T) {
	db, metrics, logger := newPurgeFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateNotification(ctx, &store.Notification{Title: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNotification(ctx, &store.Notification{Title: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification SET created_at = ? WHERE title = 'old'`, store.FormatTime(now.Add(-91*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	// Usage retention 14 days; a 20-day-old violation (inside its own
	// 30/90-day bound) and a 20-day-old insights result (inside the
	// default 180 days) must go, fresh ones stay.
	for _, age := range []time.Duration{20 * 24 * time.Hour, time.Hour} {
		if err := db.InsertSecgwViolations(ctx, []*store.SecgwViolation{{UserID: "u", Kind: string(store.SecgwCheckSecrets), CreatedAt: now.Add(-age)}}); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertClassificationResult(ctx, &store.ClassificationResult{UserID: "u", CreatedAt: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPurger(db, quota.NewEngine(db), metrics, logger, 14, 730, "02:00")
	p.RunOnce(ctx)

	count := func(q string) int {
		var n int
		if err := db.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM notification`); n != 1 {
		t.Errorf("notifications = %d, want only the recent one", n)
	}
	if n := count(`SELECT COUNT(*) FROM secgw_violation`); n != 1 {
		t.Errorf("violations = %d, want the one older than the usage retention gone", n)
	}
	if n := count(`SELECT COUNT(*) FROM classification_result`); n != 1 {
		t.Errorf("insights results = %d, want the one older than the usage retention gone", n)
	}
}
