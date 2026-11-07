package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFIPSEditionRefusesLegacyBcrypt: the FIPS edition does not link bcrypt,
// so an account still on a pre-PBKDF2 bcrypt hash cannot sign in (the same
// "incorrect" answer as any wrong password, never a hint) until an
// administrator resets it, and legacy bcrypt recovery codes are not
// accepted. After a reset the account works normally. CountLegacyCredentials
// tells the admin how many accounts are affected. In the default edition the
// same rows keep working (TestLegacyBcryptLoginRehashes).
func TestFIPSEditionRefusesLegacyBcrypt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := testCipher(t)
	legacy := loadLegacyHashes(t)
	u, err := s.CreateLocalUser(ctx, "old@example.com", "Old", "temporary-password-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE local_credential SET password_hash = ?, recovery_codes = ? WHERE user_id = ?`, legacy.PasswordHash, legacy.RecoveryHash, u.ID); err != nil {
		t.Fatal(err)
	}
	pw, rc, _, err := s.CountLegacyCredentials(ctx, c)
	if err != nil || pw != 1 || rc != 1 {
		t.Fatalf("legacy counts pw=%d rc=%d err=%v", pw, rc, err)
	}
	_, _, err = s.CheckPassword(ctx, "old@example.com", legacy.Password)
	ok, _ := s.UseRecoveryCode(ctx, c, u.ID, legacy.RecoveryCode)
	if legacyBcryptCompiled {
		if err != nil || !ok {
			t.Fatalf("default edition lost legacy bcrypt: password err=%v recovery ok=%v", err, ok)
		}
		return
	}
	if !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("FIPS edition legacy bcrypt sign-in: %v", err)
	}
	if ok {
		t.Fatal("FIPS edition accepted a bcrypt recovery code")
	}
	if h := storedHash(t, s, u.ID); h != legacy.PasswordHash {
		t.Fatal("a refused sign-in rewrote the hash")
	}
	if err := s.SetPassword(ctx, u.ID, "reset-by-admin-12", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CheckPassword(ctx, "old@example.com", "reset-by-admin-12"); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if h := storedHash(t, s, u.ID); !strings.HasPrefix(h, "$pbkdf2-sha512$") {
		t.Fatalf("reset stored %q", h)
	}
	if pw, _, _, _ := s.CountLegacyCredentials(ctx, c); pw != 0 {
		t.Fatalf("legacy password count after reset = %d", pw)
	}
}
