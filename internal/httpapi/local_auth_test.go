package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// jsonPost sends a JSON body without any auth header, carrying cookies from a
// previous response when given (browser-style).
func (h *harness) jsonPost(path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) withSession(method, path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "janus_csrf" {
			req.Header.Set("X-Janus-CSRF", c.Value)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

// TestLocalAuthFirstRunSetup: with no IdP and no dev auth, the very first
// request can create the administrator; the second attempt is a 404.
func TestLocalAuthFirstRunSetup(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DevAuthEnabled = false
	h.server.OIDC = nil

	rec := h.jsonPost("/auth/local/status", nil, nil)
	if rec.Code != http.StatusMethodNotAllowed { // GET only
		t.Fatalf("status POST: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/health", nil)
	hr := httptest.NewRecorder()
	h.handler.ServeHTTP(hr, req)
	if m := decode(t, hr); m["needs_setup"] != true || m["local_auth"] != true || m["status"] != "ok" {
		t.Fatalf("health before setup: %v", m)
	}

	rec = h.jsonPost("/auth/local/setup", map[string]any{"email": "Owner@Example.com", "name": "Owner", "password": "short"}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "12") {
		t.Fatalf("short password accepted: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.jsonPost("/auth/local/setup", map[string]any{"email": "Owner@Example.com", "name": "Owner", "password": "correct horse battery"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie after setup")
	}
	me := h.withSession(http.MethodGet, "/api/v1/me", nil, cookies)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"role":"admin"`) || !strings.Contains(me.Body.String(), "owner@example.com") {
		t.Fatalf("me after setup: %d %s", me.Code, me.Body.String())
	}
	// Setup is one-shot.
	rec = h.jsonPost("/auth/local/setup", map[string]any{"email": "second@example.com", "password": "another long password"}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second setup should 404, got %d", rec.Code)
	}
}

// TestLocalAuthLoginLockoutAndTOTP covers the password path end to end:
// wrong password → same 401 as unknown email; lockout after 10; TOTP
// enrollment; second-step login; recovery code; password change revokes
// other sessions.
func TestLocalAuthLoginLockoutAndTOTP(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	u, err := h.store.CreateLocalUser(ctx, "alice@example.com", "Alice", "alice-password-12", false)
	if err != nil {
		t.Fatal(err)
	}

	bad := h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "wrong-password-1"}, nil)
	unknown := h.jsonPost("/auth/local/login", map[string]any{"email": "nobody@example.com", "password": "wrong-password-1"}, nil)
	strip := func(r *httptest.ResponseRecorder) string {
		m := decode(t, r)["error"].(map[string]any)
		delete(m, "request_id")
		b, _ := json.Marshal(m)
		return string(b)
	}
	if bad.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized || strip(bad) != strip(unknown) {
		t.Fatalf("wrong vs unknown must be indistinguishable: %d %s / %d %s", bad.Code, bad.Body.String(), unknown.Code, unknown.Body.String())
	}

	ok := h.jsonPost("/auth/local/login", map[string]any{"email": "ALICE@example.com", "password": "alice-password-12", "redirect_to": "/models"}, nil)
	if ok.Code != http.StatusOK || decode(t, ok)["redirect_to"] != "/models" {
		t.Fatalf("login: %d %s", ok.Code, ok.Body.String())
	}
	cookies := ok.Result().Cookies()

	// Lockout: 9 more wrong (1 already) → 10th locks.
	for i := 0; i < 8; i++ {
		h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "wrong-password-1"}, nil)
	}
	// Successful login above reset the counter, so we need 10 fresh failures.
	var last *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		last = h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "wrong-password-1"}, nil)
	}
	if last.Code != http.StatusLocked {
		t.Fatalf("expected 423 after 10 failures, got %d %s", last.Code, last.Body.String())
	}
	if r := h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12"}, nil); r.Code != http.StatusLocked {
		t.Fatalf("right password during lockout must still be refused: %d", r.Code)
	}
	if err := h.store.SetPassword(ctx, u.ID, "alice-password-12", false); err != nil { // clears lock
		t.Fatal(err)
	}

	// Enroll TOTP.
	start := h.withSession(http.MethodPost, "/api/v1/me/local/totp/start", map[string]any{}, cookies)
	if start.Code != http.StatusOK {
		t.Fatalf("totp start: %d %s", start.Code, start.Body.String())
	}
	secret := decode(t, start)["secret"].(string)
	if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/confirm", map[string]any{"code": "000000"}, cookies); r.Code != http.StatusBadRequest {
		t.Fatalf("bad confirm code accepted: %d", r.Code)
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	confirm := h.withSession(http.MethodPost, "/api/v1/me/local/totp/confirm", map[string]any{"code": code}, cookies)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", confirm.Code, confirm.Body.String())
	}
	codes := decode(t, confirm)["recovery_codes"].([]any)
	if len(codes) != 10 {
		t.Fatalf("recovery codes: %d", len(codes))
	}
	meLocal := h.withSession(http.MethodGet, "/api/v1/me/local", nil, cookies)
	if m := decode(t, meLocal); m["totp_enabled"] != true || m["recovery_codes_left"] != float64(10) {
		t.Fatalf("me/local: %v", m)
	}

	// Login now needs the second step.
	step1 := h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12"}, nil)
	if step1.Code != http.StatusOK || decode(t, step1)["totp_required"] != true || len(step1.Result().Cookies()) != 0 {
		t.Fatalf("step1: %d %s cookies=%d", step1.Code, step1.Body.String(), len(step1.Result().Cookies()))
	}
	pending := decode(t, step1)["pending"].(string)
	if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "123456"}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("wrong totp accepted: %d", r.Code)
	}
	code, _ = totp.GenerateCode(secret, time.Now())
	step2 := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": code}, nil)
	if step2.Code != http.StatusOK || len(step2.Result().Cookies()) == 0 {
		t.Fatalf("step2: %d %s", step2.Code, step2.Body.String())
	}
	if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": code}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("pending token must be single-use: %d", r.Code)
	}

	// Recovery code works once.
	step1 = h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12"}, nil)
	pending = decode(t, step1)["pending"].(string)
	rc := codes[0].(string)
	if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": rc}, nil); r.Code != http.StatusOK {
		t.Fatalf("recovery code: %d %s", r.Code, r.Body.String())
	}
	step1 = h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12"}, nil)
	pending = decode(t, step1)["pending"].(string)
	if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": rc}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("recovery code reused: %d", r.Code)
	}

	// Password change: wrong current refused; right one revokes the other session.
	sess2 := step2.Result().Cookies()
	if r := h.withSession(http.MethodPut, "/api/v1/me/local/password", map[string]any{"current_password": "nope-nope-nope-1", "new_password": "alice-new-password-1"}, cookies); r.Code != http.StatusForbidden {
		t.Fatalf("wrong current accepted: %d", r.Code)
	}
	if r := h.withSession(http.MethodPut, "/api/v1/me/local/password", map[string]any{"current_password": "alice-password-12", "new_password": "alice-new-password-1"}, cookies); r.Code != http.StatusOK {
		t.Fatalf("change: %d %s", r.Code, r.Body.String())
	}
	if r := h.withSession(http.MethodGet, "/api/v1/me", nil, sess2); r.Code != http.StatusUnauthorized {
		t.Fatalf("other session should be revoked after password change: %d", r.Code)
	}
	if _, _, err := h.store.CheckPassword(ctx, "alice@example.com", "alice-new-password-1"); err != store.ErrTOTPRequired {
		t.Fatalf("new password not in effect: %v", err)
	}
}

// TestLocalAuthAdminFlows: admin creates a local user with a temporary
// password (must_change), seat limit applies, reset password, reset TOTP.
func TestLocalAuthAdminFlows(t *testing.T) {
	h := newHarness(t)
	promote(t, h)

	rec := h.do(http.MethodPost, "/api/v1/admin/users/local", map[string]any{"email": "bob@example.com", "name": "Bob", "password": "temporary-pass-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	bobID := decode(t, rec)["id"].(string)
	login := h.jsonPost("/auth/local/login", map[string]any{"email": "bob@example.com", "password": "temporary-pass-1"}, nil)
	if login.Code != http.StatusOK || decode(t, login)["redirect_to"] != "/settings?password=change" {
		t.Fatalf("temp password should route to change: %d %s", login.Code, login.Body.String())
	}
	bob := login.Result().Cookies()
	// must_change: no current password needed.
	if r := h.withSession(http.MethodPut, "/api/v1/me/local/password", map[string]any{"new_password": "bobs-own-password-1"}, bob); r.Code != http.StatusOK {
		t.Fatalf("must_change change: %d %s", r.Code, r.Body.String())
	}
	if r := h.jsonPost("/auth/local/login", map[string]any{"email": "bob@example.com", "password": "bobs-own-password-1"}, nil); r.Code != http.StatusOK || decode(t, r)["redirect_to"] != "/dashboard" {
		t.Fatalf("after change: %d %s", r.Code, r.Body.String())
	}

	// Duplicate email refused.
	if r := h.do(http.MethodPost, "/api/v1/admin/users/local", map[string]any{"email": "bob@example.com", "password": "temporary-pass-1"}); r.Code != http.StatusBadRequest {
		t.Fatalf("duplicate: %d", r.Code)
	}

	// Admin reset → must_change again and sessions revoked.
	if r := h.do(http.MethodPost, "/api/v1/admin/users/"+bobID+"/password", map[string]any{"password": "reset-by-admin-1"}); r.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", r.Code, r.Body.String())
	}
	if r := h.withSession(http.MethodGet, "/api/v1/me", nil, bob); r.Code != http.StatusUnauthorized {
		t.Fatalf("session should be revoked after admin reset: %d", r.Code)
	}
	if r := h.do(http.MethodDelete, "/api/v1/admin/users/"+bobID+"/totp", nil); r.Code != http.StatusOK {
		t.Fatalf("reset totp: %d", r.Code)
	}

	// Seat limit: 1-seat business key → creating another local user is 402.
	exp := time.Now().Add(24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-1SEAT", Org: "T", IssuedTo: "t@t", Edition: license.EditionBusiness,
		Seats: 1, Nodes: 0, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	if r := h.do(http.MethodPost, "/api/v1/admin/users/local", map[string]any{"email": "carol@example.com", "password": "temporary-pass-1"}); r.Code != http.StatusPaymentRequired {
		t.Fatalf("seat-exhausted create should be 402, got %d %s", r.Code, r.Body.String())
	}
}
