package secgw

import (
	"fmt"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// The verdict cache must be bounded: it used to be an unbounded map that
// grew by one entry per distinct message ever proxied until restart.
func TestVerdictCacheIsBounded(t *testing.T) {
	p := &store.SecgwPolicy{ID: "p", Name: "p", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeBlock)}}
	snap := snapWith([]*store.SecgwPolicy{p}, []*store.SecgwBinding{{ID: "b", PolicyID: "p", ScopeType: store.SecgwScopeOrg}})
	e, err := NewEngine(snap, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	eff := Resolve(snap, Subject{UserID: "u"})

	over := VerdictCacheSize + 1000
	for i := 0; i < over; i++ {
		e.scanCached(eff, store.SecgwDirectionIngress, "user", fmt.Sprintf("message number %d with nothing secret", i))
	}
	if n := e.verdictCache.Len(); n > VerdictCacheSize {
		t.Fatalf("cache holds %d entries after %d distinct messages, want <= %d", n, over, VerdictCacheSize)
	}

	// Still a cache: the most recent entry is a hit and returns an
	// equivalent verdict without rescanning.
	last := fmt.Sprintf("message number %d with nothing secret", over-1)
	key := cacheKey(eff, store.SecgwDirectionIngress, "user", last)
	if _, ok := e.verdictCache.Get(key); !ok {
		t.Fatal("most recent message evicted; LRU order is wrong")
	}
	// And the oldest is gone.
	first := cacheKey(eff, store.SecgwDirectionIngress, "user", "message number 0 with nothing secret")
	if _, ok := e.verdictCache.Get(first); ok {
		t.Fatal("oldest message still cached; bound is not being enforced")
	}
}
