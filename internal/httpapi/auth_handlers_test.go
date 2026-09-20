package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// signIn drives completeSignIn exactly as both the OIDC callback and the
// dev-auth login do, fails the test on a login-error redirect, and returns the
// stored user.
func signIn(t *testing.T, h *harness, identity *auth.Identity) *store.User {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/start", nil)
	h.server.completeSignIn(rec, req, identity, "/dashboard")
	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in status = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "/auth/login?error=") {
		t.Fatalf("sign-in failed with redirect %q", loc)
	}
	user, err := h.store.UserByAuthID(context.Background(), identity.Subject)
	if err != nil {
		t.Fatalf("load user after sign-in: %v", err)
	}
	return user
}

// roleChangeAudits returns every group-driven role-change entry for one user.
func roleChangeAudits(t *testing.T, h *harness, userID string) []*store.AuditEntry {
	t.Helper()
	entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Action: "user_role_changed"})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	out := []*store.AuditEntry{}
	for _, e := range entries {
		if e.ResourceID == userID {
			out = append(out, e)
		}
	}
	return out
}

func decodeAuditValue(t *testing.T, raw string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode audit value %q: %v", raw, err)
	}
	return m
}

// assertRoleChange checks the who/what/why shape of one group-driven entry.
func assertRoleChange(t *testing.T, e *store.AuditEntry, userID, oldRole, newRole string) {
	t.Helper()
	if e.ActorUserID != "system" || e.ActorLabel != "system" {
		t.Errorf("actor = %q/%q, want system/system", e.ActorUserID, e.ActorLabel)
	}
	if e.ResourceType != "user" || e.ResourceID != userID {
		t.Errorf("resource = %s/%s, want user/%s", e.ResourceType, e.ResourceID, userID)
	}
	if got := decodeAuditValue(t, e.OldValue)["role"]; got != oldRole {
		t.Errorf("old role = %v, want %q", got, oldRole)
	}
	newVal := decodeAuditValue(t, e.NewValue)
	if got := newVal["role"]; got != newRole {
		t.Errorf("new role = %v, want %q", got, newRole)
	}
	if got := newVal["reason"]; got != "admin_group_membership" {
		t.Errorf("reason = %v, want admin_group_membership", got)
	}
}

// TestSignInAdminGroupMembership locks the three-avenue admin contract:
// JANUS_ADMIN_GROUPS membership is a third, per-sign-in-evaluated admin
// source that composes with (and never disturbs) the bootstrap email list and
// explicit admin-UI grants. Group-driven capability changes on an existing
// account are audited with a system actor; first-login promotions ride the
// existing user_created entry.
func TestSignInAdminGroupMembership(t *testing.T) {
	t.Run("promotion via group on first login", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = []string{"admins", "devops"}

		// Mixed case from the IdP must match the lowercased config list.
		user := signIn(t, h, &auth.Identity{Subject: "sub-alice", Email: "alice@example.com", Name: "Alice", Groups: []string{"Engineering", "DevOps"}})
		if !user.AdminViaGroup || !user.IsAdmin() {
			t.Fatalf("admin_via_group = %v, IsAdmin = %v, want true/true", user.AdminViaGroup, user.IsAdmin())
		}
		if user.Role != store.RoleUser {
			t.Errorf("stored role = %q, want %q (group signal must not touch the sticky role column)", user.Role, store.RoleUser)
		}

		// First login: no separate role-change entry — user_created records
		// the effective capability the account arrived with.
		if got := roleChangeAudits(t, h, user.ID); len(got) != 0 {
			t.Fatalf("role-change audit entries on first login = %d, want 0", len(got))
		}
		created, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Action: "user_created"})
		if err != nil {
			t.Fatalf("list user_created audit: %v", err)
		}
		var entry *store.AuditEntry
		for _, e := range created {
			if e.ResourceID == user.ID {
				entry = e
			}
		}
		if entry == nil {
			t.Fatal("no user_created audit entry for the new user")
		}
		val := decodeAuditValue(t, entry.NewValue)
		if val["role"] != store.RoleAdmin || val["admin_via_group"] != true {
			t.Errorf("user_created new_value = %v, want role=admin admin_via_group=true", val)
		}
	})

	t.Run("promotion via group on subsequent login", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = []string{"admins"}
		id := &auth.Identity{Subject: "sub-bob", Email: "bob@example.com", Name: "Bob"}

		first := signIn(t, h, id) // not in the group yet
		if first.IsAdmin() {
			t.Fatalf("user is admin before joining the group")
		}

		id.Groups = []string{"admins"}
		promoted := signIn(t, h, id)
		if !promoted.AdminViaGroup || !promoted.IsAdmin() {
			t.Fatalf("admin_via_group = %v, IsAdmin = %v after joining the group, want true/true", promoted.AdminViaGroup, promoted.IsAdmin())
		}

		entries := roleChangeAudits(t, h, promoted.ID)
		if len(entries) != 1 {
			t.Fatalf("role-change audit entries = %d, want 1", len(entries))
		}
		assertRoleChange(t, entries[0], promoted.ID, store.RoleUser, store.RoleAdmin)
	})

	t.Run("demotion when the group disappears", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = []string{"admins"}
		id := &auth.Identity{Subject: "sub-charlie", Email: "charlie@example.com", Name: "Charlie", Groups: []string{"admins"}}

		if u := signIn(t, h, id); !u.IsAdmin() {
			t.Fatalf("user not admin while in the admin group")
		}

		id.Groups = nil
		demoted := signIn(t, h, id)
		if demoted.AdminViaGroup || demoted.IsAdmin() {
			t.Fatalf("admin_via_group = %v, IsAdmin = %v after leaving the group, want false/false", demoted.AdminViaGroup, demoted.IsAdmin())
		}

		entries := roleChangeAudits(t, h, demoted.ID)
		if len(entries) != 1 {
			t.Fatalf("role-change audit entries = %d, want 1 (the demotion)", len(entries))
		}
		assertRoleChange(t, entries[0], demoted.ID, store.RoleAdmin, store.RoleUser)
	})

	t.Run("explicit admin grant overrides group removal", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = []string{"admins"}
		ctx := context.Background()
		id := &auth.Identity{Subject: "sub-frank", Email: "frank@example.com", Name: "Frank", Groups: []string{"admins"}}

		user := signIn(t, h, id)
		// An administrator explicitly promotes Frank (the PATCH /admin/users
		// path lands in the same store write).
		if err := h.store.UpdateUser(ctx, user.ID, store.RoleAdmin, true); err != nil {
			t.Fatalf("explicit promotion: %v", err)
		}

		id.Groups = nil // leaves the IdP group
		after := signIn(t, h, id)
		if after.Role != store.RoleAdmin || !after.IsAdmin() {
			t.Fatalf("role = %q, IsAdmin = %v after group removal, want sticky admin", after.Role, after.IsAdmin())
		}
		if after.AdminViaGroup {
			t.Errorf("admin_via_group still true after leaving the group")
		}
		// The group flag flipped but the capability did not change — no
		// demotion entry may be written.
		if entries := roleChangeAudits(t, h, after.ID); len(entries) != 0 {
			t.Fatalf("role-change audit entries = %d, want 0 (explicit grant absorbed the group removal)", len(entries))
		}
	})

	t.Run("bootstrap admin unaffected by groups", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.BootstrapAdminEmails = []string{"eve@example.com"}
		h.server.Config.AdminGroups = []string{"admins"}
		id := &auth.Identity{Subject: "sub-eve", Email: "eve@example.com", Name: "Eve"}

		first := signIn(t, h, id) // never in any group
		if first.Role != store.RoleAdmin {
			t.Fatalf("bootstrap first-login role = %q, want admin", first.Role)
		}

		again := signIn(t, h, id) // still no group — must stay admin
		if again.Role != store.RoleAdmin || !again.IsAdmin() {
			t.Fatalf("bootstrap admin lost capability on re-login: role=%q IsAdmin=%v", again.Role, again.IsAdmin())
		}
		if entries := roleChangeAudits(t, h, again.ID); len(entries) != 0 {
			t.Fatalf("role-change audit entries = %d, want 0 for a bootstrap admin", len(entries))
		}
	})

	t.Run("no effect when JANUS_ADMIN_GROUPS is unset", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = nil // the pinned default
		id := &auth.Identity{Subject: "sub-dave", Email: "dave@example.com", Name: "Dave", Groups: []string{"admins", "devops"}}

		first := signIn(t, h, id)
		second := signIn(t, h, id)
		for label, u := range map[string]*store.User{"first": first, "second": second} {
			if u.Role != store.RoleUser || u.AdminViaGroup || u.IsAdmin() {
				t.Errorf("%s login: role=%q admin_via_group=%v IsAdmin=%v, want plain user", label, u.Role, u.AdminViaGroup, u.IsAdmin())
			}
		}
		entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Action: "user_role_changed"})
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("role-change audit entries = %d, want 0 when the variable is unset", len(entries))
		}
	})
}

