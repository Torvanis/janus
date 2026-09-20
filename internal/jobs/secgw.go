package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// SecgwRetentionInterval is how often Security Gateway violation rows are
// pruned against their per-kind retention.
const SecgwRetentionInterval = time.Hour

// SecgwRetention is the per-kind bound on violation rows. Jailbreak
// captures are worth keeping for months (they are the tuning corpus); the
// rest are audit trail and expire sooner. "Keep forever" is deliberately not
// expressible: every kind has a bound.
var SecgwRetention = map[store.SecgwCheckKind]store.SecgwRetention{
	store.SecgwCheckPromptInjection: {MaxAgeHours: 90 * 24, MaxCount: 50_000},
	store.SecgwCheckSecrets:         {MaxAgeHours: 30 * 24, MaxCount: 100_000},
	store.SecgwCheckPII:             {MaxAgeHours: 30 * 24, MaxCount: 100_000},
	store.SecgwCheckTerms:           {MaxAgeHours: 30 * 24, MaxCount: 100_000},
	store.SecgwCheckShape:           {MaxAgeHours: 30 * 24, MaxCount: 100_000},
}

// StartSecgwRetention prunes violation rows on a fixed interval. The first
// pass runs shortly after start; every pass is independent.
func StartSecgwRetention(ctx context.Context, wg *sync.WaitGroup, s *store.Store, every time.Duration, logger *slog.Logger) {
	if s == nil {
		return
	}
	if every <= 0 {
		every = SecgwRetentionInterval
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		run := func() {
			var total int64
			for kind, r := range SecgwRetention {
				n, err := s.PruneSecgwViolations(ctx, string(kind), r)
				if err != nil {
					logger.Error("security gateway retention", "kind", kind, "error", err.Error())
					continue
				}
				total += n
			}
			if total > 0 {
				logger.Info("security gateway retention", "deleted", total)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(45 * time.Second):
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
