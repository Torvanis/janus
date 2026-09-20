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
)

// GenerateServiceToken mints a service credential and returns the plaintext
// value (shown exactly once) alongside its storage digest.
//
// The wire format differs from a user token only by its prefix
// (ServiceTokenPrefix), so an operator reading a config file or a log line can
// tell immediately which kind of principal a credential belongs to. Entropy
// and digest choice match GenerateToken: 256 bits of CSPRNG output hashed with
// SHA-256, which is preimage-safe and supports the indexed equality lookup the
// proxy hot path needs.
func GenerateServiceToken() (plaintext, digest, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("generate service token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	plaintext = ServiceTokenPrefix + body
	digest = HashToken(plaintext)
	prefix = plaintext[:len(ServiceTokenPrefix)+6]
	return plaintext, digest, prefix, nil
}

const serviceTokenColumns = `id, name, description, prefix, created_by_user_id, created_at, last_used_at, expires_at, revoked_at`

func scanServiceToken(scan func(...any) error) (*ServiceToken, error) {
	var t ServiceToken
	var created, lastUsed, expires, revoked string
	if err := scan(&t.ID, &t.Name, &t.Description, &t.Prefix, &t.CreatedBy, &created, &lastUsed, &expires, &revoked); err != nil {
		return nil, err
	}
	t.CreatedAt = ParseTime(created)
	t.LastUsedAt = ParseTime(lastUsed)
	t.ExpiresAt = ParseTime(expires)
	t.RevokedAt = ParseTime(revoked)
	return &t, nil
}

// ErrServiceTokenNameTaken reports a duplicate service-token name. Names are
// the reporting key operators see on dashboards, so they must stay unique and
// a collision has to surface as a 400 rather than a generic 500.
var ErrServiceTokenNameTaken = errors.New("a service token with that name already exists")

// normalizeServiceTokenName trims a submitted name and validates it. Names are
// what appear on usage reports in place of a person, so they are constrained
// the way a display identity should be: non-empty, bounded, and free of the
// control/whitespace characters that would render ambiguously in a table.
func normalizeServiceTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", &ValidationError{Field: "name", Message: "give the service token a name; usage is reported under it"}
	}
	if len(name) > 100 {
		return "", &ValidationError{Field: "name", Message: "service token names are limited to 100 characters"}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", &ValidationError{Field: "name", Message: "service token names must not contain control characters"}
		}
	}
	return name, nil
}

