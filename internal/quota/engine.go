package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Delta is the metered contribution of one request.
type Delta struct {
	TokensIn  int64
	TokensOut int64
	CostNano  int64
	Requests  int64
}

// For returns the delta value for a given quota metric.
func (d Delta) For(metric string) int64 {
	switch metric {
	case store.MetricTokensIn:
		return d.TokensIn
	case store.MetricTokensOut:
		return d.TokensOut
	case store.MetricCostUSD:
		return d.CostNano
	case store.MetricRequests:
		return d.Requests
	default:
		return 0
	}
}

// Status is the evaluated state of one quota for one subject.
type Status struct {
	Quota    *store.Quota `json:"quota"`
	Current  int64        `json:"current_value"`
	Limit    int64        `json:"limit_value"`
	ResetAt  time.Time    `json:"reset_at"`
	Percent  float64      `json:"percent"`
	AtRisk   bool         `json:"at_risk"`
	Breached bool         `json:"breached"`
}

// Decision is the outcome of the pre-flight quota check.
type Decision struct {
	Allowed  bool
	Breached *Status
	// HardKill means an in-flight stream must be cut the moment the limit is
	// crossed rather than being allowed to finish.
	HardKill bool
}

// ThresholdEvent is emitted once per quota, window, and threshold crossing.
type ThresholdEvent struct {
	Quota     *store.Quota
	Threshold int
	Current   int64
	Limit     int64
	ResetAt   time.Time
}

// Notifier receives threshold crossings so the alerting layer can fan them out.
type Notifier func(ctx context.Context, ev ThresholdEvent)

// Engine evaluates and records quota consumption.
//
// User/service calendar counters use a durable ledger with an in-process
// cache. Rolling windows and all team counters read the usage-event log on
// every evaluation. Team event attribution can be explicitly reassigned, so a
// cached or historical ledger value cannot be authoritative for a team.
type Engine struct {
	store    *store.Store
	notifier Notifier
	// localOnly mirrors config.Config.LocalOnly: cost tracking is disabled,
	// so USD-metric quotas are inert — excluded from enforcement and accrual
	// (they would otherwise keep blocking traffic on consumption recorded
	// before the mode was enabled). Status listings still include them so
	// dashboards and admins can see and manage the dormant rules.
	localOnly bool

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	value       int64
	windowStart time.Time
	loadedAt    time.Time
}

// NewEngine builds a quota engine.
func NewEngine(s *store.Store) *Engine {
	return &Engine{store: s, cache: map[string]*cacheEntry{}}
}

// SetNotifier registers the threshold-crossing sink.
func (e *Engine) SetNotifier(n Notifier) { e.notifier = n }

// SetLocalOnly puts the engine in local-only mode (JANUS_LOCAL_ONLY): USD
// quotas stop being enforced or accrued. Call once at startup, before serving.
func (e *Engine) SetLocalOnly(v bool) { e.localOnly = v }

// Subject identifies the principal a quota evaluation applies to.
//
// It exists so the engine has ONE vocabulary for "who is asking", rather than
// a userID parameter that every service-token call site would have to fake.
// Exactly one of UserID / ServiceTokenID is set. A service token belongs to no
// team, so TeamIDs is always empty for one.
type Subject struct {
	UserID         string
	TeamIDs        []string
	ServiceTokenID string
}

// UserSubject builds the subject for a human principal.
func UserSubject(userID string, teamIDs []string) Subject {
	return Subject{UserID: userID, TeamIDs: teamIDs}
}

// ServiceTokenSubject builds the subject for an integration credential.
func ServiceTokenSubject(serviceTokenID string) Subject {
	return Subject{ServiceTokenID: serviceTokenID}
}

// IsServiceToken reports whether the subject is an integration credential.
func (s Subject) IsServiceToken() bool { return s.ServiceTokenID != "" }

// Applicable returns the quotas governing a subject (including team-inherited
// rules for users), narrowed to those that cover the requested model.
func (e *Engine) Applicable(ctx context.Context, subject Subject, modelID string) ([]*store.Quota, error) {
	var quotas []*store.Quota
	var err error
	if subject.IsServiceToken() {
		quotas, err = e.store.QuotasForServiceToken(ctx, subject.ServiceTokenID)
	} else {
		quotas, err = e.store.QuotasForSubject(ctx, subject.UserID, subject.TeamIDs)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*store.Quota, 0, len(quotas))
	for _, q := range quotas {
		if q.ModelID != "" && modelID != "" && q.ModelID != modelID {
			continue
		}
		if e.localOnly && q.Metric == store.MetricCostUSD {
			// Local-only mode: cost accrues zero, and a USD quota breached
			// before the mode was enabled must not keep blocking traffic.
			continue
		}
		out = append(out, q)
	}
	return out, nil
}

