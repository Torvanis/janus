package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// TestCachedReadLoadsOnceWithinTTL locks the read-through contract: within the
// TTL the loader runs once, errors are never cached, and invalidate() forces a
// fresh load.
func TestCachedReadLoadsOnceWithinTTL(t *testing.T) {
	c := &configCache{}
	loads := 0
	load := func() (int, error) {
		loads++
		return 42, nil
	}
	for i := 0; i < 5; i++ {
		v, err := cachedRead(c, "k", load)
		if err != nil || v != 42 {
			t.Fatalf("cachedRead = (%d, %v), want (42, nil)", v, err)
		}
	}
	if loads != 1 {
		t.Fatalf("loader ran %d times within the TTL, want 1", loads)
	}

	c.invalidate()
	if _, err := cachedRead(c, "k", load); err != nil {
		t.Fatalf("cachedRead after invalidate: %v", err)
	}
	if loads != 2 {
		t.Fatalf("loader ran %d times after invalidate, want 2", loads)
	}

	// Errors must not be cached: a transient failure may not pin a zero value.
	c.invalidate()
	failures := 0
	failing := func() (int, error) {
		failures++
		return 0, http.ErrServerClosed
	}
	for i := 0; i < 3; i++ {
		if _, err := cachedRead(c, "k", failing); err == nil {
			t.Fatal("cachedRead swallowed the loader error")
		}
	}
	if failures != 3 {
		t.Fatalf("failing loader ran %d times, want 3 (errors are never cached)", failures)
	}

	// A stale entry is reloaded.
	c.entries.Store("k", configCacheEntry{value: 7, cachedAt: time.Now().Add(-2 * c.effectiveTTL())})
	v, err := cachedRead(c, "k", load)
	if err != nil || v != 42 {
		t.Fatalf("stale cachedRead = (%d, %v), want a fresh 42", v, err)
	}
}

// TestConfigCacheTTLBoundsCrossReplicaStaleness locks the documented
// contract: the default TTL is at most 5 seconds (an admin enabling or
// disabling a model must be visible to users within 5s even on replicas that
// did not process the write), and a configured TTL overrides the default.
func TestConfigCacheTTLBoundsCrossReplicaStaleness(t *testing.T) {
	c := &configCache{}
	if c.effectiveTTL() > 5*time.Second {
		t.Fatalf("default config cache TTL is %v; must be <= 5s (documented cross-replica visibility bound)", c.effectiveTTL())
	}

	// A configured TTL wins over the default.
	c.ttl = 2 * time.Second
	if c.effectiveTTL() != 2*time.Second {
		t.Fatalf("effectiveTTL = %v with ttl set, want 2s", c.effectiveTTL())
	}

	// An entry older than the configured TTL is reloaded even though it would
	// still be fresh under a longer default.
	loads := 0
	c.entries.Store("k", configCacheEntry{value: 7, cachedAt: time.Now().Add(-3 * time.Second)})
	v, err := cachedRead(c, "k", func() (int, error) { loads++; return 42, nil })
	if err != nil || v != 42 || loads != 1 {
		t.Fatalf("cachedRead past configured TTL = (%d, %v, loads=%d), want fresh (42, nil, 1)", v, err, loads)
	}
}

// TestProxyConfigCacheInvalidatedByAdminWrites proves an admin write takes
// effect on the proxy hot path immediately — not one cache TTL later — because
// the write handler clears the config cache it shares a replica with.
func TestProxyConfigCacheInvalidatedByAdminWrites(t *testing.T) {
	h := newHarness(t)

	// Warm every hot-path cache with a successful proxied request.
	if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("warm-up request returned %d: %s", rec.Code, rec.Body.String())
	}

	// Create a blocking rule through the ADMIN API (the caller is an admin).
	created := h.do(http.MethodPost, "/api/v1/admin/rules/blocking", map[string]any{
		"name": "block-this-model", "combinator": "and", "reason": "cache invalidation test",
		"clauses": []map[string]any{{"type": "model_name", "pattern": "test-model"}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create blocking rule returned %d: %s", created.Code, created.Body.String())
	}

	// The very next proxied request must be blocked; a stale rules cache would
	// let it through for up to configCacheTTL.
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("request after admin write returned %d, want 403 (config cache was not invalidated)", rec.Code)
	}
}
