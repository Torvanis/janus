package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// pending_login holds password-verified local sign-ins that still owe a
// TOTP code. It lives in the database, not process memory, so a second
// replica behind the same Service can complete a sign-in the first one
// started (Business: ha). Only a hash of the token is stored.
var pendingLoginMigration = migration{name: "0038_pending_login", stmt: []string{
	`CREATE TABLE IF NOT EXISTS pending_login (
		token_hash TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		expires_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_pending_login_expires ON pending_login(expires_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_pending_login_expires`,
	`DROP TABLE IF EXISTS pending_login`,
}}

// PendingLoginMaxAttempts is how many wrong codes a pending token survives.
const PendingLoginMaxAttempts = 5

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// PutPendingLogin records a token for userID valid until expires. Expired
// rows are swept opportunistically so the table never needs a job.
func (s *Store) PutPendingLogin(ctx context.Context, token, userID string, expires time.Time) error {
	_ = s.exec(ctx, `DELETE FROM pending_login WHERE expires_at < ?`, FormatTime(nowUTC()))
	if err := s.exec(ctx, `INSERT INTO pending_login (token_hash, user_id, attempts, expires_at) VALUES (?,?,0,?)`,
		hashToken(token), userID, FormatTime(expires)); err != nil {
		return fmt.Errorf("put pending login: %w", err)
	}
	return nil
}

// TakePendingLogin returns the user for a token and counts one attempt.
// success consumes the row; so does the PendingLoginMaxAttempts-th attempt.
// Unknown, expired or exhausted tokens return ok=false.
func (s *Store) TakePendingLogin(ctx context.Context, token string, success bool) (userID string, ok bool, err error) {
	h := hashToken(token)
	var expires string
	var attempts int
	row := s.queryRow(ctx, `UPDATE pending_login SET attempts = attempts + 1 WHERE token_hash = ? RETURNING user_id, attempts, expires_at`, h)
	if err := row.Scan(&userID, &attempts, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("take pending login: %w", err)
	}
	if !ParseTime(expires).After(nowUTC()) {
		_ = s.exec(ctx, `DELETE FROM pending_login WHERE token_hash = ?`, h)
		return "", false, nil
	}
	if success || attempts >= PendingLoginMaxAttempts {
		_ = s.exec(ctx, `DELETE FROM pending_login WHERE token_hash = ?`, h)
	}
	if attempts > PendingLoginMaxAttempts {
		return "", false, nil
	}
	return userID, true, nil
}
