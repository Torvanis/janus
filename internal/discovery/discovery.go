// Package discovery polls configured upstreams for the models they expose and
// records them for admin curation.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/ratecards"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// Result summarises one discovery run.
type Result struct {
	UpstreamID string `json:"upstream_id"`
	Upstream   string `json:"upstream"`
	Found      int    `json:"models_found"`
	New        int    `json:"models_new"`
	Stale      int    `json:"models_stale"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Service runs discovery on a schedule and on demand.
//
// The schedule has two layers: interval is the process-wide default from
// JANUS_DISCOVERY_INTERVAL_MINUTES, and an administrator may override it at
// runtime through the system_setting table (store.SettingDiscoveryInterval).
// The loop re-reads the effective interval before every wait, so a change
// takes effect without a restart, and Reschedule wakes the loop so a shorter
// interval applies immediately rather than after the current (longer) wait.
type Service struct {
	store     *store.Store
	cipher    *crypto.Cipher
	client    *http.Client
	metrics   *telemetry.Metrics
	alerts    *alerting.Dispatcher
	logger    *slog.Logger
	interval  time.Duration
	failures  sync.Map // upstreamID -> int
	lastRunAt time.Time
	mu        sync.RWMutex
	wake      chan struct{}
}

// New builds a discovery service.
func New(s *store.Store, cipher *crypto.Cipher, client *http.Client, metrics *telemetry.Metrics, alerts *alerting.Dispatcher, logger *slog.Logger, interval time.Duration) *Service {
	return &Service{store: s, cipher: cipher, client: client, metrics: metrics, alerts: alerts, logger: logger, interval: interval, wake: make(chan struct{}, 1)}
}

// LastRun reports when discovery last completed.
func (s *Service) LastRun() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastRunAt
}

// DefaultInterval is the environment-configured polling interval, before any
// runtime override.
func (s *Service) DefaultInterval() time.Duration { return s.interval }

// EffectiveInterval is the interval the scheduler will actually honour: the
// stored runtime override when one is set, otherwise the environment default.
// A store read failure falls back to the default so a database blip can never
// stall discovery; it is logged because the operator's setting is being
// ignored while it lasts.
func (s *Service) EffectiveInterval(ctx context.Context) time.Duration {
	if s.store == nil {
		return s.interval
	}
	o, err := s.store.DiscoveryIntervalOverride(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "read discovery interval override; using environment default", "error", err.Error())
		return s.interval
	}
	if o.Minutes > 0 {
		return time.Duration(o.Minutes) * time.Minute
	}
	return s.interval
}

// Reschedule wakes the scheduling loop so it re-reads the effective interval
// now. Call it after changing the override; without it a change from 60 to 2
// minutes would only take effect after the in-flight 60-minute wait. It never
// blocks: a wake already pending is enough.
func (s *Service) Reschedule() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run polls every enabled upstream once.
func (s *Service) Run(ctx context.Context) []Result {
	upstreams, err := s.store.ListUpstreams(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "list upstreams for discovery", "error", err.Error())
		return nil
	}
	results := []Result{}
	for _, up := range upstreams {
		if !up.Enabled {
			continue
		}
		results = append(results, s.RunFor(ctx, up))
	}
	s.mu.Lock()
	s.lastRunAt = time.Now().UTC()
	s.mu.Unlock()
	return results
}

// RunFor polls a single upstream and records the outcome.
func (s *Service) RunFor(ctx context.Context, up *store.Upstream) Result {
	started := time.Now()
	res := Result{UpstreamID: up.ID, Upstream: up.Name}

	a, err := adapter.Get(up.AdapterType)
	if err != nil {
		return s.fail(ctx, up, res, err, started)
	}
	apiKey, err := s.cipher.Decrypt(up.EncryptedKey())
	if err != nil {
		return s.fail(ctx, up, res, err, started)
	}
	discoverCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	models, err := a.Discover(discoverCtx, adapter.Upstream{
		ID: up.ID, Name: up.Name, BaseURL: up.BaseURL, APIKey: apiKey, AdapterType: up.AdapterType,
	}, s.client)
	if err != nil {
		return s.fail(ctx, up, res, err, started)
	}

	seen := make([]string, 0, len(models))
	for _, m := range models {
		reference, warnings := ratecards.Automatic(adapter.MetadataProvider(up.AdapterType, up.BaseURL), m.Name)
		values := m.Rates
		if values == nil {
			values = map[string]int64{}
		}
		if m.ContextWindow > 0 {
			values["context_window"] = m.ContextWindow
		}
		created, err := s.store.UpsertDiscoveredMetadata(ctx, up.ID, m.Name, m.Modalities, store.AutomaticMetadata{Upstream: values, Reference: reference, Warnings: append(warnings, m.MetadataWarnings...)})
		if err != nil {
			return s.fail(ctx, up, res, err, started)
		}
		if created {
			res.New++
		}
		// A provider that can identify a guard classifier at discovery time
		// (TEI's /info names the label set) hands the role over, but only on
		// a model that has never been given one. An admin's explicit
		// assignment — or explicit clearing — is never undone by a poll:
		// only a NEWLY created model can be blank here, so a cleared role on
		// an existing model is left alone.
		if m.ClassifierRole != "" && created {
			if existing, err := s.store.ModelByUpstreamAndName(ctx, up.ID, m.Name); err == nil && existing.ClassifierRole == "" {
				if err := s.store.SetModelClassifierRole(ctx, existing.ID, m.ClassifierRole); err != nil {
					s.logger.WarnContext(ctx, "apply discovered classifier role", "model", m.Name, "error", err.Error())
				}
			}
		}
		seen = append(seen, m.Name)
	}
	stale, err := s.store.MarkStaleModels(ctx, up.ID, seen)
	if err != nil {
		return s.fail(ctx, up, res, err, started)
	}
	res.Found = len(models)
	res.Stale = stale
	res.DurationMs = time.Since(started).Milliseconds()

	s.failures.Delete(up.ID)
	s.metrics.DiscoveryRuns.WithLabelValues(up.Name, "ok").Inc()
	if err := s.store.RecordUpstreamCheck(ctx, up.ID, time.Since(started), ""); err != nil {
		s.logger.WarnContext(ctx, "record upstream check", "error", err.Error())
	}
	if res.New > 0 {
		s.alerts.Dispatch(ctx, alerting.Event{
			Trigger: alerting.TriggerSystem, Severity: "info", NotifyAdmins: true,
			Title: fmt.Sprintf("%d new model(s) discovered on %s", res.New, up.Name),
			Body:  "Review and enable them from Admin → Models. New models stay disabled until an administrator approves them.",
		})
	}
	return res
}

func (s *Service) fail(ctx context.Context, up *store.Upstream, res Result, cause error, started time.Time) Result {
	res.Error = cause.Error()
	res.DurationMs = time.Since(started).Milliseconds()
	s.metrics.DiscoveryRuns.WithLabelValues(up.Name, "error").Inc()
	s.metrics.UpstreamErrors.WithLabelValues(up.Name, "discovery").Inc()
	if err := s.store.RecordUpstreamCheck(ctx, up.ID, time.Since(started), cause.Error()); err != nil {
		s.logger.WarnContext(ctx, "record upstream check", "error", err.Error())
	}

	count := 1
	if prev, ok := s.failures.Load(up.ID); ok {
		count = prev.(int) + 1
	}
	s.failures.Store(up.ID, count)
	// Alert only once the failure looks persistent, so a single blip stays quiet.
	if count == 3 {
		s.alerts.Dispatch(ctx, alerting.Event{
			Trigger: alerting.TriggerUpstreamDown, Severity: "critical", NotifyAdmins: true,
			Title: "Upstream " + up.Name + " is unreachable",
			Body:  "Model discovery has failed three times in a row. Last error: " + cause.Error(),
		})
	}
	s.logger.WarnContext(ctx, "model discovery failed", "upstream", up.Name, "error", cause.Error(), "consecutive_failures", count)
	return res
}

// Start runs discovery immediately and then on the configured interval until
// the context is cancelled.
func (s *Service) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Run(ctx)
		for {
			// The next run is anchored to the LAST run, not to "now", so a
			// wake that shortens the interval below the time already
			// elapsed runs discovery immediately instead of restarting the
			// wait, and a wake that lengthens it simply waits out the
			// remainder of the new interval.
			wait := time.Until(s.LastRun().Add(s.EffectiveInterval(ctx)))
			if wait < 0 {
				wait = 0
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.wake:
				timer.Stop()
				continue
			case <-timer.C:
				s.Run(ctx)
			}
		}
	}()
}
