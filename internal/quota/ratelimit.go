package quota

import (
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// RateLimiter enforces requests-per-minute rules with in-memory token buckets.
//
// Buckets are per process. To make the configured rate a cluster-wide
// ceiling rather than a per-replica one, each process divides the rule by
// the number of live gateway processes (SetReplicas, fed by the instance
// heartbeat). No shared counter, nothing on the request path: the only
// cost is a heartbeat every few seconds. The bound is approximate around a
// scale event — for up to InstanceStaleAfter after a pod dies the survivors
// still divide by the old count and admit slightly less than the rule —
// and exact in steady state.
type RateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	lastSweep time.Time
	replicas  int
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

// NewRateLimiter returns an empty limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[string]*tokenBucket{}, lastSweep: time.Now(), replicas: 1}
}

// SetReplicas updates the live process count the rule is divided by.
// Values below 1 are treated as 1.
func (l *RateLimiter) SetReplicas(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	l.replicas = n
	l.mu.Unlock()
}

// Replicas reports the divisor currently in effect.
func (l *RateLimiter) Replicas() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replicas
}

// localRate is this process's share of a cluster-wide rule, as a refill
// rate. A 1 rpm rule on 4 replicas refills each replica at one token per
// four minutes; see Allow for why burst capacity is floored at one.
func localRate(rpm, replicas int) float64 {
	if replicas < 1 {
		replicas = 1
	}
	return float64(rpm) / float64(replicas)
}

// Allow consumes one request from the bucket identified by key, with capacity
// and refill rate both derived from rpm (requests per minute). When refused it
// reports the instant the next request would be admitted.
func (l *RateLimiter) Allow(key string, rpm int, now time.Time) (bool, time.Time) {
	if rpm <= 0 {
		return true, time.Time{}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	share := localRate(rpm, l.replicas)
	ratePerSecond := share / 60.0
	// Burst capacity is this replica's share, but never below one token:
	// a share under 1 (a 1 rpm rule on 4 replicas) would otherwise sit at
	// 0.25 forever and admit nothing. One token of burst with the refill
	// still divided keeps the steady-state rate exact.
	capacity := share
	if capacity < 1 {
		capacity = 1
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: capacity, lastFill: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * ratePerSecond
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.lastFill = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, time.Time{}
	}
	waitSeconds := (1 - b.tokens) / ratePerSecond
	return false, now.Add(time.Duration(waitSeconds * float64(time.Second)))
}

// sweep drops buckets that have been idle long enough to be full again, so the
// map stays bounded by the recently-active key set.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < 5*time.Minute {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if now.Sub(b.lastFill) > 10*time.Minute {
			delete(l.buckets, key)
		}
	}
}

// MatchRateLimitRule reports whether a rule applies to this user and path.
func MatchRateLimitRule(rule *store.RateLimitRule, userID, path string) bool {
	if rule.RequestsPerMinute <= 0 {
		return false
	}
	if rule.SubjectID != "" && rule.SubjectID != userID {
		return false
	}
	return matchEndpoint(rule.Endpoint, path)
}

// matchEndpoint supports '*' (everything), a trailing-'*' prefix, or an exact
// path.
func matchEndpoint(pattern, path string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == path
	}
}
