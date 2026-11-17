package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// QuotedEstimateBackfillDelay is how long after start the one backfill pass
// waits, so it never competes with startup work.
const QuotedEstimateBackfillDelay = 90 * time.Second

// StartQuotedEstimateBackfill runs one pass that gives provider-quoted calls
// recorded before Janus kept its own price beside the quote that price, from
// the price history (store.BackfillQuotedEstimates). One pass per start:
// calls written by an older replica during a rolling update are caught by
// the next start. Nothing billed changes. Off in local-only mode (no prices).
func StartQuotedEstimateBackfill(ctx context.Context, wg *sync.WaitGroup, s *store.Store, localOnly bool, delay time.Duration, logger *slog.Logger) {
	if s == nil || localOnly {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		started := time.Now()
		res, err := s.BackfillQuotedEstimates(ctx, 500)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("pricing accuracy: backfill of Janus prices for quoted calls", "error", err.Error(), "filled", res.Filled)
			}
			return
		}
		if res.Scanned > 0 {
			logger.Info("pricing accuracy: Janus prices filled in for older quoted calls from the price history",
				"looked_at", res.Scanned, "filled", res.Filled, "left_without", res.Scanned-res.Filled,
				"took_ms", time.Since(started).Milliseconds())
		}
	}()
}
