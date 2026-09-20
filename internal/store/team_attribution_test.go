package store

import (
	"context"
	"testing"
	"time"
)

func attributionUser(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.exec(context.Background(), `INSERT INTO app_user(id,auth_provider_id,email,created_at) VALUES (?,?,?,?)`, id, id, id+"@example.test", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTeamAttributionInactiveAdd(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "inactive")
	if err := s.UpdateUser(ctx, "inactive", "user", false); err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "inactive-team", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember(ctx, team.ID, "inactive", "member", "manual", ""); err == nil {
		t.Fatal("inactive user admitted")
	}
}

func TestTeamAttributionDirectoryRevokesSoleLeader(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "leader")
	team, err := s.CreateTeam(ctx, "directory", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember(ctx, team.ID, "leader", "leader", "group", "directory"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "leader", "group", "directory"); err != nil {
		t.Fatalf("directory revocation blocked: %v", err)
	}
	if _, err := s.TeamRole(ctx, team.ID, "leader"); err != ErrNotFound {
		t.Fatalf("access retained: %v", err)
	}
	loaded, err := s.TeamByID(ctx, team.ID)
	if err != nil || loaded.LeadUserID != "" {
		t.Fatalf("stale leader: %+v %v", loaded, err)
	}
	if err := s.AddTeamMember(ctx, team.ID, "leader", "leader", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, team.ID, "leader", "manual", ""); err != ErrLastTeamLeader {
		t.Fatalf("manual guard lost: %v", err)
	}
}

func TestTeamAttributionLateWriteUsesEarliestJoin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "late")
	a, err := s.CreateTeam(ctx, "first", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "second", "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := nowUTC
	nowUTC = func() time.Time { return start.Add(time.Hour) }
	defer func() { nowUTC = oldNow }()
	if err := s.AddTeamMember(ctx, a.ID, "late", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberSource(ctx, a.ID, "late", "manual", ""); err != nil {
		t.Fatal(err)
	}
	nowUTC = func() time.Time { return start.Add(3 * time.Hour) }
	if err := s.AddTeamMember(ctx, b.ID, "late", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	e := &UsageEvent{UserID: "late", CreatedAt: start}
	if err := s.InsertUsageEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, e.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != a.ID {
		t.Fatalf("late write team = %q, want earliest %q", got, a.ID)
	}
}

func TestTeamAttributionSetUserTeamsPreservesSources(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, uid := range []string{"target", "other"} {
		attributionUser(t, s, uid)
	}
	a, err := s.CreateTeam(ctx, "source-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "source-b", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"target", "other"} {
		if err := s.AddTeamMember(ctx, a.ID, uid, "member", "group", "directory"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserTeams(ctx, "target", []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserTeams(ctx, "target", nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE source_type='manual'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("manual sources leaked: %d", count)
	}
	for _, uid := range []string{"target", "other"} {
		if _, err := s.TeamRole(ctx, a.ID, uid); err != nil {
			t.Fatalf("group source removed: %v", err)
		}
	}
	if _, err := s.TeamRole(ctx, b.ID, "target"); err != ErrNotFound {
		t.Fatalf("manual source retained: %v", err)
	}
	if err := s.SetUserTeams(ctx, "target", []string{b.ID, "missing"}); err != ErrNotFound {
		t.Fatalf("invalid team accepted: %v", err)
	}
	if _, err := s.TeamRole(ctx, b.ID, "target"); err != ErrNotFound {
		t.Fatalf("partial add committed: %v", err)
	}
	if err := s.AddTeamMember(ctx, b.ID, "target", "leader", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserTeams(ctx, "target", []string{a.ID}); err != ErrLastTeamLeader {
		t.Fatalf("leader guard bypass: %v", err)
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE team_id=? AND source_type='manual'`, a.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed replacement did not roll back")
	}
}

func TestTeamAttributionPersonalAndExplicitSnapshots(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "snapshots")
	a, err := s.CreateTeam(ctx, "snapshot-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "snapshot-b", "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	oldNow := nowUTC
	nowUTC = func() time.Time { return base.Add(time.Hour) }
	defer func() { nowUTC = oldNow }()
	insert := func(at time.Time, team string) *UsageEvent {
		t.Helper()
		e := &UsageEvent{UserID: "snapshots", CreatedAt: at, TeamIDs: team}
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	check := func(e *UsageEvent, want string) {
		t.Helper()
		var got string
		if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, e.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("event %s: got %q want %q", e.ID, got, want)
		}
	}
	historical := insert(base, "")
	explicit := insert(base, "explicit,other")
	if err := s.AddTeamMember(ctx, a.ID, "snapshots", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	check(historical, a.ID)
	personal := insert(base.Add(2*time.Hour), "")
	check(personal, "")
	nowUTC = func() time.Time { return base.Add(3 * time.Hour) }
	if err := s.AddTeamMember(ctx, a.ID, "snapshots", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember(ctx, b.ID, "snapshots", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	check(personal, "")
	var transitions int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_attribution_transition WHERE user_id='snapshots'`).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if transitions != 1 {
		t.Fatalf("duplicate/second membership recorded transition: %d", transitions)
	}
	for _, id := range []string{a.ID, b.ID} {
		if err := s.RemoveTeamMemberSource(ctx, id, "snapshots", "manual", ""); err != nil {
			t.Fatal(err)
		}
	}
	nowUTC = func() time.Time { return base.Add(4 * time.Hour) }
	if err := s.AddTeamMember(ctx, b.ID, "snapshots", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	check(personal, b.ID)
	check(historical, a.ID)
	check(explicit, "explicit,other")
	check(insert(base, "explicit,other"), "explicit,other")
	check(insert(base.Add(2*time.Hour), ""), b.ID)
	check(insert(base.Add(5*time.Hour), ""), "")
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_attribution_transition WHERE user_id='snapshots'`).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if transitions != 2 {
		t.Fatalf("rejoin did not record transition: %d", transitions)
	}
}

func TestTeamAttributionConcurrentJoinAndUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "racing")
	team, err := s.CreateTeam(ctx, "race", "")
	if err != nil {
		t.Fatal(err)
	}
	e := &UsageEvent{UserID: "racing", CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- s.InsertUsageEvent(ctx, e) }()
	go func() { <-start; results <- s.AddTeamMember(ctx, team.ID, "racing", "member", "manual", "") }()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var got string
	if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, e.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != team.ID {
		t.Fatalf("concurrent write missed attribution: %q", got)
	}
}

func TestTeamAttributionUsageScopeUsesSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	attributionUser(t, s, "former")
	a, err := s.CreateTeam(ctx, "report-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "report-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember(ctx, a.ID, "former", "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{a.ID, b.ID, "", a.ID + "suffix", "prefix" + a.ID, b.ID + "," + a.ID} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{UserID: "former", TeamIDs: snapshot, CreatedAt: time.Now().UTC().Add(time.Hour), CostNano: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveTeamMemberSource(ctx, a.ID, "former", "manual", ""); err != nil {
		t.Fatal(err)
	}
	totals, err := s.AggregateUsage(ctx, UsageScope{TeamIDs: []string{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 2 || totals.CostNano != 2 {
		t.Fatalf("snapshot aggregate: %+v", totals)
	}
	_, count, err := s.ListRequests(ctx, RequestFilter{TeamIDs: []string{a.ID}})
	if err != nil || count != 2 {
		t.Fatalf("snapshot log: %d %v", count, err)
	}
	totals, err = s.AggregateUsage(ctx, UsageScope{TeamIDs: []string{a.ID, b.ID}})
	if err != nil || totals.Requests != 3 {
		t.Fatalf("union duplicates snapshots: %+v %v", totals, err)
	}
}

func TestTeamAttributionNewTeamListed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, "new", "")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.TeamByID(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !team.Listed || !loaded.Listed {
		t.Fatal("new teams must be listed by default")
	}
}
