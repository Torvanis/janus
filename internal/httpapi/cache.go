package httpapi

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// defaultConfigCacheTTL bounds how stale the proxy hot path may see
// slow-changing configuration (blocking rules, feature flags such as
// spend_emphasis and per_user_rate_limits_enabled, rate-limit
// rules, models, grants, upstreams, rate cards). Without a cache every
// proxied request issued ~7 database queries for data that changes on admin
// timescales — untenable against Postgres at the 10k req/min target. The
// replica that processes an admin write clears its own cache immediately
// (InvalidateConfigCache); every other replica converges within this TTL,
// which is why the default is 5s: an admin
// enabling or disabling a model is visible to users within 5 seconds even on
// multi-replica deployments. Operators can tune it with
// JANUS_CONFIG_CACHE_TTL_SECONDS (config.ConfigCacheTTL). Quota checks are
// deliberately NOT cached: they are correctness-critical and consumption
// moves per request.
const defaultConfigCacheTTL = 5 * time.Second

// configCache is a tiny read-through cache for hot-path configuration reads,
// following the tokenCache pattern (server.go). Values are stored whole and
// never mutated after insertion; errors are never cached.
type configCache struct {
	entries sync.Map // string key -> configCacheEntry
	// ttl overrides defaultConfigCacheTTL when positive; set once from
	// config.ConfigCacheTTL before the server starts handling requests.
	ttl time.Duration
}

// effectiveTTL returns the configured TTL, or the 5s default when unset.
func (c *configCache) effectiveTTL() time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return defaultConfigCacheTTL
}

type configCacheEntry struct {
	value    any
	cachedAt time.Time
}

// invalidate drops every entry. Admin writes are rare and the cache is small,
// so wholesale invalidation is simpler and safer than tracking which keys a
// given write could affect (a group-membership change, for example, alters
// grant resolution for every member).
func (c *configCache) invalidate() {
	c.entries.Range(func(k, _ any) bool {
		c.entries.Delete(k)
		return true
	})
}

// cachedRead returns the cached value for key when fresh, otherwise calls load
// and caches the result. Load errors are returned as-is and never cached, so a
// transient database failure does not pin a zero value for a TTL.
func cachedRead[T any](c *configCache, key string, load func() (T, error)) (T, error) {
	if v, ok := c.entries.Load(key); ok {
		entry := v.(configCacheEntry)
		if time.Since(entry.cachedAt) < c.effectiveTTL() {
			return entry.value.(T), nil
		}
		c.entries.Delete(key)
	}
	value, err := load()
	if err != nil {
		var zero T
		return zero, err
	}
	c.entries.Store(key, configCacheEntry{value: value, cachedAt: time.Now()})
	return value, nil
}

// InvalidateConfigCache drops cached hot-path configuration so an admin write
// takes effect on this replica at once rather than after the cache TTL.
func (s *Server) InvalidateConfigCache() {
	s.configCache.invalidate()
}

// --- cached hot-path reads ----------------------------------------------------
//
// These wrappers exist for the proxy hot path only. Admin and dashboard
// handlers keep reading the store directly so operators always see fresh data.

func (s *Server) cachedBlockingRules(ctx context.Context) ([]*store.BlockingRule, error) {
	return cachedRead(&s.configCache, "blocking_rules", func() ([]*store.BlockingRule, error) {
		return s.Store.ListBlockingRules(ctx)
	})
}

func (s *Server) cachedFeatureFlags(ctx context.Context) (map[string]bool, error) {
	return cachedRead(&s.configCache, "feature_flags", func() (map[string]bool, error) {
		return s.Store.FeatureFlags(ctx)
	})
}

// cachedUpstreamTimeoutOverrides reads the administrator-set upstream timeout
// overrides through the config cache, so every proxied request can check for
// a change without a database round trip and every replica converges on a new
// value within the cache TTL (the acting replica is flushed immediately by the
// PATCH handler).
func (s *Server) cachedUpstreamTimeoutOverrides(ctx context.Context) (store.UpstreamTimeoutOverrides, error) {
	return cachedRead(&s.configCache, "upstream_timeouts", func() (store.UpstreamTimeoutOverrides, error) {
		return s.Store.UpstreamTimeoutOverrides(ctx)
	})
}

func (s *Server) cachedRateLimitRules(ctx context.Context) ([]*store.RateLimitRule, error) {
	return cachedRead(&s.configCache, "rate_limit_rules", func() ([]*store.RateLimitRule, error) {
		return s.Store.ListRateLimitRules(ctx)
	})
}

// modelNameCacheKey is the configCache key for a plain model-by-name lookup.
// Nothing populates it today — the hot path resolves through
// resolvedModelCacheKey — but InvalidateModelNameCache still flushes it on
// rename so a future per-name reader cannot serve a dropped alias.
func modelNameCacheKey(name string) string { return "model|" + name }

// resolvedModelCacheKey namespaces the full resolution (real model OR managed
// alias) separately from the plain model lookup above, so the two cannot
// shadow each other.
func resolvedModelCacheKey(name string) string { return "resolved|" + name }

