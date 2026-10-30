package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// oneSeatLicense installs a Business key with a single seat, which the
// harness user (signed in) already holds.
func oneSeatLicense(t *testing.T, h *harness) {
	t.Helper()
	exp := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-1SEAT", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 1, Nodes: 1, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
}

func signInRefused(t *testing.T, h *harness, identity *auth.Identity) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.server.completeSignIn(rec, httptest.NewRequest(http.MethodGet, "/auth/start", nil), identity, "/dashboard")
	return rec.Header().Get("Location")
}

func TestSeatLimitAppliesToPreProvisionedFirstSignIn(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	h.server.Sessions = auth.NewDBSessionStore(h.store, time.Hour, time.Hour)
	ctx := context.Background()
	// Pre-provision before the seat limit takes effect (directory push).
	withBusinessLicense(t, h)
	saved, err := h.store.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "pre@example.com", "active": true})
	if err != nil {
		t.Fatal(err)
	}
	local, err := h.store.CreateLocalUser(ctx, "local@example.com", "Local", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}
	oneSeatLicense(t, h)

	loc := signInRefused(t, h, &auth.Identity{Subject: "pre-subject", Email: "pre@example.com", Name: "Pre", EmailVerified: true})
	if !strings.Contains(loc, "/auth/login?error=") || !strings.Contains(loc, "seats") {
		t.Fatalf("SCIM-provisioned first sign-in with no free seat: Location %q, want a seat refusal", loc)
	}
	loc = signInRefused(t, h, &auth.Identity{Subject: local.AuthID, Email: local.Email, Name: local.Name, EmailVerified: true})
	if !strings.Contains(loc, "/auth/login?error=") || !strings.Contains(loc, "seats") {
		t.Fatalf("admin-created local user's first sign-in with no free seat: Location %q, want a seat refusal", loc)
	}
	if u, _ := h.store.UserByID(ctx, saved["id"].(string)); u == nil || !u.LastLoginAt.IsZero() {
		t.Fatal("refused sign-in must not stamp last_login_at")
	}

	// A user that already signed in keeps signing in with seats full.
	withBusinessLicense(t, h)
	signIn(t, h, &auth.Identity{Subject: "pre-subject", Email: "pre@example.com", Name: "Pre", EmailVerified: true})
	exp := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-2SEAT", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 2, Nodes: 1, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	signIn(t, h, &auth.Identity{Subject: "pre-subject", Email: "pre@example.com", Name: "Pre", EmailVerified: true})
}

func TestSeatLimitAppliesToReactivation(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	dormant, _, err := h.store.UpsertUserFromIdentity(ctx, "dormant-sub", "dormant@example.com", "Dormant", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateUser(ctx, dormant.ID, store.RoleUser, false); err != nil {
		t.Fatal(err)
	}
	oneSeatLicense(t, h)

	rec := h.do(http.MethodPatch, "/api/v1/admin/users/"+dormant.ID, map[string]any{"is_active": true})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("admin reactivation with no free seat = %d %s, want 402", rec.Code, rec.Body.String())
	}
	// Other edits of a disabled user are not seat-gated.
	if rec := h.do(http.MethodPatch, "/api/v1/admin/users/"+dormant.ID, map[string]any{"role": store.RoleTeamLead}); rec.Code != http.StatusOK {
		t.Fatalf("role edit of disabled user = %d %s", rec.Code, rec.Body.String())
	}

	// SCIM reactivation is gated the same way.
	withBusinessLicense(t, h)
	_, secret, err := h.store.CreateSCIMToken(ctx, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	w, saved := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"off@example.com","active":false}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision: %d %s", w.Code, w.Body.String())
	}
	oneSeatLicense(t, h)
	id := saved["id"].(string)
	w, _ = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":true}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SCIM reactivation with no free seat = %d %s, want 403", w.Code, w.Body.String())
	}
	if u, _ := h.store.UserByID(ctx, id); u == nil || u.IsActive {
		t.Fatal("refused SCIM reactivation must leave the account disabled")
	}
	// New SCIM provisioning is never refused for seats.
	w, _ = scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"new@example.com","active":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("SCIM provisioning with no free seat = %d %s, want 201", w.Code, w.Body.String())
	}
	// With a free seat both reactivations work.
	withBusinessLicense(t, h)
	if rec := h.do(http.MethodPatch, "/api/v1/admin/users/"+dormant.ID, map[string]any{"is_active": true}); rec.Code != http.StatusOK {
		t.Fatalf("admin reactivation with seats free = %d %s", rec.Code, rec.Body.String())
	}
	w, _ = scimRequest(t, r, "PATCH", "/scim/v2/Users/"+id, secret, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":true}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("SCIM reactivation with seats free = %d %s", w.Code, w.Body.String())
	}
}
