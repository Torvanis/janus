package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/store"
)

// withSTIGProfile turns the profile on for one test.
func withSTIGProfile(t *testing.T) {
	t.Helper()
	prev := stigprofile.Set(true)
	t.Cleanup(func() { stigprofile.Set(prev) })
}

// breakAuditLog makes every audit_log insert fail, as a full disk or a
// revoked grant would, while every other table keeps working.
func breakAuditLog(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.store.DB().Exec(`CREATE TRIGGER janus_test_audit_fail BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.store.DB().Exec(`DROP TRIGGER IF EXISTS janus_test_audit_fail`) })
}

func teamNamed(t *testing.T, h *harness, name string) bool {
	t.Helper()
	teams, err := h.store.ListTeams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range teams {
		if tm.Name == name {
			return true
		}
	}
	return false
}

func auditCount(t *testing.T, h *harness, action string) int {
	t.Helper()
	_, n, err := h.store.ListAudit(context.Background(), store.AuditFilter{Action: action, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Default behavior (no profile): an audit failure is logged and the change
// stands. This is the existing ruling and must not change.
func TestAuditFailureWithoutProfileKeepsTheChange(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	breakAuditLog(t, h)
	rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "kept"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body)
	}
	if !teamNamed(t, h, "kept") {
		t.Fatal("change was lost without the STIG profile")
	}
}

// STIG profile: an administrative change whose audit record cannot be
// written answers 500 audit.write_failed and does not persist.
func TestSTIGProfileAuditFailureBlocksTheChange(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withSTIGProfile(t)
	breakAuditLog(t, h)

	// Generic s.audit path, store work inside InTx (team + leader row).
	rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "refused", "lead_user_id": h.user.ID})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d %s", rec.Code, rec.Body)
	}
	if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != CodeAuditWriteFailed {
		t.Fatalf("code = %v", code)
	}
	if teamNamed(t, h, "refused") {
		t.Fatal("the change persisted although its audit record was not written")
	}

	// A handler that writes audit rows with Store.AppendAudit directly
	// (token revocation) is covered too.
	tokens, err := h.store.ListTokens(context.Background(), h.user.ID)
	if err != nil || len(tokens) == 0 {
		t.Fatalf("tokens: %v", err)
	}
	if rec := h.do(http.MethodDelete, "/api/v1/tokens/"+tokens[0].ID, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("token revoke with broken audit: %d %s", rec.Code, rec.Body)
	}
	// The token was not revoked: it still authenticates.
	h.server.InvalidateTokenCache()
	if rec := h.do(http.MethodGet, "/api/v1/me", nil); rec.Code != http.StatusOK {
		t.Fatalf("token revoked despite refused change: %d", rec.Code)
	}

	// Reads are never held up.
	if rec := h.do(http.MethodGet, "/api/v1/admin/teams", nil); rec.Code != http.StatusOK {
		t.Fatalf("read: %d", rec.Code)
	}
}

// STIG profile with a healthy audit log: the change and its audit row
// commit together, and client errors still answer as before.
func TestSTIGProfileAuditSuccessCommitsBoth(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withSTIGProfile(t)
	rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "committed", "lead_user_id": h.user.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", rec.Code, rec.Body)
	}
	if !teamNamed(t, h, "committed") || auditCount(t, h, "team_created") != 1 {
		t.Fatal("change or audit row missing after commit")
	}
	// A refused request keeps its normal status and envelope.
	if rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": " "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("validation error: %d %s", rec.Code, rec.Body)
	}
	// A unique violation inside the transaction is a normal 4xx, and the
	// next request still works (the failing statement rolled back alone).
	if rec := h.do(http.MethodPost, "/api/v1/admin/service-tokens", map[string]any{"name": "svc-a"}); rec.Code != http.StatusCreated {
		t.Fatalf("service token: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/service-tokens", map[string]any{"name": "svc-a"}); rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("duplicate service token: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "after-conflict"}); rec.Code != http.StatusCreated || !teamNamed(t, h, "after-conflict") {
		t.Fatalf("request after a conflict: %d %s", rec.Code, rec.Body)
	}
}

// Audit export is never license-gated: no key, Community, expired,
// invalid, and over the seat limit all export. Admin is still required.
func TestAuditExportNeverLicenseGated(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	export := func(name string) {
		t.Helper()
		rec := h.do(http.MethodGet, "/api/v1/admin/audit/export", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
			t.Fatalf("%s: export %d %s", name, rec.Code, rec.Body)
		}
	}
	export("no key")

	exp := time.Now().Add(-90 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-EXP", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	if st := h.server.licenseState(); st.Status != license.StatusExpired {
		t.Fatalf("status %s", st.Status)
	}
	export("expired")

	grace := time.Now().Add(-5 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-GR", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Features: license.BusinessFeatures, Exp: &grace, GraceDays: 30, Term: license.TermSubscription})
	export("grace")

	year := time.Now().Add(365 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-COM", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionCommunity,
		Exp: &year, Term: license.TermSubscription})
	export("community")

	// Over the seat limit: a one-seat key with two active users.
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-1", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 1, Features: license.BusinessFeatures, Exp: &year, Term: license.TermSubscription})
	ctx := context.Background()
	for _, sub := range []string{"seat-a", "seat-b"} {
		// Creating the user stamps last_login_at: an active seat.
		if _, _, err := h.store.UpsertUserFromIdentity(ctx, sub, sub+"@example.com", sub, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if used, err := h.store.ActiveSeatCount(ctx, time.Now().Add(-seatWindow)); err != nil || used <= 1 {
		t.Fatalf("seats used = %d (%v), want over the limit of 1", used, err)
	}
	export("over seats")

	// Invalid: a key the gateway cannot verify.
	if err := h.store.PutLicenseKey(ctx, "JANUS-LICENSE-1.e30.AAAA"); err != nil {
		t.Fatal(err)
	}
	if err := h.server.License.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.server.licenseState(); st.Status != license.StatusInvalid {
		t.Fatalf("status %s", st.Status)
	}
	export("invalid")

	// Still admin-only.
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateTokenCache()
	if rec := h.do(http.MethodGet, "/api/v1/admin/audit/export", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin export: %d", rec.Code)
	}
}

// Decision C: the STIG profile enrolls HMAC-SHA-256 TOTP and refuses SHA-1
// in either edition, reusing the FIPS edition's logic. LDAP skip_verify is
// refused when saved, like the FIPS edition.
func TestSTIGProfileApprovedTOTPAndLDAPVerify(t *testing.T) {
	withSTIGProfile(t)
	if got := totpEnrollAlgorithm(); got != store.TOTPAlgorithmSHA256 {
		t.Fatalf("enrollment algorithm = %q", got)
	}
	if totpUsable(store.TOTPAlgorithmSHA1) || !totpUsable(store.TOTPAlgorithmSHA256) {
		t.Fatal("SHA-1 TOTP must be refused in the STIG profile")
	}
	key, alg, err := generateTOTPKey("ada@example.com")
	if err != nil || alg != store.TOTPAlgorithmSHA256 || key.Algorithm().String() != "SHA256" {
		t.Fatalf("generated key: alg=%q %v err=%v", alg, key, err)
	}
	if _, err := (directoryBody{URL: "ldaps://dir.example.mil:636", BaseDN: "dc=example,dc=mil", SkipVerify: true}).validate(); err == nil {
		t.Fatal("skip_verify saved in the STIG profile")
	}
	if _, err := (directoryBody{URL: "ldaps://dir.example.mil:636", BaseDN: "dc=example,dc=mil"}).validate(); err != nil {
		t.Fatalf("verifying directory refused: %v", err)
	}
}
