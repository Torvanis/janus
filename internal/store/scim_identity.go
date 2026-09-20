package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func scimNormalize(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
func scimString(r map[string]any, key string) string {
	v, _ := r[key].(string)
	return strings.TrimSpace(v)
}
func scimKind(kind string) error {
	if kind != "Users" && kind != "Groups" {
		return scimInvalid("unsupported resource type")
	}
	return nil
}

func (s *Store) SCIMResource(ctx context.Context, kind, id string) (map[string]any, error) {
	if err := scimKind(kind); err != nil {
		return nil, err
	}
	var body string
	err := s.queryRow(ctx, `SELECT body_json FROM scim_resource WHERE kind=? AND id=? AND deleted_at=''`, kind, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal([]byte(body), &out)
	return out, err
}
func (s *Store) ListSCIMResources(ctx context.Context, kind string) ([]map[string]any, error) {
	if err := scimKind(kind); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT body_json FROM scim_resource WHERE kind=? AND deleted_at='' ORDER BY created_at,id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveSCIMResource replaces a resource and materializes its identity in the same
// transaction. The client cannot set server identity/metadata or application roles.
func (s *Store) SaveSCIMResource(ctx context.Context, kind, id string, resource map[string]any) (map[string]any, error) {
	if err := scimKind(kind); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resource)
	if err != nil {
		return nil, scimInvalid("resource must be JSON")
	}
	var r map[string]any
	if err = json.Unmarshal(raw, &r); err != nil || r == nil {
		return nil, scimInvalid("resource must be an object")
	}
	delete(r, "password")
	delete(r, "meta")
	delete(r, "id")
	for _, key := range []string{"displayName", "userName", "externalId"} {
		if v, exists := r[key]; exists {
			if _, ok := v.(string); !ok {
				return nil, scimInvalid(key + " must be a string")
			}
		}
	}
	if v, exists := r["name"]; exists {
		nm, ok := v.(map[string]any)
		if !ok {
			return nil, scimInvalid("name must be an object")
		}
		for _, key := range []string{"formatted", "givenName", "familyName", "middleName", "honorificPrefix", "honorificSuffix"} {
			if v, exists := nm[key]; exists {
				if _, ok := v.(string); !ok {
					return nil, scimInvalid("name fields must be strings")
				}
			}
		}
	}
	var out map[string]any
	err = s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		creating := id == ""
		created := FormatTime(nowUTC())
		if !creating {
			if _, err := s.SCIMResource(ctx, kind, id); err != nil {
				return err
			}
			if err := s.queryRow(ctx, `SELECT created_at FROM scim_resource WHERE id=?`, id).Scan(&created); err != nil {
				return err
			}
		}
		external, _ := r["externalId"].(string)
		if v, ok := r["externalId"]; ok {
			if _, ok = v.(string); !ok {
				return scimInvalid("externalId must be a string")
			}
		}
		if external != "" {
			var n int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM scim_resource WHERE kind=? AND external_id=? AND id<>?`, kind, external, id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return scimConflict("externalId is already reserved")
			}
		}
		username := ""
		if kind == "Users" {
			username = scimNormalize(scimString(r, "userName"))
			if username == "" {
				return scimInvalid("userName is required")
			}
			var n int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM scim_resource WHERE kind='Users' AND username_norm=? AND id<>?`, username, id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return scimConflict("userName is already reserved")
			}
			active := true
			if v, ok := r["active"]; ok {
				b, ok := v.(bool)
				if !ok {
					return scimInvalid("active must be a boolean")
				}
				active = b
			}
			r["active"] = active
			name := scimString(r, "displayName")
			if name == "" {
				if nm, ok := r["name"].(map[string]any); ok {
					name = scimString(nm, "formatted")
					if name == "" {
						name = strings.TrimSpace(scimString(nm, "givenName") + " " + scimString(nm, "familyName"))
					}
				}
			}
			if name == "" {
				name = scimString(r, "userName")
			}
			email := username
			if value, exists := r["emails"]; exists {
				emails, ok := value.([]any)
				if !ok {
					return scimInvalid("emails must be an array")
				}
				for i, v := range emails {
					e, ok := v.(map[string]any)
					if !ok || scimString(e, "value") == "" {
						return scimInvalid("email value is required")
					}
					primary := false
					if v, exists := e["primary"]; exists {
						var valid bool
						primary, valid = v.(bool)
						if !valid {
							return scimInvalid("email primary must be a boolean")
						}
					}
					if i == 0 || primary {
						email = scimNormalize(scimString(e, "value"))
					}

				}
			}
			if creating {
				// The immutable directory subject (externalId, equal to the
				// sign-in provider subject) is the strongest identity link and
				// is consulted first: userName is often a directory shortname,
				// not the address an existing account signed in with.
				bySubject := []string{}
				if external != "" {
					var err error
					bySubject, err = s.teamStringIDs(ctx, `SELECT id FROM app_user WHERE auth_provider_id=? ORDER BY id`, external)
					if err != nil {
						return err
					}
					if len(bySubject) > 1 {
						return scimConflict("externalId matches multiple existing identities")
					}
				}
				matches, err := s.teamStringIDs(ctx, `SELECT id FROM app_user WHERE LOWER(TRIM(email))=? ORDER BY id`, username)
				if err != nil {
					return err
				}
				if len(matches) > 1 {
					return scimConflict("userName matches multiple existing identities")
				}
				if len(bySubject) == 1 && len(matches) == 1 && bySubject[0] != matches[0] {
					return scimConflict("externalId and userName identify different existing identities")
				}
				if len(bySubject) == 1 {
					matches = bySubject
				}
				if len(matches) == 1 {
					id = matches[0]
					var n int
					if err := s.queryRow(ctx, `SELECT COUNT(*) FROM scim_resource WHERE id=?`, id).Scan(&n); err != nil {
						return err
					}
					if n > 0 {
						return scimConflict("identity is already managed")
					}
				} else {
					id = NewID()
					if err := s.exec(ctx, `INSERT INTO app_user(id,auth_provider_id,email,name,role,is_active,created_at) VALUES(?,?,?,?,?,?,?)`, id, "scim:"+id, email, name, RoleUser, boolInt(active), created); err != nil {
						return err
					}
				}
			}
			if err := s.exec(ctx, `UPDATE app_user SET email=?,name=?,is_active=? WHERE id=?`, email, name, boolInt(active), id); err != nil {
				return err
			}
			if !active {
				if err := s.scimDeactivate(ctx, id); err != nil {
					return err
				}
			}
			if active {
				groups, err := s.GroupIDsForUser(ctx, id)
				if err != nil {
					return err
				}
				for _, gid := range groups {
					if err := s.ReconcileSCIMTeamGroup(ctx, gid); err != nil {
						return err
					}
				}
			}
		} else {
			name := scimString(r, "displayName")
			if name == "" {
				return scimInvalid("displayName is required")
			}
			var n int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM user_group WHERE LOWER(TRIM(name))=? AND id<>?`, scimNormalize(name), id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return scimConflict("displayName already exists")
			}
			members := []any{}
			if v, ok := r["members"]; ok {
				var valid bool
				members, valid = v.([]any)
				if !valid {
					return scimInvalid("members must be an array")
				}
			}
			ids := []string{}
			seen := map[string]bool{}
			for _, v := range members {
				m, ok := v.(map[string]any)
				if !ok {
					return scimInvalid("member must be an object")
				}
				uid, _ := m["value"].(string)
				if v, exists := m["type"]; exists {
					if _, ok := v.(string); !ok {
						return scimInvalid("member type must be a string")
					}
				}
				if uid == "" || (scimString(m, "type") != "" && !strings.EqualFold(scimString(m, "type"), "User")) {
					return scimInvalid("members must reference Users; nested groups are unsupported")
				}
				if _, err := s.SCIMResource(ctx, "Users", uid); err != nil {
					if errors.Is(err, ErrNotFound) {
						return scimInvalid("member must reference an existing SCIM User")
					}
					return err
				}
				if !seen[uid] {
					ids = append(ids, uid)
					seen[uid] = true
				}
			}
			if creating {
				id = NewID()
				if err := s.exec(ctx, `INSERT INTO user_group(id,name,from_idp,created_at) VALUES(?,?,0,?)`, id, name, created); err != nil {
					return err
				}
			} else {
				if err := s.exec(ctx, `UPDATE user_group SET name=? WHERE id=?`, name, id); err != nil {
					return err
				}
			}
			if err := s.exec(ctx, `DELETE FROM group_member WHERE group_id=?`, id); err != nil {
				return err
			}
			for _, uid := range ids {
				if err := s.exec(ctx, `INSERT INTO group_member(group_id,user_id) VALUES(?,?)`, id, uid); err != nil {
					return err
				}
			}
			r["members"] = members
			if err := s.ReconcileSCIMTeamGroup(ctx, id); err != nil {
				return err
			}
		}
		now := FormatTime(nowUTC())
		r["id"] = id
		r["meta"] = map[string]any{"resourceType": strings.TrimSuffix(kind, "s"), "created": created, "lastModified": now}
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if creating {
			err = s.exec(ctx, `INSERT INTO scim_resource(id,kind,external_id,username_norm,body_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, kind, external, username, string(body), created, now)
		} else {
			err = s.exec(ctx, `UPDATE scim_resource SET external_id=?,username_norm=?,body_json=?,updated_at=? WHERE id=? AND kind=?`, external, username, string(body), now, id, kind)
		}
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) scimDeactivate(ctx context.Context, id string) error {
	if err := s.exec(ctx, `UPDATE app_user SET is_active=0 WHERE id=?`, id); err != nil {
		return err
	}
	if err := s.exec(ctx, `UPDATE api_token SET revoked_at=? WHERE user_id=? AND revoked_at=''`, FormatTime(nowUTC()), id); err != nil {
		return err
	}
	if err := s.exec(ctx, `DELETE FROM session_team_context WHERE user_id=?`, id); err != nil {
		return err
	}
	if err := s.DeleteWebSessionsForUser(ctx, id); err != nil {
		return err
	}
	teams, err := s.teamStringIDs(ctx, `SELECT DISTINCT team_id FROM team_membership_source WHERE user_id=? AND source_type='group'`, id)
	if err != nil {
		return err
	}
	if err := s.exec(ctx, `DELETE FROM team_membership_source WHERE user_id=? AND source_type='group'`, id); err != nil {
		return err
	}
	for _, tid := range teams {
		if err := s.scimPruneMembership(ctx, tid, id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) DeleteSCIMResource(ctx context.Context, kind, id string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		if _, err := s.SCIMResource(ctx, kind, id); err != nil {
			return err
		}
		if kind == "Users" {
			if err := s.scimDeactivate(ctx, id); err != nil {
				return err
			}
		} else {
			if err := s.exec(ctx, `DELETE FROM group_member WHERE group_id=?`, id); err != nil {
				return err
			}
			if err := s.exec(ctx, `DELETE FROM team_group_mapping WHERE group_id=?`, id); err != nil {
				return err
			}
			if err := s.ReconcileSCIMTeamGroup(ctx, id); err != nil {
				return err
			}
			if err := s.exec(ctx, `DELETE FROM user_group WHERE id=?`, id); err != nil {
				return err
			}
		}
		return s.exec(ctx, `UPDATE scim_resource SET deleted_at=?,updated_at=? WHERE kind=? AND id=?`, FormatTime(nowUTC()), FormatTime(nowUTC()), kind, id)
	})
}

// scimPruneMembership deliberately bypasses the interactive last-leader guard:
// authoritative directory revocation must not preserve access for governance.
// The empty legacy lead field exposes the resulting orphan for administrator repair.
func (s *Store) scimPruneMembership(ctx context.Context, teamID, userID string) error {
	if err := s.exec(ctx, `DELETE FROM team_member WHERE team_id=? AND user_id=? AND NOT EXISTS (SELECT 1 FROM team_membership_source WHERE team_id=? AND user_id=?)`, teamID, userID, teamID, userID); err != nil {
		return err
	}
	return s.syncLegacyTeamLead(ctx, teamID)
}

// ReconcileSCIMTeamGroup is the authoritative directory variant. It keeps manual
// and other-group sources and existing roles, but never retains a revoked last
// leader. Callers handling SCIM-owned mappings should use this instead of the
// interactive ReconcileTeamGroup path.
func (s *Store) ReconcileSCIMTeamGroup(ctx context.Context, groupID string) error {
	return s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		teams, err := s.teamStringIDs(ctx, `SELECT id FROM team WHERE id IN (SELECT team_id FROM team_group_mapping WHERE group_id=?) OR id IN (SELECT team_id FROM team_membership_source WHERE source_type='group' AND source_id=?) ORDER BY id`, groupID, groupID)
		if err != nil {
			return err
		}
		users, err := s.teamStringIDs(ctx, `SELECT m.user_id FROM group_member m JOIN app_user u ON u.id=m.user_id WHERE m.group_id=? AND u.is_active=1 ORDER BY m.user_id`, groupID)
		if err != nil {
			return err
		}
		for _, tid := range teams {
			var mapped int
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_group_mapping g JOIN team t ON t.id=g.team_id WHERE g.team_id=? AND g.group_id=? AND t.archived_at=''`, tid, groupID).Scan(&mapped); err != nil {
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
			old, err := s.teamStringIDs(ctx, `SELECT user_id FROM team_membership_source WHERE team_id=? AND source_type='group' AND source_id=?`, tid, groupID)
			if err != nil {
				return err
			}
			for _, uid := range old {
				if !keep[uid] {
					if err := s.RemoveTeamMemberSource(ctx, tid, uid, "group", groupID); err != nil {
						if !errors.Is(err, ErrTeamArchived) {
							return err
						}
						// Archived teams reject interactive writes but directory revocation
						// still must remove the withdrawn source from their retained roster.
						if err := s.exec(ctx, `DELETE FROM team_membership_source WHERE team_id=? AND user_id=? AND source_type='group' AND source_id=?`, tid, uid, groupID); err != nil {
							return err
						}
						if err := s.scimPruneMembership(ctx, tid, uid); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
}

// ClaimSCIMIdentity is a provisioning-to-login bridge, not an email based login.
// A trusted directory externalId equal to the OIDC subject is checked first;
// providers without email_verified MUST map externalId to that immutable subject
// and the administrator must configure the same trusted issuer for both protocols.
// Otherwise linking requires an exact unique userName and verified email. Already
// bound immutable subjects do not need a repeated email-verification claim.
// An inactive/tombstoned identity stays disabled; callers must enforce IsActive.
func (s *Store) ClaimSCIMIdentity(ctx context.Context, authID, email, name string, emailVerified bool) (*User, error) {
	if authID == "" || strings.HasPrefix(authID, "scim:") {
		return nil, scimInvalid("invalid authentication subject")
	}
	var out *User
	err := s.modelTx(ctx, func(s *Store) error {
		if err := s.lockTeamMembership(ctx); err != nil {
			return err
		}
		var id, emailID string
		err := s.queryRow(ctx, `SELECT id FROM scim_resource WHERE kind='Users' AND external_id=?`, authID).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		bySubject := err == nil
		err = s.queryRow(ctx, `SELECT id FROM scim_resource WHERE kind='Users' AND username_norm=?`, scimNormalize(email)).Scan(&emailID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if bySubject && emailID != "" && emailID != id {
			return scimConflict("external subject and email identify different directory users")
		}
		if !bySubject {
			id = emailID
		}
		if id == "" {
			err = s.queryRow(ctx, `SELECT r.id FROM scim_resource r JOIN app_user u ON u.id=r.id WHERE r.kind='Users' AND u.auth_provider_id=?`, authID).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		u, err := s.UserByID(ctx, id)
		if err != nil {
			return err
		}
		if u.AuthID == authID {
			out = u
			return nil
		}
		if u.AuthID != "scim:"+id {
			return &SCIMStoreError{403, "", "directory identity is already bound"}
		}
		if !bySubject {
			if !emailVerified {
				return &SCIMStoreError{403, "", "a verified email is required for directory identity linking"}
			}
			matches, err := s.teamStringIDs(ctx, `SELECT id FROM app_user WHERE LOWER(TRIM(email))=?`, scimNormalize(email))
			if err != nil {
				return err
			}
			if len(matches) != 1 || matches[0] != id {
				return scimConflict("identity email is ambiguous")
			}
		}
		existing, err := s.UserByAuthID(ctx, authID)
		if err == nil && existing.ID != id {
			return scimConflict("authentication subject already exists")
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.exec(ctx, `UPDATE app_user SET auth_provider_id=? WHERE id=? AND auth_provider_id=?`, authID, id, "scim:"+id); err != nil {
			return err
		}
		out, err = s.UserByID(ctx, id)
		return err
	})
	return out, err
}
