package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// --- Groups -----------------------------------------------------------------

// EnsureGroup returns the group with the given name, creating it if absent.
func (s *Store) EnsureGroup(ctx context.Context, name string, fromIDP bool) (*Group, error) {
	g, err := s.GroupByName(ctx, name)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	g = &Group{ID: NewID(), Name: name, FromIDP: fromIDP, CreatedAt: nowUTC()}
	if err := s.exec(ctx, `INSERT INTO user_group (id, name, from_idp, created_at) VALUES (?,?,?,?)`,
		g.ID, g.Name, boolInt(fromIDP), FormatTime(g.CreatedAt)); err != nil {
		// A concurrent replica may have inserted the same IdP group first.
		if existing, lookupErr := s.GroupByName(ctx, name); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return g, nil
}

// GroupByName loads a group by its unique name.
func (s *Store) GroupByName(ctx context.Context, name string) (*Group, error) {
	var g Group
	var fromIDP int
	var created string
	err := s.queryRow(ctx, `SELECT id, name, from_idp, created_at FROM user_group WHERE name = ?`, name).
		Scan(&g.ID, &g.Name, &fromIDP, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load group: %w", err)
	}
	g.FromIDP = fromIDP == 1
	g.CreatedAt = ParseTime(created)
	return &g, nil
}

// ListGroups returns all groups with member counts.
func (s *Store) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := s.query(ctx, `SELECT g.id, g.name, g.from_idp, g.created_at,
		(SELECT COUNT(*) FROM group_member m WHERE m.group_id = g.id)
		FROM user_group g ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Group{}
	for rows.Next() {
		var g Group
		var fromIDP int
		var created string
		if err := rows.Scan(&g.ID, &g.Name, &fromIDP, &created, &g.MemberCount); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		g.FromIDP = fromIDP == 1
		g.CreatedAt = ParseTime(created)
		out = append(out, &g)
	}
	return out, rows.Err()
}

// CreateGroup adds an in-app group.
func (s *Store) CreateGroup(ctx context.Context, name string) (*Group, error) {
	if _, err := s.GroupByName(ctx, name); err == nil {
		return nil, fmt.Errorf("a group named %q already exists", name)
	}
	return s.EnsureGroup(ctx, name, false)
}

// RenameGroup changes an in-app group's name.
func (s *Store) RenameGroup(ctx context.Context, id, name string) error {
	return s.exec(ctx, `UPDATE user_group SET name = ? WHERE id = ? AND from_idp = 0`, name, id)
}

// DeleteGroup removes an in-app group and its memberships.
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		var fromIDP int
		if err := s.queryRow(ctx, `SELECT from_idp FROM user_group WHERE id=?`, id).Scan(&fromIDP); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if fromIDP != 0 {
			return errors.New("cannot delete an IdP-managed group")
		}
		if err := s.SetGroupMembers(ctx, id, nil); err != nil {
			return err
		}
		if err := s.exec(ctx, `DELETE FROM team_group_mapping WHERE group_id=?`, id); err != nil {
			return err
		}
		return s.exec(ctx, `DELETE FROM user_group WHERE id=?`, id)
	})
}

// SetGroupMembers replaces the membership of an in-app group.
func (s *Store) SetGroupMembers(ctx context.Context, groupID string, userIDs []string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.exec(ctx, `DELETE FROM group_member WHERE group_id=?`, groupID); err != nil {
			return err
		}
		for _, uid := range userIDs {
			if err := s.exec(ctx, `INSERT INTO group_member(group_id,user_id) VALUES (?,?) ON CONFLICT(group_id,user_id) DO NOTHING`, groupID, uid); err != nil {
				return err
			}
		}
		return s.ReconcileTeamGroup(ctx, groupID)
	})
}

// ReplaceIDPGroupsForUser applies the login-time group sync: the user's
// IdP-sourced memberships become exactly the set carried by the token, while
// in-app group memberships are left untouched.
func (s *Store) ReplaceIDPGroupsForUser(ctx context.Context, userID string, groupNames []string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		old, err := s.GroupIDsForUser(ctx, userID)
		if err != nil {
			return err
		}
		if err := s.exec(ctx, `DELETE FROM group_member WHERE user_id=? AND group_id IN (SELECT id FROM user_group WHERE from_idp=1)`, userID); err != nil {
			return err
		}
		added := []string{}
		for _, name := range groupNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			g, err := s.EnsureGroup(ctx, name, true)
			if err != nil {
				return err
			}
			// Names are not membership authority. Local and SCIM-owned groups
			// remain controlled by their own source, even if an OIDC claim matches.
			if !g.FromIDP {
				continue
			}
			if err := s.exec(ctx, `INSERT INTO group_member(group_id,user_id) VALUES (?,?) ON CONFLICT(group_id,user_id) DO NOTHING`, g.ID, userID); err != nil {
				return err
			}
			added = append(added, g.ID)
		}
		// Reconcile additions first so switching between groups on the same team
		// preserves its role and never invents a temporary zero-team transition.
		for _, gid := range append(added, old...) {
			if err := s.ReconcileTeamGroup(ctx, gid); err != nil {
				return err
			}
		}
		return nil
	})
}

// GroupIDsForUser lists every group the user belongs to.
func (s *Store) GroupIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT group_id FROM group_member WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan group id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GroupNamesForUser lists the user's group names for display.
func (s *Store) GroupNamesForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT g.name FROM user_group g JOIN group_member m ON m.group_id = g.id WHERE m.user_id = ? ORDER BY g.name`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan group name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- Teams ------------------------------------------------------------------

var ErrLastTeamLeader = errors.New("team must retain at least one leader")
var ErrInvalidTeamRole = errors.New("invalid team role")
var ErrTeamArchived = errors.New("team is archived")

func validTeamRole(role string) bool {
	return role == "member" || role == "moderator" || role == "leader"
}
func validTeamSource(typ, id string) bool {
	return (typ == "manual" && id == "") || (typ == "group" && id != "")
}

// The migration ledger row is a portable transaction mutex. Taking it before
// any membership/group writes serializes cross-team zero-to-one transitions and
// multi-user last-leader changes without lock-order inversions in bulk syncs.
// PostgreSQL holds the row lock until commit; SQLite serializes its writer.
func (s *Store) lockTeamMembership(ctx context.Context) error {
	return s.exec(ctx, `UPDATE schema_migration SET name=name WHERE name='0027_team_membership'`)
}
func (s *Store) activeTeam(ctx context.Context, id string) error {
	var archived string
	err := s.queryRow(ctx, `SELECT archived_at FROM team WHERE id=?`, id).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if archived != "" {
		return ErrTeamArchived
	}
	return nil
}

// AddTeamMember adds an independent source. Existing membership roles are never
// overwritten by source reconciliation; use SetTeamMemberRole to change them.
func (s *Store) AddTeamMember(ctx context.Context, teamID, userID, role, sourceType, sourceID string) error {
	if !validTeamRole(role) {
		return ErrInvalidTeamRole
	}
	if !validTeamSource(sourceType, sourceID) {
		return errors.New("invalid team membership source")
	}
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, teamID); err != nil {
			return err
		}
		if err := s.lockTeamAttributionUser(ctx, userID); err != nil {
			return err
		}
		var active int
		if err := s.queryRow(ctx, `SELECT is_active FROM app_user WHERE id=?`, userID).Scan(&active); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if active != 1 {
			return errors.New("cannot add inactive user to team")
		}
		var before int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_member m JOIN team t ON t.id=m.team_id WHERE m.user_id=? AND t.archived_at=''`, userID).Scan(&before); err != nil {
			return err
		}
		if before == 0 {
			effectiveAt := FormatTime(nowUTC())
			if err := s.exec(ctx, `INSERT INTO team_attribution_transition(id,user_id,team_id,effective_at) VALUES (?,?,?,?)`, NewID(), userID, teamID, effectiveAt); err != nil {
				return err
			}
			if err := s.exec(ctx, `UPDATE usage_event SET team_ids=? WHERE user_id=? AND team_ids='' AND created_at<=?`, teamID, userID, effectiveAt); err != nil {
				return err
			}
		}
		if err := s.exec(ctx, `INSERT INTO team_member(team_id,user_id,role) VALUES (?,?,?) ON CONFLICT(team_id,user_id) DO NOTHING`, teamID, userID, role); err != nil {
			return err
		}
		if err := s.exec(ctx, `INSERT INTO team_membership_source(team_id,user_id,source_type,source_id) VALUES (?,?,?,?) ON CONFLICT(team_id,user_id,source_type,source_id) DO NOTHING`, teamID, userID, sourceType, sourceID); err != nil {
			return err
		}
		return s.syncLegacyTeamLead(ctx, teamID)
	})
}
func (s *Store) TeamRole(ctx context.Context, teamID, userID string) (string, error) {
	var role string
	err := s.queryRow(ctx, `SELECT role FROM team_member WHERE team_id=? AND user_id=?`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}
func (s *Store) guardTeamLeader(ctx context.Context, teamID, userID string) error {
	role, err := s.TeamRole(ctx, teamID, userID)
	if err != nil {
		return err
	}
	if role != "leader" {
		return nil
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_member WHERE team_id=? AND role='leader'`, teamID).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastTeamLeader
	}
	return nil
}

