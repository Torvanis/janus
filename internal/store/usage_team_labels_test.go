package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRequestChargedTeamSerialization(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "charged-team", "charged@example.com", "User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "Original team", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := &UsageEvent{UserID: u.ID, TeamIDs: a.ID}
	if err = s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	events, _, err := s.ListRequests(ctx, RequestFilter{UserID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.UsageEventByID(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range append(events, one) {
		raw, _ := json.Marshal(event)
		var body map[string]any
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body["team_ids"] != a.ID {
			t.Fatalf("missing recorded team: %s", raw)
		}
		names, ok := body["team_names"].([]any)
		if !ok || len(names) != 1 || names[0] != "Original team" {
			t.Fatalf("missing charged team name: %s", raw)
		}
	}
}
