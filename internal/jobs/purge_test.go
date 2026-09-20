package jobs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

func newPurgeFixture(t *testing.T) (*store.Store, *telemetry.Metrics, *slog.Logger) {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, telemetry.New("test", "test"), slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestPurgeRunOnceRespectsRetentionCutoffs(t *testing.T) {
	db, metrics, logger := newPurgeFixture(t)
	ctx := context.Background()

	user, _, err := db.UpsertUserFromIdentity(ctx, "sub-purge", "purge@example.com", "Purge", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	insert := func(age time.Duration) {
		t.Helper()
		if err := db.InsertUsageEvent(ctx, &store.UsageEvent{
			UserID: user.ID, ModelName: "m", EndpointPath: "/v1/chat/completions",
			HTTPMethod: "POST", HTTPStatus: 200, CreatedAt: now.Add(-age),
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}
	insert(400 * 24 * time.Hour) // beyond the 365-day window
	insert(370 * 24 * time.Hour) // beyond the 365-day window
	insert(24 * time.Hour)       // well inside it

	// A fresh audit entry must survive the 730-day audit window.
	if err := db.AppendAudit(ctx, &store.AuditEntry{
		ActorLabel: "test", Action: "create", ResourceType: "upstream", ResourceID: "up-1",
	}); err != nil {
		t.Fatalf("append audit: %v", err)
	}

	p := NewPurger(db, quota.NewEngine(db), metrics, logger, 365, 730, "02:00")
	p.RunOnce(ctx)

	events, _, err := db.ListRequests(ctx, store.RequestFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d usage events remain, want 1 (only the event inside the window)", len(events))
	}
	if got := now.Sub(events[0].CreatedAt); got > 48*time.Hour {
		t.Errorf("the surviving event is %v old; the recent event should have been kept", got)
	}

	audits, total, err := db.ListAudit(ctx, store.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if total != 1 || len(audits) != 1 {
		t.Fatalf("audit entries = %d (total %d), want the fresh entry retained", len(audits), total)
	}

	if got := testutil.ToFloat64(metrics.PurgeDeletedRows.WithLabelValues("usage_event")); got != 2 {
		t.Errorf("janus_purge_deleted_rows_total{table=usage_event} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.PurgeDeletedRows.WithLabelValues("audit_log")); got != 0 {
		t.Errorf("janus_purge_deleted_rows_total{table=audit_log} = %v, want 0", got)
	}

	lastRun, deleted := p.LastRun()
	if lastRun.IsZero() {
		t.Error("LastRun timestamp was not recorded")
	}
	if deleted != 2 {
		t.Errorf("LastRun deleted count = %d, want 2", deleted)
	}
}

func TestPurgeRemovesExpiredAuditPastWindow(t *testing.T) {
	db, metrics, logger := newPurgeFixture(t)
	ctx := context.Background()

	if err := db.AppendAudit(ctx, &store.AuditEntry{
		ActorLabel: "test", Action: "delete", ResourceType: "token", ResourceID: "tok-1",
	}); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	// AppendAudit always stamps "now", so shrink the window below zero to put
	// the cutoff in the future and prove the delete path (cutoff math itself is
	// covered above with realistic windows).
	p := NewPurger(db, quota.NewEngine(db), metrics, logger, 365, -1, "02:00")
	p.RunOnce(ctx)

	_, total, err := db.ListAudit(ctx, store.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if total != 0 {
		t.Fatalf("%d audit entries remain, want 0 once past the retention window", total)
	}
	if got := testutil.ToFloat64(metrics.PurgeDeletedRows.WithLabelValues("audit_log")); got != 1 {
		t.Errorf("janus_purge_deleted_rows_total{table=audit_log} = %v, want 1", got)
	}
}

// TestPurgeRemovesAgedQuotaAlertState covers the documented retention leg:
// threshold-alert dedup rows must be swept by the nightly purge once they are
// older than any window that could still deduplicate (45 days), while rows a
// current window may still consult are retained.
func TestPurgeRemovesAgedQuotaAlertState(t *testing.T) {
	db, metrics, logger := newPurgeFixture(t)
	ctx := context.Background()

	user, _, err := db.UpsertUserFromIdentity(ctx, "sub-alert-purge", "alert-purge@example.com", "AlertPurge", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	q := &store.Quota{
		SubjectType: "user", SubjectID: user.ID,
		Metric: store.MetricRequests, Limit: 10,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}
	if err := db.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	now := time.Now().UTC()
	seed := func(windowStart time.Time, threshold int, notifiedAt time.Time) {
		t.Helper()
		fresh, err := db.MarkThresholdNotified(ctx, q.ID, windowStart, threshold)
		if err != nil || !fresh {
			t.Fatalf("seed alert state: fresh=%v err=%v", fresh, err)
		}
		if _, err := db.DB().ExecContext(ctx,
			"UPDATE quota_alert_state SET notified_at = ? WHERE quota_id = ? AND threshold = ?",
			store.FormatTime(notifiedAt), q.ID, threshold); err != nil {
			t.Fatalf("age alert state: %v", err)
		}
	}
	seed(now.AddDate(0, 0, -90), 80, now.AddDate(0, 0, -90)) // long past any window
	seed(now, 95, now)                                       // current window — must survive

	p := NewPurger(db, quota.NewEngine(db), metrics, logger, 365, 730, "02:00")
	p.RunOnce(ctx)

	var remaining int
	if err := db.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM quota_alert_state WHERE quota_id = ?", q.ID).Scan(&remaining); err != nil {
		t.Fatalf("count alert state: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("%d quota_alert_state rows remain, want 1 (only the current-window row)", remaining)
	}
	if got := testutil.ToFloat64(metrics.PurgeDeletedRows.WithLabelValues("quota_alert_state")); got != 1 {
		t.Errorf("janus_purge_deleted_rows_total{table=quota_alert_state} = %v, want 1", got)
	}
}

func TestDurationUntilBoundaries(t *testing.T) {
	base := time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		hhmm string
		now  time.Time
		want time.Duration
	}{
		{"later today", "02:00", base.Add(1 * time.Hour), 1 * time.Hour},
		{"exactly at the target rolls to tomorrow", "02:00", base.Add(2 * time.Hour), 24 * time.Hour},
		{"already past rolls to tomorrow", "02:00", base.Add(3 * time.Hour), 23 * time.Hour},
		{"midnight target from one second in", "00:00", base.Add(1 * time.Second), 24*time.Hour - time.Second},
		{"invalid spec falls back to 02:00", "not-a-time", base.Add(1 * time.Hour), 1 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := durationUntil(tc.hhmm, tc.now); got != tc.want {
				t.Errorf("durationUntil(%q, %v) = %v, want %v", tc.hhmm, tc.now, got, tc.want)
			}
		})
	}
}
