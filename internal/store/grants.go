package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Team grant populations never participate in personal grant resolution.
const (
	GranteeTeam     = "team"
	GranteeAllTeams = "all_teams"
)

// ListGrants returns grants with resolved display names for the admin matrix.
// Grants on real models and grants on managed models (aliases) both appear;
// ModelKind tells them apart and the name resolution follows the kind.
func (s *Store) ListGrants(ctx context.Context, modelID string) ([]*Grant, error) {
	query := `SELECT g.id, g.model_id, g.model_kind, g.grantee_type, g.grantee_id, g.created_at
		FROM model_grant g`
	args := []any{}
	if modelID != "" {
		query += ` WHERE g.model_id = ?`
		args = append(args, modelID)
	}
	query += ` ORDER BY g.model_kind, g.grantee_type`
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Grant{}
	for rows.Next() {
		var g Grant
		var created string
		if err := rows.Scan(&g.ID, &g.ModelID, &g.ModelKind, &g.GranteeType, &g.GranteeID, &created); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		g.CreatedAt = ParseTime(created)
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		g.GranteeName = s.granteeLabel(ctx, g.GranteeType, g.GranteeID)
		g.ModelName = s.grantedModelLabel(ctx, g.ModelKind, g.ModelID)
	}
	return out, nil
}

// grantedModelLabel resolves the display label for a grant's target, whether
// that is a real catalog model or a managed alias.
func (s *Store) grantedModelLabel(ctx context.Context, modelKind, modelID string) string {
	if modelKind == ModelKindManaged {
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM managed_model WHERE id = ?`, modelID).Scan(&name); err == nil {
			return name
		}
		return modelID
	}
	var name string
	if err := s.queryRow(ctx, `SELECT name FROM model WHERE id = ?`, modelID).Scan(&name); err == nil {
		return name
	}
	return modelID
}

func (s *Store) granteeLabel(ctx context.Context, granteeType, granteeID string) string {
	switch granteeType {
	case GranteeAllTeams:
		return "All teams (including future teams)"
	case GranteeTeam:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM team WHERE id = ?`, granteeID).Scan(&name); err == nil {
			return name
		}
	case GranteeAllUsers:
		return "All authenticated users"
	case GranteeAllServiceTokens:
		return "Every service token"
	case GranteeServiceToken:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM service_token WHERE id = ?`, granteeID).Scan(&name); err == nil {
			return name
		}
	case GranteeUser:
		if u, err := s.UserByID(ctx, granteeID); err == nil {
			if u.Name != "" {
				return u.Name
			}
			return u.Email
		}
	case GranteeGroup:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM user_group WHERE id = ?`, granteeID).Scan(&name); err == nil {
			return name
		}
	}
	return granteeID
}

