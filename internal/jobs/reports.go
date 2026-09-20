package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// ReportWorker executes SQL-persisted jobs. The lease exceeds the query deadline,
// so an abandoned job can be recovered without allowing concurrent publication.
// All replicas may run a worker; claim/finalization are fenced in the store.
type ReportWorker struct {
	Store      *store.Store
	LocalOnly  bool
	Logger     *slog.Logger
	Every      time.Duration
	RunTimeout time.Duration
}

func NewReportWorker(s *store.Store, localOnly bool, logger *slog.Logger) *ReportWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReportWorker{Store: s, LocalOnly: localOnly, Logger: logger, Every: 2 * time.Second, RunTimeout: 2 * time.Minute}
}

// RunOnce processes at most one run, with scheduling and expiry independently
// persisted before claiming. A crash after completion cannot duplicate delivery.
func (w *ReportWorker) RunOnce(ctx context.Context) (bool, error) {
	if err := w.Store.EnsureReportQuotaHistory(ctx); err != nil {
		return false, err
	}
	if _, err := w.Store.EnqueueDueReports(ctx, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := w.Store.ExpireReportRuns(ctx, time.Now().UTC()); err != nil {
		return false, err
	}
	timeout := w.RunTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	run, err := w.Store.ClaimReportRun(ctx, timeout+time.Minute)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Cancellation is a DB state, not an in-memory signal tied to one replica.
	// The monitor requests query cancellation; the final CAS also rejects any
	// cancelled run even when the query finishes before the monitor observes it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-queryCtx.Done():
				return
			case <-ticker.C:
				current, e := w.Store.ReportRunByID(queryCtx, run.ID)
				if e == nil && (current.Status != "running" || current.LeaseToken != run.LeaseToken) {
					cancel()
					return
				}
			}
		}
	}()
	scope, queryErr := w.Store.ResolveReportScope(queryCtx, run.OwnerUserID, run.Definition)
	scope.HideCosts = w.LocalOnly
	var failure string
	if queryErr == nil {
		result, e := w.Store.QueryReport(queryCtx, run.Definition, scope)
		queryErr = e
		cancel()
		<-done
		if e == nil {
			finishCtx, finishCancel := context.WithTimeout(ctx, 10*time.Second)
			defer finishCancel()
			err = w.Store.FinishReportRun(finishCtx, run.ID, run.LeaseToken, result, "")
			if errors.Is(err, store.ErrReportConflict) {
				return true, nil
			}
			return true, err
		}
	} else {
		cancel()
		<-done
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	} // shutdown: leave lease for recovery
	failure = "query_failed"
	if errors.Is(queryErr, context.DeadlineExceeded) {
		failure = "query_timeout"
	}
	w.Logger.Error("report generation failed", "run_id", run.ID, "error", queryErr, "category", failure)
	finishCtx, finishCancel := context.WithTimeout(ctx, 10*time.Second)
	defer finishCancel()
	err = w.Store.FinishReportRun(finishCtx, run.ID, run.LeaseToken, nil, failure)
	if errors.Is(err, store.ErrReportConflict) {
		return true, nil
	}
	return true, err
}

func (w *ReportWorker) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		every := w.Every
		if every <= 0 {
			every = 2 * time.Second
		}
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			worked, err := w.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				w.Logger.Error("report worker", "error", err)
			}
			// Drain a queue without delaying each run; failure waits rather than spins.
			if worked && err == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
