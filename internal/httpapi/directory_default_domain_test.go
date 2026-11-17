package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// A directory's default domain lets people sign in with a short name. This
// part needs no directory server: the setting is validated and normalized,
// the sign-in page learns the domain, and a local account in that domain
// signs in by its short name (the local check runs first).
func TestDirectoryDefaultDomainSettingAndLocalShortName(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	ctx := context.Background()
	if _, err := h.store.CreateLocalUser(ctx, "alice@corp.example.com", "Alice", "alice-password-12", false); err != nil {
		t.Fatal(err)
	}
	dir := fixtureDirectory("ldap://127.0.0.1:1") // never dialed by these checks
	for _, bad := range []string{`CORP\x`, "corp example.com", "ada@corp.example.com", "corp..example.com", "-corp.com"} {
		dir["default_domain"] = bad
		if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "default_domain") {
			t.Fatalf("default_domain %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	dir["default_domain"] = " @Corp.Example.COM "
	rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir)
	if rec.Code != 200 || decodeBody(t, rec)["default_domain"] != "corp.example.com" {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/directory", nil))
	if d, _ := got["directory"].(map[string]any); d["default_domain"] != "corp.example.com" {
		t.Fatalf("get: %v", got)
	}
	rows := auditRows(t, h, store.AuditFilter{})
	if !hasAudit(rows, "directory.update", "", "corp.example.com") {
		t.Fatalf("directory.update audit lacks the domain: %+v", rows)
	}
	health := decodeBody(t, h.get("/auth/health"))
	if health["directory_sign_in"] != true || health["default_domain"] != "corp.example.com" {
		t.Fatalf("health: %v", health)
	}

	login := func(name, pw string) *httptest.ResponseRecorder {
		return h.jsonPost("/auth/local/login", map[string]string{"email": name, "password": pw}, nil)
	}
	if rec := login("alice", "alice-password-12"); rec.Code != 200 || decodeBody(t, rec)["signed_in"] != true {
		t.Fatalf("short name, local account: %d %s", rec.Code, rec.Body)
	}
	rows = auditRows(t, h, store.AuditFilter{})
	if !hasAudit(rows, "sign_in", store.AuditSuccess, "alice@corp.example.com") {
		t.Fatalf("sign-in audit does not name the full account: %+v", rows)
	}

	// Clearing the domain: short names are used as typed again, and the
	// page no longer promises a domain.
	dir["default_domain"] = ""
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	health = decodeBody(t, h.get("/auth/health"))
	if _, has := health["default_domain"]; has || health["directory_sign_in"] != true {
		t.Fatalf("health after clear: %v", health)
	}
	// Disabled directory: no hint, and the short name means nothing.
	dir["enabled"] = false
	dir["default_domain"] = "corp.example.com"
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	health = decodeBody(t, h.get("/auth/health"))
	if _, has := health["default_domain"]; has || health["directory_sign_in"] != false {
		t.Fatalf("health when disabled: %v", health)
	}
	if rec := login("alice", "alice-password-12"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("short name with the directory off: %d %s", rec.Code, rec.Body)
	}
}

// Against the glauth fixture (filter mail={login}): "ada" signs in as
// ada@example.com, and the short and full names are one person — one user
// row, one subject, one lockout counter, one audit name.
func TestDirectoryShortNameSignInUsesDefaultDomain(t *testing.T) {
	url := ldapURL(t)
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	ctx := context.Background()
	dir := fixtureDirectory(url)
	// Without a default domain the fixture's filter cannot match "ada".
	if rec := h.jsonPost("/auth/local/login", map[string]string{"email": "ada", "password": "pw-ada"}, nil); rec.Code == http.StatusOK {
		t.Fatalf("no directory yet, yet short name signed in: %s", rec.Body)
	}
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := h.jsonPost("/auth/local/login", map[string]string{"email": "ada", "password": "pw-ada"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("short name without a default domain: %d %s", rec.Code, rec.Body)
	}

	// Test resolves a short name with the candidate domain and says which
	// name it searched for.
	test := fixtureDirectory(url)
	test["default_domain"] = "example.com"
	test["login"] = "ada"
	out := decodeBody(t, h.do(http.MethodPost, "/api/v1/admin/directory/test", test))
	u, _ := out["user"].(map[string]any)
	if out["ok"] != true || out["login"] != "ada@example.com" || u["email"] != "ada@example.com" {
		t.Fatalf("test with short name: %v", out)
	}

	dir["default_domain"] = "example.com"
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != 200 {
		t.Fatalf("put with domain: %d %s", rec.Code, rec.Body)
	}
	login := func(name, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(`{"email":"`+strings.ReplaceAll(name, `\`, `\\`)+`","password":"`+pw+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.61:5555"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := login("ada", "pw-ada"); rec.Code != 200 || decodeBody(t, rec)["signed_in"] != true {
		t.Fatalf("short name: %d %s", rec.Code, rec.Body)
	}
	first, err := h.store.UserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rec := login("ada@example.com", "pw-ada"); rec.Code != 200 {
		t.Fatalf("full name: %d %s", rec.Code, rec.Body)
	}
	if rec := login("  Ada ", "pw-ada"); rec.Code != 200 {
		t.Fatalf("short name, other case and spaces: %d %s", rec.Code, rec.Body)
	}
	again, err := h.store.UserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || first.AuthID != again.AuthID || !strings.HasPrefix(first.AuthID, "ldap:cn=ada,") || !again.AdminViaGroup {
		t.Fatalf("short and full name differ: %+v vs %+v", first, again)
	}
	all, _, err := h.store.ListUsers(ctx, store.UserFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, u := range all {
		if strings.Contains(strings.ToLower(u.Email), "ada") || strings.Contains(u.AuthID, "cn=ada,") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one user for ada, found %d", n)
	}

	// A name with its own domain is not rewritten.
	if rec := login("ada@other.example.org", "pw-ada"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("other domain: %d %s", rec.Code, rec.Body)
	}
	// A Windows name for another domain is refused (EXAMPLE\ada, the
	// matching one, signs in: see directory_windows_domain_test.go).
	if rec := login(`OTHER\ada`, "pw-ada"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("backslash form, other domain: %d %s", rec.Code, rec.Body)
	}
	// A failure by short name is recorded against the full name.
	if rec := login("bob", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob wrong pw: %d %s", rec.Code, rec.Body)
	}
	rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.61"})
	if !hasAudit(rows, "sign_in_failed", store.AuditFailure, "bob@example.com") {
		t.Fatalf("failed short-name sign-in not audited under the full name: %+v", rows)
	}
	if !hasAudit(rows, "sign_in", store.AuditSuccess, "ada@example.com") {
		t.Fatalf("sign-in audit: %+v", rows)
	}
}
