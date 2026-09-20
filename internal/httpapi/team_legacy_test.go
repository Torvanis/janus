package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

func TestTeamLegacyAdminRosterRollback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Before", h.user.ID)
	legacyCheck(t, err)
	lead, _, err := h.store.UpsertUserFromIdentity(ctx, "rollback-lead", "rollback@example.com", "Rollback", false, false)
	legacyCheck(t, err)
	rec := h.do(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, map[string]any{"name": "After", "listed": false, "lead_user_id": lead.ID, "lead_can_edit_quotas": true, "member_ids": []string{h.user.ID, "missing-user"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("invalid roster: %d %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.TeamByID(ctx, team.ID)
	legacyCheck(t, err)
	if got.Name != "Before" || got.LeadCanEditQuotas || !got.Listed || got.LeadUserID != h.user.ID {
		t.Fatalf("partial metadata update: %+v", got)
	}
	if _, err := h.store.TeamRole(ctx, team.ID, lead.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed patch left a new manual leader: %v", err)
	}
	_, total, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "team_updated", ResourceType: "team", Limit: 50})
	legacyCheck(t, err)
	if total != 0 {
		t.Fatalf("failed patch committed %d audit events", total)
	}
}

func TestTeamLegacyUserRollback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user, _, err := h.store.UpsertUserFromIdentity(ctx, "legacy-user", "legacy@example.com", "Legacy", false, false)
	legacyCheck(t, err)
	rec := h.do(http.MethodPatch, "/api/v1/admin/users/"+user.ID, map[string]any{"role": store.RoleTeamLead, "is_active": false, "team_ids": []string{"missing-team"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad team: %d %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.UserByID(ctx, user.ID)
	legacyCheck(t, err)
	if got.Role != user.Role || got.IsActive != user.IsActive {
		t.Fatalf("partial user update: %+v", got)
	}
}

type legacyBeforeRead struct {
	io.Reader
	before func()
}

func (r *legacyBeforeRead) Read(p []byte) (int, error) {
	if r.before != nil {
		f := r.before
		r.before = nil
		f()
	}
	return r.Reader.Read(p)
}

func TestTeamLegacyLeadAuthorizationIsFresh(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Roster", h.user.ID)
	legacyCheck(t, err)
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "other-leader", "other@example.com", "Other", false, false)
	legacyCheck(t, err)
	legacyCheck(t, h.store.AddTeamMember(ctx, team.ID, other.ID, "leader", "manual", ""))
	payload := []byte(`{"member_ids":["` + h.user.ID + `","` + other.ID + `"]}`)
	body := &legacyBeforeRead{Reader: bytes.NewReader(payload), before: func() {
		legacyCheck(t, h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true))
		legacyCheck(t, h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "member"))
	}}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/members", body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked actor authorized: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTeamLegacyLeadLastLeaderRollback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Roster", h.user.ID)
	legacyCheck(t, err)
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "other-member", "other@example.com", "Other", false, false)
	legacyCheck(t, err)
	rec := h.do(http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/members", map[string]any{"member_ids": []string{other.ID}})
	if rec.Code != http.StatusConflict {
		t.Errorf("last leader: %d %s", rec.Code, rec.Body.String())
	}
	ids, err := h.store.TeamMemberIDs(ctx, team.ID)
	legacyCheck(t, err)
	if len(ids) != 1 || ids[0] != h.user.ID {
		t.Fatalf("partial roster: %v", ids)
	}
}

