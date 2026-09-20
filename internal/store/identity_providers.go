package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/crypto"
)

// IdentityProvider is an OIDC provider configured in the admin UI (as opposed
// to the single provider from JANUS_OIDC_* env, which needs no row). The
// client secret is stored AES-GCM encrypted under the gateway key and is
// never returned by list/get — only "has_secret".
type IdentityProvider struct {
	ID           string    `json:"id"`
	Slug         string    `json:"slug"`
	Name         string    `json:"name"`
	IssuerURL    string    `json:"issuer_url"`
	ClientID     string    `json:"client_id"`
	HasSecret    bool      `json:"has_secret"`
	Scopes       []string  `json:"scopes"`
	EmailClaim   string    `json:"email_claim"`
	NameClaim    string    `json:"name_claim"`
	GroupsClaim  string    `json:"groups_claim"`
	AdminGroups  []string  `json:"admin_groups"`
	Enabled      bool      `json:"enabled"`
	SortOrder    int       `json:"sort_order"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	secretCipher string
}

// ErrProviderSlugTaken: slugs are part of the callback URL, so they must be unique.
var ErrProviderSlugTaken = errors.New("that provider slug is already in use")

var identityProviderMigration = migration{name: "0037_identity_providers", stmt: []string{
	`CREATE TABLE IF NOT EXISTS identity_provider (
		id TEXT PRIMARY KEY,
		slug TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		issuer_url TEXT NOT NULL,
		client_id TEXT NOT NULL,
		client_secret_enc TEXT NOT NULL DEFAULT '',
		scopes TEXT NOT NULL DEFAULT 'openid,email,profile',
		email_claim TEXT NOT NULL DEFAULT 'email',
		name_claim TEXT NOT NULL DEFAULT 'name',
		groups_claim TEXT NOT NULL DEFAULT 'groups',
		admin_groups TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
}}

const idpColumns = `id, slug, name, issuer_url, client_id, client_secret_enc, scopes, email_claim, name_claim, groups_claim, admin_groups, enabled, sort_order, created_at, updated_at`

func scanIdentityProvider(scan func(...any) error) (*IdentityProvider, error) {
	p := &IdentityProvider{}
	var scopes, admin, created, updated string
	var enabled int
	if err := scan(&p.ID, &p.Slug, &p.Name, &p.IssuerURL, &p.ClientID, &p.secretCipher, &scopes, &p.EmailClaim, &p.NameClaim, &p.GroupsClaim, &admin, &enabled, &p.SortOrder, &created, &updated); err != nil {
		return nil, err
	}
	p.Scopes = splitCSV(scopes)
	p.AdminGroups = splitCSV(admin)
	p.HasSecret = p.secretCipher != ""
	p.Enabled = enabled == 1
	p.CreatedAt, p.UpdatedAt = ParseTime(created), ParseTime(updated)
	return p, nil
}

func joinCSV(v []string) string { return strings.Join(v, ",") }

