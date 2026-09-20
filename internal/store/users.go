package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const userColumns = `id, auth_provider_id, email, name, role, admin_via_group, is_active, timezone, locale, created_at, last_login_at`

func scanUser(scan func(dest ...any) error) (*User, error) {
	var u User
	var active int
	var created, lastLogin string
	// admin_via_group is scanned as a Go bool because the column is a real
	// BOOLEAN on PostgreSQL and an INTEGER 0/1 on SQLite; database/sql
	// converts both into bool, whereas an int destination only works on one.
	if err := scan(&u.ID, &u.AuthID, &u.Email, &u.Name, &u.Role, &u.AdminViaGroup, &active, &u.Timezone, &u.Locale, &created, &lastLogin); err != nil {
		return nil, err
	}
	u.IsActive = active == 1
	u.CreatedAt = ParseTime(created)
	u.LastLoginAt = ParseTime(lastLogin)
	return &u, nil
}

// UpsertUserFromIdentity applies the first-login / every-login contract: create
// the row keyed on the immutable subject identifier, refresh the display fields,
// and stamp the login time. Administrator capability composes from three
// sources with OR:
//
//   - bootstrapAdmin (the JANUS_BOOTSTRAP_ADMIN_EMAILS match) promotes role to
//     admin, and the promotion is sticky: this method never downgrades role,
//     even after the address leaves the bootstrap list.
//   - adminGroup (membership in a configured IdP administrator group) is
//     recorded in admin_via_group and re-evaluated on EVERY sign-in, so
//     leaving the group revokes that capability at the next sign-in unless
//     another source still grants it.
//   - an explicit grant written by the admin UI sets role directly and is
//     equally sticky against this method.
//
// Both signals land atomically in the single sign-in write; the role promotion
// reads the currently stored role inside the statement so a concurrent
// explicit role change can never be clobbered by a stale read. Group
// membership synchronisation is handled separately.
func (s *Store) UpsertUserFromIdentity(ctx context.Context, authID, email, name string, bootstrapAdmin, adminGroup bool) (*User, bool, error) {
	existing, err := s.UserByAuthID(ctx, authID)
	switch {
	case err == nil:
		if err := s.exec(ctx,
			`UPDATE app_user SET email = ?, name = ?, role = CASE WHEN ? THEN ? ELSE role END, admin_via_group = ?, last_login_at = ? WHERE id = ?`,
			email, name, bootstrapAdmin, RoleAdmin, adminGroup, FormatTime(nowUTC()), existing.ID); err != nil {
			return nil, false, err
		}
		updated, err := s.UserByAuthID(ctx, authID)
		if err != nil {
			return nil, false, fmt.Errorf("reload user after sign-in write: %w", err)
		}
		return updated, false, nil
	case errors.Is(err, ErrNotFound):
		role := RoleUser
		if bootstrapAdmin {
			role = RoleAdmin
		}
		u := &User{
			ID: NewID(), AuthID: authID, Email: email, Name: name, Role: role,
			AdminViaGroup: adminGroup,
			IsActive:      true, CreatedAt: nowUTC(), LastLoginAt: nowUTC(),
		}
		if err := s.exec(ctx,
			`INSERT INTO app_user (`+userColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			u.ID, u.AuthID, u.Email, u.Name, u.Role, u.AdminViaGroup, 1, "", "", FormatTime(u.CreatedAt), FormatTime(u.LastLoginAt)); err != nil {
			return nil, false, err
		}
		return u, true, nil
	default:
		return nil, false, err
	}
}

// UserByAuthID looks a user up by identity-provider subject.
func (s *Store) UserByAuthID(ctx context.Context, authID string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userColumns+` FROM app_user WHERE auth_provider_id = ?`, authID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by auth id: %w", err)
	}
	return u, nil
}

// UserByID loads one user.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userColumns+` FROM app_user WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	return u, nil
}

// UserFilter narrows an admin user listing.
type UserFilter struct {
	Search  string
	Role    string
	GroupID string
	TeamID  string
	Active  string // "", "active", "inactive"
	Sort    string
	Limit   int
	Offset  int
}

// ListUsers returns a filtered, sorted page of users plus the total match count.
func (s *Store) ListUsers(ctx context.Context, f UserFilter) ([]*User, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Search != "" {
		where = append(where, "(LOWER(email) LIKE ? OR LOWER(name) LIKE ?)")
		like := "%" + strings.ToLower(f.Search) + "%"
		args = append(args, like, like)
	}
	if f.Role != "" {
		where = append(where, "role = ?")
		args = append(args, f.Role)
	}
	switch f.Active {
	case "active":
		where = append(where, "is_active = 1")
	case "inactive":
		where = append(where, "is_active = 0")
	}
	if f.GroupID != "" {
		where = append(where, "id IN (SELECT user_id FROM group_member WHERE group_id = ?)")
		args = append(args, f.GroupID)
	}
	if f.TeamID != "" {
		where = append(where, "id IN (SELECT user_id FROM team_member WHERE team_id = ?)")
		args = append(args, f.TeamID)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM app_user WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	// the listing sorts by email, name, created_at, or last_login in
	// either direction. Bare field names keep their historical meaning
	// (descending for time-like fields), and every field also accepts the
	// explicit `<field>_asc` / `<field>_desc` forms the admin UI emits.
	// Unknown values fall back to the default email ASC rather than erroring.
	order := "email ASC"
	switch f.Sort {
	case "", "email", "email_asc":
		order = "email ASC"
	case "email_desc":
		order = "email DESC"
	case "name", "name_asc":
		order = "name ASC, email ASC"
	case "name_desc":
		order = "name DESC, email ASC"
	case "created", "created_desc":
		order = "created_at DESC"
	case "created_asc":
		order = "created_at ASC"
	case "last_login", "last_login_desc":
		order = "last_login_at DESC"
	case "last_login_asc":
		order = "last_login_at ASC"
	case "role":
		order = "role DESC, email ASC"
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT `+userColumns+` FROM app_user WHERE `+clause+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// UpdateUser applies admin changes to role / active state.
func (s *Store) UpdateUser(ctx context.Context, id, role string, isActive bool) error {
	return s.exec(ctx, `UPDATE app_user SET role = ?, is_active = ? WHERE id = ?`, role, boolInt(isActive), id)
}

// UpdateUserPreferences stores viewer-owned display preferences.
func (s *Store) UpdateUserPreferences(ctx context.Context, id, timezone, locale string) error {
	return s.exec(ctx, `UPDATE app_user SET timezone = ?, locale = ? WHERE id = ?`, timezone, locale, id)
}

// CountUsers returns the total number of user rows.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM app_user`).Scan(&n)
	return n, err
}

// ActiveSeatCount is the licensing definition of an active user: anyone who
// signed in OR made a request in the trailing window. Disabled accounts do
// not hold a seat.
func (s *Store) ActiveSeatCount(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM app_user u WHERE u.is_active = 1 AND (
		u.last_login_at >= ? OR EXISTS (SELECT 1 FROM usage_event e WHERE e.user_id = u.id AND e.created_at >= ?))`,
		FormatTime(since), FormatTime(since)).Scan(&n)
	return n, err
}

// ActiveUserCount reports distinct users seen in the trailing window.
func (s *Store) ActiveUserCount(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(DISTINCT user_id) FROM usage_event WHERE created_at >= ?`, FormatTime(since)).Scan(&n)
	return n, err
}
