package store

import (
	"context"
	"testing"
)

func teamTestUser(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.exec(context.Background(), `INSERT INTO app_user(id,auth_provider_id,email,name,created_at) VALUES (?,?,?,?,?)`, id, id, id+"@test", id, ""); err != nil {
		t.Fatal(err)
	}
}

func TestTeamSourcesAndLeader(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	teamTestUser(t, s, "lead")
	teamTestUser(t, s, "u")
	team, err := s.CreateTeam(ctx, "sources", "lead")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []struct{ typ, id string }{{"manual", ""}, {"group", "g1"}, {"group", "g2"}} {
		if err := s.AddTeamMember(ctx, team.ID, "u", "moderator", src.typ, src.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "u", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "u", "group", "g1"); err != nil {
		t.Fatal(err)
	}
	role, err := s.TeamRole(ctx, team.ID, "u")
	if err != nil || role != "moderator" {
		t.Fatalf("surviving role %q: %v", role, err)
	}
	members, err := s.TeamMembers(ctx, team.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members: %#v %v", members, err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "lead", "manual", ""); err == nil {
		t.Fatal("removed last leader")
	}
	if err := s.SetTeamMemberRole(ctx, team.ID, "lead", "member"); err == nil {
		t.Fatal("demoted last leader")
	}
	if err := s.SetTeamMemberRole(ctx, team.ID, "u", "invalid"); err == nil {
		t.Fatal("accepted invalid role")
	}
	if err := s.SetTeamMemberRole(ctx, team.ID, "u", "leader"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "lead", "manual", ""); err != nil {
		t.Fatal(err)
	}
	teams, err := s.TeamsForUser(ctx, "u")
	if err != nil || len(teams) != 1 || teams[0].Role != "leader" {
		t.Fatalf("my role: %#v %v", teams, err)
	}
}

func TestTeamBackfillTransitions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	teamTestUser(t, s, "u")
	a, err := s.CreateTeam(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "b", "")
	if err != nil {
		t.Fatal(err)
	}
	addUsage := func(id, team string) {
		t.Helper()
		if err := s.exec(ctx, `INSERT INTO usage_event(id,created_at,user_id,team_ids) VALUES (?,'','u',?)`, id, team); err != nil {
			t.Fatal(err)
		}
	}
	check := func(id, want string) {
		t.Helper()
		var got string
		if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s attributed to %q want %q", id, got, want)
		}
	}
	addUsage("old", "")
	addUsage("assigned", "historic,other")
	if err := s.AddTeamMember(ctx, a.ID, "u", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	check("old", a.ID)
	check("assigned", "historic,other")
	addUsage("unassigned-while-member", "")
	if err := s.AddTeamMember(ctx, b.ID, "u", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	check("unassigned-while-member", "")
	check("old", a.ID)
	for _, id := range []string{a.ID, b.ID} {
		if err := s.RemoveTeamMemberSource(ctx, id, "u", "manual", ""); err != nil {
			t.Fatal(err)
		}
	}
	addUsage("later", "")
	if err := s.AddTeamMember(ctx, b.ID, "u", "member", "group", "g"); err != nil {
		t.Fatal(err)
	}
	check("later", b.ID)
	check("unassigned-while-member", b.ID)
	check("old", a.ID)
	if err := s.DeleteTeam(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	check("later", b.ID)
	historical, err := s.TeamByID(ctx, b.ID)
	if err != nil || historical.ArchivedAt.IsZero() {
		t.Fatalf("archived identity: %#v %v", historical, err)
	}
	active, err := s.TeamsForUser(ctx, "u")
	if err != nil || len(active) != 0 {
		t.Fatalf("archived active: %#v %v", active, err)
	}
	if err := s.AddTeamMember(ctx, b.ID, "u", "member", "manual", ""); err == nil {
		t.Fatal("added archived membership")
	}
}

func TestTeamGroupReconciliation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"lead", "u", "v"} {
		teamTestUser(t, s, id)
	}
	team, err := s.CreateTeam(ctx, "mapped", "lead")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	idp, err := s.EnsureGroup(ctx, "idp", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupMembers(ctx, g.ID, []string{"u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGroupMappings(ctx, team.ID, []string{g.ID, idp.ID}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.TeamGroupIDs(ctx, team.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("mappings %v %v", ids, err)
	}
	if role, err := s.TeamRole(ctx, team.ID, "u"); err != nil || role != "member" {
		t.Fatalf("group membership %q %v", role, err)
	}
	if err := s.SetTeamMemberRole(ctx, team.ID, "u", "moderator"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceIDPGroupsForUser(ctx, "u", []string{"idp", "idp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMembers(ctx, team.ID, []string{"lead", "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupMembers(ctx, g.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMembers(ctx, team.ID, []string{"lead"}); err != nil {
		t.Fatal(err)
	}
	if role, err := s.TeamRole(ctx, team.ID, "u"); err != nil || role != "moderator" {
		t.Fatalf("role/source lost %q %v", role, err)
	}
	if err := s.ReplaceIDPGroupsForUser(ctx, "u", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamRole(ctx, team.ID, "u"); err != ErrNotFound {
		t.Fatalf("removed IdP source still member: %v", err)
	}
	if err := s.SetTeamMembers(ctx, team.ID, nil); err != ErrLastTeamLeader {
		t.Fatalf("legacy bypassed guard: %v", err)
	}
	if err := s.UpdateTeam(ctx, team.ID, "renamed", "v", true); err != nil {
		t.Fatal(err)
	}
	if role, err := s.TeamRole(ctx, team.ID, "v"); err != nil || role != "leader" {
		t.Fatalf("legacy update did not promote: %q %v", role, err)
	}
	if err := s.SetTeamMembers(ctx, team.ID, []string{"v"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGroupMappings(ctx, team.ID, nil); err != nil {
		t.Fatal(err)
	}
	ids, err = s.TeamGroupIDs(ctx, team.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("unmap %v %v", ids, err)
	}
}

func TestTeamGroupDeletionAndLeaderRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	teamTestUser(t, s, "u")
	team, err := s.CreateTeam(ctx, "draft", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, "delete-me")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupMembers(ctx, g.ID, []string{"u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGroupMappings(ctx, team.ID, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamRole(ctx, team.ID, "u"); err != ErrNotFound {
		t.Fatalf("deleted group still grants team: %v", err)
	}
	if err := s.AddTeamMember(ctx, team.ID, "u", "leader", "manual", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.TeamByID(ctx, team.ID)
	if err != nil || got.LeadUserID != "u" {
		t.Fatalf("legacy lead: %#v %v", got, err)
	}
	g, err = s.CreateGroup(ctx, "leader-group")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupMembers(ctx, g.ID, []string{"u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGroupMappings(ctx, team.ID, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "u", "manual", ""); err != nil {
		t.Fatal(err)
	}
	// Directory removal is authoritative even for a group-only last leader.
	// Keeping access would defeat deprovisioning; an administrator repairs governance.
	if err := s.SetGroupMembers(ctx, g.ID, nil); err != nil {
		t.Fatalf("leader sync: %v", err)
	}
	ids, err := s.GroupIDsForUser(ctx, "u")
	if err != nil || len(ids) != 0 {
		t.Fatalf("directory removal not applied %v %v", ids, err)
	}
	if _, err := s.TeamRole(ctx, team.ID, "u"); err != ErrNotFound {
		t.Fatalf("removed directory leader still has access: %v", err)
	}
	if err := s.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGroupMappings(ctx, team.ID, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTeamMembershipMigration(t *testing.T) {
	all := migrations
	defer func() { migrations = all }()
	var old []migration
	for _, m := range all {
		if m.name < "0027_team_membership" {
			old = append(old, m)
		}
	}
	migrations = old
	s := newTestStore(t)
	migrations = all
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO team(id,name,lead_user_id,created_at) VALUES ('t','legacy','lead','')`,
		`INSERT INTO team_member(team_id,user_id) VALUES ('t','member')`,
	} {
		if err := s.exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var role string
	if err := s.queryRow(ctx, `SELECT role FROM team_member WHERE team_id='t' AND user_id='lead'`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "leader" {
		t.Fatalf("lead role = %q", role)
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE source_type='manual' AND source_id=''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("manual sources = %d", n)
	}
}
