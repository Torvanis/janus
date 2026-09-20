package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
)

// InstanceHeartbeatEvery is how often a process refreshes its liveness row.
// InstanceStaleAfter is three of these, so one slow tick never drops a live
// replica from the count.
const InstanceHeartbeatEvery = 15 * time.Second

// NewInstanceID builds the id this process heartbeats under: the hostname
// (the pod name on Kubernetes, unique per replica) plus a short random
// suffix so two processes on one host during a local run never share a
// row.
func NewInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "janus"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}

// StartInstanceHeartbeat keeps this process's gateway_instance row fresh
// and feeds the live count to the rate limiter. The first heartbeat runs
// synchronously so the limiter starts with the real divisor rather than 1
// for its first fifteen seconds. On shutdown the row is deleted so the
// survivors stop dividing by a dead replica at once.
func StartInstanceHeartbeat(ctx context.Context, wg *sync.WaitGroup, s *store.Store, rl *quota.RateLimiter, id string, logger *slog.Logger, opts ...HeartbeatOption) {
	var o heartbeatOptions
	for _, fn := range opts {
		fn(&o)
	}
	overLimit := false
	beat := func() {
		n, err := s.HeartbeatInstance(ctx, id, time.Now().UTC())
		if err != nil {
			// Keep the last known divisor. Falling back to 1 here would
			// make every replica admit the full rule the moment the
			// database blinked, which is the wrong direction to fail.
			logger.WarnContext(ctx, "instance heartbeat", "error", err.Error())
			return
		}
		if n != rl.Replicas() {
			logger.InfoContext(ctx, "gateway replica count changed", "replicas", n)
			rl.SetReplicas(n)
		}
		// Node limit is advisory by ruling: warn on the transition, never
		// refuse traffic. The Admin → System card shows the same number.
		if o.licensedNodes != nil {
			limit := o.licensedNodes()
			over := limit > 0 && n > limit
			if over && !overLimit {
				logger.WarnContext(ctx, "more gateway replicas than the license covers", "replicas", n, "licensed_nodes", limit,
					"hint", "add nodes at https://janusedge.com/portal or scale down; nothing is blocked")
			} else if !over && overLimit {
				logger.InfoContext(ctx, "gateway replica count back within the license", "replicas", n, "licensed_nodes", limit)
			}
			overLimit = over
		}
	}
	beat()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(InstanceHeartbeatEvery)
		defer ticker.Stop()
		pruneTick := 0
		for {
			select {
			case <-ctx.Done():
				// ctx is done; use a fresh short one for the goodbye.
				bye, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := s.RemoveInstance(bye, id); err != nil {
					logger.Warn("remove instance row", "error", err.Error())
				}
				cancel()
				return
			case <-ticker.C:
				beat()
				pruneTick++
				if pruneTick%240 == 0 { // roughly hourly
					if _, err := s.PruneInstances(ctx, time.Now().UTC()); err != nil {
						logger.WarnContext(ctx, "prune instance rows", "error", err.Error())
					}
				}
			}
		}
	}()
}

type heartbeatOptions struct {
	licensedNodes func() int
}

// HeartbeatOption tunes StartInstanceHeartbeat.
type HeartbeatOption func(*heartbeatOptions)

// WithLicensedNodes supplies the current node limit (0 = unlimited) so the
// heartbeat can warn when live replicas exceed it.
func WithLicensedNodes(fn func() int) HeartbeatOption {
	return func(o *heartbeatOptions) { o.licensedNodes = fn }
}
