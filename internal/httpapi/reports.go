package httpapi

import (
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/reporting"
	"github.com/torvanis/janus/internal/store"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func reportHTTPError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}
func reportStoreError(w http.ResponseWriter, err error) {
	status := 500
	if errors.Is(err, store.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, store.ErrReportConflict) {
		status = 409
	}
	reportHTTPError(w, status, "Report operation unavailable")
}
func reportDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		reportHTTPError(w, 400, "Invalid report request")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		reportHTTPError(w, 400, "Expected one JSON object")
		return false
	}
	return true
}
func (s *Server) reportActor(w http.ResponseWriter, r *http.Request) *store.User {
	u := UserFrom(r.Context())
	if u == nil {
		reportHTTPError(w, 401, "Sign in required")
		return nil
	}
	fresh, e := s.Store.UserByID(r.Context(), u.ID)
	if e != nil || !fresh.IsActive {
		reportHTTPError(w, 403, "Report access denied")
		return nil
	}
	return fresh
}
func (s *Server) reportScope(r *http.Request, u *store.User, d reporting.Definition) bool {
	_, e := s.Store.ResolveReportScope(r.Context(), u.ID, d)
	return e == nil
}
func (s *Server) reportValid(w http.ResponseWriter, r *http.Request, u *store.User, d reporting.Definition) bool {
	if reporting.Validate(d) != nil {
		reportHTTPError(w, 400, "Invalid report definition")
		return false
	}
	if !s.reportScope(r, u, d) {
		reportHTTPError(w, 403, "Report scope denied")
		return false
	}
	return true
}
func (s *Server) mountReportRoutes(r chi.Router) {
	r.Get("/reports/catalog", s.reportCatalog)
	r.Get("/reports/options", s.reportOptions)
	r.Get("/reports/budgets", s.reportBudgets)
	r.Post("/reports/budgets", s.reportBudgets)
	r.Put("/reports/budgets/{id}", s.reportBudgets)
	r.Delete("/reports/budgets/{id}", s.reportBudgets)
	r.Get("/reports/schedules", s.reportSchedules)
	r.Post("/reports/schedules", s.gateFeature("reports_scheduled", s.gateCreate("report schedule", s.reportSchedules)))
	r.Put("/reports/schedules/{id}", s.reportSchedules)
	r.Delete("/reports/schedules/{id}", s.reportSchedules)
	r.Get("/reports/runs", s.reportRuns)
	r.Post("/reports/runs", s.reportRuns)
	r.Get("/reports/runs/{id}", s.reportRuns)
	r.Post("/reports/runs/{id}/cancel", s.reportRuns)
	r.Post("/reports/runs/{id}/retry", s.reportRuns)
	r.Get("/reports/runs/{id}/download", s.reportRuns)
	r.Get("/reports", s.reportDefinitions)
	r.Post("/reports", s.gateCreate("report", s.reportDefinitions))
	r.Put("/reports/{id}", s.reportDefinitions)
	r.Delete("/reports/{id}", s.reportDefinitions)
}
func (s *Server) reportRunAllowed(r *http.Request, u *store.User, v store.ReportRun) bool {
	// Administrators may inspect the owner's frozen self results, never rerun under that owner.
	return u.IsAdmin() || (v.OwnerUserID == u.ID && s.reportScope(r, u, v.Definition))
}
func (s *Server) reportRunView(v store.ReportRun) store.ReportRun {
	v.LeaseToken = ""
	if s.Config != nil && s.Config.LocalOnly {
		v.Definition = reporting.RedactCosts(reporting.Result{Definition: v.Definition}).Definition
		if v.Result != nil {
			clean := reporting.RedactCosts(*v.Result)
			v.Result = &clean
		}
	}
	return v
}
func (s *Server) reportEnqueue(w http.ResponseWriter, r *http.Request, u *store.User, reportID string, d reporting.Definition) {
	if !s.reportValid(w, r, u, d) {
		return
	}
	all, e := s.Store.ListReportRuns(r.Context())
	if e != nil {
		reportStoreError(w, e)
		return
	}
	active := 0
	for _, v := range all {
		if v.OwnerUserID == u.ID && (v.Status == "queued" || v.Status == "running") {
			active++
		}
	} // Best-effort backpressure, not an atomic quota across concurrent requests.
	if active >= 10 {
		reportHTTPError(w, 429, "Too many active reports; wait or cancel an existing run")
		return
	}
	v, e := s.Store.EnqueueReport(r.Context(), u.ID, reportID, d)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	s.audit(r, "report.run", "report_run", v.ID, nil, map[string]string{"report_id": reportID})
	WriteJSON(w, 202, map[string]any{"run": s.reportRunView(*v)})
}
func (s *Server) reportRuns(w http.ResponseWriter, r *http.Request) {
	u := s.reportActor(w, r)
	if u == nil {
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if id == "" && r.Method == "GET" {
		all, e := s.Store.ListReportRuns(ctx)
		if e != nil {
			reportStoreError(w, e)
			return
		}
		out := []store.ReportRun{}
		for _, v := range all {
			if s.reportRunAllowed(r, u, v) {
				out = append(out, s.reportRunView(v))
			}
		}
		WriteJSON(w, 200, map[string]any{"runs": out})
		return
	}
	if id == "" {
		var b struct {
			Definition reporting.Definition `json:"definition"`
			ReportID   string               `json:"report_id"`
		}
		if !reportDecode(w, r, &b) {
			return
		}
		if b.ReportID != "" {
			saved, e := s.Store.ReportDefinitionByID(ctx, b.ReportID)
			if e != nil {
				reportStoreError(w, e)
				return
			}
			if (!u.IsAdmin() && saved.OwnerUserID != u.ID && !saved.Shared) || !s.reportScope(r, u, saved.Definition) {
				reportHTTPError(w, 403, "Report access denied")
				return
			}
		}
		s.reportEnqueue(w, r, u, b.ReportID, b.Definition)
		return
	}
	v, e := s.Store.ReportRunByID(ctx, id)
	if e != nil {
		reportStoreError(w, e)
		return
	}
	if !s.reportRunAllowed(r, u, *v) {
		reportHTTPError(w, 403, "Report access denied")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/download") {
		s.reportDownload(w, r, *v)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		var b struct{}
		if !reportDecode(w, r, &b) {
			return
		}
		if e = s.Store.CancelReportRun(ctx, id); e != nil {
			reportStoreError(w, e)
			return
		}
		v, e = s.Store.ReportRunByID(ctx, id)
		if e != nil {
			reportStoreError(w, e)
			return
		}
		s.audit(r, "report.cancel", "report_run", id, nil, nil)
	} else if strings.HasSuffix(r.URL.Path, "/retry") {
		var b struct{}
		if !reportDecode(w, r, &b) {
			return
		}
		if v.Status != "failed" && v.Status != "cancelled" && v.Status != "expired" {
			reportHTTPError(w, 409, "Only failed, cancelled or expired runs can be retried")
			return
		}
		s.reportEnqueue(w, r, u, v.ReportID, v.Definition)
		return
	}
	WriteJSON(w, 200, map[string]any{"run": s.reportRunView(*v)})
}
func (s *Server) reportDownload(w http.ResponseWriter, r *http.Request, v store.ReportRun) {
	if v.Status == "expired" || !v.ExpiresAt.After(time.Now()) {
		reportHTTPError(w, 410, "Report expired; create a new run")
		return
	}
	if v.Status != "complete" || v.Result == nil {
		reportHTTPError(w, 409, "Report is not complete")
		return
	}
	format := r.URL.Query().Get("format")
	switch format {
	case "csv", "xlsx", "pdf", "json":
	default:
		reportHTTPError(w, 400, "Choose csv, xlsx, pdf or json")
		return
	}
	v = s.reportRunView(v)
	f, e := os.CreateTemp("", "janus-report-*")
	if e != nil {
		reportStoreError(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = reporting.Export(f, *v.Result, format); e != nil {
		reportHTTPError(w, 500, "Report export failed")
		return
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		reportStoreError(w, e)
		return
	}
	info, e := f.Stat()
	if e != nil {
		reportStoreError(w, e)
		return
	}
	safe := strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			return c
		}
		return '-'
	}, v.ID)
	w.Header().Set("Content-Type", reporting.ContentType(format))
	w.Header().Set("Content-Disposition", `attachment; filename="report-`+safe+reporting.Extension(format)+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "report-"+safe, info.ModTime(), f)
	s.audit(r, "report.download", "report_run", v.ID, nil, map[string]string{"format": format})
}
func (s *Server) reportDefinitionView(v store.ReportDefinition) store.ReportDefinition {
	if s.Config != nil && s.Config.LocalOnly {
		v.Definition = reporting.RedactCosts(reporting.Result{Definition: v.Definition}).Definition
	}
	return v
}
func (s *Server) reportDefinitions(w http.ResponseWriter, r *http.Request) {
	u := s.reportActor(w, r)
	if u == nil {
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if r.Method == "GET" {
		all, e := s.Store.ListReportDefinitions(ctx)
		if e != nil {
			reportStoreError(w, e)
			return
		}
		out := []store.ReportDefinition{}
		for _, v := range all {
			if (u.IsAdmin() || v.OwnerUserID == u.ID || v.Shared) && s.reportScope(r, u, v.Definition) {
				out = append(out, s.reportDefinitionView(v))
			}
		}
		WriteJSON(w, 200, map[string]any{"reports": out})
		return
	}
	var old *store.ReportDefinition
	if id != "" {
		var e error
		old, e = s.Store.ReportDefinitionByID(ctx, id)
		if e != nil {
			reportStoreError(w, e)
			return
		}
		if (!u.IsAdmin() && old.OwnerUserID != u.ID) || !s.reportScope(r, u, old.Definition) {
			reportHTTPError(w, 403, "Report access denied")
			return
		}
	}
	if r.Method == "DELETE" {
		if e := s.Store.DeleteReportDefinition(ctx, id); e != nil {
			reportStoreError(w, e)
			return
		}
		s.audit(r, "report.delete", "report", id, old, nil)
		w.WriteHeader(204)
		return
	}
	var body struct {
		Definition reporting.Definition `json:"definition"`
		Shared     bool                 `json:"shared"`
		Revision   int                  `json:"revision"`
	}
	if !reportDecode(w, r, &body) || !s.reportValid(w, r, u, body.Definition) {
		return
	}
	v := store.ReportDefinition{OwnerUserID: u.ID, Definition: body.Definition, Shared: body.Shared}
	status := 201
	if old != nil {
		v = *old
		v.Definition = body.Definition
		v.Shared = body.Shared
		v.Revision = body.Revision
		status = 200
	}
	if e := s.Store.SaveReportDefinition(ctx, &v); e != nil {
		reportStoreError(w, e)
		return
	}
	s.audit(r, "report.save", "report", v.ID, old, v)
	WriteJSON(w, status, map[string]any{"report": s.reportDefinitionView(v)})
}
