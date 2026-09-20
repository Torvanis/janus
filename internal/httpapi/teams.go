package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
)

func (s *Server) mountTeamRoutes(r chi.Router) {
	r.Get("/teams", s.handleTeamsBrowse)
	r.Get("/teams/{id}", s.handleTeamView)
	r.Patch("/teams/{id}", s.handleTeamMetadata)
	r.Put("/teams/{id}/groups", s.handleTeamGroups)
	r.Get("/teams/{id}/candidates", s.handleTeamCandidates)
	r.Post("/teams/{id}/members", s.handleTeamAddMember)
	r.Patch("/teams/{id}/members/{userID}", s.handleTeamMemberRole)
	r.Delete("/teams/{id}/members/{userID}", s.handleTeamRemoveMember)
	r.Get("/team-requests", s.handleMyTeamRequests)
	r.Post("/teams/{id}/join-requests", s.handleTeamJoinRequest)
	r.Delete("/teams/{id}/join-requests", s.handleTeamCancelRequest)
	r.Post("/teams/{id}/requests/{requestID}/decision", s.handleTeamDecision)
}
func (s *Server) handleTeamMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   *string `json:"name"`
		Listed *bool   `json:"listed"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Name != nil {
		*body.Name = strings.TrimSpace(*body.Name)
		if *body.Name == "" || len(*body.Name) > 200 {
			WriteError(w, r, ErrInvalidRequest("name must contain 1 to 200 bytes"))
			return
		}
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	var team *store.Team
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		if role != "admin" && role != "leader" {
			return store.ErrTeamForbidden
		}
		if err := tx.UpdateTeamProfile(ctx, id, body.Name, body.Listed); err != nil {
			return err
		}
		team, err = tx.TeamByID(ctx, id)
		if err != nil {
			return err
		}
		detail, _ := json.Marshal(body)
		return tx.TeamActionEvent(ctx, id, actor, "team.updated", "", string(detail))
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"team": team})
}
func (s *Server) handleTeamGroups(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GroupIDs []string `json:"group_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.GroupIDs == nil {
		WriteError(w, r, ErrInvalidRequest("group_ids array is required"))
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	var ids []string
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		if role != "admin" {
			return store.ErrTeamForbidden
		}
		if err := tx.SetTeamGroupMappings(ctx, id, body.GroupIDs); err != nil {
			return err
		}
		ids, err = tx.TeamGroupIDs(ctx, id)
		if err != nil {
			return err
		}
		detail, _ := json.Marshal(ids)
		return tx.TeamActionEvent(ctx, id, actor, "team.groups.updated", "", string(detail))
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"group_ids": ids})
}
func (s *Server) handleTeamCandidates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	out := []map[string]string{}
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		if !teamManager(role) {
			return store.ErrTeamForbidden
		}
		users, _, err := tx.ListUsers(ctx, store.UserFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), Active: "active", Limit: 50})
		if err != nil {
			return err
		}
		for _, u := range users {
			out = append(out, map[string]string{"id": u.ID, "email": u.Email, "name": u.Name})
		}
		return nil
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"users": out})
}
func (s *Server) handleTeamAddMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.UserID == "" {
		WriteError(w, r, ErrInvalidRequest("user_id is required"))
		return
	}
	if body.Role == "" {
		body.Role = "member"
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		if !teamManager(role) || (role == "moderator" && body.Role != "member") {
			return store.ErrTeamForbidden
		}
		target, err := tx.UserByID(ctx, body.UserID)
		if err != nil {
			return err
		}
		if !target.IsActive {
			return store.ErrTeamForbidden
		}
		existing, err := tx.TeamRole(ctx, id, body.UserID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if role == "moderator" && existing != "" && existing != "member" {
			return store.ErrTeamForbidden
		}
		if err := tx.AddTeamMember(ctx, id, body.UserID, body.Role, "manual", ""); err != nil {
			return err
		}
		// Adding a source must never silently demote an existing member. Role
		// changes are explicit through the PATCH endpoint.
		return tx.TeamActionEvent(ctx, id, actor, "team.member.added", body.UserID, "Manual membership added to team "+id)
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"status": "added"})
}
func (s *Server) handleTeamMemberRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	target := chi.URLParam(r, "userID")
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		if role != "admin" && role != "leader" {
			return store.ErrTeamForbidden
		}
		if err := tx.SetTeamMemberRole(ctx, id, target, body.Role); err != nil {
			return err
		}
		return tx.TeamActionEvent(ctx, id, actor, "team.member.role_changed", target, "Team "+id+" role: "+body.Role)
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"role": body.Role})
}
func (s *Server) handleTeamRemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	actor := UserFrom(ctx).ID
	target := chi.URLParam(r, "userID")
	retained := false
	sources := []store.TeamMembershipSource{}
	err := s.Store.WithTeamAction(ctx, id, actor, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(ctx, id, actor)
		if err != nil {
			return err
		}
		targetRole, err := tx.TeamRole(ctx, id, target)
		if err != nil {
			return err
		}
		if actor != target && (!teamManager(role) || (role == "moderator" && targetRole != "member")) {
			return store.ErrTeamForbidden
		}
		if err := tx.RemoveTeamMemberSource(ctx, id, target, "manual", ""); err != nil {
			return err
		}
		members, err := tx.TeamMembers(ctx, id)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.UserID == target {
				retained = true
				sources = m.Sources
			}
		}
		detail := "Manual membership removed from team " + id
		if retained {
			detail += "; directory membership remains. Ask an administrator to update the source group or team mapping."
		}
		return tx.TeamActionEvent(ctx, id, actor, "team.member.removed", target, detail)
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	message := "Manual membership removed."
	if retained {
		message = "Directory membership remains. Ask an administrator to update the source group or team mapping."
	}
	WriteJSON(w, 200, map[string]any{"membership_retained": retained, "sources": sources, "message": message})
}
func (s *Server) handleMyTeamRequests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := s.Store.UserByID(ctx, UserFrom(ctx).ID)
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	if !u.IsActive {
		teamHTTPError(w, r, store.ErrTeamForbidden)
		return
	}
	requests, err := s.Store.TeamRequestsForUser(ctx, u.ID)
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"requests": requests})
}
func (s *Server) handleTeamJoinRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	req, err := s.Store.CreateTeamJoinRequest(r.Context(), chi.URLParam(r, "id"), UserFrom(r.Context()).ID, body.Reason)
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"request": req})
}
func (s *Server) handleTeamCancelRequest(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.CancelTeamJoinRequest(r.Context(), chi.URLParam(r, "id"), UserFrom(r.Context()).ID); err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"status": "cancelled"})
}
func (s *Server) handleTeamDecision(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve *bool  `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Approve == nil {
		WriteError(w, r, ErrInvalidRequest("approve is required"))
		return
	}
	if err := s.Store.DecideTeamJoinRequest(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "requestID"), UserFrom(r.Context()).ID, *body.Approve, body.Reason); err != nil {
		teamHTTPError(w, r, err)
		return
	}
	status := "rejected"
	if *body.Approve {
		status = "approved"
	}
	WriteJSON(w, 200, map[string]any{"status": status})
}
func teamHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrTeamForbidden):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrTeamUnlisted), errors.Is(err, store.ErrTeamArchived), errors.Is(err, store.ErrTeamRequestState), errors.Is(err, store.ErrLastTeamLeader):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInvalidTeamRole):
		status = http.StatusBadRequest
	default:
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": "team_action_failed", "message": err.Error()}})
}
func teamManager(role string) bool { return role == "admin" || role == "leader" || role == "moderator" }
func (s *Server) handleTeamsBrowse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := s.Store.UserByID(ctx, UserFrom(ctx).ID)
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	if !u.IsActive {
		teamHTTPError(w, r, store.ErrTeamForbidden)
		return
	}
	teams, err := s.Store.ListTeams(ctx)
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	type directoryTeam struct {
		*store.Team
		CanManage           bool `json:"can_manage"`
		PendingRequestCount *int `json:"pending_request_count,omitempty"`
	}
	out := []directoryTeam{}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for _, team := range teams {
		role, err := s.Store.TeamRole(ctx, team.ID, u.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			teamHTTPError(w, r, err)
			return
		}
		if (team.Listed || role != "" || u.IsAdmin()) && strings.Contains(strings.ToLower(team.Name), q) {
			team.Role = role
			item := directoryTeam{Team: team, CanManage: u.IsAdmin() || teamManager(role)}
			if item.CanManage {
				requests, err := s.Store.PendingTeamRequests(ctx, team.ID)
				if err != nil {
					teamHTTPError(w, r, err)
					return
				}
				count := len(requests)
				item.PendingRequestCount = &count
			}
			out = append(out, item)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"teams": out})
}
func (s *Server) handleTeamView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var out map[string]any
	err := s.Store.WithTeamAction(ctx, id, UserFrom(ctx).ID, func(tx *store.Store) error {
		team, err := tx.TeamByID(ctx, id)
		if err != nil {
			return err
		}
		role, err := tx.TeamActorRole(ctx, id, UserFrom(ctx).ID)
		if err != nil {
			return err
		}
		if !team.Listed && role == "" {
			return store.ErrNotFound
		}
		membershipRole, err := tx.TeamRole(ctx, id, UserFrom(ctx).ID)
		if errors.Is(err, store.ErrNotFound) {
			membershipRole = ""
		} else if err != nil {
			return err
		}
		out = map[string]any{
			"team": team, "my_role": role,
			"membership_role": membershipRole, "actor_role": role,
			"can_manage": teamManager(role),
		}
		if role != "" {
			members, err := tx.TeamMembers(ctx, id)
			if err != nil {
				return err
			}
			out["members"] = members
		}
		if teamManager(role) {
			requests, err := tx.PendingTeamRequests(ctx, id)
			if err != nil {
				return err
			}
			out["requests"] = requests
			out["pending_request_count"] = len(requests)
			groups, err := tx.TeamGroupIDs(ctx, id)
			if err != nil {
				return err
			}
			out["group_ids"] = groups
		}
		return nil
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}
