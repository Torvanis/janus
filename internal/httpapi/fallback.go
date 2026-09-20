package httpapi

import (
	"context"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Managed-model fallback at request time.
//
// ResolveModelForRequest already handles the CONFIGURATION failure mode (the
// target is gone or disabled → FallbackTriggerTargetUnavailable). The runtime
// failure modes need signals the store alone does not have on the hot path,
// so they are evaluated here, from the same cached reads the proxy already
// performs:
//
//   - FallbackTriggerTargetUnavailable also covers a DISABLED upstream, which
//     the resolver cannot see because the model row itself is still enabled.
//   - FallbackTriggerUpstreamUnreachable: the target's upstream failed its
//     latest discovery probe (Upstream.ProbedUnreachable).
//   - FallbackTriggerModelDown: every request to the target in the health
//     window failed (ModelHealthStats.TrafficDown) — the same rule that badges
//     a model "down" in the catalog.
//   - FallbackTriggerModelDegraded: error rate ≥ 10% (ModelHealthStats.Degraded).
//
// None of these fire on a single failed request: a transient 503 for one
// caller never reroutes everyone. The fallback is only used when it is itself
// not in the same state — switching from a dead model to a dead model buys
// nothing, and a stale probe could make the primary look worse than it is, so
// in that case the request proceeds to the primary (fail-open) and the
// exhaustion is logged.

// Response header naming the failure mode that sent a request to its alias's
// fallback, so a caller can see the substitution happened.
const headerFallbackReason = "X-Janus-Fallback-Reason"

// cachedModelHealth returns the per-model health rollup over the health
// window, cached for one config-cache TTL so the hot path pays one aggregate
// query every few seconds rather than one per request.
func (s *Server) cachedModelHealth(ctx context.Context) (map[string]store.ModelHealthStats, error) {
	return cachedRead(&s.configCache, "model_health", func() (map[string]store.ModelHealthStats, error) {
		return s.Store.ModelHealthStats(ctx, time.Now().UTC().Add(-store.ModelHealthWindow))
	})
}

// runtimeUnavailability returns the first FallbackTrigger* the given model is
// currently exhibiting among those the alias reacts to, or "" when it looks
// servable. The order is most-certain first.
func (s *Server) runtimeUnavailability(ctx context.Context, model *store.Model, managed *store.ManagedModel) string {
	if model == nil {
		return store.FallbackTriggerTargetUnavailable
	}
	if model.Status != store.ModelEnabled && managed.HasFallbackTrigger(store.FallbackTriggerTargetUnavailable) {
		return store.FallbackTriggerTargetUnavailable
	}
	up, err := s.cachedUpstreamByID(ctx, model.UpstreamID)
	if err != nil || !up.Enabled {
		if managed.HasFallbackTrigger(store.FallbackTriggerTargetUnavailable) {
			return store.FallbackTriggerTargetUnavailable
		}
	} else if up.ProbedUnreachable() && managed.HasFallbackTrigger(store.FallbackTriggerUpstreamUnreachable) {
		return store.FallbackTriggerUpstreamUnreachable
	}
	if !managed.HasFallbackTrigger(store.FallbackTriggerModelDown) && !managed.HasFallbackTrigger(store.FallbackTriggerModelDegraded) {
		return ""
	}
	health, err := s.cachedModelHealth(ctx)
	if err != nil {
		// A storage blip must not decide routing; the primary is served.
		s.Logger.WarnContext(ctx, "managed model fallback: health rollup unavailable; serving primary", "alias", managed.Name, "error", err.Error())
		return ""
	}
	stats := health[model.ID]
	if stats.TrafficDown() && managed.HasFallbackTrigger(store.FallbackTriggerModelDown) {
		return store.FallbackTriggerModelDown
	}
	if stats.Degraded() && managed.HasFallbackTrigger(store.FallbackTriggerModelDegraded) {
		return store.FallbackTriggerModelDegraded
	}
	return ""
}

// fallbackUsable reports whether the alias's fallback is in a state to take
// over: enabled, on an enabled upstream that has not failed its latest probe,
// and not itself "down" on recent traffic.
func (s *Server) fallbackUsable(ctx context.Context, fallback *store.Model) (bool, string) {
	if fallback == nil {
		return false, "no fallback is configured"
	}
	if fallback.Status != store.ModelEnabled {
		return false, "the fallback model is " + fallback.Status
	}
	up, err := s.cachedUpstreamByID(ctx, fallback.UpstreamID)
	if err != nil || !up.Enabled {
		return false, "the fallback's upstream is disabled"
	}
	if up.ProbedUnreachable() {
		return false, "the fallback's upstream failed its latest reachability probe"
	}
	if health, err := s.cachedModelHealth(ctx); err == nil && health[fallback.ID].TrafficDown() {
		return false, "the fallback model is failing every recent request too"
	}
	return true, ""
}

// applyManagedFallback evaluates the runtime failure modes for a resolution
// that went through a managed alias and switches it to the fallback when one
// fires and the fallback can take over. Resolutions that already fell back
// (configuration-level), have no fallback, or did not use an alias are
// returned unchanged.
func (s *Server) applyManagedFallback(ctx context.Context, resolved store.ResolvedModel) store.ResolvedModel {
	if resolved.Managed == nil || resolved.FellBack || resolved.Fallback == nil {
		return resolved
	}
	trigger := s.runtimeUnavailability(ctx, resolved.Model, resolved.Managed)
	if trigger == "" {
		return resolved
	}
	ok, why := s.fallbackUsable(ctx, resolved.Fallback)
	if !ok {
		s.Logger.WarnContext(ctx, "managed model fallback exhausted: target unavailable and fallback cannot take over; serving primary",
			"alias", resolved.Managed.Name, "trigger", trigger,
			"target", resolved.Model.PublicName(), "fallback", resolved.Fallback.PublicName(), "fallback_state", why)
		return resolved
	}
	s.Logger.InfoContext(ctx, "managed model falling back",
		"alias", resolved.Managed.Name, "trigger", trigger,
		"target", resolved.Model.PublicName(), "fallback", resolved.Fallback.PublicName())
	if s.Metrics != nil && s.Metrics.ManagedModelFallbacks != nil {
		s.Metrics.ManagedModelFallbacks.WithLabelValues(resolved.Managed.Name, trigger).Inc()
	}
	return resolved.WithFallback(trigger)
}
