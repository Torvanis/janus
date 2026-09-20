package httpapi

import (
	"context"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

func TestTeamUXMembershipAndPendingPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, membership, actor string
		admin, manage           bool
	}{
		{"nonmember", "", "", false, false},
		{"member", "member", "member", false, false},
		{"moderator", "moderator", "moderator", false, true},
		{"leader", "leader", "leader", false, true},
		{"admin member", "member", "admin", true, true},
		{"admin nonmember", "", "admin", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := teamHarness(t)
			ctx := context.Background()
			lead, _, err := h.store.UpsertUserFromIdentity(ctx, "ux-lead", "ux-lead@example.com", "Lead", false, false)
			if err != nil {
				t.Fatal(err)
			}
			team, err := h.store.CreateTeam(ctx, "UX permissions", lead.ID)
			if err != nil {
				t.Fatal(err)
			}
			base := "/api/v1/teams/" + team.ID
			r := h.do("POST", base+"/join-requests", map[string]any{"reason": "private reason"})
			if r.Code != 200 {
				t.Fatalf("request: %d %s", r.Code, r.Body)
			}
			if tc.membership != "" {
				if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, tc.membership, "manual", ""); err != nil {
					t.Fatal(err)
				}
			}
			if tc.admin {
				if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"/api/v1/teams", base} {
				r = h.do("GET", path, nil)
				if r.Code != 200 {
					t.Fatalf("%s: %d %s", path, r.Code, r.Body)
				}
				got := decodeBody(t, r)
				if path == "/api/v1/teams" {
					got = got["teams"].([]any)[0].(map[string]any)
					if role, _ := got["my_role"].(string); role != tc.membership {
						t.Fatalf("directory membership: %#v", got)
					}
				} else {
					if got["membership_role"] != tc.membership || got["actor_role"] != tc.actor || got["my_role"] != tc.actor {
						t.Fatalf("detail roles: %#v", got)
					}
					if tc.actor == "" {
						if _, ok := got["members"]; ok {
							t.Fatalf("members leaked: %#v", got)
						}
					}
				}
				if got["can_manage"] != tc.manage {
					t.Fatalf("management: %#v", got)
				}
				count, exposed := got["pending_request_count"]
				if exposed != tc.manage || (tc.manage && count != float64(1)) {
					t.Fatalf("pending count: %#v", got)
				}
				if !tc.manage {
					for _, key := range []string{"requests", "group_ids"} {
						if _, ok := got[key]; ok {
							t.Fatalf("%s leaked: %#v", key, got)
						}
					}
				}
			}
		})
	}
}

func TestTeamUXPendingPermissionsRefreshAfterDemotion(t *testing.T) {
	h := teamHarness(t)
	ctx := context.Background()
	lead, _, err := h.store.UpsertUserFromIdentity(ctx, "ux-refresh-lead", "refresh@example.com", "Lead", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := h.store.CreateTeam(ctx, "Refresh", lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, "moderator", "manual", ""); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + team.ID
	check := func(manage bool, role string) {
		t.Helper()
		for _, path := range []string{"/api/v1/teams", base} {
			r := h.do("GET", path, nil)
			if r.Code != 200 {
				t.Fatalf("read: %d %s", r.Code, r.Body)
			}
			got := decodeBody(t, r)
			if path == "/api/v1/teams" {
				got = got["teams"].([]any)[0].(map[string]any)
			} else if got["actor_role"] != role {
				t.Fatalf("stale actor: %#v", got)
			}
			count, present := got["pending_request_count"]
			if got["can_manage"] != manage || present != manage || (manage && count != float64(0)) {
				t.Fatalf("stale permission or missing zero: %#v", got)
			}
			if !manage {
				if _, present := got["requests"]; present {
					t.Fatalf("requests leaked: %#v", got)
				}
			}
		}
	}
	check(true, "moderator")
	if err := h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "member"); err != nil {
		t.Fatal(err)
	}
	check(false, "member")
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	check(true, "admin")
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	check(false, "member")
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/teams", base} {
		r := h.do("GET", path, nil)
		if r.Code != 401 && r.Code != 403 {
			t.Fatalf("inactive read: %d %s", r.Code, r.Body)
		}
	}
}

func TestTeamUXDirectoryManagerZeroPending(t *testing.T) {
	h := teamHarness(t)
	team, err := h.store.CreateTeam(context.Background(), "UX directory", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := h.do("GET", "/api/v1/teams", nil)
	if r.Code != 200 {
		t.Fatalf("browse: %d %s", r.Code, r.Body)
	}
	teams := decodeBody(t, r)["teams"].([]any)
	if len(teams) != 1 {
		t.Fatalf("teams: %#v", teams)
	}
	got := teams[0].(map[string]any)
	if got["id"] != team.ID || got["my_role"] != "leader" {
		t.Fatalf("legacy fields: %#v", got)
	}
	if got["can_manage"] != true || got["pending_request_count"] != float64(0) {
		t.Fatalf("manager UX fields: %#v", got)
	}
}