// CreateServiceToken mints and persists a service credential, returning the
// record and the plaintext value (which is never stored and never recoverable).
// expiresAt is optional; a zero value means the credential never expires.
func (s *Store) CreateServiceToken(ctx context.Context, name, description, createdByUserID string, expiresAt time.Time) (*ServiceToken, string, error) {
	name, err := normalizeServiceTokenName(name)
	if err != nil {
		return nil, "", err
	}
	if len(description) > 500 {
		return nil, "", &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	if !expiresAt.IsZero() && !expiresAt.After(nowUTC()) {
		return nil, "", &ValidationError{Field: "expires_at", Message: "the expiry must be in the future"}
	}
	plaintext, digest, prefix, err := GenerateServiceToken()
	if err != nil {
		return nil, "", err
	}
	t := &ServiceToken{
		ID: NewID(), Name: name, Description: strings.TrimSpace(description),
		Prefix: prefix, CreatedBy: createdByUserID, CreatedAt: nowUTC(), ExpiresAt: expiresAt,
	}
	err = s.exec(ctx,
		`INSERT INTO service_token (id, name, description, token_digest, prefix, created_by_user_id, created_at, last_used_at, expires_at, revoked_at)
		 VALUES (?,?,?,?,?,?,?,'',?,'')`,
		t.ID, t.Name, t.Description, digest, t.Prefix, t.CreatedBy, FormatTime(t.CreatedAt), formatOptionalTime(t.ExpiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, "", ErrServiceTokenNameTaken
		}
		return nil, "", err
	}
	return t, plaintext, nil
}

// isUniqueViolation reports whether an error is a unique-constraint failure.
// Both supported engines phrase it differently but both include "unique".
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// ServiceTokenFilter narrows an admin service-token listing.
type ServiceTokenFilter struct {
	// Status is "active", "revoked", "expired", or "" for every token.
	Status string
	Search string
}

// ListServiceTokens returns service credentials, newest first, with the
// creating admin's label and grant count resolved for the admin table.
func (s *Store) ListServiceTokens(ctx context.Context, f ServiceTokenFilter) ([]*ServiceToken, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Search != "" {
		where = append(where, "(LOWER(name) LIKE ? OR LOWER(description) LIKE ?)")
		needle := "%" + strings.ToLower(f.Search) + "%"
		args = append(args, needle, needle)
	}
	rows, err := s.query(ctx, `SELECT `+serviceTokenColumns+` FROM service_token WHERE `+
		strings.Join(where, " AND ")+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ServiceToken{}
	for rows.Next() {
		t, err := scanServiceToken(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan service token: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Status filtering is applied in Go rather than SQL because "expired" is
	// a function of the current time against expires_at, and expressing that
	// portably across SQLite and PostgreSQL string timestamps is fragile.
	now := nowUTC()
	filtered := out[:0]
	for _, t := range out {
		if f.Status != "" && t.Status(now) != f.Status {
			continue
		}
		filtered = append(filtered, t)
	}
	out = filtered
	for _, t := range out {
		if t.CreatedBy != "" {
			if u, err := s.UserByID(ctx, t.CreatedBy); err == nil {
				t.CreatedByLabel = displayName(u)
			}
		}
		if err := s.queryRow(ctx,
			`SELECT COUNT(*) FROM model_grant WHERE grantee_type = ? AND grantee_id = ?`,
			GranteeServiceToken, t.ID).Scan(&t.GrantCount); err != nil {
			return nil, fmt.Errorf("count service token grants: %w", err)
		}
	}
	return out, nil
}

// ServiceTokenByID loads one service credential.
func (s *Store) ServiceTokenByID(ctx context.Context, id string) (*ServiceToken, error) {
	t, err := scanServiceToken(s.queryRow(ctx, `SELECT `+serviceTokenColumns+` FROM service_token WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load service token: %w", err)
	}
	return t, nil
}

// ServiceTokenByDigest resolves a presented credential. Revoked and expired
// tokens are returned so the caller can distinguish "unknown" from "withdrawn"
// and give the operator an accurate error.
func (s *Store) ServiceTokenByDigest(ctx context.Context, digest string) (*ServiceToken, error) {
	t, err := scanServiceToken(s.queryRow(ctx, `SELECT `+serviceTokenColumns+` FROM service_token WHERE token_digest = ?`, digest).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load service token by digest: %w", err)
	}
	return t, nil
}

// UpdateServiceToken changes the mutable metadata of a service credential: its
// name, description, and optional expiry. The name is the reporting key, so a
// rename is allowed (typos happen) but the caller is expected to audit it.
// Passing a zero expiresAt clears the expiry, making the credential perpetual.
func (s *Store) UpdateServiceToken(ctx context.Context, id, name, description string, expiresAt time.Time) error {
	name, err := normalizeServiceTokenName(name)
	if err != nil {
		return err
	}
	if len(description) > 500 {
		return &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	res, execErr := s.db.ExecContext(ctx, s.rebind(
		`UPDATE service_token SET name = ?, description = ?, expires_at = ? WHERE id = ?`),
		name, strings.TrimSpace(description), formatOptionalTime(expiresAt), id)
	if execErr != nil {
		if isUniqueViolation(execErr) {
			return ErrServiceTokenNameTaken
		}
		return fmt.Errorf("update service token: %w", execErr)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeServiceToken withdraws a service credential immediately. The row is
// kept (never deleted) because usage history references it and the name must
// keep resolving on historical reports.
func (s *Store) RevokeServiceToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE service_token SET revoked_at = ? WHERE id = ? AND revoked_at = ''`), FormatTime(nowUTC()), id)
	if err != nil {
		return fmt.Errorf("revoke service token: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Either the id is unknown or it was already revoked. Distinguish the
		// two so the API can 404 rather than silently reporting success.
		if _, loadErr := s.ServiceTokenByID(ctx, id); loadErr != nil {
			return loadErr
		}
	}
	return nil
}

// TouchServiceToken records last use. Called off the request's critical path.
func (s *Store) TouchServiceToken(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE service_token SET last_used_at = ? WHERE id = ?`, FormatTime(nowUTC()), id)
}

// ServiceTokenNames maps ids to display names for report labelling. Ids that
// no longer resolve are simply absent, and callers fall back to a placeholder.
func (s *Store) ServiceTokenNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.query(ctx, `SELECT id, name FROM service_token`)
	if err != nil {
		return nil, fmt.Errorf("load service token names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan service token name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

// CountServiceTokensByStatus reports how many credentials sit in each
// lifecycle state, for the admin overview.
func (s *Store) CountServiceTokensByStatus(ctx context.Context) (map[string]int, error) {
	tokens, err := s.ListServiceTokens(ctx, ServiceTokenFilter{})
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := map[string]int{"active": 0, "revoked": 0, "expired": 0}
	for _, t := range tokens {
		out[t.Status(now)]++
	}
	return out, nil
}
