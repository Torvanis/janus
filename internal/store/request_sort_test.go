package store

import (
	"strings"
	"testing"
)

// Every request-log sort ends in a unique key, so LIMIT/OFFSET paging over
// ties (most rows have ttfb_ms or classifier_ms 0) never repeats or skips
// rows.
func TestRequestSortOrdersAreTotal(t *testing.T) {
	for _, k := range []string{"", "time", "oldest", "latency", "latency_asc", "cost", "cost_asc", "tokens_in", "tokens_in_asc",
		"tokens_out", "tokens_out_asc", "model", "model_asc", "modality", "modality_asc", "status", "status_asc",
		"ttfb", "ttfb_asc", "classifier", "classifier_asc"} {
		if o := requestSortOrder(k); !strings.HasSuffix(o, ", id DESC") && !strings.HasSuffix(o, ", id ASC") {
			t.Errorf("sort %q orders by %q: no unique tie-breaker", k, o)
		}
	}
}
