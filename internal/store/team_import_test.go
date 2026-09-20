package store

import (
	"context"
	"strings"
	"testing"
)

func TestTeamImportInvalidIdentitiesAndRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, identity := range []struct{ id, email string }{{"lead", "lead@example.com"}, {"inactive", "inactive@example.com"}, {"ambiguous1", "ambiguous@example.com"}, {"ambiguous2", "AMBIGUOUS@example.com"}} {
		if _, _, err := s.UpsertUserFromIdentity(ctx, identity.id, identity.email, identity.id, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.exec(ctx, `UPDATE app_user SET is_active = 0 WHERE email = ?`, "inactive@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"missing@example.com", "inactive@example.com", "ambiguous@example.com"} {
		for _, admin := range []bool{false, true} {
			row := "Bad,lead@example.com," + email + "\n"
			if admin {
				row = "Bad," + email + ",\n"
			}
			text := "Team_name,Team_admin,Additional_members\nGood,lead@example.com,\n" + row
			p, err := s.PreviewTeamImport(ctx, text)
			if err != nil || p.Valid || len(p.Rows[1].Errors) == 0 {
				t.Fatalf("accepted %s: %+v %v", email, p, err)
			}
			if teams, err := s.ImportTeamsCSV(ctx, text); err == nil || teams != nil {
				t.Fatalf("invalid import: %v %v", teams, err)
			}
			teams, err := s.ListTeams(ctx)
			if err != nil || len(teams) != 0 {
				t.Fatalf("partial import: %v %v", teams, err)
			}
		}
	}
	// A database failure on a later row must roll back earlier successful writes.
	if err := s.exec(ctx, `CREATE TRIGGER fail_import BEFORE INSERT ON team WHEN NEW.name = 'Fail' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	text := "Team_name,Team_admin,Additional_members\nGood,lead@example.com,\nFail,lead@example.com,\n"
	if teams, err := s.ImportTeamsCSV(ctx, text); err == nil || teams != nil {
		t.Fatalf("failure not propagated: %v %v", teams, err)
	}
	teams, err := s.ListTeams(ctx)
	if err != nil || len(teams) != 0 {
		t.Fatalf("transaction not rolled back: %v %v", teams, err)
	}
}

func TestTeamImportPreviewAndApply(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	lead, _, err := s.UpsertUserFromIdentity(ctx, "import-lead", "Lead@example.com", "Lead", false, false)
	if err != nil {
		t.Fatal(err)
	}
	member, _, err := s.UpsertUserFromIdentity(ctx, "import-member", "member@example.com", "Member", false, false)
	if err != nil {
		t.Fatal(err)
	}
	text := "Team_name,Team_admin,Additional_members\nPlatform,LEAD@example.com,member@example.com\n"
	p, err := s.PreviewTeamImport(ctx, text)
	if err != nil || !p.Valid {
		t.Fatalf("preview: %+v %v", p, err)
	}
	before, err := s.ListTeams(ctx)
	if err != nil || len(before) != 0 {
		t.Fatalf("preview wrote teams: %v %v", before, err)
	}
	teams, err := s.ImportTeamsCSV(ctx, text)
	if err != nil || len(teams) != 1 {
		t.Fatalf("import: %v %v", teams, err)
	}
	if teams[0].LeadUserID != lead.ID {
		t.Fatal("wrong leader")
	}
	ids, err := s.TeamMemberIDs(ctx, teams[0].ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("members: %v %v (expected %s)", ids, err, member.ID)
	}
	if _, err := s.ImportTeamsCSV(ctx, text); err == nil {
		t.Fatal("retry created duplicate")
	}
	p, err = s.PreviewTeamImport(ctx, strings.Replace(text, "Platform", "PLATFORM", 1))
	if err != nil || p.Valid {
		t.Fatalf("existing name accepted: %+v %v", p, err)
	}
}

func TestParseTeamImportRejectsInvalid(t *testing.T) {
	for _, text := range []string{
		"team_name,Team_admin,Additional_members\nA,a@example.com,\n",
		"Team_name,Team_admin,Additional_members,extra\nA,a@example.com,,x\n",
		"Team_name,Team_admin,Additional_members\nA,a@example.com,,x\n",
		"Team_name,Team_admin,Additional_members\n",
		strings.Repeat("x", MaxTeamImportBytes+1),
		"Team_name,Team_admin,Additional_members\n" + strings.Repeat("A,a@example.com,\n", MaxTeamImportRows+1),
	} {
		if _, err := parseTeamImport(text); err == nil {
			t.Fatalf("accepted invalid CSV of %d bytes", len(text))
		}
	}
	for _, text := range []string{
		"Team_name,Team_admin,Additional_members\n,a@example.com,\n",
		"Team_name,Team_admin,Additional_members\nA,,\n",
		"Team_name,Team_admin,Additional_members\nA,a@example.com,\na,a@example.com,\n",
	} {
		p, err := parseTeamImport(text)
		if err != nil || p.Valid {
			t.Fatalf("expected invalid rows: %+v, %v", p, err)
		}
	}
}

func TestParseTeamImport(t *testing.T) {
	p, err := parseTeamImport("Team_name,Team_admin,Additional_members\r\n\"Platform, Core\", LEAD@example.com ,member@example.com;MEMBER@example.com;lead@example.com\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Valid || len(p.Rows) != 1 {
		t.Fatalf("preview: %+v", p)
	}
	r := p.Rows[0]
	if r.Row != 2 || r.TeamName != "Platform, Core" || r.TeamAdmin != "lead@example.com" || len(r.AdditionalMembers) != 1 || len(r.Errors) != 0 {
		t.Fatalf("row: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "duplicate") {
		t.Fatalf("dedup not explained: %+v", r)
	}
}
