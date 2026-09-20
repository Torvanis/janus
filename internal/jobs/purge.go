// Package jobs holds the gateway's scheduled background work.
package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// Purger enforces the data-retention policy.
type Purger struct {
	store       *store.Store
	quota       *quota.Engine
	metrics     *telemetry.Metrics
	logger      *slog.Logger
	usageDays   int
	auditDays   int
	runAtUTC    string
	mu          sync.RWMutex
	lastRun     time.Time
	lastDeleted int64
}

// NewPurger builds the retention job.
func NewPurger(s *store.Store, q *quota.Engine, m *telemetry.Metrics, logger *slog.Logger, usageDays, auditDays int, runAtUTC string) *Purger {
	return &Purger{store: s, quota: q, metrics: m, logger: logger, usageDays: usageDays, auditDays: auditDays, runAtUTC: runAtUTC}
}

// LastRun reports the most recent purge for test inspection.
// It is not exposed by the admin status API.
func (p *Purger) LastRun() (time.Time, int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastRun, p.lastDeleted
}

// RunOnce deletes everything past its retention window and reconciles quota
// ledgers, which can drift once their source events are removed.
func (p *Purger) RunOnce(ctx context.Context) {
	now := time.Now().UTC()
	var deleted int64

	if n, err := p.store.PurgeUsageEvents(ctx, now.AddDate(0, 0, -p.usageDays)); err != nil {
		p.logger.ErrorContext(ctx, "purge usage events", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("usage_event").Add(float64(n))
		deleted += n
	}
	if n, err := p.store.PurgeAudit(ctx, now.AddDate(0, 0, -p.auditDays)); err != nil {
		p.logger.ErrorContext(ctx, "purge audit log", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("audit_log").Add(float64(n))
		deleted += n
	}
	if n, err := p.store.PurgeDocsFeedback(ctx, now.AddDate(0, 0, -365)); err != nil {
		p.logger.ErrorContext(ctx, "purge docs feedback", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("docs_feedback").Add(float64(n))
		deleted += n
	}
	if n, err := p.store.PurgeExpiredWebSessions(ctx, now); err != nil {
		p.logger.ErrorContext(ctx, "purge expired sessions", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("web_session").Add(float64(n))
		deleted += n
	}
	// 45 days comfortably outlives the widest quota window (monthly /
	// rolling-30d), so no row that could still deduplicate an alert is lost.
	if n, err := p.store.PurgeQuotaAlertState(ctx, now.AddDate(0, 0, -45)); err != nil {
		p.logger.ErrorContext(ctx, "purge quota alert state", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("quota_alert_state").Add(float64(n))
		deleted += n
	}
	if n, err := p.store.PurgeExpiredOIDCStates(ctx, now); err != nil {
		p.logger.ErrorContext(ctx, "purge expired oidc states", "error", err.Error())
	} else if n > 0 {
		p.metrics.PurgeDeletedRows.WithLabelValues("oidc_state").Add(float64(n))
		deleted += n
	}
	if err := p.quota.Reconcile(ctx); err != nil {
		p.logger.ErrorContext(ctx, "reconcile quota ledgers after purge", "error", err.Error())
	}

	p.mu.Lock()
	p.lastRun, p.lastDeleted = now, deleted
	p.mu.Unlock()
	p.logger.InfoContext(ctx, "retention purge complete", "rows_deleted", deleted)
}

// Start schedules the purge for the configured UTC time each day.
func (p *Purger) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			wait := durationUntil(p.runAtUTC, time.Now().UTC())
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				p.RunOnce(ctx)
			}
		}
	}()
}

// durationUntil returns how long to wait for the next occurrence of "HH:MM" UTC.
func durationUntil(hhmm string, now time.Time) time.Duration {
	target, err := time.Parse("15:04", hhmm)
	if err != nil {
		target, _ = time.Parse("15:04", "02:00")
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), target.Hour(), target.Minute(), 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// StartQuotaCheckpoint periodically rebuilds quota ledgers from the event log so
// counters stay correct across replicas and survive a cache loss.
func StartQuotaCheckpoint(ctx context.Context, wg *sync.WaitGroup, q *quota.Engine, every time.Duration, logger *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := q.Reconcile(ctx); err != nil {
					logger.ErrorContext(ctx, "quota checkpoint failed", "error", err.Error())
				}
			}
		}
	}()
}
