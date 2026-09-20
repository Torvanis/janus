package store

import (
	"context"
	"testing"
)

func TestOIDCClaimsCannotWriteSCIMOwnedGroups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "oidc:boundary", "boundary@example.com", "Boundary", false, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.SaveSCIMResource(ctx, "Groups", "", map[string]any{"displayName": "Research", "members": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "Research team", "")
	if err != nil {
		t.Fatal(err)
	}
	gid := g["id"].(string)
	if err = s.SetTeamGroupMappings(ctx, team.ID, []string{gid}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReplaceIDPGroupsForUser(ctx, u.ID, []string{"Research"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateTeamMember(ctx, team.ID, u.ID); err == nil {
		t.Fatal("OIDC name collision injected a SCIM group membership")
	}
	if err = s.ReplaceIDPGroupsForUser(ctx, u.ID, nil); err != nil {
		t.Fatal(err)
	}
	ids, err := s.GroupIDsForUser(ctx, u.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("withdrawn claim left access: %v %v", ids, err)
	}
	doc, err := s.SCIMResource(ctx, "Groups", gid)
	if err != nil {
		t.Fatal(err)
	}
	if members, ok := doc["members"].([]any); !ok || len(members) != 0 {
		t.Fatal("SCIM membership document changed")
	}
}

func TestInactiveSCIMPeerDoesNotBreakActiveUserLoginSync(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "active@example.com", "active": true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "inactive@example.com", "active": true})
	if err != nil {
		t.Fatal(err)
	}
	aid, bid := a["id"].(string), b["id"].(string)
	g, err := s.SaveSCIMResource(ctx, "Groups", "", map[string]any{"displayName": "Provisioned", "members": []any{map[string]any{"value": aid}, map[string]any{"value": bid}}})
	if err != nil {
		t.Fatal(err)
	}
	gid := g["id"].(string)
	team, err := s.CreateTeam(ctx, "Provisioned team", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamGroupMappings(ctx, team.ID, []string{gid}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Users", bid, map[string]any{"userName": "inactive@example.com", "active": false}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReplaceIDPGroupsForUser(ctx, aid, nil); err != nil {
		t.Fatalf("active user's login blocked by inactive peer: %v", err)
	}
	if err = s.ValidateTeamMember(ctx, team.ID, aid); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateTeamMember(ctx, team.ID, bid); err == nil {
		t.Fatal("inactive peer regained access")
	}
	second, err := s.CreateTeam(ctx, "Second mapped team", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamGroupMappings(ctx, second.ID, []string{gid}); err != nil {
		t.Fatalf("cannot map group with inactive member: %v", err)
	}
	if err = s.ValidateTeamMember(ctx, second.ID, aid); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateTeamMember(ctx, second.ID, bid); err == nil {
		t.Fatal("new mapping granted inactive peer access")
	}
}
