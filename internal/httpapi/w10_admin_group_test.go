package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/auth"
)

// Removing an admin group takes group-granted admin away at once, not at the
// member's next sign-in; explicit grants survive.
func TestRemovingAdminGroupRevokesGroupAdminsImmediately(t *testing.T) {
	h := newHarness(t)
	promote(t, h) // the harness user is an explicit admin and stays one
	ctx := context.Background()
	if rec := h.do(http.MethodPost, "/api/v1/admin/admin-groups", map[string]any{"name": "Ops-Admins"}); rec.Code != http.StatusCreated {
		t.Fatalf("add admin group: %d %s", rec.Code, rec.Body.String())
	}
	both := signIn(t, h, &auth.Identity{Subject: "both", Email: "both@example.com", Name: "Both", Groups: []string{"ops-admins", "sec-admins"}})
	only := signIn(t, h, &auth.Identity{Subject: "only", Email: "only@example.com", Name: "Only", Groups: []string{"ops-admins"}})
	if !only.AdminViaGroup || !both.AdminViaGroup {
		t.Fatal("setup: group members should be admins via group")
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/admin-groups", map[string]any{"name": "sec-admins"}); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	// A bearer token of the group-only admin reaches the admin API now.
	_, onlyToken, err := h.store.CreateToken(ctx, only.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	asOnly := func() int {
		saved := h.token
		h.token = onlyToken
		defer func() { h.token = saved }()
		return h.do(http.MethodGet, "/api/v1/admin/users", nil).Code
	}
	if code := asOnly(); code != http.StatusOK {
		t.Fatalf("group admin token before removal = %d", code)
	}

	rec := h.do(http.MethodDelete, "/api/v1/admin/admin-groups/"+url.PathEscape("ops-admins"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove admin group: %d %s", rec.Code, rec.Body.String())
	}
	if u, _ := h.store.UserByID(ctx, only.ID); u.AdminViaGroup {
		t.Fatal("member of a removed admin group is still admin via group")
	}
	if code := asOnly(); code != http.StatusForbidden {
		t.Fatalf("group admin token after removal = %d, want 403", code)
	}
	if u, _ := h.store.UserByID(ctx, both.ID); !u.AdminViaGroup {
		t.Fatal("member of another admin group lost admin")
	}
	if u, _ := h.store.UserByID(ctx, h.user.ID); !u.IsAdmin() {
		t.Fatal("explicit admin lost admin")
	}
}

// Taking a member out of an admin group (in-app or via SCIM) revokes their
// group-granted admin at once.
func TestAdminGroupMembershipChangeRevokesAdmin(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withBusinessLicense(t, h)
	ctx := context.Background()
	// In-app group named as an admin group.
	g, err := h.store.CreateGroup(ctx, "platform-admins")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AddAdminGroup(ctx, "platform-admins", h.user.ID); err != nil {
		t.Fatal(err)
	}
	u, _, err := h.store.UpsertUserFromIdentity(ctx, "m1", "m1@example.com", "M1", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetGroupMembers(ctx, g.ID, []string{u.ID}); err != nil {
		t.Fatal(err)
	}
	if rec := h.do(http.MethodPatch, "/api/v1/admin/groups/"+g.ID, map[string]any{"member_ids": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("set members: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := h.store.UserByID(ctx, u.ID); got.AdminViaGroup {
		t.Fatal("removed group member is still admin via group")
	}

	// SCIM: a directory group listed as an admin group loses a member.
	_, secret, err := h.store.CreateSCIMToken(ctx, "idp", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	w, user := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"dir@example.com","active":true}`)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	uid := user["id"].(string)
	w, grp := scimRequest(t, r, "POST", "/scim/v2/Groups", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"dir-admins","members":[{"value":"`+uid+`"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	if _, err := h.store.AddAdminGroup(ctx, "dir-admins", h.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`UPDATE app_user SET admin_via_group = ? WHERE id = ?`, true, uid); err != nil {
		t.Fatal(err)
	}
	w, _ = scimRequest(t, r, "PATCH", "/scim/v2/Groups/"+grp["id"].(string), secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"remove","path":"members[value eq \"`+uid+`\"]"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("scim remove member: %d %s", w.Code, w.Body.String())
	}
	if got, _ := h.store.UserByID(ctx, uid); got.AdminViaGroup {
		t.Fatal("member removed from a SCIM admin group is still admin via group")
	}
}
