package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// windowsLogin posts a sign-in from a fixed address (so audit rows can be
// filtered) and escapes the backslash in DOMAIN\name.
func windowsLogin(h *harness, ip, name, pw string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(`{"email":"`+strings.ReplaceAll(name, `\`, `\\`)+`","password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":5555"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// errorShape is a refusal without its request id: status, code, message.
func errorShape(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	e, _ := decodeBody(t, rec)["error"].(map[string]any)
	return fmt.Sprint(rec.Code, " ", e["code"], " ", e["message"])
}

// The Windows domain name setting: validated, upper-cased, audited, needs a
// default domain, derived from the default domain when empty, and a local
// account in the domain signs in as CORP\name. No directory server needed.
func TestDirectoryWindowsDomainSettingAndLocalAccount(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	ctx := context.Background()
	if _, err := h.store.CreateLocalUser(ctx, "alice@corp.example.com", "Alice", "alice-password-12", false); err != nil {
		t.Fatal(err)
	}
	dir := fixtureDirectory("ldap://127.0.0.1:1") // never dialed by these checks
	dir["default_domain"] = "corp.example.com"
	for _, bad := range []string{"SIXTEENCHARSLONG", "CO RP", "corp.example", `CORP\`, "CORP@", "ÉCOLE"} {
		dir["windows_domain"] = bad
		if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "windows_domain") {
			t.Fatalf("windows_domain %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	// A Windows domain with nothing to map onto is refused.
	noDefault := fixtureDirectory("ldap://127.0.0.1:1")
	noDefault["windows_domain"] = "CORP"
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", noDefault); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "default_domain") {
		t.Fatalf("windows_domain without default_domain: %d %s", rec.Code, rec.Body)
	}

	// Empty: derived from the default domain.
	dir["windows_domain"] = ""
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if health := decodeBody(t, h.get("/auth/health")); health["windows_domain"] != "CORP" {
		t.Fatalf("derived windows_domain in health: %v", health)
	}
	for _, name := range []string{`CORP\alice`, `corp\alice`, ` Corp\Alice `} {
		if rec := windowsLogin(h, "192.0.2.71", name, "alice-password-12"); rec.Code != 200 || decodeBody(t, rec)["signed_in"] != true {
			t.Fatalf("%s, local account: %d %s", name, rec.Code, rec.Body)
		}
	}
	// (A wrong password here would reach the unreachable fixture directory
	// and answer 503, so compare with the plain wrong-password answer.)
	other := windowsLogin(h, "192.0.2.72", `OTHER\alice`, "alice-password-12")
	if got := errorShape(t, other); got != "401 bad_credentials "+store.ErrBadCredentials.Error() {
		t.Fatalf("other Windows domain must look like a wrong password: %q", got)
	}
	rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.72"})
	if !hasAudit(rows, "sign_in_failed", store.AuditFailure, "unknown_windows_domain", `other\alice`) {
		t.Fatalf("other-domain failure not audited with its reason: %+v", rows)
	}

	// Set explicitly: lower case is stored upper-case, audited, and replaces
	// the derived name.
	dir["windows_domain"] = " acme "
	rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir)
	if rec.Code != 200 || decodeBody(t, rec)["windows_domain"] != "ACME" {
		t.Fatalf("put explicit: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/directory", nil))
	if d, _ := got["directory"].(map[string]any); d["windows_domain"] != "ACME" {
		t.Fatalf("get: %v", got)
	}
	if !hasAudit(auditRows(t, h, store.AuditFilter{}), "directory.update", "", "ACME") {
		t.Fatal("directory.update audit lacks the Windows domain")
	}
	if health := decodeBody(t, h.get("/auth/health")); health["windows_domain"] != "ACME" {
		t.Fatalf("explicit windows_domain in health: %v", health)
	}
	if rec := windowsLogin(h, "192.0.2.73", `ACME\alice`, "alice-password-12"); rec.Code != 200 {
		t.Fatalf(`ACME\alice: %d %s`, rec.Code, rec.Body)
	}
	if rec := windowsLogin(h, "192.0.2.73", `CORP\alice`, "alice-password-12"); rec.Code != http.StatusUnauthorized {
		t.Fatalf(`CORP\alice after renaming to ACME: %d %s`, rec.Code, rec.Body)
	}
}

// Against the glauth fixture (filter mail={login}, default domain
// example.com so the derived Windows name is EXAMPLE): EXAMPLE\ada, ada and
// ada@example.com are one person with one lockout counter and one audit name.
func TestDirectoryWindowsDomainSignIn(t *testing.T) {
	url := ldapURL(t)
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	ctx := context.Background()

	// No default domain: any DOMAIN\name fails like a wrong password, and
	// the reason is on the record.
	dir := fixtureDirectory(url)
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := windowsLogin(h, "192.0.2.81", `EXAMPLE\ada`, "pw-ada"); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "bad_credentials") {
		t.Fatalf("Windows name without a default domain: %d %s", rec.Code, rec.Body)
	}
	if rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.81"}); !hasAudit(rows, "sign_in_failed", store.AuditFailure, "unknown_windows_domain") {
		t.Fatalf("no reason recorded: %+v", rows)
	}

	// Test shows the name it searched for, and tells the admin (only the
	// admin) when the Windows domain does not match.
	test := fixtureDirectory(url)
	test["default_domain"] = "example.com"
	test["login"] = `example\ada`
	out := decodeBody(t, h.do(http.MethodPost, "/api/v1/admin/directory/test", test))
	if u, _ := out["user"].(map[string]any); out["ok"] != true || out["login"] != "ada@example.com" || u["email"] != "ada@example.com" {
		t.Fatalf(`test with EXAMPLE\ada: %v`, out)
	}
	test["login"] = `OTHER\ada`
	out = decodeBody(t, h.do(http.MethodPost, "/api/v1/admin/directory/test", test))
	if msg, _ := out["error"].(string); out["ok"] != false || !strings.Contains(msg, "EXAMPLE") {
		t.Fatalf(`test with OTHER\ada: %v`, out)
	}

	dir["default_domain"] = "example.com"
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("put with domain: %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{`EXAMPLE\ada`, "ada", "ada@example.com", `example\Ada`} {
		if rec := windowsLogin(h, "192.0.2.82", name, "pw-ada"); rec.Code != 200 || decodeBody(t, rec)["signed_in"] != true {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	u, err := h.store.UserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.AuthID, "ldap:cn=ada,") || !u.AdminViaGroup {
		t.Fatalf("ada: %+v", u)
	}
	all, _, err := h.store.ListUsers(ctx, store.UserFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range all {
		if strings.Contains(strings.ToLower(x.Email), "ada") || strings.Contains(x.AuthID, "cn=ada,") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one user for ada, found %d", n)
	}
	if rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.82"}); !hasAudit(rows, "sign_in", store.AuditSuccess, "ada@example.com") || hasAudit(rows, "sign_in", "", `EXAMPLE`) {
		t.Fatalf("sign-in audit should name ada@example.com only: %+v", rows)
	}

	// Another Windows domain: same answer as a wrong password, not searched.
	wrongPw := windowsLogin(h, "192.0.2.83", "ada@example.com", "nope")
	other := windowsLogin(h, "192.0.2.83", `OTHER\ada`, "pw-ada")
	if other.Code != http.StatusUnauthorized || errorShape(t, other) != errorShape(t, wrongPw) {
		t.Fatalf("other domain: %q vs %q", errorShape(t, other), errorShape(t, wrongPw))
	}
	if rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.83"}); !hasAudit(rows, "sign_in_failed", store.AuditFailure, "unknown_windows_domain", `other\ada`) {
		t.Fatalf("other-domain reason: %+v", rows)
	}

	// One lockout counter: a failure as EXAMPLE\bob and one as
	// bob@example.com lock bob, whichever form he types next.
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Lockout = store.LockoutPolicy{MaxFailures: 2, WindowMinutes: 15, LockMinutes: 5}
	})
	if rec := windowsLogin(h, "192.0.2.84", `EXAMPLE\bob`, "nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf(`EXAMPLE\bob wrong: %d %s`, rec.Code, rec.Body)
	}
	if rec := windowsLogin(h, "192.0.2.84", "bob@example.com", "nope"); rec.Code != http.StatusLocked {
		t.Fatalf("bob second failure should lock: %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{"bob", `EXAMPLE\bob`} {
		if rec := windowsLogin(h, "192.0.2.84", name, "pw-bob"); rec.Code != http.StatusLocked {
			t.Fatalf("%s while locked: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.84"}); !hasAudit(rows, "sign_in_failed", store.AuditFailure, "bob@example.com") || hasAudit(rows, "sign_in_failed", "", "EXAMPLE") {
		t.Fatalf("bob's failures should be recorded as bob@example.com: %+v", rows)
	}
}
