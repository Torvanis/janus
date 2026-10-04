package balancer

import (
	"sync"
	"time"
)

// Breaker is a per-replica passive circuit breaker for one pool member.
//
// Closed: traffic flows; consecutive failures are counted.
// Open:   after Threshold consecutive failures the member is ejected for a
//
//	cooldown that doubles on each re-ejection (Base..Max).
//
// Half-open: when the cooldown ends ONE trial request is allowed; success
//
//	closes the breaker and resets the cooldown, failure re-opens it.
//
// Only failures that say the member is broken count (connection errors,
// 502/503/504, timeouts) — never a 4xx the caller caused.
type Breaker struct {
	Threshold int
	Base, Max time.Duration

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	cooldown  time.Duration
	trial     bool // a half-open trial is in flight
}

// NewBreaker returns a breaker with the default thresholds: 3 consecutive
// failures, 5s first cooldown, capped at 2 minutes.
func NewBreaker() *Breaker {
	return &Breaker{Threshold: 3, Base: 5 * time.Second, Max: 2 * time.Minute}
}

// Allow reports whether a request may be sent now. While open it returns
// false; at the end of the cooldown it returns true exactly once (the
// half-open trial) until that trial reports back.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if now.Before(b.openUntil) || b.trial {
		return false
	}
	b.trial = true
	return true
}

// Healthy reports the state without claiming the half-open trial.
func (b *Breaker) Healthy(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil.IsZero() || (!now.Before(b.openUntil) && !b.trial)
}

// Success records a good response.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures, b.trial, b.openUntil, b.cooldown = 0, false, time.Time{}, 0
}

// Failure records a member fault. It returns the ejection deadline when this
// failure opened (or re-opened) the breaker, so the caller can publish it to
// the other replicas; zero otherwise.
func (b *Breaker) Failure(now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.trial || b.failures >= b.Threshold {
		switch {
		case b.cooldown == 0:
			b.cooldown = b.Base
		default:
			b.cooldown = min(b.cooldown*2, b.Max)
		}
		b.trial = false
		b.failures = 0
		b.openUntil = now.Add(b.cooldown)
		return b.openUntil
	}
	return time.Time{}
}

// OpenUntil returns the current ejection deadline (zero when closed).
func (b *Breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openUntil
}
