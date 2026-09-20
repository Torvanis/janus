package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

func TestReportingAdmissionLabelsAreBoundedAndExplicit(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Janus-Project", "  Research\t Alpha  ")
	r.Header.Set("X-Janus-Cost-Center", strings.Repeat("é", 200))
	e := &store.UsageEvent{}
	stampReportingAdmission(e, r, false)
	if e.Project != "Research Alpha" {
		t.Fatalf("project=%q", e.Project)
	}
	if len([]rune(e.CostCenter)) > 128 {
		t.Fatal("label not bounded")
	}
	if e.CostStatus != "known_free" {
		t.Fatal("unforwarded request should have no upstream cost")
	}
}
func TestReportingAdmissionCostDoesNotInferFree(t *testing.T) {
	cases := []struct {
		name        string
		rates       usage.Rates
		card, local bool
		want        string
	}{
		{"unconfigured zero", usage.Rates{}, false, false, "unpriced"},
		{"explicit zero card", usage.Rates{}, true, false, "known_free"},
		{"nonzero fallback", usage.Rates{InNano: 3}, false, false, "priced"},
		{"local only", usage.Rates{InNano: 3}, true, true, "disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &store.UsageEvent{}
			stampReportingCost(e, c.rates, c.card, c.local)
			if e.CostStatus != c.want {
				t.Fatalf("got %s want %s", e.CostStatus, c.want)
			}
		})
	}
}
