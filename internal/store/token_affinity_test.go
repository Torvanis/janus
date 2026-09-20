package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestChangeTokenTeamHistoryAndAudit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "owner", "owner@example.com", "Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "A", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "B", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, plaintext, err := s.CreateTeamToken(ctx, user.ID, "key", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateTeamToken(ctx, user.ID, "other", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id string) *UsageEvent {
		t.Helper()
		e := &UsageEvent{UserID: user.ID, TokenID: id, TeamIDs: a.ID, CreatedAt: time.Now().UTC(), TokensIn: 17}
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := insert(token.ID)
	untouched := insert(other.ID)
	if _, n, err := s.ChangeTokenTeam(ctx, token.ID, user.ID, b.ID, false); err != nil || n != 0 {
		t.Fatalf("future-only: n=%d err=%v", n, err)
	}
	check := func(id, want string) {
		t.Helper()
		var got string
		if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("event %s team=%q want=%q", id, got, want)
		}
	}
	check(first.ID, a.ID)
	// Same binding plus moveHistory is useful after an earlier future-only move.
	if _, n, err := s.ChangeTokenTeam(ctx, token.ID, user.ID, b.ID, true); err != nil || n != 1 {
		t.Fatalf("history: n=%d err=%v", n, err)
	}
	check(first.ID, b.ID)
	check(untouched.ID, a.ID)
	late := insert(token.ID)
	check(late.ID, a.ID)
	if _, n, err := s.ChangeTokenTeam(ctx, token.ID, user.ID, "", true); err != nil || n != 2 {
		t.Fatalf("personal: n=%d err=%v", n, err)
	}
	check(first.ID, "")
	check(late.ID, "")
	check(untouched.ID, a.ID)
	if got, err := s.TokenByDigest(ctx, HashToken(plaintext)); err != nil || got.TeamID != "" {
		t.Fatalf("credential changed: %+v %v", got, err)
	}
	audits, total, err := s.ListAudit(ctx, AuditFilter{Action: "token.team_changed"})
	if err != nil || total != 3 || len(audits) != 3 {
		t.Fatalf("audit count=%d err=%v", total, err)
	}
}

func TestChangeTokenTeamRejectsInvalidContext(t *testing.T) {
	for _, kind := range []string{"other_owner", "revoked", "inactive", "nonmember", "archived", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			u, _, err := s.UpsertUserFromIdentity(ctx, "owner", "owner@example.com", "Owner", false, false)
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.CreateTeam(ctx, "A", u.ID)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.CreateTeam(ctx, "B", u.ID)
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := s.CreateTeamToken(ctx, u.ID, "key", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			owner, target, id := u.ID, b.ID, token.ID
			switch kind {
			case "other_owner":
				owner = "someone-else"
			case "revoked":
				err = s.RevokeToken(ctx, id)
			case "inactive":
				err = s.exec(ctx, `UPDATE app_user SET is_active=0 WHERE id=?`, u.ID)
				target = ""
			case "nonmember":
				err = s.exec(ctx, `DELETE FROM team_member WHERE team_id=? AND user_id=?`, b.ID, u.ID)
			case "archived":
				err = s.exec(ctx, `UPDATE team SET archived_at=? WHERE id=?`, FormatTime(time.Now()), b.ID)
			case "missing":
				id = "missing"
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, n, err := s.ChangeTokenTeam(ctx, id, owner, target, true); err == nil || got != nil || n != 0 {
				t.Fatalf("invalid change: %+v %d %v", got, n, err)
			}
			got, err := s.TokenByID(ctx, token.ID)
			if err != nil || got.TeamID != a.ID {
				t.Fatalf("binding changed: %+v %v", got, err)
			}
			_, n, err := s.ListAudit(ctx, AuditFilter{Action: "token.team_changed"})
			if err != nil || n != 0 {
				t.Fatalf("audit after rejection: %d %v", n, err)
			}
		})
	}
}

