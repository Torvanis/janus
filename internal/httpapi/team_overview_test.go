package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestAdminOverviewTopTeams(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	readTeams := func() []store.Breakdown {
		t.Helper()
		rec := h.do(http.MethodGet, "/api/v1/admin/overview", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Teams []store.Breakdown `json:"top_teams"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Teams == nil {
			t.Fatal("top_teams must be an array, including when empty")
		}
		return body.Teams
	}
	if got := readTeams(); len(got) != 0 {
		t.Fatalf("unused teams: %+v", got)
	}
	leader, _, err := h.store.UpsertUserFromIdentity(ctx, "overview-lead", "overview@example.com", "Leader", false, false)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := h.store.CreateTeam(ctx, "Unused", leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Personal traffic must not become a leaderboard bucket, even when it dominates.
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{UserID: h.user.ID, TokensOut: 99999, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := readTeams(); len(got) != 0 {
		t.Fatalf("personal/unused teams: %+v", got)
	}
	// A request with no output tokens is still genuine usage, not an idle team.
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{UserID: h.user.ID, TeamIDs: idle.ID, TokensIn: 42, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := readTeams(); len(got) != 1 || got[0].Key != idle.ID || got[0].Totals.TokensIn != 42 {
		t.Fatalf("input-only usage must remain visible: %+v", got)
	}
	var expected []string
	for i := 0; i < 12; i++ {
		team, err := h.store.CreateTeam(ctx, fmt.Sprintf("Team %02d", i), leader.ID)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, team.ID)
		// Snapshot attribution, not the user's current roster; cost opposes token rank.
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{UserID: h.user.ID, TeamIDs: team.ID, TokensOut: int64(i + 1), CostNano: int64(100 - i), CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	got := readTeams()
	if len(got) != 10 {
		t.Fatalf("got %d teams, want top 10", len(got))
	}
	for i, row := range got {
		n := 11 - i
		if row.Key != expected[n] || row.Label != fmt.Sprintf("Team %02d", n) || row.Totals.TokensOut != int64(n+1) || row.Totals.Requests != 1 {
			t.Fatalf("rank %d: %+v", i, row)
		}
	}
}
