package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"testing"
)

// The overview can be fetched piece by piece so the console paints each
// chart as its piece arrives; the whole document is the union of the parts.
func TestAdminOverviewParts(t *testing.T) {
	h := newHarness(t)
	keys := func(path string) []string {
		t.Helper()
		rec := h.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	want := map[string][]string{
		"totals":      {"active_users_15m", "model_counts", "range", "service_token_counts", "service_token_totals", "totals", "user_count"},
		"series":      {"range", "series"},
		"leaders":     {"by_status", "range", "top_models", "top_service_tokens", "top_spenders", "top_teams"},
		"performance": {"model_performance", "range"},
		"quotas":      {"near_breach", "range"},
	}
	union := map[string]bool{}
	for part, w := range want {
		got := keys("/api/v1/admin/overview?part=" + part)
		if len(got) != len(w) {
			t.Fatalf("part %s keys = %v, want %v", part, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("part %s keys = %v, want %v", part, got, w)
			}
			union[w[i]] = true
		}
	}
	whole := keys("/api/v1/admin/overview")
	if len(whole) != len(union) {
		t.Fatalf("whole document keys = %v, union of parts = %v", whole, union)
	}
	for _, k := range whole {
		if !union[k] {
			t.Fatalf("whole document has %q that no part returns", k)
		}
	}
	if rec := h.do(http.MethodGet, "/api/v1/admin/overview?part=nope", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown part: %d", rec.Code)
	}
}

func TestParallelReturnsFirstErrorAfterAllFinish(t *testing.T) {
	done := make([]bool, 3)
	boom := errors.New("boom")
	err := parallel(
		func() error { done[0] = true; return nil },
		func() error { done[1] = true; return boom },
		func() error { done[2] = true; return nil },
	)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	for i, d := range done {
		if !d {
			t.Fatalf("job %d did not run", i)
		}
	}
}