// cachedResolveModel resolves a caller-supplied model name — real model or
// managed alias — on the proxy hot path.
//
// A managed model that an admin has just repointed must take effect promptly;
// the write path calls InvalidateResolvedModelCache so the acting replica is
// never stale, and every other replica converges within the config-cache TTL
// (5s by default), the same visibility bound as model
// enable/disable.
//
// ErrManagedModelBroken is deliberately NOT cached (cachedRead never caches
// errors): a broken alias is a transient configuration fault an admin is
// probably fixing right now, and caching it would keep serving the failure
// after the repair.
func (s *Server) cachedResolveModel(ctx context.Context, name string) (store.ResolvedModel, error) {
	return cachedModelRead(&s.configCache, s.Store.ModelRevision(), resolvedModelCacheKey(name), func() (store.ResolvedModel, error) { return s.Store.ResolveModelForRequest(ctx, name) })
}

// InvalidateResolvedModelCache drops the cached resolution for the given
// names so a managed-model create, rename, repoint, or delete takes effect on
// this replica immediately rather than one cache TTL later. Empty names are
// ignored.
func (s *Server) InvalidateResolvedModelCache(names ...string) {
	for _, name := range names {
		if name == "" {
			continue
		}
		s.configCache.entries.Delete(resolvedModelCacheKey(name))
	}
}

// InvalidateModelNameCache drops the cachedModelByName entries for the given
// names so a display-name change takes effect on this replica immediately —
// not one cache TTL later (cross-replica staleness is bounded at
// ≤5s; the replica that processed the write must not be stale at all).
// Callers pass every key a rename could have populated: the old display name
// (so stale lookups miss), the native upstream name (it may be cached as the
// fallback resolution), and the new display name (so an entry cached between
// the database write and this flush cannot pin a pre-rename model). Empty
// names are ignored.
func (s *Server) InvalidateModelNameCache(names ...string) {
	for _, name := range names {
		if name == "" {
			continue
		}
		s.configCache.entries.Delete(modelNameCacheKey(name))
	}
}

func (s *Server) cachedGrantedModelIDs(ctx context.Context, userID string, groupIDs []string) (map[string]string, error) {
	key := "grants|" + userID + "|" + strings.Join(groupIDs, ",")
	return cachedRead(&s.configCache, key, func() (map[string]string, error) {
		return s.Store.GrantedModelIDs(ctx, userID, groupIDs)
	})
}

// cachedGrantedIDs resolves the effective access set for whichever principal
// kind is calling. The cache key namespaces the two so a service token and a
// user can never read each other's grant set.
func (s *Server) cachedGrantedIDs(ctx context.Context, subject store.GrantSubject) (map[string]string, error) {
	if subject.TeamID != "" {
		return s.Store.GrantedIDsFor(ctx, subject)
	}
	if subject.IsServiceToken() {
		key := "grants|svc|" + subject.ServiceTokenID
		return cachedRead(&s.configCache, key, func() (map[string]string, error) {
			return s.Store.GrantedModelIDsForServiceToken(ctx, subject.ServiceTokenID)
		})
	}
	return s.cachedGrantedModelIDs(ctx, subject.UserID, subject.GroupIDs)
}

func (s *Server) cachedUpstreamByID(ctx context.Context, id string) (*store.Upstream, error) {
	return cachedRead(&s.configCache, "upstream|"+id, func() (*store.Upstream, error) {
		return s.Store.UpstreamByID(ctx, id)
	})
}

// cachedRates carries the rate card resolved for a model. A scheduled rate
// change (effective_from in the near future) applies at most one TTL late,
// which is well inside admin expectations for the 5s default TTL.
type cachedRates struct {
	In, Out, Cached            int64
	CacheWrite5m, CacheWrite1h int64
	OK                         bool
}

func (s *Server) cachedRateCard(ctx context.Context, modelID string) cachedRates {
	rates, err := cachedModelRead(&s.configCache, s.Store.ModelRevision(), "ratecard|"+modelID, func() (cachedRates, error) {
		rc, err := s.Store.RateCardAt(ctx, modelID, time.Now().UTC())
		if err != nil {
			return cachedRates{}, err
		}
		return cachedRates{
			In: rc.RateInNano, Out: rc.RateOutNano, Cached: rc.RateCachedNano,
			CacheWrite5m: rc.RateCacheWrite5mNano, CacheWrite1h: rc.RateCacheWrite1hNano,
			OK: true,
		}, nil
	})
	if err != nil {
		return cachedRates{}
	}
	return rates
}

// Revision tags invalidate model and billing caches after scheduled discovery.
// A load that finishes after a write keeps its old tag and is discarded by the
// next read, so it cannot undo invalidation by repopulating an old value.
type modelRevisionCache[T any] struct {
	revision uint64
	value    T
}

func cachedModelRead[T any](cache *configCache, revision uint64, key string, load func() (T, error)) (T, error) {
	if v, ok := cache.entries.Load(key); ok {
		if entry, ok := v.(configCacheEntry).value.(modelRevisionCache[T]); !ok || entry.revision != revision {
			cache.entries.Delete(key)
		}
	}
	entry, err := cachedRead(cache, key, func() (modelRevisionCache[T], error) {
		value, err := load()
		return modelRevisionCache[T]{revision: revision, value: value}, err
	})
	return entry.value, err
}
