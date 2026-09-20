package httpapi

import (
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/reporting"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"strings"
	"time"
)

func (s *Server) reportCatalog(w http.ResponseWriter, r *http.Request) {
	if s.reportActor(w, r) == nil {
		return
	}
	c := reporting.GetCatalog()
	if s.Config != nil && s.Config.LocalOnly {
		metrics := []reporting.CatalogItem{}
		for _, m := range c.Metrics {
			if m.ID != "cost_usd" {
				metrics = append(metrics, m)
			}
		}
		c.Metrics = metrics
		for i, d := range c.Templates {
			c.Templates[i] = reporting.RedactCosts(reporting.Result{Definition: d}).Definition
		}
	}
	WriteJSON(w, 200, c)
}
func (s *Server) reportOptions(w http.ResponseWriter, r *http.Request) {
	u := s.reportActor(w, r)
	if u == nil {
		return
	}
	d := reporting.GetCatalog().Templates[1]
	d.Scope = r.URL.Query().Get("scope")
	if d.Scope == "" {
		d.Scope = "self"
	}
	d.TeamID = r.URL.Query().Get("team_id")
	if !s.reportValid(w, r, u, d) {
		return
	}
	out, e := s.Store.ReportFilterOptions(r.Context(), u.ID, d)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	// Team selectors also include currently authorized teams without historical use.
	var teams []*store.Team
	if u.IsAdmin() {
		teams, e = s.Store.ListTeams(r.Context())
	} else {
		teams, e = s.Store.TeamsForUser(r.Context(), u.ID)
	}
	if e != nil {
		reportStoreError(w, e)
		return
	}
	seen := map[string]bool{}
	for _, v := range out["team"] {
		seen[v.ID] = true
	}
	for _, team := range teams {
		check := d
		check.Scope = "team"
		check.TeamID = team.ID
		if !seen[team.ID] && s.reportScope(r, u, check) {
			out["team"] = append(out["team"], store.ReportFilterOption{ID: team.ID, Label: team.Name})
		}
	}
	WriteJSON(w, 200, map[string]any{"dimensions": out})
}
func (s *Server) reportBudgetScope(r *http.Request, u *store.User, b store.ReportBudget) bool {
	d := reporting.Definition{}
	switch b.Scope {
	case "user":
		if b.SubjectID != u.ID && !u.IsAdmin() {
			return false
		}
		d.Scope = "self"
	case "team":
		d.Scope = "team"
		d.TeamID = b.SubjectID
	case "organization":
		d.Scope = "organization"
	default:
		return false
	}
	return s.reportScope(r, u, d)
}
func (s *Server) reportBudgets(w http.ResponseWriter, r *http.Request) {
	u := s.reportActor(w, r)
	if u == nil {
		return
	}
	if s.Config != nil && s.Config.LocalOnly {
		reportHTTPError(w, 403, "Budget reporting disabled")
		return
	}
	ctx := r.Context()
	all, e := s.Store.ListReportBudgets(ctx)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	id := chi.URLParam(r, "id")
	if r.Method == "GET" {
		out := []store.ReportBudget{}
		for _, b := range all {
			if (b.OwnerUserID == u.ID || u.IsAdmin()) && s.reportBudgetScope(r, u, b) {
				out = append(out, b)
			}
		}
		WriteJSON(w, 200, map[string]any{"budgets": out})
		return
	}
	var old *store.ReportBudget
	if id != "" {
		for _, b := range all {
			if b.ID == id {
				copy := b
				old = &copy
				break
			}
		}
		if old == nil {
			reportHTTPError(w, 404, "Budget not found")
			return
		}
		if (!u.IsAdmin() && old.OwnerUserID != u.ID) || !s.reportBudgetScope(r, u, *old) {
			reportHTTPError(w, 403, "Budget access denied")
			return
		}
	}
	if r.Method == "DELETE" {
		if e = s.Store.DeleteReportBudget(ctx, id); e != nil {
			reportStoreError(w, e)
			return
		}
		s.audit(r, "report.budget.delete", "report_budget", id, old, nil)
		w.WriteHeader(204)
		return
	}
	var b struct {
		Name       string    `json:"name"`
		Scope      string    `json:"scope"`
		SubjectID  string    `json:"subject_id"`
		AmountNano int64     `json:"amount_nanousd"`
		Start      time.Time `json:"start"`
		End        time.Time `json:"end"`
	}
	if !reportDecode(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.Name) == "" || len(b.Name) > 200 || b.AmountNano < 0 || b.Start.IsZero() || !b.End.After(b.Start) || b.End.Year() > 9999 || b.Start.Year() < 1 || (b.Scope != "user" && b.Scope != "team" && b.Scope != "organization") || (b.Scope != "organization" && b.SubjectID == "") || (b.Scope == "organization" && b.SubjectID != "") {
		reportHTTPError(w, 400, "Invalid budget name, scope, amount or interval")
		return
	}
	v := store.ReportBudget{Name: b.Name, OwnerUserID: u.ID, Scope: b.Scope, SubjectID: b.SubjectID, AmountNano: b.AmountNano, Start: b.Start, End: b.End}
	status := 201
	if old != nil {
		v.ID = old.ID
		v.OwnerUserID = old.OwnerUserID
		status = 200
		if v.Scope != old.Scope || v.SubjectID != old.SubjectID {
			reportHTTPError(w, 400, "Budget scope and subject cannot be reassigned")
			return
		}
	}
	if !s.reportBudgetScope(r, u, v) {
		reportHTTPError(w, 403, "Budget scope denied")
		return
	}
	if e = s.Store.SaveReportBudget(ctx, &v); e != nil {
		reportStoreError(w, e)
		return
	}
	s.audit(r, "report.budget.save", "report_budget", v.ID, old, v)
	WriteJSON(w, status, map[string]any{"budget": v})
}
func (s *Server) reportScheduleAllowed(r *http.Request, u *store.User, v store.ReportSchedule) bool {
	if !u.IsAdmin() && v.OwnerUserID != u.ID {
		return false
	}
	d, e := s.Store.ReportDefinitionByID(r.Context(), v.ReportID)
	return e == nil && d.OwnerUserID == v.OwnerUserID && s.reportScope(r, u, d.Definition)
}
func (s *Server) reportSchedules(w http.ResponseWriter, r *http.Request) {
	u := s.reportActor(w, r)
	if u == nil {
		return
	}
	ctx := r.Context()
	all, e := s.Store.ListReportSchedules(ctx)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	id := chi.URLParam(r, "id")
	if r.Method == "GET" {
		out := []store.ReportSchedule{}
		for _, v := range all {
			if s.reportScheduleAllowed(r, u, v) {
				out = append(out, v)
			}
		}
		WriteJSON(w, 200, map[string]any{"schedules": out})
		return
	}
	var old *store.ReportSchedule
	if id != "" {
		for _, v := range all {
			if v.ID == id {
				copy := v
				old = &copy
				break
			}
		}
		if old == nil {
			reportHTTPError(w, 404, "Schedule not found")
			return
		}
		if !s.reportScheduleAllowed(r, u, *old) {
			reportHTTPError(w, 403, "Schedule access denied")
			return
		}
	}
	if r.Method == "DELETE" {
		if e = s.Store.DeleteReportSchedule(ctx, id); e != nil {
			reportStoreError(w, e)
			return
		}
		s.audit(r, "report.schedule.delete", "report_schedule", id, old, nil)
		w.WriteHeader(204)
		return
	}
	var b struct {
		ReportID  string `json:"report_id"`
		Frequency string `json:"frequency"`
		Timezone  string `json:"timezone"`
		At        string `json:"at"`
		Weekday   int    `json:"weekday"`
		Monthday  int    `json:"monthday"`
		Enabled   bool   `json:"enabled"`
	}
	if !reportDecode(w, r, &b) {
		return
	}
	v := store.ReportSchedule{OwnerUserID: u.ID, ReportID: b.ReportID, Frequency: b.Frequency, Timezone: b.Timezone, At: b.At, Weekday: b.Weekday, Monthday: b.Monthday, Enabled: b.Enabled}
	status := 201
	if old != nil {
		v.ID = old.ID
		v.OwnerUserID = old.OwnerUserID
		status = 200
	}
	d, e := s.Store.ReportDefinitionByID(ctx, v.ReportID)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	if d.OwnerUserID != v.OwnerUserID || !s.reportScope(r, u, d.Definition) {
		reportHTTPError(w, 403, "Schedules require an owned report and authorized scope")
		return
	}
	if _, e = s.Store.ResolveReportScope(ctx, v.OwnerUserID, d.Definition); e != nil {
		reportHTTPError(w, 403, "Schedule owner access changed")
		return
	}
	if _, e = store.NextReportSchedule(v, time.Now()); e != nil {
		reportHTTPError(w, 400, "Invalid schedule; use daily, weekly or monthly with IANA timezone and HH:MM")
		return
	}
	if e = s.Store.SaveReportSchedule(ctx, &v); e != nil {
		reportStoreError(w, e)
		return
	}
	s.audit(r, "report.schedule.save", "report_schedule", v.ID, old, v)
	WriteJSON(w, status, map[string]any{"schedule": v})
}
