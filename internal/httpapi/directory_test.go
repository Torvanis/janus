package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// These tests need a directory. CI starts glauth with testdata/glauth.cfg
// (svc-janus / ada / bob; passwords pw-svc / pw-ada / pw-bob). Locally:
//
//	podman run --rm -p 127.0.0.1:3893:3893 -v $PWD/internal/httpapi/testdata/glauth.cfg:/c.cfg:ro,z \
//	  --entrypoint /app/glauth docker.io/glauth/glauth:latest -c /c.cfg
//
// The tests skip when nothing listens on JANUS_TEST_LDAP_URL (default :3893).
func ldapURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("JANUS_TEST_LDAP_URL")
	if u == "" {
		u = "ldap://127.0.0.1:3893"
	}
	host := u[len("ldap://"):]
	c, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("no LDAP test directory at %s: %v", u, err)
	}
	_ = c.Close()
	return u
}

func fixtureDirectory(url string) map[string]any {
	return map[string]any{
		"url": url, "bind_dn": "cn=svc-janus,ou=svcaccts,dc=example,dc=com", "bind_password": "pw-svc",
		"base_dn": "dc=example,dc=com", "user_filter": "(&(objectClass=posixAccount)(mail={login}))",
		"email_attr": "mail", "name_attr": "cn", "groups_attr": "memberOf", "admin_groups": []string{"janus-admins"},
	}
}

func TestDirectoryAdminGateAndTest(t *testing.T) {
	url := ldapURL(t)
	h := newHarness(t)
	promote(t, h)
	// Community: GET answers with licensed=false; PUT is 402.
	if rec := h.do(http.MethodGet, "/api/v1/admin/directory", nil); rec.Code != 200 || decodeBody(t, rec)["licensed"] != false {
		t.Fatalf("community get: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", fixtureDirectory(url)); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("community put: %d", rec.Code)
	}
	bizLicense(t, h)
	// Test with a login resolves the user and predicts admin.
	body := fixtureDirectory(url)
	body["login"] = "ada@example.com"
	rec := h.do(http.MethodPost, "/api/v1/admin/directory/test", body)
	out := decodeBody(t, rec)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	u := out["user"].(map[string]any)
	if u["email"] != "ada@example.com" || u["would_be_admin"] != true {
		t.Fatalf("resolved user: %v", u)
	}
	// Wrong bind password → ok=false with a reason, never a 500.
	body["bind_password"] = "nope"
	if rec = h.do(http.MethodPost, "/api/v1/admin/directory/test", body); rec.Code != 200 || decodeBody(t, rec)["ok"] != false {
		t.Fatalf("bad bind: %d %s", rec.Code, rec.Body)
	}
	// Save; secret never echoed; blank password on re-save keeps it.
	if rec = h.do(http.MethodPut, "/api/v1/admin/directory", fixtureDirectory(url)); rec.Code != 200 || decodeBody(t, rec)["has_bind_password"] != true {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if _, ok := decodeBody(t, rec)["bind_password"]; ok {
		t.Fatal("bind password echoed")
	}
	again := fixtureDirectory(url)
	delete(again, "bind_password")
	if rec = h.do(http.MethodPut, "/api/v1/admin/directory", again); rec.Code != 200 || decodeBody(t, rec)["has_bind_password"] != true {
		t.Fatalf("re-put kept secret: %d %s", rec.Code, rec.Body)
	}
	// Validation.
	bad := fixtureDirectory(url)
	bad["user_filter"] = "(mail=x)"
	if rec = h.do(http.MethodPut, "/api/v1/admin/directory", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("filter without {login}: %d", rec.Code)
	}
	if rec = h.do(http.MethodDelete, "/api/v1/admin/directory", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = h.do(http.MethodDelete, "/api/v1/admin/directory", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}

func TestDirectorySignInThroughLoginForm(t *testing.T) {
	url := ldapURL(t)
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", fixtureDirectory(url)); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	// The login form is offered even though this harness has an OIDC-less
	// admin and no local credentials yet (directory is enabled).
	if rec := h.do(http.MethodGet, "/auth/local/status", nil); decodeBody(t, rec)["enabled"] != true {
		t.Fatalf("status: %s", rec.Body)
	}
	login := func(email, pw string) *httptest.ResponseRecorder {
		return h.jsonPost("/auth/local/login", map[string]string{"email": email, "password": pw, "redirect_to": "/models"}, nil)
	}
	errCode := func(rec *httptest.ResponseRecorder) string {
		e, _ := decodeBody(t, rec)["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}
	// Wrong password: same 401 as a local miss.
	if rec := login("ada@example.com", "wrong"); rec.Code != http.StatusUnauthorized || errCode(rec) != "bad_credentials" {
		t.Fatalf("wrong pw: %d %s", rec.Code, rec.Body)
	}
	// Right password: session, JIT user with ldap: subject, admin via group.
	rec := login("ada@example.com", "pw-ada")
	if rec.Code != 200 || decodeBody(t, rec)["signed_in"] != true || decodeBody(t, rec)["redirect_to"] != "/models" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("no session cookie")
	}
	ada, err := h.store.UserByEmail(context.Background(), "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ada.AuthID, "ldap:cn=ada,") || !ada.AdminViaGroup {
		t.Fatalf("ada: %+v", ada)
	}
	// bob is not in janus-admins.
	if rec = login("bob@example.com", "pw-bob"); rec.Code != 200 {
		t.Fatalf("bob: %d %s", rec.Code, rec.Body)
	}
	bob, _ := h.store.UserByEmail(context.Background(), "bob@example.com")
	if bob.AdminViaGroup {
		t.Fatal("bob must not be admin")
	}
	// Unknown user: 401, no account created.
	if rec = login("ghost@example.com", "pw-ada"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ghost: %d", rec.Code)
	}
	if _, err := h.store.UserByEmail(context.Background(), "ghost@example.com"); err == nil {
		t.Fatal("ghost provisioned")
	}
	// Directory disabled → the form is gone (404: nothing to sign in with
	// on this dev-auth harness), never a session.
	d := fixtureDirectory(url)
	d["enabled"] = false
	h.do(http.MethodPut, "/api/v1/admin/directory", d)
	if rec = login("ada@example.com", "pw-ada"); rec.Code == http.StatusOK {
		t.Fatalf("disabled directory still signs in: %s", rec.Body)
	}
	// Directory unreachable → 503 directory_unavailable, not a silent 401.
	d["enabled"] = true
	d["url"] = "ldap://127.0.0.1:1"
	h.do(http.MethodPut, "/api/v1/admin/directory", d)
	if rec = login("ada@example.com", "pw-ada"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable: %d %s", rec.Code, rec.Body)
	}
}

func TestLDAPAdapterDirect(t *testing.T) {
	url := ldapURL(t)
	cfg := toLDAPConfig(&store.Directory{URL: url, BindDN: "cn=svc-janus,ou=svcaccts,dc=example,dc=com", BaseDN: "dc=example,dc=com",
		UserFilter: "(&(objectClass=posixAccount)(mail={login}))", GroupsAttr: "memberOf"}, "pw-svc")
	u, err := cfg.Authenticate(context.Background(), "ada@example.com", "pw-ada")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ada@example.com" || len(u.Groups) == 0 {
		t.Fatalf("user: %+v", u)
	}
	// Filter injection: a login with LDAP metacharacters must be escaped,
	// not turned into a wildcard match.
	if _, err := cfg.Authenticate(context.Background(), "*", "pw-ada"); err != auth.ErrLDAPBadCredentials {
		t.Fatalf("wildcard login: %v", err)
	}
}