func TestTeamLegacyAdminNameAndListed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Named", h.user.ID)
	legacyCheck(t, err)
	rec := h.do(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, map[string]any{"name": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("blank name: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, map[string]any{"listed": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("listed patch: %d %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.TeamByID(ctx, team.ID)
	legacyCheck(t, err)
	if got.Name != "Named" || got.Listed {
		t.Fatalf("invalid profile: %+v", got)
	}
}

func TestTeamLegacyChangedLeadPreservesOtherLeaders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Leaders", h.user.ID)
	legacyCheck(t, err)
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "new-leader", "new@example.com", "New", false, false)
	legacyCheck(t, err)
	rec := h.do(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, map[string]any{"lead_user_id": other.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("select leader: %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{h.user.ID, other.ID} {
		role, err := h.store.TeamRole(ctx, team.ID, id)
		legacyCheck(t, err)
		if role != "leader" {
			t.Fatalf("leader %s demoted to %s", id, role)
		}
	}
}

func TestTeamLegacyLeadForeignTeamForbidden(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "foreign-leader", "foreign@example.com", "Foreign", false, false)
	legacyCheck(t, err)
	own, err := h.store.CreateTeam(ctx, "Own", other.ID)
	legacyCheck(t, err)
	foreign, err := h.store.CreateTeam(ctx, "Foreign", h.user.ID)
	legacyCheck(t, err)
	session, err := h.server.Sessions.Create(ctx, other.ID)
	legacyCheck(t, err)
	rec := h.doAsSession(session, http.MethodPut, "/api/v1/lead/teams/"+foreign.ID+"/members", map[string]any{"member_ids": []string{h.user.ID, other.ID}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign edit: %d %s", rec.Code, rec.Body.String())
	}
	ids, err := h.store.TeamMemberIDs(ctx, foreign.ID)
	legacyCheck(t, err)
	if len(ids) != 1 || ids[0] != h.user.ID {
		t.Fatalf("foreign roster changed: %v", ids)
	}
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/lead/teams/"+own.ID+"/members", map[string]any{"member_ids": []string{other.ID, h.user.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("own edit: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTeamLegacyAdminAuthorizationIsFresh(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Before", h.user.ID)
	legacyCheck(t, err)
	body := &legacyBeforeRead{Reader: bytes.NewBufferString(`{"name":"After"}`), before: func() { legacyCheck(t, h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true)) }}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, body)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked admin: %d %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.TeamByID(ctx, team.ID)
	legacyCheck(t, err)
	if got.Name != "Before" {
		t.Fatalf("revoked admin changed metadata: %+v", got)
	}
}

func TestTeamLegacyInactiveAdditionRollsBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user, _, err := h.store.UpsertUserFromIdentity(ctx, "inactive-addition", "inactive@example.com", "Inactive", false, false)
	legacyCheck(t, err)
	team, err := h.store.CreateTeam(ctx, "Active team", h.user.ID)
	legacyCheck(t, err)
	rec := h.do(http.MethodPatch, "/api/v1/admin/users/"+user.ID, map[string]any{"role": store.RoleTeamLead, "is_active": false, "team_ids": []string{team.ID}})
	if rec.Code < 400 {
		t.Fatalf("disabled user addition accepted: %d", rec.Code)
	}
	got, err := h.store.UserByID(ctx, user.ID)
	legacyCheck(t, err)
	if got.Role != user.Role || got.IsActive != user.IsActive {
		t.Fatalf("partial inactive update: %+v", got)
	}
	if _, err := h.store.TeamRole(ctx, team.ID, user.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inactive membership added: %v", err)
	}
}

func legacyCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTeamLegacyMetadataPreservesDirectoryLeader(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Directory", "")
	legacyCheck(t, err)
	group, err := h.store.EnsureGroup(ctx, "directory-leaders", true)
	legacyCheck(t, err)
	legacyCheck(t, h.store.SetTeamGroupMappings(ctx, team.ID, []string{group.ID}))
	legacyCheck(t, h.store.ReplaceIDPGroupsForUser(ctx, h.user.ID, []string{group.Name}))
	legacyCheck(t, h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "leader"))
	rec := h.do(http.MethodPatch, "/api/v1/admin/teams/"+team.ID, map[string]any{"lead_can_edit_quotas": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	members, err := h.store.TeamMembers(ctx, team.ID)
	legacyCheck(t, err)
	if len(members) != 1 || len(members[0].Sources) != 1 || members[0].Sources[0].SourceType != "group" {
		t.Fatalf("metadata edit added a membership source: %+v", members)
	}
	entries, total, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "team_updated", ResourceType: "team", Limit: 50})
	legacyCheck(t, err)
	if total != 1 || len(entries) != 1 || !strings.Contains(entries[0].NewValue, "members=1 leaders=1 manual_sources=0 group_sources=1") || strings.Contains(entries[0].NewValue, h.user.Email) {
		t.Fatalf("missing/redaction-unsafe source audit: %+v", entries)
	}
	legacyCheck(t, h.store.ReplaceIDPGroupsForUser(ctx, h.user.ID, nil))
	if _, err := h.store.TeamRole(ctx, team.ID, h.user.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("directory removal retained access: %v", err)
	}
}
