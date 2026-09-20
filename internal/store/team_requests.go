package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrTeamForbidden = errors.New("team action is not permitted")
var ErrTeamUnlisted = errors.New("unlisted teams accept direct additions only")
var ErrTeamRequestState = errors.New("request is not pending or user is already a member")

type TeamJoinRequest struct {
	TeamName       string    `json:"team_name"`
	UserEmail      string    `json:"user_email"`
	UserName       string    `json:"user_name"`
	ID             string    `json:"id"`
	TeamID         string    `json:"team_id"`
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ReviewerUserID string    `json:"reviewer_user_id"`
	Reason         string    `json:"reason"`
	DecisionReason string    `json:"decision_reason"`
}

// WithTeamAction serializes authorization with all membership mutations. Callers
// must check their action-specific role inside fn, using the transaction Store.
func (s *Store) WithTeamAction(ctx context.Context, teamID, actorID string, fn func(*Store) error) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := tx.exec(ctx, `UPDATE team SET name=name WHERE id=?`, teamID); err != nil {
			return err
		}
		if err := tx.activeTeam(ctx, teamID); err != nil {
			return err
		}
		u, err := tx.UserByID(ctx, actorID)
		if err != nil {
			return err
		}
		if !u.IsActive {
			return ErrTeamForbidden
		}
		return fn(tx)
	})
}
func (s *Store) teamRequests(ctx context.Context, filter string, args ...any) ([]*TeamJoinRequest, error) {
	rows, err := s.query(ctx, `SELECT r.id,r.team_id,r.user_id,r.status,r.created_at,r.updated_at,r.reviewer_user_id,r.reason,r.decision_reason,t.name,u.email,u.name FROM team_join_request r JOIN team t ON t.id=r.team_id JOIN app_user u ON u.id=r.user_id WHERE `+filter+` ORDER BY r.created_at,r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TeamJoinRequest{}
	for rows.Next() {
		v := &TeamJoinRequest{}
		var created, updated string
		if err := rows.Scan(&v.ID, &v.TeamID, &v.UserID, &v.Status, &created, &updated, &v.ReviewerUserID, &v.Reason, &v.DecisionReason, &v.TeamName, &v.UserEmail, &v.UserName); err != nil {
			return nil, err
		}
		v.CreatedAt = ParseTime(created)
		v.UpdatedAt = ParseTime(updated)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) TeamRequestsForUser(ctx context.Context, userID string) ([]*TeamJoinRequest, error) {
	return s.teamRequests(ctx, "r.user_id=?", userID)
}
func (s *Store) PendingTeamRequests(ctx context.Context, teamID string) ([]*TeamJoinRequest, error) {
	return s.teamRequests(ctx, "r.team_id=? AND r.status='pending'", teamID)
}
func (s *Store) TeamActionEvent(ctx context.Context, teamID, actorID, action, target, detail string) error {
	payload, err := json.Marshal(map[string]string{"user_id": target, "detail": detail})
	if err != nil {
		return err
	}
	if err := s.AppendAudit(ctx, &AuditEntry{ActorUserID: actorID, Action: action, ResourceType: "team", ResourceID: teamID, NewValue: string(payload)}); err != nil {
		return err
	}
	if target != "" {
		return s.CreateNotification(ctx, &Notification{UserID: target, Severity: "info", Title: action, Body: detail})
	}
	return nil
}

// UpdateTeamProfile only changes presentation/discoverability, not membership
// or legacy leadership. Authorization belongs in the surrounding WithTeamAction.
func (s *Store) UpdateTeamProfile(ctx context.Context, teamID string, name *string, listed *bool) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := tx.activeTeam(ctx, teamID); err != nil {
			return err
		}
		if name != nil {
			if err := tx.exec(ctx, `UPDATE team SET name=? WHERE id=?`, *name, teamID); err != nil {
				return err
			}
		}
		if listed != nil {
			return tx.exec(ctx, `UPDATE team SET listed=? WHERE id=?`, boolInt(*listed), teamID)
		}
		return nil
	})
}

// TeamActorRole uses the authoritative user and membership on this Store's transaction.
func (s *Store) TeamActorRole(ctx context.Context, teamID, userID string) (string, error) {
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		return "", err
	}
	if !u.IsActive {
		return "", ErrTeamForbidden
	}
	if u.IsAdmin() {
		return "admin", nil
	}
	role, err := s.TeamRole(ctx, teamID, userID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return role, err
}
func (s *Store) CancelTeamJoinRequest(ctx context.Context, teamID, userID string) error {
	return s.WithTeamAction(ctx, teamID, userID, func(tx *Store) error {
		reqs, err := tx.teamRequests(ctx, "r.team_id=? AND r.user_id=?", teamID, userID)
		if err != nil {
			return err
		}
		if len(reqs) == 0 {
			return ErrNotFound
		}
		if reqs[0].Status != "pending" {
			return ErrTeamRequestState
		}
		if err := tx.exec(ctx, `UPDATE team_join_request SET status='cancelled',updated_at=? WHERE id=?`, FormatTime(nowUTC()), reqs[0].ID); err != nil {
			return err
		}
		return tx.TeamActionEvent(ctx, teamID, userID, "team.request.cancelled", "", "Membership request cancelled for "+reqs[0].TeamName)
	})
}
func (s *Store) DecideTeamJoinRequest(ctx context.Context, teamID, requestID, actorID string, approve bool, reason string) error {
	return s.WithTeamAction(ctx, teamID, actorID, func(tx *Store) error {
		role, err := tx.TeamActorRole(ctx, teamID, actorID)
		if err != nil {
			return err
		}
		if role != "admin" && role != "leader" && role != "moderator" {
			return ErrTeamForbidden
		}
		reqs, err := tx.teamRequests(ctx, "r.team_id=? AND r.id=?", teamID, requestID)
		if err != nil {
			return err
		}
		if len(reqs) == 0 {
			return ErrNotFound
		}
		req := reqs[0]
		if req.UserID == actorID {
			return ErrTeamForbidden
		}
		if req.Status != "pending" {
			return ErrTeamRequestState
		}
		user, err := tx.UserByID(ctx, req.UserID)
		if err != nil {
			return err
		}
		if !user.IsActive {
			return ErrTeamForbidden
		}
		status := "rejected"
		if approve {
			team, err := tx.TeamByID(ctx, teamID)
			if err != nil {
				return err
			}
			if !team.Listed {
				return ErrTeamUnlisted
			}
			if _, err := tx.TeamRole(ctx, teamID, req.UserID); err == nil {
				return ErrTeamRequestState
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := tx.AddTeamMember(ctx, teamID, req.UserID, "member", "manual", ""); err != nil {
				return err
			}
			status = "approved"
		}
		if err := tx.exec(ctx, `UPDATE team_join_request SET status=?,updated_at=?,reviewer_user_id=?,decision_reason=? WHERE id=?`, status, FormatTime(nowUTC()), actorID, reason, requestID); err != nil {
			return err
		}
		return tx.TeamActionEvent(ctx, teamID, actorID, "team.request."+status, req.UserID, "Team "+req.TeamName+": "+reason)
	})
}
func (s *Store) CreateTeamJoinRequest(ctx context.Context, teamID, userID, reason string) (*TeamJoinRequest, error) {
	var result *TeamJoinRequest
	err := s.WithTeamAction(ctx, teamID, userID, func(tx *Store) error {
		team, err := tx.TeamByID(ctx, teamID)
		if err != nil {
			return err
		}
		if !team.Listed {
			return ErrTeamUnlisted
		}
		if _, err := tx.TeamRole(ctx, teamID, userID); err == nil {
			return ErrTeamRequestState
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		existing, err := tx.teamRequests(ctx, "r.team_id=? AND r.user_id=?", teamID, userID)
		if err != nil {
			return err
		}
		if len(existing) > 0 && existing[0].Status == "pending" {
			result = existing[0]
			return nil
		}
		now := nowUTC()
		result = &TeamJoinRequest{ID: NewID(), TeamID: teamID, UserID: userID, Status: "pending", CreatedAt: now, UpdatedAt: now, Reason: reason}
		if len(existing) > 0 {
			result.ID = existing[0].ID
			result.CreatedAt = existing[0].CreatedAt
		}
		if err := tx.exec(ctx, `INSERT INTO team_join_request(id,team_id,user_id,status,created_at,updated_at,reviewer_user_id,reason) VALUES (?,?,?,'pending',?,?,'',?) ON CONFLICT(team_id,user_id) DO UPDATE SET status='pending',updated_at=excluded.updated_at,reviewer_user_id='',reason=excluded.reason,decision_reason=''`, result.ID, teamID, userID, FormatTime(result.CreatedAt), FormatTime(now), reason); err != nil {
			return err
		}
		members, err := tx.TeamMembers(ctx, teamID)
		if err != nil {
			return err
		}
		applicant, err := tx.UserByID(ctx, userID)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.Role == "leader" || m.Role == "moderator" {
				if err := tx.CreateNotification(ctx, &Notification{UserID: m.UserID, Severity: "info", Title: "Team join request", Body: applicant.Email + " requested to join " + team.Name + ": " + reason}); err != nil {
					return err
				}
			}
		}
		return tx.TeamActionEvent(ctx, teamID, userID, "team.request.created", "", "Membership requested for "+team.Name+": "+reason)
	})
	return result, err
}
