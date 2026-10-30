package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// After a downgrade to Community, an LDAP directory and an extra identity
// provider can still be deleted (deletes are never license-gated).
func TestLicenseDowngradeStillAllowsIdentityDeletes(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	withBusinessLicense(t, h)
	if _, err := h.store.SaveDirectory(ctx, h.server.Cipher, store.Directory{URL: "ldap://127.0.0.1:1", BaseDN: "dc=x", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	idp, err := h.store.CreateIdentityProvider(ctx, h.server.Cipher, store.IdentityProviderInput{Slug: "partner", Name: "Partner", IssuerURL: "https://idp.invalid", ClientID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	h.server.License = nil // Community
	if h.server.featureRuns("ldap") {
		t.Fatal("test setup: expected Community")
	}
	if rec := h.do(http.MethodDelete, "/api/v1/admin/directory", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete directory on Community = %d %s, want 204", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodDelete, "/api/v1/admin/identity-providers/"+idp.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete identity provider on Community = %d %s, want 204", rec.Code, rec.Body.String())
	}
}

// Rotating a SCIM token mints a new secret, so it is gated like creating one.
func TestSCIMTokenRotateIsLicenseGated(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	tok, _, err := h.store.CreateSCIMToken(context.Background(), "idp", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/scim/tokens/"+tok.ID+"/rotate", nil); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("rotate on Community = %d %s, want 402", rec.Code, rec.Body.String())
	}
	exp := time.Now().Add(-60 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-EXP", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Nodes: 3, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	if rec := h.do(http.MethodPost, "/api/v1/admin/scim/tokens/"+tok.ID+"/rotate", nil); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("rotate past grace = %d %s, want 402", rec.Code, rec.Body.String())
	}
	withBusinessLicense(t, h)
	if rec := h.do(http.MethodPost, "/api/v1/admin/scim/tokens/"+tok.ID+"/rotate", nil); rec.Code != http.StatusCreated {
		t.Fatalf("rotate licensed = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// A saved Chat access restriction keeps applying after a downgrade to
// Community: only saving a new one is gated.
func TestChatAccessRestrictionSurvivesDowngrade(t *testing.T) {
	h, admin := chatHarness(t)
	outsider := h.sessionFor(t, "outsider@example.com")
	g, err := h.store.CreateGroup(context.Background(), "chatters")
	if err != nil {
		t.Fatal(err)
	}
	withBusinessLicense(t, h)
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["access"] = map[string]any{"mode": "restricted", "groups": []string{g.ID}, "teams": []string{}, "users": []string{},
			"message": "Ask IT for Chat access."}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("restrict = %d", res.StatusCode)
	}
	h.server.License = nil // downgraded to Community
	rec := h.doAsSession(outsider, http.MethodGet, "/api/v1/chat/conversations", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Ask IT") {
		t.Fatalf("outsider after downgrade = %d %s, want 403 (restriction kept)", rec.Code, rec.Body.String())
	}
}
