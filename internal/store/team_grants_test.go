package store

import (
	"context"
	"testing"
)

func TestTeamGrantContextIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Legacy grant channels remain available only outside a team context.
	for _, g := range []struct{ model, kind, id string }{
		{"personal", GranteeUser, "u"}, {"global", GranteeAllUsers, ""}, {"group", GranteeGroup, "g"},
		{"team-a", GranteeTeam, "a"}, {"team-b", GranteeTeam, "b"}, {"every-team", GranteeAllTeams, "ignored"},
	} {
		if _, err := s.CreateGrant(ctx, g.model, ModelKindModel, g.kind, g.id); err != nil {
			t.Fatal(err)
		}
	}
	// Direct SQL isolates grant semantics from membership creation's user checks.
	if err := s.exec(ctx, `INSERT INTO team (id,name,created_at) VALUES ('a','A',''),('b','B','')`); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `INSERT INTO team_member (team_id,user_id) VALUES ('a','u'),('b','u')`); err != nil {
		t.Fatal(err)
	}
	personal, err := s.GrantedIDsFor(ctx, GrantSubject{UserID: "u", GroupIDs: []string{"g"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(personal) != 3 || personal["team-a"] != "" || personal["every-team"] != "" {
		t.Fatalf("personal access leaked teams: %#v", personal)
	}
	selected, err := s.GrantedIDsFor(ctx, GrantSubject{UserID: "u", GroupIDs: []string{"g"}, TeamID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected["team-a"] == "" || selected["every-team"] == "" {
		t.Fatalf("team selection mixed grants: %#v", selected)
	}
	if _, err = s.GrantedIDsFor(ctx, GrantSubject{UserID: "outsider", TeamID: "a"}); err == nil {
		t.Fatal("nonmember obtained team grants")
	}
	if _, err = s.GrantedIDsFor(ctx, GrantSubject{ServiceTokenID: "svc", TeamID: "a"}); err == nil {
		t.Fatal("service token smuggled human team context")
	}
	if err = s.exec(ctx, `DELETE FROM team_member WHERE team_id='a' AND user_id='u'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantedIDsFor(ctx, GrantSubject{UserID: "u", TeamID: "a"}); err == nil {
		t.Fatal("removed member retained team grants")
	}
}

func TestAllTeamsGrantIncludesFutureTeam(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	g, err := s.CreateGrant(ctx, "alias", ModelKindManaged, GranteeAllTeams, "not-a-team")
	if err != nil {
		t.Fatal(err)
	}
	if g.GranteeID != "" {
		t.Fatal("all-teams must normalize grantee ID")
	}
	if err = s.exec(ctx, `INSERT INTO team(id,name,created_at) VALUES ('future','Future','')`); err != nil {
		t.Fatal(err)
	}
	if err = s.exec(ctx, `INSERT INTO team_member(team_id,user_id) VALUES ('future','u')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GrantedIDsFor(ctx, GrantSubject{UserID: "u", TeamID: "future"})
	if err != nil || got["alias"] == "" {
		t.Fatalf("future team missing managed grant: %#v %v", got, err)
	}
}
