package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/store"
)

// TestTOTPAlgorithmPerEdition: the default edition enrolls HMAC-SHA-1 (every
// authenticator app); the FIPS edition enrolls HMAC-SHA-256 and says so in
// the otpauth URL so apps that honor the parameter compute the right codes.
func TestTOTPAlgorithmPerEdition(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DevAuthEnabled = false
	h.handler = h.server.Handler()
	ctx := context.Background()
	if _, err := h.store.CreateLocalUser(ctx, "algo@example.com", "Algo", "algo-password-12", false); err != nil {
		t.Fatal(err)
	}
	login := h.jsonPost("/auth/local/login", map[string]any{"email": "algo@example.com", "password": "algo-password-12"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookies := login.Result().Cookies()
	start := h.withSession(http.MethodPost, "/api/v1/me/local/totp/start", map[string]any{}, cookies)
	body := decode(t, start)
	want := "SHA1"
	if fipsmode.IsFIPSEdition() {
		want = "SHA256"
	}
	if body["algorithm"] != want || !strings.Contains(body["otpauth_url"].(string), "algorithm="+want) {
		t.Fatalf("edition %s enrolled %v (%v), want %s", fipsmode.Edition(), body["algorithm"], body["otpauth_url"], want)
	}
	secret := body["secret"].(string)
	if fipsmode.IsFIPSEdition() {
		// A SHA-1 code for the same secret must not pass.
		var sha1Code string
		sha1Code = "000000"
		if !fipsmode.Enforced() {
			sha1Code, _ = totp.GenerateCodeCustom(secret, time.Now().UTC(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		}
		if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/confirm", map[string]any{"code": sha1Code}, cookies); r.Code != http.StatusBadRequest {
			t.Fatalf("SHA-1 code accepted for a SHA-256 enrollment: %d", r.Code)
		}
	}
	if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/confirm", map[string]any{"code": totpCode(secret)}, cookies); r.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", r.Code, r.Body)
	}
	me := decode(t, h.withSession(http.MethodGet, "/api/v1/me/local", nil, cookies))
	if me["totp_algorithm"] != want || me["totp_reenroll"] != false {
		t.Fatalf("me/local = %v", me)
	}
}

// TestFIPSEditionRefusesLegacySHA1TOTP: an account enrolled with SHA-1 by the
// default edition gets a named refusal (totp_unsupported) on the FIPS edition,
// sees totp_reenroll, and can still get in with a recovery code; the default
// edition keeps accepting it.
func TestFIPSEditionRefusesLegacySHA1TOTP(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DevAuthEnabled = false
	h.handler = h.server.Handler()
	ctx := context.Background()
	u, err := h.store.CreateLocalUser(ctx, "legacy@example.com", "Legacy", "legacy-password-1", false)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if err := h.store.StageTOTP(ctx, h.server.Cipher, u.ID, secret, store.TOTPAlgorithmSHA1); err != nil {
		t.Fatal(err)
	}
	codes, err := h.store.ConfirmTOTP(ctx, h.server.Cipher, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	step1 := h.jsonPost("/auth/local/login", map[string]any{"email": "legacy@example.com", "password": "legacy-password-1"}, nil)
	pending := decode(t, step1)["pending"].(string)
	if !fipsmode.IsFIPSEdition() {
		code, _ := totp.GenerateCode(secret, time.Now())
		if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": code}, nil); r.Code != http.StatusOK {
			t.Fatalf("default edition rejected its own SHA-1 enrollment: %d %s", r.Code, r.Body)
		}
		return
	}
	r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "123456"}, nil)
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.Body.String(), "totp_unsupported") {
		t.Fatalf("FIPS edition SHA-1 enrollment: %d %s", r.Code, r.Body)
	}
	step1 = h.jsonPost("/auth/local/login", map[string]any{"email": "legacy@example.com", "password": "legacy-password-1"}, nil)
	pending = decode(t, step1)["pending"].(string)
	r = h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": codes[0]}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("recovery code refused on FIPS edition: %d %s", r.Code, r.Body)
	}
	me := decode(t, h.withSession(http.MethodGet, "/api/v1/me/local", nil, r.Result().Cookies()))
	if me["totp_reenroll"] != true || me["totp_algorithm"] != "SHA1" {
		t.Fatalf("me/local = %v", me)
	}
	st := h.server.cryptographyStatus(ctx)["legacy_credentials"].(map[string]int)
	if st["sha1_totp"] != 1 {
		t.Fatalf("legacy credentials = %v", st)
	}
}
