package httpapi

import (
	"context"
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
)

func teamHarness(t *testing.T) *harness {
	h := newHarness(t)
	if err := h.store.UpdateUser(context.Background(), h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(h.server.resolvePrincipal)
	r.Route("/api/v1", func(r chi.Router) { r.Use(RequireUser); h.server.mountTeamRoutes(r) })
	h.handler = r
	return h
}
func TestTeamsHTTPMetadataAndCandidates(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Before", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + team.ID
	r := h.do("PATCH", base, map[string]any{"name": "After", "listed": true})
	if r.Code != 200 {
		t.Fatalf("metadata %d %s", r.Code, r.Body)
	}
	got, err := h.store.TeamByID(ctx, team.ID)
	if err != nil || got.Name != "After" || !got.Listed {
		t.Fatalf("metadata %#v %v", got, err)
	}
	r = h.do("GET", base+"/candidates?q=tester", nil)
	if r.Code != 200 {
		t.Fatalf("candidates %d %s", r.Code, r.Body)
	}
	users := decodeBody(t, r)["users"].([]any)
	if len(users) != 1 || len(users[0].(map[string]any)) != 3 {
		t.Fatalf("candidate leak %#v", users)
	}
	group, err := h.store.CreateGroup(ctx, "Mapped")
	if err != nil {
		t.Fatal(err)
	}
	r = h.do("PUT", base+"/groups", map[string]any{"group_ids": []string{group.ID}})
	if r.Code != 403 {
		t.Fatalf("leader mappings %d %s", r.Code, r.Body)
	}
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	r = h.do("PUT", base+"/groups", map[string]any{"group_ids": []string{group.ID}})
	if r.Code != 200 {
		t.Fatalf("admin mappings %d %s", r.Code, r.Body)
	}
	ids, err := h.store.TeamGroupIDs(ctx, team.ID)
	if err != nil || len(ids) != 1 || ids[0] != group.ID {
		t.Fatalf("mappings %v %v", ids, err)
	}
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "second", "second@example.com", "Second", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, other.ID, "leader", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "moderator"); err != nil {
		t.Fatal(err)
	}
	r = h.do("GET", base+"/candidates?q=tester", nil)
	if r.Code != 200 {
		t.Fatalf("mod candidates %d", r.Code)
	}
	r = h.do("PATCH", base, map[string]any{"listed": false})
	if r.Code != 403 {
		t.Fatalf("mod metadata %d", r.Code)
	}
	if err := h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "member"); err != nil {
		t.Fatal(err)
	}
	r = h.do("GET", base+"/candidates", nil)
	if r.Code != 403 {
		t.Fatalf("member candidates %d", r.Code)
	}
}
func TestTeamsHTTPMemberManagement(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Manage", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := h.store.UpsertUserFromIdentity(ctx, "target", "target@example.com", "Target", false, false)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + team.ID
	r := h.do("POST", base+"/members", map[string]any{"user_id": target.ID, "role": "moderator"})
	if r.Code != 200 {
		t.Fatalf("add %d %s", r.Code, r.Body)
	}
	r = h.do("PATCH", base+"/members/"+target.ID, map[string]any{"role": "member"})
	if r.Code != 200 {
		t.Fatalf("role %d %s", r.Code, r.Body)
	}
	group, err := h.store.CreateGroup(ctx, "Directory")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, target.ID, "member", "group", group.ID); err != nil {
		t.Fatal(err)
	}
	r = h.do("DELETE", base+"/members/"+target.ID, nil)
	if r.Code != 200 || decodeBody(t, r)["membership_retained"] != true {
		t.Fatalf("retained %d %s", r.Code, r.Body)
	}
	members, err := h.store.TeamMembers(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == target.ID && (len(m.Sources) != 1 || m.Sources[0].SourceType != "group") {
			t.Fatalf("sources %#v", m)
		}
	}
	r = h.do("DELETE", base+"/members/"+h.user.ID, nil)
	if r.Code != 409 {
		t.Fatalf("last lead %d %s", r.Code, r.Body)
	}
	ns, _, err := h.store.ListNotifications(ctx, target.ID, false, 50)
	if err != nil || len(ns) != 3 {
		t.Fatalf("notifications %d %v", len(ns), err)
	}
}
func TestTeamsHTTPModeratorLimits(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	lead, _, err := h.store.UpsertUserFromIdentity(ctx, "other-lead", "other@example.com", "Lead", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.store.CreateTeam(ctx, "Moderate", lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, "moderator", "manual", ""); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + team.ID
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/members", map[string]any{"user_id": lead.ID, "role": "leader"}},
		{"PATCH", "/members/" + lead.ID, map[string]any{"role": "member"}},
		{"DELETE", "/members/" + lead.ID, nil},
	} {
		r := h.do(tc.method, base+tc.path, tc.body)
		if r.Code != 403 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, r.Code, r.Body)
		}
	}
	// A previously cached moderator identity must not authorize after demotion.
	if err := h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "member"); err != nil {
		t.Fatal(err)
	}
	r := h.do("POST", base+"/members", map[string]any{"user_id": lead.ID})
	if r.Code != 403 {
		t.Fatalf("demoted add %d %s", r.Code, r.Body)
	}
	r = h.do("DELETE", base+"/members/"+h.user.ID, nil)
	if r.Code != 200 {
		t.Fatalf("self leave %d %s", r.Code, r.Body)
	}
}
func TestTeamsHTTPRequests(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	lead, _, err := h.store.UpsertUserFromIdentity(ctx, "req-lead", "req@example.com", "Lead", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.store.CreateTeam(ctx, "Join", lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE team SET listed=1 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + team.ID
	r := h.do("POST", base+"/join-requests", map[string]any{"reason": "Hello"})
	if r.Code != 200 {
		t.Fatalf("create %d %s", r.Code, r.Body)
	}
	req := decodeBody(t, r)["request"].(map[string]any)
	id := req["id"].(string)
	r = h.do("GET", "/api/v1/team-requests", nil)
	if r.Code != 200 || len(decodeBody(t, r)["requests"].([]any)) != 1 {
		t.Fatalf("mine %d %s", r.Code, r.Body)
	}
	r = h.do("DELETE", base+"/join-requests", nil)
	if r.Code != 200 {
		t.Fatalf("cancel %d %s", r.Code, r.Body)
	}
	r = h.do("POST", base+"/join-requests", map[string]any{"reason": "Again"})
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	_, token, err := h.store.CreateToken(ctx, lead.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	h.token = token
	r = h.do("POST", base+"/requests/"+id+"/decision", map[string]any{"approve": true, "reason": "welcome"})
	if r.Code != 200 {
		t.Fatalf("approve %d %s", r.Code, r.Body)
	}
}
func TestTeamsHTTPVisibility(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	leader, _, err := h.store.UpsertUserFromIdentity(ctx, "team-leader", "lead@example.com", "Lead", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.store.CreateTeam(ctx, "Hidden", leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE team SET listed=0 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	r := h.do("GET", "/api/v1/teams", nil)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if len(decodeBody(t, r)["teams"].([]any)) != 0 {
		t.Fatal("unlisted leaked")
	}
	r = h.do("GET", "/api/v1/teams/"+team.ID, nil)
	if r.Code != 404 {
		t.Fatalf("hidden detail %d %s", r.Code, r.Body)
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE team SET listed=1 WHERE id=?`, team.ID); err != nil {
		t.Fatal(err)
	}
	r = h.do("GET", "/api/v1/teams/"+team.ID, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("detail %d %s", r.Code, r.Body)
	}
	data := decodeBody(t, r)
	if _, ok := data["members"]; ok {
		t.Fatal("outsider sees roster")
	}
	if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	r = h.do("GET", "/api/v1/teams/"+team.ID, nil)
	data = decodeBody(t, r)
	if data["members"] == nil || data["requests"] != nil || data["my_role"] != "member" {
		t.Fatalf("member detail %#v", data)
	}
}
