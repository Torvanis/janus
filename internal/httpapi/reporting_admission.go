package httpapi

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Reporting labels are explicitly caller-supplied allocation hints. They are
// not authorization, verified ownership, or a reason to inspect prompt content.
func reportCallerLabel(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	chars := []rune(v)
	if len(chars) > 128 {
		chars = chars[:128]
	}
	return string(chars)
}
func stampReportingAdmission(e *store.UsageEvent, r *http.Request, localOnly bool) {
	e.Project = reportCallerLabel(r.Header.Get("X-Janus-Project"))
	e.CostCenter = reportCallerLabel(r.Header.Get("X-Janus-Cost-Center"))
	// Before forwarding, gateway rejection cannot consume upstream inference.
	e.CostStatus = "known_free"
	if localOnly {
		e.CostStatus = "disabled"
	}
}
func stampReportingCost(e *store.UsageEvent, rates usage.Rates, hasCard, localOnly bool) {
	switch {
	case localOnly:
		e.CostStatus = "disabled"
	case rates.InNano != 0 || rates.OutNano != 0 || rates.CachedNano != 0 || rates.CacheWrite5mNano != 0 || rates.CacheWrite1hNano != 0:
		e.CostStatus = "priced"
	case hasCard:
		e.CostStatus = "known_free"
	default:
		e.CostStatus = "unpriced"
	}
}
