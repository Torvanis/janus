package httpapi

import (
	"github.com/torvanis/janus/internal/store"
	"strings"
)

func requestChargedTeamLabel(e *store.UsageEvent) string {
	label := "Personal"
	if e.ServiceTokenID != "" {
		label = "Service token"
	}
	if e.TeamIDs != "" {
		label = "Unavailable team"
		if len(e.TeamNames) > 0 {
			label = strings.Join(e.TeamNames, ", ")
		}
	}
	// Team names are user-controlled. Do not emit spreadsheet formulas.
	if strings.ContainsAny(label[:1], "=+-@\t\r\n") {
		label = "'" + label
	}
	return label
}