// CreateGrant adds an access grant, treating a repeat as a no-op. modelKind
// selects whether modelID names a real catalog model or a managed alias; an
// empty value defaults to a real model so existing callers keep working.
func (s *Store) CreateGrant(ctx context.Context, modelID, modelKind, granteeType, granteeID string) (*Grant, error) {
	if modelKind == "" {
		modelKind = ModelKindModel
	}
	if modelKind != ModelKindModel && modelKind != ModelKindManaged {
		return nil, &ValidationError{Field: "model_kind", Message: "model kind must be model or managed"}
	}
	if modelKind == ModelKindModel {
		// A Security Gateway classifier is never grantable: a caller who can
		// invoke the judge directly has free oracle access to it.
		if m, err := s.ModelByID(ctx, modelID); err == nil && m.ClassifierRole != "" {
			return nil, &ValidationError{Field: "model_id", Message: "this model is a security classifier and cannot be granted to callers"}
		}
	}
	// Collective grantee types address a whole population, so they carry no
	// specific grantee id; normalising here keeps the unique index honest.
	if granteeType == GranteeAllUsers || granteeType == GranteeAllServiceTokens || granteeType == GranteeAllTeams {
		granteeID = ""
	}
	g := &Grant{ID: NewID(), ModelID: modelID, ModelKind: modelKind, GranteeType: granteeType, GranteeID: granteeID, CreatedAt: nowUTC()}
	err := s.exec(ctx, `INSERT INTO model_grant (id, model_id, model_kind, grantee_type, grantee_id, created_at) VALUES (?,?,?,?,?,?)`,
		g.ID, g.ModelID, g.ModelKind, g.GranteeType, g.GranteeID, FormatTime(g.CreatedAt))
	if err != nil && isUniqueViolation(err) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// DeleteGrant removes an access grant.
func (s *Store) DeleteGrant(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM model_grant WHERE id = ?`, id)
}

// DeleteGrantsForServiceToken removes every grant held by a service token.
// Called when a token is revoked so a withdrawn credential stops appearing in
// the access matrix as if it still had reach.
func (s *Store) DeleteGrantsForServiceToken(ctx context.Context, serviceTokenID string) error {
	return s.exec(ctx, `DELETE FROM model_grant WHERE grantee_type = ? AND grantee_id = ?`,
		GranteeServiceToken, serviceTokenID)
}

// GrantedModelIDs computes the effective access set for a HUMAN principal: the
// union of direct user grants, grants to any of the user's groups, and
// all-users grants. Grants compose with OR — the most permissive match wins.
//
// The returned map is keyed by real model id and by managed model id; the
// caller distinguishes them via GrantedManagedModelIDs when it needs to.
// Service-token grantee types are deliberately never consulted here: a user
// must not inherit access from an integration credential.
func (s *Store) GrantedModelIDs(ctx context.Context, userID string, groupIDs []string) (map[string]string, error) {
	out := map[string]string{}

	rows, err := s.query(ctx, `SELECT model_id FROM model_grant WHERE grantee_type = ?`, GranteeAllUsers)
	if err != nil {
		return nil, err
	}
	if err := collectInto(rows, out, "all users"); err != nil {
		return nil, err
	}

	if len(groupIDs) > 0 {
		q := `SELECT model_id FROM model_grant WHERE grantee_type = ? AND grantee_id IN (` + placeholders(len(groupIDs)) + `)`
		args := append([]any{GranteeGroup}, toArgs(groupIDs)...)
		rows, err := s.query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		if err := collectInto(rows, out, "group"); err != nil {
			return nil, err
		}
	}

	rows, err = s.query(ctx, `SELECT model_id FROM model_grant WHERE grantee_type = ? AND grantee_id = ?`, GranteeUser, userID)
	if err != nil {
		return nil, err
	}
	// A direct grant is the most specific source and overrides the label.
	if err := collectOverriding(rows, out, "direct"); err != nil {
		return nil, err
	}
	return out, nil
}

// GrantedModelIDsForServiceToken computes the effective access set for a
// service token: its own direct grants plus any all-service-tokens grants.
//
// It deliberately does NOT consult GranteeAllUsers. "All authenticated users"
// is a statement about people; letting it also cover every unattended
// integration would mean an admin broadening access for staff silently hands
// the same model to every website and agent holding a service credential.
func (s *Store) GrantedModelIDsForServiceToken(ctx context.Context, serviceTokenID string) (map[string]string, error) {
	out := map[string]string{}

	rows, err := s.query(ctx, `SELECT model_id FROM model_grant WHERE grantee_type = ?`, GranteeAllServiceTokens)
	if err != nil {
		return nil, err
	}
	if err := collectInto(rows, out, "all service tokens"); err != nil {
		return nil, err
	}

	rows, err = s.query(ctx, `SELECT model_id FROM model_grant WHERE grantee_type = ? AND grantee_id = ?`,
		GranteeServiceToken, serviceTokenID)
	if err != nil {
		return nil, err
	}
	if err := collectOverriding(rows, out, "direct"); err != nil {
		return nil, err
	}
	return out, nil
}

// GrantSubject identifies whichever kind of principal is asking for access, so
// grant resolution has exactly one entry point rather than two call sites that
// can drift apart.
type GrantSubject struct {
	TeamID         string
	UserID         string
	GroupIDs       []string
	ServiceTokenID string
}

// IsServiceToken reports whether the subject is an integration credential.
func (g GrantSubject) IsServiceToken() bool { return g.ServiceTokenID != "" }

// GrantedIDsFor resolves the effective access set for either principal kind.
func (s *Store) GrantedIDsFor(ctx context.Context, subject GrantSubject) (map[string]string, error) {
	if subject.TeamID != "" {
		if subject.IsServiceToken() || subject.UserID == "" {
			return nil, &ValidationError{Field: "team_id", Message: "team context requires a human member"}
		}
		var member string
		if err := s.queryRow(ctx, `SELECT user_id FROM team_member WHERE team_id = ? AND user_id = ?`, subject.TeamID, subject.UserID).Scan(&member); err != nil {
			if err == sql.ErrNoRows {
				return nil, &ValidationError{Field: "team_id", Message: "you are no longer a member of the selected team"}
			}
			return nil, err
		}
		rows, err := s.query(ctx, `SELECT model_id FROM model_grant WHERE grantee_type = ? OR (grantee_type = ? AND grantee_id = ?)`, GranteeAllTeams, GranteeTeam, subject.TeamID)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		if err := collectInto(rows, out, "team"); err != nil {
			return nil, err
		}
		return out, nil
	}
	if subject.IsServiceToken() {
		return s.GrantedModelIDsForServiceToken(ctx, subject.ServiceTokenID)
	}
	return s.GrantedModelIDs(ctx, subject.UserID, subject.GroupIDs)
}

// GranteeExists reports whether a specific grantee id still resolves, so the
// API can reject a grant aimed at a deleted user, group, or service token
// instead of persisting a dangling row.
func (s *Store) GranteeExists(ctx context.Context, granteeType, granteeID string) (bool, error) {
	var table string
	switch granteeType {
	case GranteeAllUsers, GranteeAllServiceTokens, GranteeAllTeams:
		return true, nil
	case GranteeTeam:
		table = "team"
	case GranteeUser:
		table = "app_user"
	case GranteeGroup:
		table = "user_group"
	case GranteeServiceToken:
		table = "service_token"
	default:
		return false, fmt.Errorf("unsupported grantee type %q", granteeType)
	}
	var found string
	err := s.queryRow(ctx, `SELECT id FROM `+table+` WHERE id = ?`, granteeID).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check grantee: %w", err)
	}
	return true, nil
}

func collectInto(rows *sql.Rows, out map[string]string, label string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan grant model id: %w", err)
		}
		if _, exists := out[id]; !exists {
			out[id] = label
		}
	}
	return rows.Err()
}

func collectOverriding(rows *sql.Rows, out map[string]string, label string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan grant model id: %w", err)
		}
		out[id] = label
	}
	return rows.Err()
}