// TestSignInDevAuthGroupsParity proves the dev-auth `groups` query parameter
// drives the same admin-group evaluation as a real OIDC groups claim, end to
// end through the HTTP login route.
func TestSignInDevAuthGroupsParity(t *testing.T) {
	devLogin := func(t *testing.T, h *harness, query string) *store.User {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/auth/start?"+query, nil)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("dev login status = %d, want %d", rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "error=") {
			t.Fatalf("dev login failed with redirect %q", loc)
		}
		user, err := h.store.UserByAuthID(context.Background(), auth.DevIdentity("dev@example.com", "", nil).Subject)
		if err != nil {
			t.Fatalf("load dev user: %v", err)
		}
		return user
	}

	t.Run("groups parameter grants and revokes admin", func(t *testing.T) {
		h := newHarness(t) // DevAuthEnabled is on in the harness config
		h.server.Config.AdminGroups = []string{"admins"}

		user := devLogin(t, h, "email=dev@example.com&groups=Admins,platform")
		if !user.AdminViaGroup || !user.IsAdmin() {
			t.Fatalf("dev-auth group promotion missing: admin_via_group=%v IsAdmin=%v", user.AdminViaGroup, user.IsAdmin())
		}

		demoted := devLogin(t, h, "email=dev@example.com&groups=platform")
		if demoted.AdminViaGroup || demoted.IsAdmin() {
			t.Fatalf("dev-auth group demotion missing: admin_via_group=%v IsAdmin=%v", demoted.AdminViaGroup, demoted.IsAdmin())
		}

		entries := roleChangeAudits(t, h, demoted.ID)
		if len(entries) != 1 {
			t.Fatalf("role-change audit entries = %d, want 1 (the demotion)", len(entries))
		}
		assertRoleChange(t, entries[0], demoted.ID, store.RoleAdmin, store.RoleUser)
	})

	t.Run("groups parameter is inert when the variable is unset", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.AdminGroups = nil

		user := devLogin(t, h, "email=dev@example.com&groups=admins")
		if user.AdminViaGroup || user.IsAdmin() || user.Role != store.RoleUser {
			t.Fatalf("unset variable must ignore groups: role=%q admin_via_group=%v", user.Role, user.AdminViaGroup)
		}
	})
}