// ListIdentityProviders returns every configured provider in display order.
func (s *Store) ListIdentityProviders(ctx context.Context) ([]*IdentityProvider, error) {
	rows, err := s.query(ctx, `SELECT `+idpColumns+` FROM identity_provider ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("list identity providers: %w", err)
	}
	defer rows.Close()
	out := []*IdentityProvider{}
	for rows.Next() {
		p, err := scanIdentityProvider(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// IdentityProviderBySlug fetches one provider (any enabled state).
func (s *Store) IdentityProviderBySlug(ctx context.Context, slug string) (*IdentityProvider, error) {
	p, err := scanIdentityProvider(s.queryRow(ctx, `SELECT `+idpColumns+` FROM identity_provider WHERE slug = ?`, slug).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// IdentityProviderByID fetches one provider by primary key.
func (s *Store) IdentityProviderByID(ctx context.Context, id string) (*IdentityProvider, error) {
	p, err := scanIdentityProvider(s.queryRow(ctx, `SELECT `+idpColumns+` FROM identity_provider WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ClientSecret decrypts the stored secret for the OIDC exchange.
func (p *IdentityProvider) ClientSecret(c *crypto.Cipher) (string, error) {
	if p.secretCipher == "" {
		return "", nil
	}
	return c.Decrypt(p.secretCipher)
}

// IdentityProviderInput is the admin-editable subset.
type IdentityProviderInput struct {
	Slug         string
	Name         string
	IssuerURL    string
	ClientID     string
	ClientSecret string // "" on update = keep existing
	Scopes       []string
	EmailClaim   string
	NameClaim    string
	GroupsClaim  string
	AdminGroups  []string
	Enabled      bool
	SortOrder    int
}

func (in *IdentityProviderInput) defaults() {
	if len(in.Scopes) == 0 {
		in.Scopes = []string{"openid", "email", "profile"}
	}
	if in.EmailClaim == "" {
		in.EmailClaim = "email"
	}
	if in.NameClaim == "" {
		in.NameClaim = "name"
	}
	if in.GroupsClaim == "" {
		in.GroupsClaim = "groups"
	}
}

// CreateIdentityProvider inserts a provider; the secret is encrypted here.
func (s *Store) CreateIdentityProvider(ctx context.Context, c *crypto.Cipher, in IdentityProviderInput) (*IdentityProvider, error) {
	in.defaults()
	enc := ""
	if in.ClientSecret != "" {
		var err error
		if enc, err = c.Encrypt(in.ClientSecret); err != nil {
			return nil, err
		}
	}
	id := NewID()
	now := FormatTime(time.Now().UTC())
	err := s.exec(ctx, `INSERT INTO identity_provider (`+idpColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Slug, in.Name, in.IssuerURL, in.ClientID, enc, joinCSV(in.Scopes), in.EmailClaim, in.NameClaim, in.GroupsClaim, joinCSV(in.AdminGroups), boolInt(in.Enabled), in.SortOrder, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrProviderSlugTaken
		}
		return nil, fmt.Errorf("create identity provider: %w", err)
	}
	return s.IdentityProviderByID(ctx, id)
}

// UpdateIdentityProvider rewrites the editable columns; an empty ClientSecret
// keeps the stored one.
func (s *Store) UpdateIdentityProvider(ctx context.Context, c *crypto.Cipher, id string, in IdentityProviderInput) (*IdentityProvider, error) {
	in.defaults()
	secretSQL := ""
	args := []any{in.Slug, in.Name, in.IssuerURL, in.ClientID, joinCSV(in.Scopes), in.EmailClaim, in.NameClaim, in.GroupsClaim, joinCSV(in.AdminGroups), boolInt(in.Enabled), in.SortOrder, FormatTime(time.Now().UTC())}
	if in.ClientSecret != "" {
		enc, err := c.Encrypt(in.ClientSecret)
		if err != nil {
			return nil, err
		}
		secretSQL = ", client_secret_enc = ?"
		args = append(args, enc)
	}
	args = append(args, id)
	err := s.exec(ctx, `UPDATE identity_provider SET slug = ?, name = ?, issuer_url = ?, client_id = ?, scopes = ?, email_claim = ?, name_claim = ?, groups_claim = ?, admin_groups = ?, enabled = ?, sort_order = ?, updated_at = ?`+secretSQL+` WHERE id = ?`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrProviderSlugTaken
		}
		return nil, fmt.Errorf("update identity provider: %w", err)
	}
	return s.IdentityProviderByID(ctx, id)
}

// DeleteIdentityProvider removes a provider. Users who signed in through it
// keep their accounts (auth_provider_id stays), they just need another way in.
func (s *Store) DeleteIdentityProvider(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM identity_provider WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete identity provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountIdentityProviders is what the multi_oidc feature gate looks at.
func (s *Store) CountIdentityProviders(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM identity_provider`).Scan(&n)
	return n, err
}
