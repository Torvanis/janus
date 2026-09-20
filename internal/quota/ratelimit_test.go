package quota

import (
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestRateLimiterBucketRefill(t *testing.T) {
	l := NewRateLimiter()
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// A 3-rpm bucket admits exactly 3 immediate requests, then refuses.
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k", 3, now); !ok {
			t.Fatalf("request %d refused; the bucket should start full", i+1)
		}
	}
	ok, resetAt := l.Allow("k", 3, now)
	if ok {
		t.Fatal("fourth immediate request admitted; the bucket should be empty")
	}
	// At 3 rpm one token refills every 20s.
	if want := now.Add(20 * time.Second); !resetAt.Equal(want) {
		t.Errorf("reset at %v, want %v", resetAt, want)
	}

	// Before the refill instant the request is still refused…
	if ok, _ := l.Allow("k", 3, now.Add(10*time.Second)); ok {
		t.Fatal("request admitted before a token refilled")
	}
	// …after it, admitted again. (10s at 3rpm refilled 0.5 tokens; 12s more
	// completes the token.)
	if ok, _ := l.Allow("k", 3, now.Add(25*time.Second)); !ok {
		t.Fatal("request refused after a token refilled")
	}

	// Idle time never overfills the bucket beyond its capacity.
	if ok, _ := l.Allow("k2", 2, now); !ok {
		t.Fatal("fresh bucket refused")
	}
	later := now.Add(time.Hour)
	admitted := 0
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k2", 2, later); ok {
			admitted++
		}
	}
	if admitted != 2 {
		t.Errorf("after a long idle period %d requests admitted at once, want the capacity of 2", admitted)
	}
}

// The rule is a cluster-wide ceiling. N replicas each admit rule/N, so
// the sum across the cluster equals the rule — the property that makes
// replicas>=2 honest without a shared counter.
func TestRateLimiterDividesByReplicas(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	const rule = 60

	admittedAcross := func(replicas int) int {
		total := 0
		for r := 0; r < replicas; r++ {
			l := NewRateLimiter()
			l.SetReplicas(replicas)
			for i := 0; i < rule*2; i++ {
				if ok, _ := l.Allow("k", rule, now); ok {
					total++
				}
			}
		}
		return total
	}
	for _, n := range []int{1, 2, 3, 4} {
		if got := admittedAcross(n); got != rule {
			t.Errorf("%d replicas admitted %d in one burst, want the rule value %d", n, got, rule)
		}
	}

	// Refill is divided too: at 60 rpm on 2 replicas each refills a token
	// every 2s, not every 1s.
	l := NewRateLimiter()
	l.SetReplicas(2)
	for i := 0; i < rule; i++ {
		l.Allow("k", rule, now)
	}
	ok, resetAt := l.Allow("k", rule, now)
	if ok {
		t.Fatal("bucket should be empty")
	}
	if want := now.Add(2 * time.Second); !resetAt.Equal(want) {
		t.Errorf("reset at %v, want %v (refill rate must be divided by replicas)", resetAt, want)
	}

	// A tiny rule on many replicas still admits a fair share instead of
	// rounding down to nothing.
	l = NewRateLimiter()
	l.SetReplicas(4)
	if ok, _ := l.Allow("k", 1, now); !ok {
		t.Fatal("1 rpm on 4 replicas must still admit the first request")
	}
	if ok, reset := l.Allow("k", 1, now); ok || !reset.Equal(now.Add(4*time.Minute)) {
		t.Errorf("second request: ok=%v reset=%v, want refused with reset in 4m", ok, reset)
	}

	// Below-1 and unset are both a divisor of 1.
	l = NewRateLimiter()
	l.SetReplicas(0)
	if l.Replicas() != 1 {
		t.Errorf("replicas=%d, want 1", l.Replicas())
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	l := NewRateLimiter()
	now := time.Now()
	if ok, _ := l.Allow("user-a", 1, now); !ok {
		t.Fatal("first request for user-a refused")
	}
	if ok, _ := l.Allow("user-a", 1, now); ok {
		t.Fatal("second request for user-a admitted over a 1 rpm limit")
	}
	if ok, _ := l.Allow("user-b", 1, now); !ok {
		t.Fatal("user-b should have their own bucket")
	}
}

func TestMatchRateLimitRule(t *testing.T) {
	cases := []struct {
		rule   store.RateLimitRule
		userID string
		path   string
		want   bool
	}{
		{store.RateLimitRule{Endpoint: "*", RequestsPerMinute: 10}, "u1", "/v1/chat/completions", true},
		{store.RateLimitRule{Endpoint: "/v1/chat/completions", RequestsPerMinute: 10}, "u1", "/v1/chat/completions", true},
		{store.RateLimitRule{Endpoint: "/v1/chat/completions", RequestsPerMinute: 10}, "u1", "/v1/embeddings", false},
		{store.RateLimitRule{Endpoint: "/v1/audio/*", RequestsPerMinute: 10}, "u1", "/v1/audio/transcriptions", true},
		{store.RateLimitRule{SubjectID: "u2", Endpoint: "*", RequestsPerMinute: 10}, "u1", "/v1/chat/completions", false},
		{store.RateLimitRule{SubjectID: "u1", Endpoint: "*", RequestsPerMinute: 10}, "u1", "/v1/chat/completions", true},
		{store.RateLimitRule{Endpoint: "*", RequestsPerMinute: 0}, "u1", "/v1/chat/completions", false},
	}
	for i, tc := range cases {
		if got := MatchRateLimitRule(&tc.rule, tc.userID, tc.path); got != tc.want {
			t.Errorf("case %d: match = %v, want %v", i, got, tc.want)
		}
	}
}
