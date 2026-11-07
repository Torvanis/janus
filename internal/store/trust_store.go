package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Trusted certificates uploaded by administrators (the trust store's
// "uploaded" source; see internal/truststore) and the per-upstream
// "allow untrusted certificate" setting.
var trustStoreMigration = migration{name: "0072_trust_store", stmt: []string{
	`CREATE TABLE trusted_ca (id TEXT PRIMARY KEY, name TEXT NOT NULL, pem TEXT NOT NULL, sha256 TEXT NOT NULL UNIQUE, created_by TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
	`ALTER TABLE upstream ADD COLUMN allow_untrusted_tls INTEGER NOT NULL DEFAULT 0`,
}, down: []string{
	`DROP TABLE trusted_ca`,
}}

// TrustedCA is one uploaded certificate (PEM may hold a chain).
type TrustedCA struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	PEM       string    `json:"pem"`
	SHA256    string    `json:"sha256"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrTrustedCAExists is returned when the same certificate is uploaded twice.
var ErrTrustedCAExists = &ValidationError{Field: "pem", Message: "That certificate is already trusted."}

// ListTrustedCAs returns every uploaded certificate, oldest first.
func (s *Store) ListTrustedCAs(ctx context.Context) ([]*TrustedCA, error) {
	rows, err := s.query(ctx, `SELECT id, name, pem, sha256, created_by, created_at FROM trusted_ca ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*TrustedCA{}
	for rows.Next() {
		var c TrustedCA
		var created string
		if err := rows.Scan(&c.ID, &c.Name, &c.PEM, &c.SHA256, &c.CreatedBy, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = ParseTime(created)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// AddTrustedCA stores an uploaded certificate. sha256 identifies it (the
// first certificate's fingerprint) so a duplicate upload is refused.
func (s *Store) AddTrustedCA(ctx context.Context, name, pem, sha256, createdBy string) (*TrustedCA, error) {
	c := &TrustedCA{ID: NewID(), Name: strings.TrimSpace(name), PEM: pem, SHA256: sha256, CreatedBy: createdBy, CreatedAt: nowUTC()}
	err := s.exec(ctx, `INSERT INTO trusted_ca (id, name, pem, sha256, created_by, created_at) VALUES (?,?,?,?,?,?)`,
		c.ID, c.Name, c.PEM, c.SHA256, c.CreatedBy, FormatTime(c.CreatedAt))
	if isUniqueViolation(err) {
		return nil, ErrTrustedCAExists
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteTrustedCA removes an uploaded certificate (ErrNotFound when absent).
func (s *Store) DeleteTrustedCA(ctx context.Context, id string) (*TrustedCA, error) {
	var c TrustedCA
	var created string
	if err := s.queryRow(ctx, `SELECT id, name, pem, sha256, created_by, created_at FROM trusted_ca WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.PEM, &c.SHA256, &c.CreatedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = ParseTime(created)
	if err := s.execOne(ctx, `DELETE FROM trusted_ca WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &c, nil
}

// SetUpstreamAllowUntrustedTLS changes an upstream's "allow untrusted
// certificate" setting.
func (s *Store) SetUpstreamAllowUntrustedTLS(ctx context.Context, id string, allow bool) error {
	return s.execOne(ctx, `UPDATE upstream SET allow_untrusted_tls = ? WHERE id = ? AND deleted_at = ''`, boolInt(allow), id)
}

// UntrustedTLSUpstreams lists enabled, non-deleted upstreams that allow an
// unverified certificate.
func (s *Store) UntrustedTLSUpstreams(ctx context.Context) ([]*Upstream, error) {
	rows, err := s.query(ctx, `SELECT `+upstreamColumns+` FROM upstream WHERE deleted_at = '' AND allow_untrusted_tls = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Upstream{}
	for rows.Next() {
		u, err := scanUpstream(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
