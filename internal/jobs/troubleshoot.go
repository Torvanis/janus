package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/troubleshoot"
)

// TroubleshootRetentionInterval is how often captured payloads are checked
// against the troubleshooting session's retention policy. Hourly keeps the
// worst-case overshoot of a time-based bound to an hour; count and size
// bounds are also applied on every pass.
const TroubleshootRetentionInterval = time.Hour

// StartTroubleshootRetention enforces the troubleshooting retention policy
// on a fixed interval and auto-disables a session past its expiry. The first
// pass runs shortly after start so a restart never leaves stale captures
// until the next full interval. Every pass is independent: an error is
// logged and the next tick tries again.
func StartTroubleshootRetention(ctx context.Context, wg *sync.WaitGroup, rec *troubleshoot.Recorder, every time.Duration, logger *slog.Logger) {
	if rec == nil {
		return
	}
	if every <= 0 {
		every = TroubleshootRetentionInterval
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		run := func() {
			res, err := rec.EnforceRetention(ctx)
			if err != nil {
				logger.Error("troubleshooting retention", "error", err.Error())
				return
			}
			if res.Deleted > 0 || res.SessionExpired {
				logger.Info("troubleshooting retention",
					"deleted", res.Deleted, "session_expired", res.SessionExpired,
					"max_age", res.AppliedMaxAge, "max_count", res.AppliedMaxCount, "max_bytes", res.AppliedMaxBytes)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			run()
		}
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
