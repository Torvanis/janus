package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// Performance mode (JANUS_PERFORMANCE_MODE, Business edition).
//
// Normal mode writes each usage event in its own transaction and waits for the
// WAL flush before reporting success. Performance mode trades a small, bounded
// metering-loss window for throughput:
//
//   - usage events are queued and written by one writer per replica as
//     multi-row INSERTs every flush interval (or when a batch fills), and
//   - those transactions commit without waiting for the WAL flush
//     (synchronous_commit=off on that transaction only).
//
// Loss window: a PostgreSQL crash can lose about the last 0.5 s of committed
// usage rows; a replica killed without a graceful shutdown (OOM, SIGKILL) can
// lose the events still queued, at most one flush interval. A graceful
// shutdown drains the queue first. Quota counters and thresholds are applied
// after each flush, so they trail the request by at most one interval.
//
// Gating: the mode is decided once at startup. It needs a valid Business or
// Enterprise license; without one the request is logged and ignored. A
// license that later lapses does not switch it off (expiry never disrupts
// running work), and a license installed later takes effect at the next start.

const (
	usageFlushInterval = 100 * time.Millisecond
	usageBatchMax      = 500
	usageQueueDepth    = 20000
)

// PerformanceMode is the startup decision, reported on Admin → System.
type PerformanceMode struct {
	Requested bool   `json:"requested"`
	Active    bool   `json:"active"`
	Reason    string `json:"reason,omitempty"`
}

// DecidePerformanceMode applies the edition gate to the operator's request.
func DecidePerformanceMode(requested bool, st license.State) PerformanceMode {
	m := PerformanceMode{Requested: requested}
	if !requested {
		return m
	}
	switch {
	case st.Edition != license.EditionBusiness && st.Edition != license.EditionEnterprise:
		m.Reason = "Performance mode requires a Business license; running in normal mode."
	case st.Restricted():
		m.Reason = "Performance mode requires a valid license; the installed key is expired or invalid, so the gateway runs in normal mode."
	default:
		m.Active = true
	}
	return m
}

// usageWriteTimeout bounds each database step for one event (insert, then the
// follow-up work). Derived when the step runs, not when the event is queued.
const usageWriteTimeout = 15 * time.Second

type usageItem struct {
	ctx   context.Context // uncancelled request context (values only)
	event *store.UsageEvent
	after func(context.Context)
	done  func()
}

// usageBatcher is the single per-replica writer behind performance mode.
type usageBatcher struct {
	store *store.Store
	log   *slog.Logger
	queue chan usageItem
	stop  chan struct{}
	wg    sync.WaitGroup

	// mu orders enqueue against close: once stopped is set nothing new
	// enters the queue, so the final drain in run cannot miss an event.
	mu      sync.RWMutex
	stopped bool
}

func newUsageBatcher(st *store.Store, log *slog.Logger) *usageBatcher {
	b := &usageBatcher{store: st, log: log, queue: make(chan usageItem, usageQueueDepth), stop: make(chan struct{})}
	b.wg.Add(1)
	go b.run()
	return b
}

// enqueue hands an event to the writer. When the queue is full, or the writer
// has already stopped (a stream outliving shutdown), the event is written
// directly instead, so backpressure and shutdown cost throughput, never data.
func (b *usageBatcher) enqueue(it usageItem) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.stopped {
		select {
		case b.queue <- it:
			return
		default:
		}
	}
	go b.writeOne(it)
}

func (b *usageBatcher) run() {
	defer b.wg.Done()
	tick := time.NewTicker(usageFlushInterval)
	defer tick.Stop()
	batch := make([]usageItem, 0, usageBatchMax)
	for {
		select {
		case it := <-b.queue:
			batch = append(batch, it)
			if len(batch) >= usageBatchMax {
				b.flush(batch)
				batch = batch[:0]
			}
		case <-tick.C:
			if len(batch) > 0 {
				b.flush(batch)
				batch = batch[:0]
			}
		case <-b.stop:
			for {
				select {
				case it := <-b.queue:
					batch = append(batch, it)
				default:
					if len(batch) > 0 {
						b.flush(batch)
					}
					return
				}
			}
		}
	}
}

func (b *usageBatcher) flush(batch []usageItem) {
	events := make([]*store.UsageEvent, len(batch))
	for i, it := range batch {
		events[i] = it.event
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := b.store.InsertUsageEvents(ctx, events, true)
	cancel()
	if err != nil {
		// The batch is all-or-nothing; retry row by row so one bad event
		// cannot take the rest of the batch with it.
		b.log.Warn("batched usage write failed; retrying individually", "events", len(batch), "error", err.Error())
		for _, it := range batch {
			b.writeOne(it)
		}
		return
	}
	for _, it := range batch {
		go b.finish(it)
	}
}

func (b *usageBatcher) writeOne(it usageItem) {
	ctx, cancel := context.WithTimeout(it.ctx, usageWriteTimeout)
	err := b.store.InsertUsageEvent(ctx, it.event)
	cancel()
	if err != nil {
		b.log.ErrorContext(it.ctx, "write usage event", "error", err.Error(), "request_id", it.event.RequestID)
	}
	b.finish(it)
}

func (b *usageBatcher) finish(it usageItem) {
	defer it.done()
	if it.after != nil {
		ctx, cancel := context.WithTimeout(it.ctx, usageWriteTimeout)
		defer cancel()
		it.after(ctx)
	}
}

// close flushes everything queued and stops the writer.
func (b *usageBatcher) close() {
	b.mu.Lock()
	if !b.stopped {
		b.stopped = true
		close(b.stop)
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// StartPerformanceMode records the startup decision and, when active, starts
// the batched usage writer. Call once, before serving.
func (s *Server) StartPerformanceMode(m PerformanceMode) {
	s.Performance = m
	if m.Active {
		s.batcher = newUsageBatcher(s.Store, s.Logger)
	}
}

// StopPerformanceMode flushes and stops the batched writer. Call after Drain.
func (s *Server) StopPerformanceMode() {
	if s.batcher != nil {
		s.batcher.close()
	}
}

// writeUsage persists one usage event and then runs after (quota consumption,
// security violations, captures). Normal mode writes inline on a tracked
// goroutine; performance mode queues it for the batched writer. Either way the
// work is tracked by s.pending, so Drain waits for it.
func (s *Server) writeUsage(parent context.Context, event *store.UsageEvent, logMsg string, after func(context.Context)) {
	base := context.WithoutCancel(parent)
	s.pending.Add(1)
	if s.batcher != nil {
		s.batcher.enqueue(usageItem{ctx: base, event: event, after: after, done: s.pending.Done})
		return
	}
	go func() {
		defer s.pending.Done()
		ctx, cancel := context.WithTimeout(base, usageWriteTimeout)
		defer cancel()
		if err := s.Store.InsertUsageEvent(ctx, event); err != nil {
			s.Logger.ErrorContext(ctx, logMsg, "error", err.Error(), "request_id", event.RequestID)
		}
		if after != nil {
			after(ctx)
		}
	}()
}