// Check evaluates every applicable quota before the upstream is contacted.
// Multiple quotas compose with AND: the most restrictive one wins.
func (e *Engine) Check(ctx context.Context, subject Subject, modelID string) (Decision, error) {
	return e.CheckWithPending(ctx, subject, modelID, Delta{})
}

// CheckWithPending evaluates quotas like Check, but adds a not-yet-recorded
// in-flight delta on top of recorded consumption. Hard-kill mid-stream checks
// use it so a single stream's own consumption can breach its limit:
// usage is only recorded post-flight, so without the pending delta a lone
// large stream would never be cut by what it is itself consuming.
func (e *Engine) CheckWithPending(ctx context.Context, subject Subject, modelID string, pending Delta) (Decision, error) {
	quotas, err := e.Applicable(ctx, subject, modelID)
	if err != nil {
		return Decision{Allowed: true}, err
	}
	now := time.Now().UTC()
	for _, q := range quotas {
		status, err := e.status(ctx, q, now)
		if err != nil {
			// A quota that cannot be evaluated must not silently permit spend.
			return Decision{Allowed: false}, err
		}
		if status.Current+pending.For(q.Metric) >= status.Limit {
			return Decision{Allowed: false, Breached: status, HardKill: q.BreachBehavior == store.BreachHardKill}, nil
		}
	}
	return Decision{Allowed: true}, nil
}

// HardKillActive reports whether any applicable quota demands that a stream be
// cut mid-flight once its limit is crossed.
func (e *Engine) HardKillActive(ctx context.Context, subject Subject, modelID string) bool {
	quotas, err := e.Applicable(ctx, subject, modelID)
	if err != nil {
		return false
	}
	for _, q := range quotas {
		if q.BreachBehavior == store.BreachHardKill {
			return true
		}
	}
	return false
}

