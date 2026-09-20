package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func requestFixture(t *testing.T) (*Store, *Team, *User, *User) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	leader, _, err := s.UpsertUserFromIdentity(ctx, "leader", "leader@example.com", "Leader", false, false)
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := s.UpsertUserFromIdentity(ctx, "member", "member@example.com", "Member", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "Requests", leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE team SET listed=1 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	return s, team, leader, user
}
func TestTeamRequestKeepsApplicantAndDecisionReasons(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	r, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Building a report")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DecideTeamJoinRequest(ctx, team.ID, r.ID, leader.ID, false, "Wrong team"); err != nil {
		t.Fatal(err)
	}
	list, err := s.TeamRequestsForUser(ctx, user.ID)
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	data, _ := json.Marshal(list[0])
	var record map[string]any
	_ = json.Unmarshal(data, &record)
	if record["reason"] != "Building a report" || record["decision_reason"] != "Wrong team" {
		t.Fatalf("lost request or decision reason: %s", data)
	}
	if _, err = s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "New project"); err != nil {
		t.Fatal(err)
	}
	list, err = s.TeamRequestsForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(list[0])
	_ = json.Unmarshal(data, &record)
	if record["reason"] != "New project" || record["decision_reason"] != "" {
		t.Fatalf("reopened request retained old decision: %s", data)
	}
}

