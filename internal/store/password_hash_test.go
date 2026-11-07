package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/crypto"
	"golang.org/x/crypto/bcrypt"
)

// legacyHashes were written by the pre-hardening code (commit fc50f8f):
// a bcrypt cost-12 password hash and a bcrypt cost-6 recovery code, exactly
// as they sit in existing local_credential rows.
type legacyHashes struct {
	Password     string `json:"password"`
	PasswordHash string `json:"password_hash"`
	RecoveryCode string `json:"recovery_code"`
	RecoveryHash string `json:"recovery_hash"`
}

func loadLegacyHashes(t *testing.T) legacyHashes {
	t.Helper()
	raw, err := os.ReadFile("testdata/legacy_bcrypt_fc50f8f.json")
	if err != nil {
		t.Fatal(err)
	}
	var l legacyHashes
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func testCipher(t testing.TB) *crypto.Cipher {
	t.Helper()
	c, err := crypto.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func storedHash(t *testing.T, s *Store, userID string) string {
	t.Helper()
	c, err := s.LocalCredential(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return c.passwordHash
}

func TestHashPasswordFormat(t *testing.T) {
	h, err := hashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$pbkdf2-sha512$i=400000$") {
		t.Fatalf("hash %q is not self-describing PBKDF2-SHA512 at 400000 iterations", h)
	}
	if pbkdf2Iterations < 210_000 {
		t.Fatal("iterations below the OWASP minimum for PBKDF2-HMAC-SHA-512")
	}
	iter, salt, key, err := parsePBKDF2(h)
	if err != nil || iter != pbkdf2Iterations || len(salt) != 16 || len(key) != 64 {
		t.Fatalf("parse: iter=%d salt=%d key=%d err=%v", iter, len(salt), len(key), err)
	}
	h2, _ := hashPassword("correct-horse-battery-staple")
	if h == h2 {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
	if ok, rehash := verifyPassword(h, "correct-horse-battery-staple"); !ok || rehash {
		t.Fatalf("verify current hash: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := verifyPassword(h, "correct-horse-battery-stapl"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	for _, h := range []string{
		"", "plain", "$pbkdf2-sha512$i=0$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$pbkdf2-sha512$i=99999999999$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$pbkdf2-sha512$i=1000$AAAA$AAAAAAAAAAAAAAAAAAAAAA", // salt too short
		"$pbkdf2-sha256$i=1000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$pbkdf2-sha512$1000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
	} {
		if ok, _ := verifyPassword(h, "anything-at-all"); ok {
			t.Fatalf("malformed hash %q verified", h)
		}
	}
}

func TestWeakerPBKDF2IsUpgraded(t *testing.T) {
	salt := bytes.Repeat([]byte{1}, 16)
	key, err := pbkdf2Derive("pw-pw-pw-pw-pw", salt, 1000)
	if err != nil {
		t.Fatal(err)
	}
	h := "$pbkdf2-sha512$i=1000$AQEBAQEBAQEBAQEBAQEBAQ$" + b64(key)
	if ok, rehash := verifyPassword(h, "pw-pw-pw-pw-pw"); !ok || !rehash {
		t.Fatalf("low-iteration hash: ok=%v rehash=%v, want true/true", ok, rehash)
	}
}

// TestLegacyBcryptLoginRehashes proves an account created by an older
// release keeps signing in and is moved to PBKDF2 on that sign-in, after
// which the old password still works and a wrong one still fails.
func TestLegacyBcryptLoginRehashes(t *testing.T) {
	if !legacyBcryptCompiled {
		t.Skip("bcrypt is not compiled into the Janus FIPS edition; see TestFIPSEditionRefusesLegacyBcrypt")
	}
	s := newTestStore(t)
	ctx := context.Background()
	legacy := loadLegacyHashes(t)
	u, err := s.CreateLocalUser(ctx, "old@example.com", "Old", "temporary-password-1", false)
	if err != nil {
		t.Fatal(err)
	}
	// Put the row back into its pre-upgrade state.
	if err := s.exec(ctx, `UPDATE local_credential SET password_hash = ? WHERE user_id = ?`, legacy.PasswordHash, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CheckPassword(ctx, "old@example.com", "not-the-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password against legacy hash: %v", err)
	}
	if storedHash(t, s, u.ID) != legacy.PasswordHash {
		t.Fatal("a failed sign-in must not rewrite the hash")
	}
	if _, _, err := s.CheckPassword(ctx, "old@example.com", legacy.Password); err != nil {
		t.Fatalf("legacy bcrypt password no longer signs in: %v", err)
	}
	h := storedHash(t, s, u.ID)
	if !strings.HasPrefix(h, "$pbkdf2-sha512$i=400000$") {
		t.Fatalf("hash after legacy sign-in = %q, want PBKDF2", h)
	}
	if _, _, err := s.CheckPassword(ctx, "old@example.com", legacy.Password); err != nil {
		t.Fatalf("sign-in after rehash: %v", err)
	}
	if storedHash(t, s, u.ID) != h {
		t.Fatal("a current hash was rewritten on a normal sign-in")
	}
	if _, _, err := s.CheckPassword(ctx, "old@example.com", "not-the-password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password after rehash: %v", err)
	}
}

func TestNewAccountsAndResetsUsePBKDF2(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.CreateLocalUser(ctx, "new@example.com", "New", "first-password-12", false)
	if err != nil {
		t.Fatal(err)
	}
	if h := storedHash(t, s, u.ID); !strings.HasPrefix(h, "$pbkdf2-sha512$") {
		t.Fatalf("CreateLocalUser stored %q", h)
	}
	if err := s.SetPassword(ctx, u.ID, "second-password-12", false); err != nil {
		t.Fatal(err)
	}
	if h := storedHash(t, s, u.ID); !strings.HasPrefix(h, "$pbkdf2-sha512$") {
		t.Fatalf("SetPassword stored %q", h)
	}
	if _, _, err := s.CheckPassword(ctx, "new@example.com", "second-password-12"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryCodesHMACAndLegacy(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := testCipher(t)
	legacy := loadLegacyHashes(t)
	u, err := s.CreateLocalUser(ctx, "rc@example.com", "RC", "recovery-password-1", false)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := s.ConfirmTOTP(ctx, c, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := s.LocalCredential(ctx, u.ID)
	for _, line := range strings.Split(cred.recoveryCodes, "\n") {
		if !strings.HasPrefix(line, "hmac-sha256$") {
			t.Fatalf("stored recovery code %q is not HMAC", line)
		}
		for _, code := range codes {
			if strings.Contains(line, code) {
				t.Fatal("recovery code stored in clear")
			}
		}
	}
	// A code is bound to its user: another account's row does not accept it.
	other, _ := s.CreateLocalUser(ctx, "other@example.com", "Other", "recovery-password-2", false)
	if err := s.exec(ctx, `UPDATE local_credential SET recovery_codes = ? WHERE user_id = ?`, cred.recoveryCodes, other.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, c, other.ID, codes[0]); ok {
		t.Fatal("recovery code copied to another user was accepted")
	}
	// A different instance key does not accept it either.
	c2, _ := crypto.New(bytes.Repeat([]byte{0x43}, 32))
	if ok, _ := s.UseRecoveryCode(ctx, c2, u.ID, codes[0]); ok {
		t.Fatal("recovery code accepted under a different encryption key")
	}
	if ok, err := s.UseRecoveryCode(ctx, c, u.ID, strings.ToUpper(codes[0])); !ok || err != nil {
		t.Fatalf("use code: %v %v", ok, err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, c, u.ID, codes[0]); ok {
		t.Fatal("a recovery code worked twice")
	}
	cred, _ = s.LocalCredential(ctx, u.ID)
	if cred.RecoveryCodesLeft() != recoveryLen-1 {
		t.Fatalf("left = %d", cred.RecoveryCodesLeft())
	}

	// Legacy bcrypt recovery codes from older releases still work, once
	// (default edition; the FIPS edition refuses them, see
	// TestFIPSEditionRefusesLegacyBcrypt).
	if !legacyBcryptCompiled {
		return
	}
	mixed := legacy.RecoveryHash + "\n" + hashRecoveryCode(c.DeriveKey(recoveryKeyLabel), u.ID, "zzzz9999")
	if err := s.exec(ctx, `UPDATE local_credential SET recovery_codes = ? WHERE user_id = ?`, mixed, u.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UseRecoveryCode(ctx, c, u.ID, legacy.RecoveryCode); !ok || err != nil {
		t.Fatalf("legacy bcrypt recovery code rejected: %v %v", ok, err)
	}
	cred, _ = s.LocalCredential(ctx, u.ID)
	if strings.Contains(cred.recoveryCodes, "$2a$") || cred.RecoveryCodesLeft() != 1 {
		t.Fatalf("legacy code not consumed: %q", cred.recoveryCodes)
	}
	if ok, _ := s.UseRecoveryCode(ctx, c, u.ID, legacy.RecoveryCode); ok {
		t.Fatal("legacy recovery code worked twice")
	}
	if ok, _ := s.UseRecoveryCode(ctx, c, u.ID, "zzzz9999"); !ok {
		t.Fatal("HMAC code next to a legacy code rejected")
	}
}

// TestLegacyBcryptFixtureIsReal guards the fixture itself.
func TestLegacyBcryptFixtureIsReal(t *testing.T) {
	l := loadLegacyHashes(t)
	if cost, err := bcrypt.Cost([]byte(l.PasswordHash)); err != nil || cost != 12 {
		t.Fatalf("fixture password hash cost=%d err=%v", cost, err)
	}
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := OpenMigratedSQLite(context.Background(), filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// Login latency benchmarks (go test -run '^$' -bench Login ./internal/store/).
// Legacy = a bcrypt cost-12 row, the format every existing account has today;
// PBKDF2 = the format after the first sign-in; Unknown = a wrong email, which
// must cost about the same as PBKDF2 (dummy-hash timing equalization).
func benchLogin(b *testing.B, legacy, unknownEmail bool) {
	if legacy && !legacyBcryptCompiled {
		b.Skip("no bcrypt in the FIPS edition")
	}
	s := newBenchStore(b)
	ctx := context.Background()
	u, err := s.CreateLocalUser(ctx, "bench@example.com", "B", "bench-password-12", false)
	if err != nil {
		b.Fatal(err)
	}
	var bh string
	if legacy {
		h, _ := bcrypt.GenerateFromPassword([]byte("bench-password-12"), 12)
		bh = string(h)
	}
	email := "bench@example.com"
	if unknownEmail {
		email = "nobody@example.com"
	}
	burnPasswordTime("warm-up") // build the dummy hash outside the timer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if legacy {
			b.StopTimer()
			_ = s.exec(ctx, `UPDATE local_credential SET password_hash = ? WHERE user_id = ?`, bh, u.ID)
			b.StartTimer()
		}
		_, _, err := s.CheckPassword(ctx, email, "bench-password-12")
		if unknownEmail != (err != nil) {
			b.Fatalf("CheckPassword: %v", err)
		}
	}
}

func BenchmarkLoginLegacyBcryptWithRehash(b *testing.B) { benchLogin(b, true, false) }
func BenchmarkLoginPBKDF2(b *testing.B)                 { benchLogin(b, false, false) }
func BenchmarkLoginUnknownEmail(b *testing.B)           { benchLogin(b, false, true) }

// BenchmarkLoginBcryptVerifyOnly is the pre-hardening cost of one sign-in
// (bcrypt cost 12 compare, no rehash).
func BenchmarkLoginBcryptVerifyOnly(b *testing.B) {
	h, _ := bcrypt.GenerateFromPassword([]byte("bench-password-12"), 12)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bcrypt.CompareHashAndPassword(h, []byte("bench-password-12"))
	}
}