func TestChangeTokenTeamAuditFailureRollsBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "owner", "owner@example.com", "Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "A", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateTeamToken(ctx, u.ID, "key", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := &UsageEvent{UserID: u.ID, TokenID: token.ID, TeamIDs: a.ID}
	if err := s.InsertUsageEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `CREATE TRIGGER reject_affinity_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if got, n, err := s.ChangeTokenTeam(ctx, token.ID, u.ID, "", true); err == nil || got != nil || n != 0 {
		t.Fatalf("expected rollback: %+v %d %v", got, n, err)
	}
	got, err := s.TokenByID(ctx, token.ID)
	if err != nil || got.TeamID != a.ID {
		t.Fatalf("binding rollback: %+v %v", got, err)
	}
	var team string
	if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, ev.ID).Scan(&team); err != nil || team != a.ID {
		t.Fatalf("history rollback: %s %v", team, err)
	}
}

func TestChangeTokenTeamConcurrentMovesStayConsistent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "owner", "owner@example.com", "Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "A", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "B", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateTeamToken(ctx, u.ID, "key", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := &UsageEvent{UserID: u.ID, TokenID: token.ID, TeamIDs: a.ID}
	if err := s.InsertUsageEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		target := a.ID
		if i%2 == 0 {
			target = b.ID
		}
		go func() { _, _, err := s.ChangeTokenTeam(ctx, token.ID, u.ID, target, true); errors <- err }()
	}
	for i := 0; i < 12; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TokenByID(ctx, token.ID)
	if err != nil {
		t.Fatal(err)
	}
	var team string
	if err := s.queryRow(ctx, `SELECT team_ids FROM usage_event WHERE id=?`, ev.ID).Scan(&team); err != nil || team != got.TeamID {
		t.Fatalf("binding/history disagree: %s %+v %v", team, got, err)
	}
	_, n, err := s.ListAudit(ctx, AuditFilter{Action: "token.team_changed"})
	if err != nil || n != 12 {
		t.Fatalf("concurrent audits: %d %v", n, err)
	}
}

func TestSumTeamMetricSinceFailsClosedAfterActiveWindowPurge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cutoff := start.AddDate(0, 0, 7)
	for _, at := range []time.Time{start, cutoff} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{TeamIDs: "team_1", ModelID: "model_1", CreatedAt: at, TokensIn: 7, TokensOut: 3, CostNano: 11}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.SumTeamMetricSince(ctx, "team_1", "", MetricTokensIn, start); err != nil || got != 14 {
		t.Fatalf("before purge: total=%d err=%v", got, err)
	}
	if n, err := s.PurgeUsageEvents(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("active-window purge: n=%d err=%v", n, err)
	}
	checkMissingCoverage := func() {
		t.Helper()
		for _, metric := range []string{MetricTokensIn, MetricTokensOut, MetricCostUSD, MetricRequests} {
			for _, model := range []string{"", "model_1", "no-events"} {
				for _, team := range []string{"team_1", "no-events"} {
					got, err := s.SumTeamMetricSince(ctx, team, model, metric, start)
					if err == nil || !strings.Contains(err.Error(), "missing quota usage coverage") || got != 0 {
						t.Errorf("incomplete %s/%s/%s: total=%d err=%v; want missing quota usage coverage", team, model, metric, got, err)
					}
				}
			}
		}
	}
	checkMissingCoverage()
	// Raising retention moves the next purge cutoff backwards, but cannot
	// restore deleted usage or make the existing active window enforceable.
	if n, err := s.PurgeUsageEvents(ctx, start.AddDate(0, 0, -31)); err != nil || n != 0 {
		t.Fatalf("raised retention purge: n=%d err=%v", n, err)
	}
	checkMissingCoverage()
	for _, tc := range []struct {
		since time.Time
		want  int64
	}{{cutoff, 7}, {cutoff.Add(time.Nanosecond), 0}} {
		if got, err := s.SumTeamMetricSince(ctx, "team_1", "model_1", MetricTokensIn, tc.since); err != nil || got != tc.want {
			t.Fatalf("covered window %s: total=%d err=%v want=%d", tc.since, got, err, tc.want)
		}
	}
}

func TestSumTeamMetricSinceUsesExactSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, snapshot := range []string{"team_1", "teamX1", "prefixteam_1", "team_1suffix", "other,team_1,team_1", ""} {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{TeamIDs: snapshot, CreatedAt: now, TokensIn: 7}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SumTeamMetricSince(ctx, "team_1", "", MetricTokensIn, now.Add(-time.Second))
	if err != nil || got != 14 {
		t.Fatalf("exact snapshot total=%d %v", got, err)
	}
}

func TestChangeTokenTeamPersonalKeepsCredential(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "affinity-owner", "owner@example.com", "Owner", false, false)
	if err != nil {
		t.Fatal(err)
	}
	token, plaintext, err := s.CreateToken(ctx, user.ID, "unchanged")
	if err != nil {
		t.Fatal(err)
	}
	changed, moved, err := s.ChangeTokenTeam(ctx, token.ID, user.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.TokenByDigest(ctx, HashToken(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 || changed.ID != token.ID || resolved.ID != token.ID || resolved.Description != "unchanged" || resolved.TeamID != "" {
		t.Fatalf("unexpected change: %+v moved=%d", resolved, moved)
	}
}
