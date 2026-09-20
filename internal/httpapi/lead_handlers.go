package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Lead-scoped quota management: an admin can delegate a team's quota
// management to its lead by toggling lead_can_edit_quotas on the team. These
// endpoints are the consumer of that flag — they authorize the CALLER AS THE
// LEAD of the specific team, never as a global role, and only while the
// delegation toggle is on. Every mutation is audit-logged like the admin
// equivalents.

// leadTeamForEdit authorizes the caller to manage quotas for team id.
func (s *Server) leadTeamForEdit(r *http.Request, teamID string) (*store.Team, *APIError) {
	user := UserFrom(r.Context())
	if user == nil {
		return nil, ErrUnauthenticated()
	}
	team, err := s.Store.TeamByID(r.Context(), teamID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFoundf("That team")
	}
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "load team for lead quota edit", "error", err.Error())
		return nil, ErrInternal()
	}
	role, roleErr := s.Store.TeamRole(r.Context(), teamID, user.ID)
	if roleErr != nil || role != "leader" {
		return nil, ErrForbidden("Only this team's lead can manage its quotas.")
	}
	if !team.LeadCanEditQuotas {
		return nil, ErrForbidden("Quota management has not been delegated to this team's lead. An administrator can enable it on the team.")
	}
	return team, nil
}

// leadTeamForMembers authorises a lead to manage their own team's roster.
//
// Deliberately NOT gated on LeadCanEditQuotas: that flag delegates spending
// authority, which is a different and much larger decision than deciding who
// is on your team. A lead who cannot set budgets can still say who reports to
// them; conflating the two would mean an admin has to hand over quota control
// just to let a lead add a teammate.
func (s *Server) leadTeamForMembers(r *http.Request, teamID string) (*store.Team, *APIError) {
	user := UserFrom(r.Context())
	if user == nil {
		return nil, ErrUnauthenticated()
	}
	team, err := s.Store.TeamByID(r.Context(), teamID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFoundf("That team")
	}
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "load team for lead member edit", "error", err.Error())
		return nil, ErrInternal()
	}
	// An admin can manage any team; a lead only their own.
	role, roleErr := s.Store.TeamRole(r.Context(), teamID, user.ID)
	if (roleErr != nil || role != "leader") && !user.IsAdmin() {
		return nil, ErrForbidden("Only this team's lead can manage its members.")
	}
	return team, nil
}