func TestTeamRequestReadableIdentity(t *testing.T) {
	s, team, _, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Please")
	if err != nil {
		t.Fatal(err)
	}
	for label, list := range map[string]func() ([]*TeamJoinRequest, error){
		"history": func() ([]*TeamJoinRequest, error) { return s.TeamRequestsForUser(ctx, user.ID) },
		"pending": func() ([]*TeamJoinRequest, error) { return s.PendingTeamRequests(ctx, team.ID) },
	} {
		t.Run(label, func(t *testing.T) {
			rows, err := list()
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			if rows[0].ID != req.ID || rows[0].Reason != "Please" || rows[0].Status != "pending" {
				t.Fatalf("request changed: %+v", rows[0])
			}
			raw, err := json.Marshal(rows[0])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"team_name": team.Name, "user_name": "Member", "user_email": "member@example.com"} {
				if fields[key] != want {
					t.Errorf("%s=%v want %q", key, fields[key], want)
				}
			}
		})
	}
}
func TestTeamRequestReadableNotifications(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Collaborate")
	if err != nil {
		t.Fatal(err)
	}
	notices, _, err := s.ListNotifications(ctx, leader.ID, false, 50)
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices=%v err=%v", notices, err)
	}
	if !strings.Contains(notices[0].Body, user.Email) || strings.Contains(notices[0].Body, user.ID) {
		t.Errorf("applicant identity unreadable: %q", notices[0].Body)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, true, "Welcome"); err != nil {
		t.Fatal(err)
	}
	notices, _, err = s.ListNotifications(ctx, user.ID, false, 50)
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices=%v err=%v", notices, err)
	}
	if !strings.Contains(notices[0].Body, team.Name) || strings.Contains(notices[0].Body, team.ID) {
		t.Errorf("team identity unreadable: %q", notices[0].Body)
	}
}
func TestTeamRequestReadableAuditDetails(t *testing.T) {
	s, team, _, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Please")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelTeamJoinRequest(ctx, team.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"team.request.created", "team.request.cancelled"} {
		entries, _, err := s.ListAudit(ctx, AuditFilter{Action: action})
		if err != nil || len(entries) != 1 {
			t.Fatalf("audit=%v err=%v", entries, err)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(entries[0].NewValue), &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload["detail"], team.Name) || strings.Contains(payload["detail"], req.ID) {
			t.Errorf("unreadable audit detail: %q", payload["detail"])
		}
	}
}
func TestTeamRequestTransitionGuards(t *testing.T) {
	for _, tc := range []string{"unlisted", "inactive", "archived", "member"} {
		t.Run(tc, func(t *testing.T) {
			s, team, _, user := requestFixture(t)
			ctx := context.Background()
			var want error
			switch tc {
			case "unlisted":
				if err := s.exec(ctx, `UPDATE team SET listed=0 WHERE id=?`, team.ID); err != nil {
					t.Fatal(err)
				}
				want = ErrTeamUnlisted
			case "inactive":
				if err := s.UpdateUser(ctx, user.ID, RoleUser, false); err != nil {
					t.Fatal(err)
				}
				want = ErrTeamForbidden
			case "archived":
				if err := s.exec(ctx, `UPDATE team SET archived_at=? WHERE id=?`, FormatTime(nowUTC()), team.ID); err != nil {
					t.Fatal(err)
				}
				want = ErrTeamArchived
			case "member":
				if err := s.AddTeamMember(ctx, team.ID, user.ID, "member", "manual", ""); err != nil {
					t.Fatal(err)
				}
				want = ErrTeamRequestState
			}
			if _, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, ""); err != want {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}
func TestTeamRequestDecisionRollback(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "join")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `CREATE TRIGGER reject_decision_notification BEFORE INSERT ON notification BEGIN SELECT RAISE(ABORT,'notification failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, true, ""); err == nil {
		t.Fatal("expected notification failure")
	}
	if _, err := s.TeamRole(ctx, team.ID, user.ID); err != ErrNotFound {
		t.Fatalf("membership committed: %v", err)
	}
	mine, err := s.TeamRequestsForUser(ctx, user.ID)
	if err != nil || len(mine) != 1 || mine[0].Status != "pending" {
		t.Fatalf("decision committed: %#v %v", mine, err)
	}
	_, n, err := s.ListAudit(ctx, AuditFilter{Action: "team.request.approved"})
	if err != nil || n != 0 {
		t.Fatalf("audit committed %d %v", n, err)
	}
}
func TestTeamRequestConcurrentDuplicate(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	type result struct {
		r   *TeamJoinRequest
		err error
	}
	ch := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func() { r, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "join"); ch <- result{r, err} }()
	}
	id := ""
	for i := 0; i < 8; i++ {
		v := <-ch
		if v.err != nil {
			t.Fatal(v.err)
		}
		if id != "" && id != v.r.ID {
			t.Fatalf("different ids %s %s", id, v.r.ID)
		}
		id = v.r.ID
	}
	ns, _, err := s.ListNotifications(ctx, leader.ID, false, 50)
	if err != nil || len(ns) != 1 {
		t.Fatalf("duplicate fanout %d %v", len(ns), err)
	}
}
func TestTeamRequestAuditIdentifiesTarget(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "join")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, true, "welcome"); err != nil {
		t.Fatal(err)
	}
	audit, _, err := s.ListAudit(ctx, AuditFilter{Action: "team.request.approved"})
	if err != nil || len(audit) != 1 {
		t.Fatalf("audit %v %v", audit, err)
	}
	if !strings.Contains(audit[0].NewValue, user.ID) {
		t.Fatalf("audit omits affected user: %q", audit[0].NewValue)
	}
}
func TestTeamRequestRollbackOnNotificationFailure(t *testing.T) {
	s, team, _, user := requestFixture(t)
	ctx := context.Background()
	if err := s.exec(ctx, `CREATE TRIGGER reject_team_notification BEFORE INSERT ON notification BEGIN SELECT RAISE(ABORT,'notification failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "join"); err == nil {
		t.Fatal("expected notification failure")
	}
	mine, err := s.TeamRequestsForUser(ctx, user.ID)
	if err != nil || len(mine) != 0 {
		t.Fatalf("request committed despite failed notification: %v %v", mine, err)
	}
	_, n, err := s.ListAudit(ctx, AuditFilter{Action: "team.request.created"})
	if err != nil || n != 0 {
		t.Fatalf("audit committed %d %v", n, err)
	}
}
func TestTeamRequestLifecycle(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	req, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "join")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelTeamJoinRequest(ctx, team.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	mine, err := s.TeamRequestsForUser(ctx, user.ID)
	if err != nil || len(mine) != 1 || mine[0].Status != "cancelled" || mine[0].ID != req.ID {
		t.Fatalf("cancel: %v %v", mine, err)
	}
	req, err = s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "again")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, false, "no"); err != nil {
		t.Fatal(err)
	}
	req, err = s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "again")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE team SET listed=0 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, true, ""); err != ErrTeamUnlisted {
		t.Fatalf("unlisted approve: %v", err)
	}
	if err := s.exec(ctx, `UPDATE team SET listed=1 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, user.ID, true, ""); err != ErrTeamForbidden {
		t.Fatalf("self approve: %v", err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, true, "welcome"); err != nil {
		t.Fatal(err)
	}
	role, err := s.TeamRole(ctx, team.ID, user.ID)
	if err != nil || role != "member" {
		t.Fatalf("role %s %v", role, err)
	}
	if err := s.DecideTeamJoinRequest(ctx, team.ID, req.ID, leader.ID, false, ""); err != ErrTeamRequestState {
		t.Fatalf("repeat decision: %v", err)
	}
	ns, _, err := s.ListNotifications(ctx, user.ID, false, 50)
	if err != nil || len(ns) != 2 {
		t.Fatalf("decision notifications: %v %v", ns, err)
	}
}
func TestTeamRequestPendingIdempotent(t *testing.T) {
	s, team, leader, user := requestFixture(t)
	ctx := context.Background()
	a, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Please")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeamJoinRequest(ctx, team.ID, user.ID, "Again")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || b.Status != "pending" {
		t.Fatalf("duplicate: %#v %#v", a, b)
	}
	ns, _, err := s.ListNotifications(ctx, leader.ID, false, 50)
	if err != nil || len(ns) != 1 {
		t.Fatalf("notifications=%v err=%v", ns, err)
	}
}
