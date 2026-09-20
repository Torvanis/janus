package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// installLicense signs claims with a throwaway key the harness server trusts
// and installs them, so tests can drive every license state.
func installLicense(t *testing.T, h *harness, c license.Claims) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c.KeyID = "test"
	if c.Issued.IsZero() {
		c.Issued = time.Now().Add(-time.Hour)
	}
	raw, err := license.Sign(priv, c)
	if err != nil {
		t.Fatal(err)
	}
	h.server.License = license.NewManager("", h.store, map[string]ed25519.PublicKey{"test": pub}, nil)
	if _, err := h.server.License.Install(context.Background(), raw); err != nil {
		t.Fatalf("install: %v", err)
	}
}

// withBusinessLicense installs a valid Business key so tests of Business-only
// features (SCIM, scheduled reports…) exercise the feature, not the gate.
func withBusinessLicense(t *testing.T, h *harness) {
	t.Helper()
	exp := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-TEST", Org: "Test", IssuedTo: "t@t", Edition: license.EditionBusiness,
		Seats: 0, Nodes: 0, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
}

func promote(t *testing.T, h *harness) {
	t.Helper()
	if err := h.store.UpdateUser(context.Background(), h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateTokenCache()
}

func TestLicenseGateBlocksCreateOnlyWhenExpired(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	exp := time.Now().Add(-60 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-EXP", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Nodes: 3, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})

	// CREATE is refused with 402 and the license code.
	rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "blocked"})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expired create returned %d, want 402: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if code := body["error"].(map[string]any)["code"]; code != CodeLicenseRequired {
		t.Fatalf("code=%v", code)
	}
	// User self-service tokens are creation too.
	if rec := h.do(http.MethodPost, "/api/v1/tokens", map[string]any{"description": "x"}); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("token create returned %d, want 402", rec.Code)
	}
	// READ and the proxy path keep working (never disrupt work).
	if rec := h.do(http.MethodGet, "/api/v1/admin/teams", nil); rec.Code != http.StatusOK {
		t.Fatalf("list returned %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/api/v1/models", nil); rec.Code != http.StatusOK {
		t.Fatalf("models returned %d", rec.Code)
	}
	// Business feature already licensed is still reported as present.
	if st := h.server.licenseState(); !st.Has("scim") || !st.Restricted() {
		t.Fatalf("state: %+v", st)
	}

	// Grace: creation allowed again.
	exp2 := time.Now().Add(-5 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-GRACE", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Nodes: 3, Features: license.BusinessFeatures, Exp: &exp2, GraceDays: 30, Term: license.TermSubscription})
	if rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "grace-ok"}); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("grace create returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCommunityCannotEnableBusinessFeature(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	// No key at all = Community.
	rec := h.do(http.MethodPost, "/api/v1/admin/scim/tokens", map[string]any{"label": "x"})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("scim on community returned %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != CodeFeatureNotLicensed {
		t.Fatalf("code=%v", code)
	}
	// Community creation of ordinary things is fine.
	if rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "community-ok"}); rec.Code >= 300 {
		t.Fatalf("community team create returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLicenseAdminEndpoints(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	rec := h.do(http.MethodGet, "/api/v1/admin/system/license", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get license %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["license"].(map[string]any)["edition"] != "community" || body["instance_id"] == "" {
		t.Fatalf("body: %v", body)
	}
	// Garbage is rejected with 400, not stored.
	h.server.License = license.NewManager("", h.store, map[string]ed25519.PublicKey{}, nil)
	_ = h.server.License.Refresh(context.Background())
	rec = h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": "nonsense"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage key returned %d", rec.Code)
	}
	rec = h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": license.Prefix + ".e30.AAAA"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsigned key returned %d: %s", rec.Code, rec.Body.String())
	}
	// A real key installs and the summary reaches /me for the banner.
	exp := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-OK", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 50, Nodes: 3, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	me := decodeBody(t, h.do(http.MethodGet, "/api/v1/me", nil))
	lic := me["license"].(map[string]any)
	if lic["edition"] != "business" || lic["status"] != "valid" {
		t.Fatalf("me.license: %v", lic)
	}
	// Remove returns to community.
	rec = h.do(http.MethodDelete, "/api/v1/admin/system/license", nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["license"].(map[string]any)["edition"] != "community" {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeatLimitBlocksNewUserOnly(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	exp := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-1SEAT", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 1, Nodes: 1, Features: nil, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	ctx := context.Background()
	// The harness user signed in (last_login_at set) → 1 of 1 seats used.
	if used, _ := h.store.ActiveSeatCount(ctx, time.Now().Add(-seatWindow)); used != 1 {
		t.Fatalf("seats used = %d, want 1", used)
	}
	if err := h.server.checkSeatAvailable(ctx); err == nil {
		t.Fatal("new user must be refused when seats are full")
	}
	// Unlimited seats never refuse.
	installLicense(t, h, license.Claims{LicenseID: "JNS-ENT-0", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionEnterprise,
		Seats: 0, Nodes: 0, Features: license.EnterpriseFeatures, Term: license.TermPerpetual, MaintenanceUntil: "2030-01-01"})
	if err := h.server.checkSeatAvailable(ctx); err != nil {
		t.Fatalf("unlimited refused: %v", err)
	}
}