// Record applies a completed request's usage to every applicable quota and
// raises threshold alerts at 80%, 95%, and breach.
func (e *Engine) Record(ctx context.Context, subject Subject, modelID string, d Delta) error {
	quotas, err := e.Applicable(ctx, subject, modelID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, q := range quotas {
		delta := d.For(q.Metric)
		if delta == 0 {
			continue
		}
		windowStart, resetAt, err := WindowBounds(q.Window, now)
		if err != nil {
			return err
		}
		var current int64
		if IsRolling(q.Window) || q.SubjectType == "team" {
			// Rolling and team counters use events already persisted before
			// Record. Adding the delta again would double-count team usage.
			current, err = e.sumEvents(ctx, q, windowStart)
			if err != nil {
				return err
			}
		} else {
			current, err = e.store.AddLedger(ctx, q.ID, windowStart, delta)
			if err != nil {
				return err
			}
			e.mu.Lock()
			e.cache[q.ID] = &cacheEntry{value: current, windowStart: windowStart, loadedAt: now}
			e.mu.Unlock()
		}
		e.raiseThresholds(ctx, q, current, windowStart, resetAt)
	}
	return nil
}

// DefaultAlertThresholds are the documented warning tiers used when a quota rule
// carries no per-rule configuration.
var DefaultAlertThresholds = []int{95, 80}

// EffectiveWarningThresholds returns the warning percentages in effect for a
// rule — the admin-configured list or the 80/95 defaults — ascending, and
// excluding the always-on 100% breach tier.
func EffectiveWarningThresholds(q *store.Quota) []int {
	ts := q.AlertThresholds
	if len(ts) == 0 {
		ts = DefaultAlertThresholds
	}
	out := make([]int, 0, len(ts))
	for _, t := range ts {
		if t >= 1 && t < 100 {
			out = append(out, t)
		}
	}
	sort.Ints(out)
	return out
}

// alertThresholds resolves the descending threshold ladder for one rule: the
// admin-configured percentages (or the 80/95 defaults), always topped by the
// 100% breach alert, which is documented and not configurable away.
func alertThresholds(q *store.Quota) []int {
	out := append(EffectiveWarningThresholds(q), 100)
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// alertDedupStart returns the window_start value used to deduplicate threshold
// alerts. Calendar windows have a fixed start that naturally repeats for every
// request inside the window. Rolling windows slide continuously — their
// windowStart is unique on every call — so they are quantized to fixed buckets
// one window-width wide (aligned to the Unix epoch, deterministic across
// replicas): each threshold then alerts at most once per window width instead
// of on every metered request.
func alertDedupStart(window string, windowStart time.Time) time.Time {
	if !IsRolling(window) {
		return windowStart
	}
	width := 24 * time.Hour * time.Duration(RollingWindowDays(window))
	return windowStart.Truncate(width)
}

func (e *Engine) raiseThresholds(ctx context.Context, q *store.Quota, current int64, windowStart, resetAt time.Time) {
	if e.notifier == nil || q.Limit <= 0 {
		return
	}
	dedupStart := alertDedupStart(q.Window, windowStart)
	percent := float64(current) / float64(q.Limit) * 100
	for _, threshold := range alertThresholds(q) {
		if percent < float64(threshold) {
			continue
		}
		fresh, err := e.store.MarkThresholdNotified(ctx, q.ID, dedupStart, threshold)
		if err != nil {
			slog.ErrorContext(ctx, "record quota threshold state", "error", err.Error(), "quota_id", q.ID)
			return
		}
		if fresh {
			e.notifier(ctx, ThresholdEvent{Quota: q, Threshold: threshold, Current: current, Limit: q.Limit, ResetAt: resetAt})
		}
		return // only the highest crossed threshold alerts
	}
}

// Status evaluates all quotas applying to a user, for the quota dashboard.
func (e *Engine) Status(ctx context.Context, userID string, teamIDs []string) ([]*Status, error) {
	quotas, err := e.store.QuotasForSubject(ctx, userID, teamIDs)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]*Status, 0, len(quotas))
	for _, q := range quotas {
		s, err := e.status(ctx, q, now)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// StatusFor evaluates a single quota.
func (e *Engine) StatusFor(ctx context.Context, q *store.Quota) (*Status, error) {
	return e.status(ctx, q, time.Now().UTC())
}

func (e *Engine) status(ctx context.Context, q *store.Quota, now time.Time) (*Status, error) {
	windowStart, resetAt, err := WindowBounds(q.Window, now)
	if err != nil {
		return nil, err
	}
	current, err := e.currentValue(ctx, q, windowStart, now)
	if err != nil {
		return nil, err
	}
	s := &Status{Quota: q, Current: current, Limit: q.Limit, ResetAt: resetAt}
	if q.Limit > 0 {
		s.Percent = float64(current) / float64(q.Limit) * 100
	}
	s.AtRisk = s.Percent >= 80
	s.Breached = current >= q.Limit
	return s, nil
}

// sumEvents keeps team enforcement based on attributed events, never today's
// membership. Non-team quota accounting retains its existing semantics.
func (e *Engine) sumEvents(ctx context.Context, q *store.Quota, start time.Time) (int64, error) {
	if q.SubjectType == "team" {
		return e.store.SumTeamMetricSince(ctx, q.SubjectID, q.ModelID, q.Metric, start)
	}
	return e.store.SumMetricSince(ctx, q.SubjectType, q.SubjectID, q.ModelID, q.Metric, start)
}

// currentValue bypasses ledgers and caches for teams: explicit history moves
// must affect both teams across replicas immediately, without reconciliation.
func (e *Engine) currentValue(ctx context.Context, q *store.Quota, windowStart, now time.Time) (int64, error) {
	if IsRolling(q.Window) || q.SubjectType == "team" {
		return e.sumEvents(ctx, q, windowStart)
	}
	e.mu.Lock()
	entry, ok := e.cache[q.ID]
	e.mu.Unlock()
	if ok && entry.windowStart.Equal(windowStart) && now.Sub(entry.loadedAt) < 5*time.Second {
		return entry.value, nil
	}

	value, err := e.store.LedgerValue(ctx, q.ID, windowStart)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		// Either a genuinely fresh window or a ledger that was lost. Rebuilding
		// from the immutable event log makes both cases correct.
		rebuilt, rebuildErr := e.sumEvents(ctx, q, windowStart)
		if rebuildErr != nil {
			return 0, rebuildErr
		}
		if rebuilt > 0 {
			if err := e.store.SetLedger(ctx, q.ID, windowStart, rebuilt); err != nil {
				return 0, err
			}
		}
		value = rebuilt
	}
	e.mu.Lock()
	e.cache[q.ID] = &cacheEntry{value: value, windowStart: windowStart, loadedAt: now}
	e.mu.Unlock()
	return value, nil
}

// Reconcile rewrites every calendar-window ledger row from the event log. It
// runs on a schedule and after a cache loss, and is the recovery path when the
// fast counter store is unavailable.
func (e *Engine) Reconcile(ctx context.Context) error {
	quotas, err := e.store.ListQuotas(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, q := range quotas {
		if IsRolling(q.Window) {
			continue
		}
		windowStart, _, err := WindowBounds(q.Window, now)
		if err != nil {
			return err
		}
		value, err := e.sumEvents(ctx, q, windowStart)
		if err != nil {
			return err
		}
		if err := e.store.SetLedger(ctx, q.ID, windowStart, value); err != nil {
			return fmt.Errorf("reconcile quota %s: %w", q.ID, err)
		}
	}
	e.mu.Lock()
	e.cache = map[string]*cacheEntry{}
	e.mu.Unlock()
	return nil
}
