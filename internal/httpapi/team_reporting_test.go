package httpapi

import (
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
	"time"
)

func TestTeamDashboardUsesHistoricalAttributionNotCurrentRoster(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	leader, _, err := h.store.UpsertUserFromIdentity(ctx, "report-lead", "report@example.com", "Leader", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.store.CreateTeam(ctx, "Reporting", leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.store.AddTeamMember(ctx, team.ID, h.user.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*store.UsageEvent{{ID: store.NewID(), UserID: h.user.ID, TeamIDs: team.ID, CostNano: 111, CreatedAt: time.Now().UTC()}, {ID: store.NewID(), UserID: h.user.ID, CostNano: 222, CreatedAt: time.Now().UTC()}} {
		if err = h.store.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	assertTotal := func() {
		t.Helper()
		resp := h.do(http.MethodGet, "/api/v1/dashboard/team?team_id="+team.ID, nil)
		var body struct {
			Totals store.Totals `json:"totals"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.Code != 200 || body.Totals.CostNano != 111 {
			t.Fatalf("team report status=%d total=%d, wanted stored team 111", resp.Code, body.Totals.CostNano)
		}
	}
	assertTotal()
	if err = h.store.RemoveTeamMemberSource(ctx, team.ID, h.user.ID, "manual", ""); err != nil {
		t.Fatal(err)
	}
	assertTotal()
}
