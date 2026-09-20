package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// SCIMToken is safe to return in admin listings: it contains no digest or secret.
type SCIMToken struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	RevokedAt  time.Time `json:"revoked_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// SCIMStoreError carries the protocol error without leaking database details.
type SCIMStoreError struct {
	Status   int
	SCIMType string
	Detail   string
}

func (e *SCIMStoreError) Error() string { return e.Detail }
func scimInvalid(detail string) error   { return &SCIMStoreError{400, "invalidValue", detail} }
func scimConflict(detail string) error  { return &SCIMStoreError{409, "uniqueness", detail} }

const scimTokenColumns = `id,name,prefix,created_at,expires_at,revoked_at,last_used_at`

func scanSCIMToken(scan func(...any) error) (*SCIMToken, error) {
	t := &SCIMToken{}
	var created, expires, revoked, used string
	if err := scan(&t.ID, &t.Name, &t.Prefix, &created, &expires, &revoked, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = ParseTime(created)
	t.ExpiresAt = ParseTime(expires)
	t.RevokedAt = ParseTime(revoked)
	t.LastUsedAt = ParseTime(used)
	return t, nil
}
func (s *Store) CreateSCIMToken(ctx context.Context, name string, expires time.Time) (*SCIMToken, string, error) {
	name = strings.TrimSpace(name)
	now := nowUTC()
	if name == "" || (!expires.IsZero() && !expires.After(now)) {
		return nil, "", scimInvalid("name and a future expiry (if supplied) are required")
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, "", err
	}
	secret := "scim_" + base64.RawURLEncoding.EncodeToString(entropy[:])
	t := &SCIMToken{ID: NewID(), Name: name, Prefix: secret[:13], CreatedAt: now, ExpiresAt: expires}
	exp := ""
	if !expires.IsZero() {
		exp = FormatTime(expires)
	}
	if err := s.exec(ctx, `INSERT INTO scim_token(id,name,token_digest,prefix,created_at,expires_at) VALUES(?,?,?,?,?,?)`, t.ID, name, HashToken(secret), t.Prefix, FormatTime(now), exp); err != nil {
		return nil, "", err
	}
	return t, secret, nil
}
func (s *Store) ListSCIMTokens(ctx context.Context) ([]*SCIMToken, error) {
	rows, err := s.query(ctx, `SELECT `+scimTokenColumns+` FROM scim_token ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SCIMToken{}
	for rows.Next() {
		t, err := scanSCIMToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) RevokeSCIMToken(ctx context.Context, id string) error {
	var found string
	err := s.queryRow(ctx, `UPDATE scim_token SET revoked_at=CASE WHEN revoked_at='' THEN ? ELSE revoked_at END WHERE id=? RETURNING id`, FormatTime(nowUTC()), id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// RotateSCIMToken consumes one live credential and creates its replacement in
// the same transaction. A concurrent rotation/revoke cannot resurrect a token.
func (s *Store) RotateSCIMToken(ctx context.Context, id string) (*SCIMToken, string, error) {
	var next *SCIMToken
	var secret string
	err := s.modelTx(ctx, func(tx *Store) error {
		now := FormatTime(nowUTC())
		old, err := scanSCIMToken(tx.queryRow(ctx, `UPDATE scim_token SET revoked_at=? WHERE id=? AND revoked_at='' AND (expires_at='' OR expires_at>?) RETURNING `+scimTokenColumns, now, id, now).Scan)
		if err != nil {
			return err
		}
		next, secret, err = tx.CreateSCIMToken(ctx, old.Name, old.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return next, secret, nil
}

func (s *Store) AuthenticateSCIMToken(ctx context.Context, presented string) (*SCIMToken, error) {
	if !strings.HasPrefix(presented, "scim_") || len(presented) != 48 {
		return nil, ErrNotFound
	}
	// One conditional write makes revoke/expiry and last-used update atomic.
	now := FormatTime(nowUTC())
	return scanSCIMToken(s.queryRow(ctx, `UPDATE scim_token SET last_used_at=? WHERE token_digest=? AND revoked_at='' AND (expires_at='' OR expires_at>?) RETURNING `+scimTokenColumns, now, HashToken(presented), now).Scan)
}
