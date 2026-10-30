package jobs

import (
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// Every violation kind the gateway writes has a retention bound: a kind
// missing here is kept forever.
func TestEveryViolationKindHasARetention(t *testing.T) {
	for _, k := range []store.SecgwCheckKind{store.SecgwCheckSecrets, store.SecgwCheckPII, store.SecgwCheckTerms,
		store.SecgwCheckShape, store.SecgwCheckPromptInjection, store.SecgwCheckContentSafety} {
		r, ok := SecgwRetention[k]
		if !ok || r.MaxAgeHours <= 0 || r.MaxCount <= 0 {
			t.Errorf("%s: no retention bound (%+v)", k, r)
		}
	}
}
