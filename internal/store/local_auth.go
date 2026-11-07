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

// Password policy: the administrator's (Administration → Settings →
// Security; SecurityPolicy.Passwords). PasswordMinLen is the default
// minimum; PasswordMaxLen bounds every policy.
const (
	PasswordMinLen = 12
	PasswordMaxLen = 128
	recoveryLen    = 10
)

// ErrLocked is returned while a sign-in name is locked out. Use
// LockedMessage for the text the person sees.
var ErrLocked = errors.New("too many failed sign-in attempts")

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
	// PasswordChangedAt is when the password was last set (CreatedAt for
	// passwords set before this was recorded).
	PasswordChangedAt time.Time
	// Expired: this sign-in found the password past the policy's maximum
	// age and set MustChange.
	Expired         bool
	passwordHash    string
	totpSecretEnc   string
	recoveryCodes   string
	passwordHistory string
}

// CreateLocalUser provisions a user with a password. Role is admin only when
// the caller says so (bootstrap or an admin creating another admin).
func (s *Store) CreateLocalUser(ctx context.Context, email, name, password string, admin bool) (*User, error) {
	if err := CheckPasswordPolicy(s.SecurityPolicy(ctx).Passwords, password, ""); err != nil {
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
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO local_credential (user_id, password_hash, password_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`), id, hash, now, now, now)
		return err
	}, id)
}

func (s *Store) withTxUser(ctx context.Context, fn func(tx *sql.Tx) error, id string) (*User, error) {
	if err := s.InTx(ctx, func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return fmt.Errorf("create local user: %w", err)
		}
		return nil
	}); err != nil {
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
	var confirmed, locked, created, updated, changed string
	var mustChange int
	err := s.queryRow(ctx, `SELECT user_id, password_hash, totp_secret_enc, totp_confirmed_at, recovery_codes, failed_attempts, locked_until, must_change, created_at, updated_at, password_history, password_changed_at FROM local_credential WHERE user_id = ?`, userID).
		Scan(&c.UserID, &c.passwordHash, &c.totpSecretEnc, &confirmed, &c.recoveryCodes, &c.FailedAttempts, &locked, &mustChange, &created, &updated, &c.passwordHistory, &changed)
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
	c.PasswordChangedAt = ParseTime(changed)
	if c.PasswordChangedAt.IsZero() {
		c.PasswordChangedAt = c.CreatedAt
	}
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
//
// Lockout is per sign-in name (SignInLocked/RecordSignInFailure), so the
// directory path shares it. A wrong password does not count here: the
// caller tries the directory first and records one failure for the whole
// attempt. Use CheckPasswordCounted for a password check that counts.
func (s *Store) CheckPassword(ctx context.Context, email, password string) (*User, *LocalCredential, error) {
	if locked, _ := s.SignInLocked(ctx, email); locked {
		burnPasswordTime(password)
		return nil, nil, ErrLocked
	}
	u, err := s.UserByEmail(ctx, email)
	if err != nil {
		// Burn the same time as a real compare so timing does not reveal
		// whether the email exists.
		burnPasswordTime(password)
		return nil, nil, ErrBadCredentials
	}
	c, err := s.LocalCredential(ctx, u.ID)
	if err != nil {
		burnPasswordTime(password)
		return nil, nil, ErrBadCredentials
	}
	ok, needsRehash := verifyPassword(c.passwordHash, password)
	if !ok {
		return nil, nil, ErrBadCredentials
	}
	_ = s.ClearSignInFailures(ctx, email)
	if pol := s.SecurityPolicy(ctx).Passwords; pol.MaxAgeDays > 0 && !c.MustChange &&
		nowUTC().Sub(c.PasswordChangedAt) > time.Duration(pol.MaxAgeDays)*24*time.Hour {
		if err := s.exec(ctx, `UPDATE local_credential SET must_change = 1 WHERE user_id = ?`, u.ID); err == nil {
			c.MustChange, c.Expired = true, true
		}
	}
	if needsRehash {
		// Transparent upgrade: a legacy bcrypt (or weaker PBKDF2) hash is
		// replaced by a current PBKDF2 hash now that we hold the verified
		// password. The WHERE clause pins the old hash so a concurrent
		// password change is never overwritten. Failure only delays the
		// upgrade to the next sign-in.
		if h, herr := hashPassword(password); herr == nil {
			if err := s.exec(ctx, `UPDATE local_credential SET password_hash = ? WHERE user_id = ? AND password_hash = ?`, h, u.ID, c.passwordHash); err == nil {
				c.passwordHash = h
			} else {
				s.logger().WarnContext(ctx, "upgrade password hash", "user_id", u.ID, "error", err.Error())
			}
		}
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
//
// The administrator's password policy applies. A self-service change
// (mustChange false) also applies the history and minimum age; an
// administrator's temporary password (mustChange true) is exempt from both,
// as the person must replace it at once.
func (s *Store) SetPassword(ctx context.Context, userID, password string, mustChange bool) error {
	return s.setPassword(ctx, userID, password, "", mustChange)
}

// ChangeOwnPassword is a person changing their own password: current is the
// password they typed (for the characters-changed rule; "" when the change
// was forced at sign-in and the current one is not known).
func (s *Store) ChangeOwnPassword(ctx context.Context, userID, current, password string) error {
	return s.setPassword(ctx, userID, password, current, false)
}

// ErrPasswordTooNew refuses a change inside the policy's minimum age.
var ErrPasswordTooNew = errors.New("this password was changed too recently")

func (s *Store) setPassword(ctx context.Context, userID, password, current string, mustChange bool) error {
	pol := s.SecurityPolicy(ctx).Passwords
	if err := CheckPasswordPolicy(pol, password, current); err != nil {
		return err
	}
	prev, err := s.LocalCredential(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if prev != nil && !mustChange {
		if pol.MinAgeHours > 0 && !prev.MustChange && nowUTC().Sub(prev.PasswordChangedAt) < time.Duration(pol.MinAgeHours)*time.Hour {
			return fmt.Errorf("%w: a password can be changed once every %d hours; ask an administrator to reset it if you need to change it now", ErrPasswordTooNew, pol.MinAgeHours)
		}
		if pol.History > 0 {
			for i, h := range append([]string{prev.passwordHash}, prev.history()...) {
				if i > pol.History {
					break
				}
				if ok, _ := verifyPassword(h, password); ok {
					return fmt.Errorf("password was used recently: choose one not among your last %d", pol.History)
				}
			}
		}
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	history := ""
	if prev != nil && pol.History > 0 {
		h := append([]string{prev.passwordHash}, prev.history()...)
		if len(h) > pol.History {
			h = h[:pol.History]
		}
		history = strings.Join(h, "\n")
	}
	now := FormatTime(nowUTC())
	mc := 0
	if mustChange {
		mc = 1
	}
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`UPDATE local_credential SET password_hash = ?, must_change = ?, failed_attempts = 0, locked_until = '', password_history = ?, password_changed_at = ?, updated_at = ? WHERE user_id = ?`), hash, mc, history, now, now, userID)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	// A new password also ends a sign-in lock on the person's name (the
	// administrator's reset is an approved way to unlock an account).
	if u, err := s.UserByID(ctx, userID); err == nil {
		_ = s.exec(ctx, `DELETE FROM sign_in_lockout WHERE login = ?`, normLogin(u.Email))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// User exists (OIDC) but has no local credential yet: adding a
		// password makes the account also usable locally (break-glass).
		_, err = s.runner(ctx).ExecContext(ctx, s.rebind(`INSERT INTO local_credential (user_id, password_hash, must_change, password_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`), userID, hash, mc, now, now, now)
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

// StageTOTP stores a new, unconfirmed secret (encrypted at rest) for the
// given algorithm (TOTPAlgorithmSHA1 or TOTPAlgorithmSHA256). Enrollment
// completes with ConfirmTOTP once the user proves they can produce a code.
func (s *Store) StageTOTP(ctx context.Context, c *crypto.Cipher, userID, secret, algorithm string) error {
	enc, err := c.Encrypt(encodeTOTPSecret(secret, algorithm))
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE local_credential SET totp_secret_enc = ?, totp_confirmed_at = '', updated_at = ? WHERE user_id = ?`, enc, FormatTime(nowUTC()), userID)
}

// TOTPSecret returns the decrypted secret (staged or confirmed) and its
// algorithm, or "" when there is none.
func (s *Store) TOTPSecret(ctx context.Context, c *crypto.Cipher, userID string) (secret, algorithm string, err error) {
	cred, err := s.LocalCredential(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if cred.totpSecretEnc == "" {
		return "", "", nil
	}
	stored, err := c.Decrypt(cred.totpSecretEnc)
	if err != nil {
		return "", "", err
	}
	secret, algorithm = decodeTOTPSecret(stored)
	return secret, algorithm, nil
}

// CountLegacyCredentials reports, for the FIPS edition's admin status, how
// many local accounts still hold material the FIPS edition cannot use:
// bcrypt password hashes, bcrypt recovery codes, and confirmed HMAC-SHA-1
// TOTP enrollments.
func (s *Store) CountLegacyCredentials(ctx context.Context, c *crypto.Cipher) (bcryptPasswords, bcryptRecovery, sha1TOTP int, err error) {
	rows, err := s.runner(ctx).QueryContext(ctx, `SELECT password_hash, recovery_codes, totp_secret_enc, totp_confirmed_at FROM local_credential`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count legacy credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pw, rc, totpEnc, confirmed string
		if err := rows.Scan(&pw, &rc, &totpEnc, &confirmed); err != nil {
			return 0, 0, 0, err
		}
		if isBcryptHash(pw) {
			bcryptPasswords++
		}
		for _, line := range strings.Split(rc, "\n") {
			if isBcryptHash(line) {
				bcryptRecovery++
				break
			}
		}
		if totpEnc != "" && confirmed != "" {
			stored, derr := c.Decrypt(totpEnc)
			if derr == nil {
				if _, alg := decodeTOTPSecret(stored); alg == TOTPAlgorithmSHA1 {
					sha1TOTP++
				}
			}
		}
	}
	return bcryptPasswords, bcryptRecovery, sha1TOTP, rows.Err()
}

// ConfirmTOTP marks enrollment complete and issues recovery codes (returned
// once in clear; stored as keyed HMAC-SHA-256 digests, see password_hash.go).
func (s *Store) ConfirmTOTP(ctx context.Context, c *crypto.Cipher, userID string) ([]string, error) {
	key := c.DeriveKey(recoveryKeyLabel)
	codes := make([]string, recoveryLen)
	hashes := make([]string, recoveryLen)
	for i := range codes {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		codes[i] = strings.ToLower(base64.RawURLEncoding.EncodeToString(b))[:8]
		hashes[i] = hashRecoveryCode(key, userID, codes[i])
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
// Both the current HMAC format and legacy bcrypt codes are accepted; a match
// is removed from the list either way, so a legacy code never survives its
// use. Every stored line is checked (no early exit on a match position) so
// the response time does not reveal which slot matched.
func (s *Store) UseRecoveryCode(ctx context.Context, c *crypto.Cipher, userID, code string) (bool, error) {
	cred, err := s.LocalCredential(ctx, userID)
	if err != nil {
		return false, err
	}
	key := c.DeriveKey(recoveryKeyLabel)
	code = strings.ToLower(strings.TrimSpace(code))
	lines := strings.Split(cred.recoveryCodes, "\n")
	match := -1
	for i, h := range lines {
		if h == "" {
			continue
		}
		if recoveryCodeMatches(key, userID, h, code) && match < 0 {
			match = i
		}
	}
	if match < 0 {
		return false, nil
	}
	old := cred.recoveryCodes
	lines[match] = ""
	// Pin the previous list so two concurrent uses of the same code cannot
	// both succeed.
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`UPDATE local_credential SET recovery_codes = ?, updated_at = ? WHERE user_id = ? AND recovery_codes = ?`), strings.Join(lines, "\n"), FormatTime(nowUTC()), userID, old)
	if err != nil {
		return false, fmt.Errorf("consume recovery code: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	return true, nil
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

func (c *LocalCredential) history() []string {
	if c.passwordHistory == "" {
		return nil
	}
	return strings.Split(c.passwordHistory, "\n")
}

// --- lockout -----------------------------------------------------------------
//
// Failed sign-ins are counted per sign-in name (lower-cased), for local
// accounts and directory accounts alike, and for names that match nobody
// (so the answer never reveals which names exist).

// SignInLock is one locked sign-in name, for the administrator's list.
type SignInLock struct {
	Login       string    `json:"login"`
	Failed      int       `json:"failed"`
	LockedAt    time.Time `json:"locked_at"`
	LockedUntil time.Time `json:"locked_until,omitempty"`
	// UntilUnlocked: the lock lasts until an administrator unlocks it.
	UntilUnlocked bool `json:"until_unlocked"`
}

func normLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

// SignInLocked reports whether login is locked now, and until when (zero:
// until an administrator unlocks it).
func (s *Store) SignInLocked(ctx context.Context, login string) (bool, time.Time) {
	var lockedAt, until string
	err := s.queryRow(ctx, `SELECT locked_at, locked_until FROM sign_in_lockout WHERE login = ?`, normLogin(login)).Scan(&lockedAt, &until)
	if err != nil || lockedAt == "" {
		return false, time.Time{}
	}
	if until == "" {
		return true, time.Time{}
	}
	u := ParseTime(until)
	return nowUTC().Before(u), u
}

// LockedMessage is what a locked person is told.
func LockedMessage(until time.Time) string {
	if until.IsZero() {
		return "This account is locked after too many failed sign-in attempts. Ask an administrator to unlock it."
	}
	mins := int(time.Until(until).Minutes()) + 1
	return fmt.Sprintf("Too many failed sign-in attempts. Try again in %d minute%s.", mins, map[bool]string{true: "", false: "s"}[mins == 1])
}

// RecordSignInFailure counts one failed attempt for login under the
// policy, and reports whether it locked the name now.
func (s *Store) RecordSignInFailure(ctx context.Context, login string, p LockoutPolicy) (bool, error) {
	login = normLogin(login)
	if login == "" {
		return false, nil
	}
	now := nowUTC()
	var failed int
	var first, lockedAt, until string
	err := s.queryRow(ctx, `SELECT failed, first_failed_at, locked_at, locked_until FROM sign_in_lockout WHERE login = ?`, login).Scan(&failed, &first, &lockedAt, &until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// A lock that has run out, or failures outside the window, start over.
	expired := lockedAt != "" && until != "" && !now.Before(ParseTime(until))
	if expired || first == "" || now.Sub(ParseTime(first)) > time.Duration(p.WindowMinutes)*time.Minute {
		failed, first, lockedAt, until = 0, FormatTime(now), "", ""
	}
	if lockedAt != "" {
		return false, nil // already locked
	}
	failed++
	lockedNow := failed >= p.MaxFailures
	if lockedNow {
		lockedAt = FormatTime(now)
		if p.LockMinutes > 0 {
			until = FormatTime(now.Add(time.Duration(p.LockMinutes) * time.Minute))
		}
	}
	err = s.exec(ctx, `INSERT INTO sign_in_lockout (login, failed, first_failed_at, locked_until, locked_at) VALUES (?,?,?,?,?)
		ON CONFLICT (login) DO UPDATE SET failed = excluded.failed, first_failed_at = excluded.first_failed_at, locked_until = excluded.locked_until, locked_at = excluded.locked_at`,
		login, failed, first, until, lockedAt)
	return lockedNow, err
}

// ClearSignInFailures forgets failed attempts after a successful sign-in
// (never an active lock: a locked name cannot sign in).
func (s *Store) ClearSignInFailures(ctx context.Context, login string) error {
	return s.exec(ctx, `DELETE FROM sign_in_lockout WHERE login = ? AND locked_at = ''`, normLogin(login))
}

// UnlockSignIn removes a lock (an administrator's unlock). It reports
// whether the name was locked.
func (s *Store) UnlockSignIn(ctx context.Context, login string) (bool, error) {
	locked, _ := s.SignInLocked(ctx, login)
	if err := s.exec(ctx, `DELETE FROM sign_in_lockout WHERE login = ?`, normLogin(login)); err != nil {
		return false, err
	}
	return locked, nil
}

// LockedSignIns lists names locked now.
func (s *Store) LockedSignIns(ctx context.Context) ([]SignInLock, error) {
	rows, err := s.query(ctx, `SELECT login, failed, locked_at, locked_until FROM sign_in_lockout WHERE locked_at <> '' ORDER BY locked_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	now := nowUTC()
	out := []SignInLock{}
	for rows.Next() {
		var l SignInLock
		var at, until string
		if err := rows.Scan(&l.Login, &l.Failed, &at, &until); err != nil {
			return nil, err
		}
		l.LockedAt = ParseTime(at)
		if until == "" {
			l.UntilUnlocked = true
		} else {
			l.LockedUntil = ParseTime(until)
			if !now.Before(l.LockedUntil) {
				continue
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