// handleLeadTeamMembers returns the roster of a team the caller leads.
func (s *Server) handleLeadTeamMembers(w http.ResponseWriter, r *http.Request) {
	team, apiErr := s.leadTeamForMembers(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	members, err := s.Store.TeamMemberIDs(r.Context(), team.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows := make([]map[string]any, 0, len(members))
	for _, id := range members {
		user, err := s.Store.UserByID(r.Context(), id)
		if err != nil {
			// A membership row for a deleted account: report the id rather
			// than dropping it silently, so the lead can clean it up.
			rows = append(rows, map[string]any{"id": id, "email": "", "name": ""})
			continue
		}
		rows = append(rows, map[string]any{"id": user.ID, "email": user.Email, "name": user.Name})
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"team":    map[string]any{"id": team.ID, "name": team.Name},
		"members": rows,
	})
}

// handleLeadSetTeamMembers replaces the roster of a team the caller leads.
//
// The whole set is sent rather than add/remove deltas so two concurrent edits
// cannot interleave into a state neither editor asked for.
func (s *Server) handleLeadSetTeamMembers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := UserFrom(r.Context())
	if actor == nil {
		WriteError(w, r, ErrUnauthenticated())
		return
	}
	var body struct {
		MemberIDs []string `json:"member_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	var members []string
	err := s.Store.WithTeamAction(r.Context(), id, actor.ID, func(tx *store.Store) error {
		role, err := tx.TeamActorRole(r.Context(), id, actor.ID)
		if err != nil {
			return err
		}
		if role != "admin" && role != "leader" {
			return store.ErrTeamForbidden
		}
		// Keep legacy field validation, but unexpected database failures remain
		// generic server errors rather than being disguised as invalid users.
		for _, uid := range body.MemberIDs {
			if _, err := tx.UserByID(r.Context(), uid); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return ErrInvalidRequest("One of the selected people no longer exists.").WithParam("member_ids")
				}
				return err
			}
		}
		if err := tx.SetTeamMembers(r.Context(), id, body.MemberIDs); err != nil {
			return err
		}
		members, err = tx.TeamMemberIDs(r.Context(), id)
		if err != nil {
			return err
		}
		detail, err := legacyTeamAuditDetail(r, tx, id)
		if err != nil {
			return err
		}
		return tx.TeamActionEvent(r.Context(), id, actor.ID, "team_members_updated", "", detail)
	})
	if err != nil {
		teamHTTPError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"member_ids": members})
}

// handleLeadTeams lists the teams the caller leads, each with its delegation
// state and, where delegated, the team's current quotas.
func (s *Server) handleLeadTeams(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	teams, err := s.Store.ListTeams(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	all, err := s.Store.ListQuotas(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, t := range teams {
		role, roleErr := s.Store.TeamRole(r.Context(), t.ID, user.ID)
		if roleErr != nil || role != "leader" {
			continue
		}
		quotas := []map[string]any{}
		if t.LeadCanEditQuotas {
			for _, q := range all {
				if q.SubjectType != "team" || q.SubjectID != t.ID {
					continue
				}
				status, err := s.Quota.StatusFor(r.Context(), q)
				if err != nil {
					WriteError(w, r, err)
					return
				}
				quotas = append(quotas, quotaStatusPayload(status))
			}
		}
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "member_count": t.MemberCount,
			"can_edit_quotas": t.LeadCanEditQuotas, "quotas": quotas,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"teams":   out,
		"metrics": s.quotaMetricOptions(),
		"windows": []map[string]string{
			{"value": store.WindowDaily, "label": "Per day"},
			{"value": store.WindowWeekly, "label": "Per week"},
			{"value": store.WindowMonthly, "label": "Per month"},
			{"value": store.WindowRolling24, "label": "Rolling 24 hours"},
			{"value": store.WindowRolling7, "label": "Rolling 7 days"},
			{"value": store.WindowRolling30, "label": "Rolling 30 days"},
		},
	})
}

// handleLeadCreateQuota creates a quota whose subject is forced to the team in
// the URL — a lead can never widen scope to another subject.
func (s *Server) handleLeadCreateQuota(w http.ResponseWriter, r *http.Request) {
	team, apiErr := s.leadTeamForEdit(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	var body quotaPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if !quota.ValidMetric(body.Metric) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported metric.").WithParam("metric"))
		return
	}
	if apiErr := s.localOnlyQuotaError(body.Metric); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if !quota.ValidWindow(body.Window) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported window.").WithParam("window"))
		return
	}
	if apiErr := s.quotaWindowRetentionError(body.Window, "team"); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if body.Limit == nil || *body.Limit <= 0 {
		WriteError(w, r, ErrInvalidRequest("The limit must be greater than zero.").WithParam("limit_value"))
		return
	}
	behavior := body.BreachBehavior
	if behavior != store.BreachHardKill {
		behavior = store.BreachLetFinish
	}
	limit := int64(*body.Limit)
	if body.Metric == store.MetricCostUSD {
		limit = usage.NanoFromUSD(*body.Limit)
	}
	thresholds, apiErr := resolveAlertThresholds(body.AlertThresholds, nil)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	q := &store.Quota{
		SubjectType: "team", SubjectID: team.ID, ModelID: body.ModelID,
		Metric: body.Metric, Limit: limit, Window: body.Window, BreachBehavior: behavior,
		AlertThresholds: thresholds,
	}
	if err := s.Store.CreateQuota(r.Context(), q); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_created", "quota", q.ID, nil, map[string]any{
		"subject_type": q.SubjectType, "subject_id": q.SubjectID, "metric": q.Metric,
		"limit": *body.Limit, "window": q.Window, "breach_behavior": q.BreachBehavior,
		"via": "team_lead_delegation",
	})
	WriteJSON(w, http.StatusCreated, map[string]any{"quota": q})
}

// leadQuotaInScope loads a quota and confirms it belongs to the given team.
func (s *Server) leadQuotaInScope(r *http.Request, teamID, quotaID string) (*store.Quota, *APIError) {
	q, err := s.Store.QuotaByID(r.Context(), quotaID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFoundf("That quota")
	}
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "load quota for lead edit", "error", err.Error())
		return nil, ErrInternal()
	}
	if q.SubjectType != "team" || q.SubjectID != teamID {
		// Do not leak the existence of quotas outside the lead's scope.
		return nil, ErrNotFoundf("That quota")
	}
	return q, nil
}

func (s *Server) handleLeadUpdateQuota(w http.ResponseWriter, r *http.Request) {
	team, apiErr := s.leadTeamForEdit(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	existing, apiErr := s.leadQuotaInScope(r, team.ID, chi.URLParam(r, "quotaID"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if apiErr := s.localOnlyQuotaError(existing.Metric); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	var body quotaPayload
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Limit == nil || *body.Limit <= 0 {
		WriteError(w, r, ErrInvalidRequest("The limit must be greater than zero.").WithParam("limit_value"))
		return
	}
	window := body.Window
	if window == "" {
		window = existing.Window
	}
	if !quota.ValidWindow(window) {
		WriteError(w, r, ErrInvalidRequest("Choose a supported window.").WithParam("window"))
		return
	}
	if apiErr := s.quotaWindowRetentionError(window, "team"); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	behavior := body.BreachBehavior
	if behavior != store.BreachHardKill {
		behavior = store.BreachLetFinish
	}
	limit := int64(*body.Limit)
	if existing.Metric == store.MetricCostUSD {
		limit = usage.NanoFromUSD(*body.Limit)
	}
	thresholds, apiErr := resolveAlertThresholds(body.AlertThresholds, existing.AlertThresholds)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.UpdateQuota(r.Context(), existing.ID, limit, window, behavior, thresholds); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_updated", "quota", existing.ID,
		map[string]any{"limit": existing.Limit, "window": existing.Window, "breach_behavior": existing.BreachBehavior},
		map[string]any{"limit": limit, "window": window, "breach_behavior": behavior, "via": "team_lead_delegation"})
	WriteJSON(w, http.StatusOK, map[string]any{"id": existing.ID, "updated": true})
}

func (s *Server) handleLeadDeleteQuota(w http.ResponseWriter, r *http.Request) {
	team, apiErr := s.leadTeamForEdit(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	existing, apiErr := s.leadQuotaInScope(r, team.ID, chi.URLParam(r, "quotaID"))
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.DeleteQuota(r.Context(), existing.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "quota_deleted", "quota", existing.ID,
		map[string]any{"id": existing.ID, "via": "team_lead_delegation"}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": existing.ID, "deleted": true})
}
