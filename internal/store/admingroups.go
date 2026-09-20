package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AdminGroup is an identity-provider group whose members are promoted to
// administrator on sign-in.
//
// Admin capability composes from three independent sources, OR'd together and
// deliberately layered so no single removal can lock an organisation out:
//
//  1. The bootstrap email list (JANUS_BOOTSTRAP_ADMIN_EMAILS) — the failsafe
//     path. It is environment configuration, so it survives any database
//     change and cannot be revoked from the UI.
//  2. An explicit per-user grant made in the admin UI — a sticky role on the
//     user row.
//  3. Membership of an admin group — re-evaluated on every sign-in.
//
// Because the sources are independent, removing a group does not revoke a user
// who also holds an explicit grant, and revoking an explicit grant does not
// demote a bootstrap admin. Each layer can be withdrawn without collapsing the
// ones beneath it.
type AdminGroup struct {
	// Name is the IdP group name, stored lowercased so matching against the
	// identity claim is case-insensitive.
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by_user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// normalizeAdminGroupName trims and lowercases a group name. IdP claims vary
// in case between providers and even between logins, so the stored form is
// canonical and comparisons never depend on how a name was typed.
func normalizeAdminGroupName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", &ValidationError{Field: "name", Message: "give the group a name as it appears in your identity provider"}
	}
	if len(name) > 200 {
		return "", &ValidationError{Field: "name", Message: "group names are limited to 200 characters"}
	}
	return name, nil
}

// ListAdminGroups returns every configured admin group, ordered by name.
func (s *Store) ListAdminGroups(ctx context.Context) ([]*AdminGroup, error) {
	rows, err := s.query(ctx, `SELECT name, created_by_user_id, created_at FROM admin_group ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list admin groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*AdminGroup{}
	for rows.Next() {
		var g AdminGroup
		var created string
		if err := rows.Scan(&g.Name, &g.CreatedBy, &created); err != nil {
			return nil, fmt.Errorf("scan admin group: %w", err)
		}
		g.CreatedAt = ParseTime(created)
		out = append(out, &g)
	}
	return out, rows.Err()
}

// AddAdminGroup records a group whose members become administrators. Adding a
// group that already exists is a no-op rather than an error: the desired end
// state is the same either way, and an admin re-adding a name should not see a
// failure.
func (s *Store) AddAdminGroup(ctx context.Context, name, actorUserID string) (*AdminGroup, error) {
	normalized, err := normalizeAdminGroupName(name)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := s.exec(ctx,
		`INSERT INTO admin_group (name, created_by_user_id, created_at) VALUES (?,?,?)
		 ON CONFLICT (name) DO NOTHING`,
		normalized, actorUserID, FormatTime(now)); err != nil {
		return nil, fmt.Errorf("add admin group: %w", err)
	}
	return &AdminGroup{Name: normalized, CreatedBy: actorUserID, CreatedAt: now}, nil
}

// RemoveAdminGroup stops promoting members of a group.
//
// It deliberately does NOT demote anyone: a user who was promoted through this
// group keeps whatever role they hold until it is changed explicitly, and a
// user who also holds a bootstrap or per-user grant is unaffected entirely.
// Removing the broadest layer must never silently revoke the narrower ones.
func (s *Store) RemoveAdminGroup(ctx context.Context, name string) error {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if err := s.exec(ctx, `DELETE FROM admin_group WHERE name = ?`, normalized); err != nil {
		return fmt.Errorf("remove admin group: %w", err)
	}
	return nil
}

// AdminGroupNames returns the configured group names as a lookup set, for the
// sign-in path. Empty means group-based promotion is inert.
func (s *Store) AdminGroupNames(ctx context.Context) (map[string]struct{}, error) {
	groups, err := s.ListAdminGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		out[g.Name] = struct{}{}
	}
	return out, nil
}
