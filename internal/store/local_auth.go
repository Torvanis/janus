package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"golang.org/x/crypto/bcrypt"
)

// Local accounts: email + password (+ optional TOTP) held by Janus itself.
//
// They are a second identity source next to OIDC, not a replacement:
// auth_provider_id is "local|<user id>" so seats, roles, teams, grants and
// audit all work unchanged. Trials and small shops sign in this way; the
// moment an IdP is configured a local account keeps working (an admin
// break-glass) unless it is disabled.
var localAuthMigration = migration{name: "0036_local_auth", stmt: []string{
	`CREATE TABLE IF NOT EXISTS local_credential (
		user_id TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		totp_secret_enc TEXT NOT NULL DEFAULT '',
		totp_confirmed_at TEXT NOT NULL DEFAULT '',
		recovery_codes TEXT NOT NULL DEFAULT '',
		failed_attempts INTEGER NOT NULL DEFAULT 0,
		locked_until TEXT NOT NULL DEFAULT '',
		must_change INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
}}

// LocalAuthPrefix marks a user provisioned by local auth in auth_provider_id.
const LocalAuthPrefix = "local|"

// Password policy: length-only. NIST 800-63B: no composition rules, no
// rotation; check length, check the breach list later if wanted.
const (
	PasswordMinLen = 12
	PasswordMaxLen = 128
	bcryptCost     = 12
	// Lockout: 10 failures → 15 minutes. Same numbers as the customer portal.
	lockAfter   = 10
	lockPeriod  = 15 * time.Minute
	recoveryLen = 10
)

// ErrLocked is returned while a credential is locked out. The message is the
// one the user sees.
var ErrLocked = errors.New("too many failed sign-in attempts; try again in 15 minutes")

// ErrBadCredentials is the single answer to wrong email, wrong password and
// unknown account alike, so the sign-in form cannot be used to enumerate.
var ErrBadCredentials = errors.New("email or password is incorrect")

// ErrTOTPRequired means the password was right and a second factor is needed.
var ErrTOTPRequired = errors.New("verification code required")

// LocalCredential is a user's local sign-in record (hashes never leave the store).
type LocalCredential struct {
	UserID         string
	TOTPEnabled    bool
	MustChange     bool
	FailedAttempts int
	LockedUntil    time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	passwordHash   string
	totpSecretEnc  string
	recoveryCodes  string
}

// ValidatePassword applies the policy; the message is user-facing.
func ValidatePassword(pw string) error {
	if n := len(pw); n < PasswordMinLen || n > PasswordMaxLen {
		return fmt.Errorf("password must be %d–%d characters", PasswordMinLen, PasswordMaxLen)
	}
	return nil
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CreateLocalUser provisions a user with a password. Role is admin only when
// the caller says so (bootstrap or an admin creating another admin).
func (s *Store) CreateLocalUser(ctx context.Context, email, name, password string, admin bool) (*User, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, errors.New("a valid email is required")
	}
	if _, err := s.UserByEmail(ctx, email); err == nil {
		return nil, errors.New("an account with that email already exists")
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	id := NewID()
	role := RoleUser
	if admin {
		role = RoleAdmin
	}
	now := FormatTime(nowUTC())
	if name == "" {
		name = email[:strings.Index(email, "@")]
	}
	return s.withTxUser(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO app_user (id, auth_provider_id, email, name, role, is_active, created_at, last_login_at) VALUES (?, ?, ?, ?, ?, 1, ?, '')`),
			id, LocalAuthPrefix+id, email, name, role, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO local_credential (user_id, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`), id, hash, now, now)
		return err
	}, id)
}

func (s *Store) withTxUser(ctx context.Context, fn func(tx *sql.Tx) error, id string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("create local user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, id)
}

// UserByEmail finds a user by (case-insensitive) email.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userColumns+` FROM app_user WHERE LOWER(email) = ? ORDER BY created_at LIMIT 1`, strings.ToLower(strings.TrimSpace(email))).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by email: %w", err)
	}
	return u, nil
}

// LocalCredential loads the credential for a user, or ErrNotFound.
func (s *Store) LocalCredential(ctx context.Context, userID string) (*LocalCredential, error) {
	var c LocalCredential
	var confirmed, locked, created, updated string
	var mustChange int
	err := s.queryRow(ctx, `SELECT user_id, password_hash, totp_secret_enc, totp_confirmed_at, recovery_codes, failed_attempts, locked_until, must_change, created_at, updated_at FROM local_credential WHERE user_id = ?`, userID).
		Scan(&c.UserID, &c.passwordHash, &c.totpSecretEnc, &confirmed, &c.recoveryCodes, &c.FailedAttempts, &locked, &mustChange, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load local credential: %w", err)
	}
	c.TOTPEnabled = confirmed != ""
	c.MustChange = mustChange == 1
	c.LockedUntil = ParseTime(locked)
	c.CreatedAt = ParseTime(created)
	c.UpdatedAt = ParseTime(updated)
	return &c, nil
}

// HasLocalCredentials reports whether any local account exists — the login
// page uses it to decide whether to show the password form.
func (s *Store) HasLocalCredentials(ctx context.Context) (bool, error) {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM local_credential`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CheckPassword verifies email+password with lockout. On success it returns
// the user and credential; when TOTP is enabled it returns ErrTOTPRequired
// together with the user so the caller can move to the second step. Any
// other failure is ErrBadCredentials (or ErrLocked), never a hint about
// which part was wrong.
func (s *Store) CheckPassword(ctx context.Context, email, password string) (*User, *LocalCredential, error) {
	u, err := s.UserByEmail(ctx, email)
	if err != nil {
		// Burn the same time as a real compare so timing does not reveal
		// whether the email exists.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$C6UzMDM.H6dfI/f/IKcEeO7l0f4bYyHbZ1Uu2YyYbIi.mZGyAO4Ba"), []byte(password))
		return nil, nil, ErrBadCredentials
	}
	c, err := s.LocalCredential(ctx, u.ID)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$C6UzMDM.H6dfI/f/IKcEeO7l0f4bYyHbZ1Uu2YyYbIi.mZGyAO4Ba"), []byte(password))
		return nil, nil, ErrBadCredentials
	}
	now := nowUTC()
	if !c.LockedUntil.IsZero() && now.Before(c.LockedUntil) {
		return nil, nil, ErrLocked
	}
	if bcrypt.CompareHashAndPassword([]byte(c.passwordHash), []byte(password)) != nil {
		c.FailedAttempts++
		locked := ""
		if c.FailedAttempts >= lockAfter {
			locked = FormatTime(now.Add(lockPeriod))
			c.FailedAttempts = 0
		}
		_ = s.exec(ctx, `UPDATE local_credential SET failed_attempts = ?, locked_until = ?, updated_at = ? WHERE user_id = ?`, c.FailedAttempts, locked, FormatTime(now), u.ID)
		if locked != "" {
			return nil, nil, ErrLocked
		}
		return nil, nil, ErrBadCredentials
	}
	if c.FailedAttempts != 0 || !c.LockedUntil.IsZero() {
		_ = s.exec(ctx, `UPDATE local_credential SET failed_attempts = 0, locked_until = '' WHERE user_id = ?`, u.ID)
	}
	if !u.IsActive {
		return nil, nil, ErrBadCredentials
	}
	if c.TOTPEnabled {
		return u, c, ErrTOTPRequired
	}
	return u, c, nil
}

// SetPassword replaces the password (admin reset or self-service change) and
// clears any lockout. mustChange forces a change at next sign-in — used for
// admin-issued temporary passwords.
func (s *Store) SetPassword(ctx context.Context, userID, password string, mustChange bool) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	now := FormatTime(nowUTC())
	mc := 0
	if mustChange {
		mc = 1
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE local_credential SET password_hash = ?, must_change = ?, failed_attempts = 0, locked_until = '', updated_at = ? WHERE user_id = ?`), hash, mc, now, userID)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// User exists (OIDC) but has no local credential yet: adding a
		// password makes the account also usable locally (break-glass).
		_, err = s.db.ExecContext(ctx, s.rebind(`INSERT INTO local_credential (user_id, password_hash, must_change, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`), userID, hash, mc, now, now)
		if err != nil {
			return fmt.Errorf("add local credential: %w", err)
		}
	}
	return nil
}

// RemoveLocalCredential deletes the password/TOTP for a user (the user row
// stays; an OIDC-linked user keeps signing in via the IdP).
func (s *Store) RemoveLocalCredential(ctx context.Context, userID string) error {
	return s.exec(ctx, `DELETE FROM local_credential WHERE user_id = ?`, userID)
}

// TOTP -----------------------------------------------------------------------

// StageTOTP stores a new, unconfirmed secret (encrypted at rest). Enrollment
// completes with ConfirmTOTP once the user proves they can produce a code.
func (s *Store) StageTOTP(ctx context.Context, c *crypto.Cipher, userID, secret string) error {
	enc, err := c.Encrypt(secret)
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE local_credential SET totp_secret_enc = ?, totp_confirmed_at = '', updated_at = ? WHERE user_id = ?`, enc, FormatTime(nowUTC()), userID)
}

// TOTPSecret returns the decrypted secret (staged or confirmed) or "".
func (s *Store) TOTPSecret(ctx context.Context, c *crypto.Cipher, userID string) (string, error) {
	cred, err := s.LocalCredential(ctx, userID)
	if err != nil {
		return "", err
	}
	if cred.totpSecretEnc == "" {
		return "", nil
	}
	return c.Decrypt(cred.totpSecretEnc)
}

// ConfirmTOTP marks enrollment complete and issues recovery codes (returned
// once in clear; stored hashed).
func (s *Store) ConfirmTOTP(ctx context.Context, userID string) ([]string, error) {
	codes := make([]string, recoveryLen)
	hashes := make([]string, recoveryLen)
	for i := range codes {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		codes[i] = strings.ToLower(base64.RawURLEncoding.EncodeToString(b))[:8]
		h, err := bcrypt.GenerateFromPassword([]byte(codes[i]), 6)
		if err != nil {
			return nil, err
		}
		hashes[i] = string(h)
	}
	now := FormatTime(nowUTC())
	if err := s.exec(ctx, `UPDATE local_credential SET totp_confirmed_at = ?, recovery_codes = ?, updated_at = ? WHERE user_id = ?`, now, strings.Join(hashes, "\n"), now, userID); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTOTP removes the second factor.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	return s.exec(ctx, `UPDATE local_credential SET totp_secret_enc = '', totp_confirmed_at = '', recovery_codes = '', updated_at = ? WHERE user_id = ?`, FormatTime(nowUTC()), userID)
}

// UseRecoveryCode consumes one recovery code; false when none matched.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	cred, err := s.LocalCredential(ctx, userID)
	if err != nil {
		return false, err
	}
	code = strings.ToLower(strings.TrimSpace(code))
	lines := strings.Split(cred.recoveryCodes, "\n")
	for i, h := range lines {
		if h == "" {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(code)) == nil {
			lines[i] = ""
			return true, s.exec(ctx, `UPDATE local_credential SET recovery_codes = ?, updated_at = ? WHERE user_id = ?`, strings.Join(lines, "\n"), FormatTime(nowUTC()), userID)
		}
	}
	return false, nil
}

// RecoveryCodesLeft counts unused recovery codes.
func (c *LocalCredential) RecoveryCodesLeft() int {
	n := 0
	for _, h := range strings.Split(c.recoveryCodes, "\n") {
		if h != "" {
			n++
		}
	}
	return n
}