// syncLegacyTeamLead keeps the single-lead compatibility field on a real leader.
func (s *Store) syncLegacyTeamLead(ctx context.Context, teamID string) error {
	return s.exec(ctx, `UPDATE team SET lead_user_id=COALESCE((SELECT user_id FROM team_member WHERE team_id=? AND role='leader' ORDER BY user_id LIMIT 1),'') WHERE id=? AND NOT EXISTS (SELECT 1 FROM team_member WHERE team_id=? AND user_id=team.lead_user_id AND role='leader')`, teamID, teamID, teamID)
}
func (s *Store) RemoveTeamMemberSource(ctx context.Context, teamID, userID, sourceType, sourceID string) error {
	if !validTeamSource(sourceType, sourceID) {
		return errors.New("invalid team membership source")
	}
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, teamID); err != nil {
			return err
		}
		var target, others int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE team_id=? AND user_id=? AND source_type=? AND source_id=?`, teamID, userID, sourceType, sourceID).Scan(&target); err != nil {
			return err
		}
		if target == 0 {
			return nil
		}
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE team_id=? AND user_id=?`, teamID, userID).Scan(&others); err != nil {
			return err
		}
		// Directory removal is authoritative even if it leaves governance orphaned.
		// Only a manual removal may be rejected to retain the final leader.
		if others == 1 && sourceType == "manual" {
			if err := s.guardTeamLeader(ctx, teamID, userID); err != nil {
				return err
			}
		}
		if err := s.exec(ctx, `DELETE FROM team_membership_source WHERE team_id=? AND user_id=? AND source_type=? AND source_id=?`, teamID, userID, sourceType, sourceID); err != nil {
			return err
		}
		if others == 1 {
			if err := s.exec(ctx, `DELETE FROM team_member WHERE team_id=? AND user_id=?`, teamID, userID); err != nil {
				return err
			}
			return s.syncLegacyTeamLead(ctx, teamID)
		}
		return nil
	})
}
func (s *Store) SetTeamMemberRole(ctx context.Context, teamID, userID, role string) error {
	if !validTeamRole(role) {
		return ErrInvalidTeamRole
	}
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, teamID); err != nil {
			return err
		}
		if _, err := s.TeamRole(ctx, teamID, userID); err != nil {
			return err
		}
		if role != "leader" {
			if err := s.guardTeamLeader(ctx, teamID, userID); err != nil {
				return err
			}
		}
		if err := s.exec(ctx, `UPDATE team_member SET role=? WHERE team_id=? AND user_id=?`, role, teamID, userID); err != nil {
			return err
		}
		return s.syncLegacyTeamLead(ctx, teamID)
	})
}
func (s *Store) TeamMembers(ctx context.Context, teamID string) ([]*TeamMember, error) {
	rows, err := s.query(ctx, `SELECT m.user_id,COALESCE(u.email,''),COALESCE(u.name,''),m.role FROM team_member m LEFT JOIN app_user u ON u.id=m.user_id WHERE m.team_id=? ORDER BY m.user_id`, teamID)
	if err != nil {
		return nil, err
	}
	out := []*TeamMember{}
	for rows.Next() {
		m := &TeamMember{Sources: []TeamMembershipSource{}}
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.ID = m.UserID
		out = append(out, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, m := range out {
		rows, err := s.query(ctx, `SELECT source_type,source_id FROM team_membership_source WHERE team_id=? AND user_id=? ORDER BY source_type,source_id`, teamID, m.UserID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var src TeamMembershipSource
			if err := rows.Scan(&src.SourceType, &src.SourceID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			m.Sources = append(m.Sources, src)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListTeams returns all teams with lead names and member counts.
func (s *Store) ListTeams(ctx context.Context) ([]*Team, error) {
	return s.listTeams(ctx, " WHERE t.archived_at='' ")
}
func (s *Store) listTeams(ctx context.Context, filter string, args ...any) ([]*Team, error) {
	rows, err := s.query(ctx, `SELECT t.id,t.name,t.lead_user_id,t.lead_can_edit_quotas,t.created_at,
 COALESCE((SELECT u.name FROM app_user u WHERE u.id=t.lead_user_id),''),
 (SELECT COUNT(*) FROM team_member m WHERE m.team_id=t.id),t.listed,t.archived_at
 FROM team t `+filter+` ORDER BY t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Team{}
	for rows.Next() {
		var t Team
		var delegate, listed int
		var created, archived string
		if err := rows.Scan(&t.ID, &t.Name, &t.LeadUserID, &delegate, &created, &t.LeadName, &t.MemberCount, &listed, &archived); err != nil {
			return nil, fmt.Errorf("scan team: %w", err)
		}
		t.LeadCanEditQuotas = delegate == 1
		t.Listed = listed == 1
		t.ArchivedAt = ParseTime(archived)
		t.CreatedAt = ParseTime(created)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// TeamByID loads one team.
func (s *Store) TeamByID(ctx context.Context, id string) (*Team, error) {
	teams, err := s.listTeams(ctx, " WHERE t.id=? ", id)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

// CreateTeam adds a listed team. Migrated teams remain conservatively unlisted.
func (s *Store) CreateTeam(ctx context.Context, name, leadUserID string) (*Team, error) {
	t := &Team{ID: NewID(), Name: name, LeadUserID: leadUserID, Listed: true, CreatedAt: nowUTC()}
	err := s.modelTx(ctx, func(s *Store) error {
		if err := s.exec(ctx, `INSERT INTO team (id,name,lead_user_id,lead_can_edit_quotas,created_at,listed) VALUES (?,?,?,?,?,?)`, t.ID, name, leadUserID, 0, FormatTime(t.CreatedAt), 1); err != nil {
			return err
		}
		// Empty lead is supported for administrator-created draft teams.
		if leadUserID != "" {
			return s.AddTeamMember(ctx, t.ID, leadUserID, "leader", "manual", "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// UpdateTeam changes name, lead, and quota delegation.
func (s *Store) UpdateTeam(ctx context.Context, id, name, leadUserID string, delegate bool) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, id); err != nil {
			return err
		}
		var oldLead string
		if err := s.queryRow(ctx, `SELECT lead_user_id FROM team WHERE id=?`, id).Scan(&oldLead); err != nil {
			return err
		}
		// Legacy metadata edits must not turn a directory leader into a manual
		// member. Selecting a different lead is explicit; other leaders remain.
		if leadUserID != "" && leadUserID != oldLead {
			if err := s.AddTeamMember(ctx, id, leadUserID, "leader", "manual", ""); err != nil {
				return err
			}
			if err := s.SetTeamMemberRole(ctx, id, leadUserID, "leader"); err != nil {
				return err
			}
		} else if leadUserID == "" {
			// Clearing a legacy display field must not bypass the leader invariant.
			var n int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_member WHERE team_id=? AND role='leader'`, id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return ErrLastTeamLeader
			}
		}
		return s.exec(ctx, `UPDATE team SET name=?,lead_user_id=?,lead_can_edit_quotas=? WHERE id=?`, name, leadUserID, boolInt(delegate), id)
	})
}

// DeleteTeam archives identity and roster for historical labels. No usage moves.
func (s *Store) DeleteTeam(ctx context.Context, id string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if _, err := s.TeamByID(ctx, id); err != nil {
			return err
		}
		return s.exec(ctx, `UPDATE team SET archived_at=?,listed=0 WHERE id=? AND archived_at=''`, FormatTime(nowUTC()), id)
	})
}

// SetTeamMembers replaces manual sources only; group membership and roles survive.
func (s *Store) SetTeamMembers(ctx context.Context, teamID string, userIDs []string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, teamID); err != nil {
			return err
		}
		old, err := s.teamStringIDs(ctx, `SELECT user_id FROM team_membership_source WHERE team_id=? AND source_type='manual' AND source_id='' ORDER BY user_id`, teamID)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, uid := range userIDs {
			keep[uid] = true
			if err := s.AddTeamMember(ctx, teamID, uid, "member", "manual", ""); err != nil {
				return err
			}
		}
		for _, uid := range old {
			if !keep[uid] {
				if err := s.RemoveTeamMemberSource(ctx, teamID, uid, "manual", ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// UpdateUserAndTeams commits account and optional manual-team edits together.
// Validate team identities before updating the account; additions then observe
// the resulting active flag, so disabling cannot silently add memberships.
func (s *Store) UpdateUserAndTeams(ctx context.Context, userID, role string, active bool, teamIDs *[]string) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockTeamMembership(ctx); err != nil {
			return err
		}
		if _, err := tx.UserByID(ctx, userID); err != nil {
			return err
		}
		if teamIDs != nil {
			for _, id := range *teamIDs {
				if err := tx.activeTeam(ctx, id); err != nil {
					return err
				}
			}
		}
		if err := tx.UpdateUser(ctx, userID, role, active); err != nil {
			return err
		}
		if teamIDs != nil {
			return tx.SetUserTeams(ctx, userID, *teamIDs)
		}
		return nil
	})
}

// SetUserTeams replaces only this user's manual sources on active teams. Other
// users, directory sources, and archived historical rosters remain untouched.
func (s *Store) SetUserTeams(ctx context.Context, userID string, teamIDs []string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		var id string
		if err := s.queryRow(ctx, `SELECT id FROM app_user WHERE id=?`, userID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		keep := make(map[string]bool, len(teamIDs))
		for _, tid := range teamIDs {
			if err := s.activeTeam(ctx, tid); err != nil {
				return err
			}
			keep[tid] = true
		}
		old, err := s.teamStringIDs(ctx, `SELECT src.team_id FROM team_membership_source src JOIN team t ON t.id=src.team_id WHERE src.user_id=? AND src.source_type='manual' AND src.source_id='' AND t.archived_at='' ORDER BY src.team_id`, userID)
		if err != nil {
			return err
		}
		// Add first to avoid a spurious zero-to-one transition when switching teams.
		for _, tid := range teamIDs {
			if err := s.AddTeamMember(ctx, tid, userID, "member", "manual", ""); err != nil {
				return err
			}
		}
		for _, tid := range old {
			if !keep[tid] {
				if err := s.RemoveTeamMemberSource(ctx, tid, userID, "manual", ""); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// teamStringIDs drains and closes rows before callers perform nested queries.
func (s *Store) teamStringIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) TeamGroupIDs(ctx context.Context, teamID string) ([]string, error) {
	return s.teamStringIDs(ctx, `SELECT group_id FROM team_group_mapping WHERE team_id=? ORDER BY group_id`, teamID)
}
func (s *Store) SetTeamGroupMappings(ctx context.Context, teamID string, groupIDs []string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := s.activeTeam(ctx, teamID); err != nil {
			return err
		}
		old, err := s.TeamGroupIDs(ctx, teamID)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, gid := range groupIDs {
			var id string
			if err := s.queryRow(ctx, `SELECT id FROM user_group WHERE id=?`, gid).Scan(&id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			keep[gid] = true
			if err := s.exec(ctx, `INSERT INTO team_group_mapping(team_id,group_id) VALUES (?,?) ON CONFLICT(team_id,group_id) DO NOTHING`, teamID, gid); err != nil {
				return err
			}
			if err := s.ReconcileTeamGroup(ctx, gid); err != nil {
				return err
			}
		}
		for _, gid := range old {
			if !keep[gid] {
				if err := s.exec(ctx, `DELETE FROM team_group_mapping WHERE team_id=? AND group_id=?`, teamID, gid); err != nil {
					return err
				}
				if err := s.ReconcileTeamGroup(ctx, gid); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ReconcileTeamGroup applies authoritative group sources only. Revocation may
// leave a team without a leader, but must never preserve directory-revoked access.
func (s *Store) ReconcileTeamGroup(ctx context.Context, groupID string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		teams, err := s.teamStringIDs(ctx, `SELECT id FROM team WHERE archived_at='' AND (id IN (SELECT team_id FROM team_group_mapping WHERE group_id=?) OR id IN (SELECT team_id FROM team_membership_source WHERE source_type='group' AND source_id=?)) ORDER BY id`, groupID, groupID)
		if err != nil {
			return err
		}
		users, err := s.teamStringIDs(ctx, `SELECT m.user_id FROM group_member m JOIN app_user u ON u.id=m.user_id WHERE m.group_id=? AND u.is_active=1 ORDER BY m.user_id`, groupID)
		if err != nil {
			return err
		}
		for _, tid := range teams {
			var mapped int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_group_mapping WHERE team_id=? AND group_id=?`, tid, groupID).Scan(&mapped); err != nil {
				return err
			}
			keep := map[string]bool{}
			if mapped > 0 {
				for _, uid := range users {
					keep[uid] = true
					if err := s.AddTeamMember(ctx, tid, uid, "member", "group", groupID); err != nil {
						return err
					}
				}
			}
			old, err := s.teamStringIDs(ctx, `SELECT user_id FROM team_membership_source WHERE team_id=? AND source_type='group' AND source_id=? ORDER BY user_id`, tid, groupID)
			if err != nil {
				return err
			}
			for _, uid := range old {
				if !keep[uid] {
					if err := s.RemoveTeamMemberSource(ctx, tid, uid, "group", groupID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// TeamsForUser lists the teams a user belongs to.
func (s *Store) TeamsForUser(ctx context.Context, userID string) ([]*Team, error) {
	all, err := s.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT team_id,role FROM team_member WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	member := map[string]string{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		member[id] = role
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []*Team{}
	for _, t := range all {
		if role := member[t.ID]; role != "" {
			t.Role = role
			out = append(out, t)
		}
	}
	return out, nil
}

// TeamMemberIDs lists the user ids in a team.
func (s *Store) TeamMemberIDs(ctx context.Context, teamID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT user_id FROM team_member WHERE team_id = ?`, teamID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan team member: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
