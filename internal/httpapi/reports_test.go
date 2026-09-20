package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/reporting"
	"github.com/torvanis/janus/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func reportingRequest(t *testing.T, h *harness, u *store.User, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if u != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, u))
	}
	rr := httptest.NewRecorder()
	router := chi.NewRouter()
	h.server.mountReportRoutes(router)
	router.ServeHTTP(rr, req)
	return rr
}
func TestReportingHTTPRunLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := reportDefinitionHTTP()
	rr := reportingRequest(t, h, h.user, "POST", "/reports/runs", map[string]any{"definition": d})
	if rr.Code != 202 {
		t.Fatalf("enqueue: %d %s", rr.Code, rr.Body)
	}
	var b struct {
		Run store.ReportRun `json:"run"`
	}
	json.Unmarshal(rr.Body.Bytes(), &b)
	id := b.Run.ID
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+id+"/download?format=csv", nil)
	if rr.Code != 409 {
		t.Fatalf("pending download %d", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "POST", "/reports/runs/"+id+"/cancel", map[string]any{})
	if rr.Code != 200 {
		t.Fatalf("cancel %d %s", rr.Code, rr.Body)
	}
	rr = reportingRequest(t, h, h.user, "POST", "/reports/runs/"+id+"/retry", map[string]any{})
	if rr.Code != 202 {
		t.Fatalf("retry %d %s", rr.Code, rr.Body)
	}
	json.Unmarshal(rr.Body.Bytes(), &b)
	if b.Run.ID == id {
		t.Fatal("retry reused id")
	}
	id = b.Run.ID
	claimed, e := h.store.ClaimReportRun(ctx, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	n := float64(3)
	res := reporting.Result{Version: 1, Definition: d, Columns: []reporting.Column{{Key: "requests", Label: "requests", Unit: "count"}}, Rows: []reporting.Row{{Values: map[string]*float64{"requests": &n}}}, Totals: map[string]*float64{"requests": &n}}
	if e = h.store.FinishReportRun(ctx, id, claimed.LeaseToken, &res, ""); e != nil {
		t.Fatal(e)
	}
	for _, f := range []string{"csv", "xlsx", "pdf", "json"} {
		rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+id+"/download?format="+f, nil)
		if rr.Code != 200 || rr.Body.Len() == 0 {
			t.Fatalf("export %s: %d %s", f, rr.Code, rr.Body)
		}
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+id, nil)
	if bytes.Contains(rr.Body.Bytes(), []byte(claimed.LeaseToken)) {
		t.Fatal("lease leaked")
	}
	other, _, e := h.store.UpsertUserFromIdentity(ctx, "other", "other@example.test", "Other", false, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/reports/runs/" + id, "/reports/runs/" + id + "/download?format=csv"} {
		rr = reportingRequest(t, h, other, "GET", path, nil)
		if rr.Code != 403 {
			t.Fatalf("foreign %s: %d", path, rr.Code)
		}
	}
	rr = reportingRequest(t, h, other, "POST", "/reports/runs/"+id+"/cancel", map[string]any{})
	if rr.Code != 403 {
		t.Fatalf("foreign cancel: %d", rr.Code)
	}
}
func TestReportingHTTPScheduleOwnership(t *testing.T) {
	h := newHarness(t)
	withBusinessLicense(t, h)
	ctx := context.Background()
	d := store.ReportDefinition{OwnerUserID: h.user.ID, Definition: reportDefinitionHTTP(), Shared: true}
	if e := h.store.SaveReportDefinition(ctx, &d); e != nil {
		t.Fatal(e)
	}
	b := map[string]any{"report_id": d.ID, "frequency": "daily", "timezone": "UTC", "at": "09:00", "weekday": 0, "monthday": 1, "enabled": true}
	rr := reportingRequest(t, h, h.user, "POST", "/reports/schedules", b)
	if rr.Code != 201 {
		t.Fatalf("schedule %d %s", rr.Code, rr.Body)
	}
	var saved struct {
		Schedule store.ReportSchedule `json:"schedule"`
	}
	json.Unmarshal(rr.Body.Bytes(), &saved)
	other, _, e := h.store.UpsertUserFromIdentity(ctx, "schedother", "sched@example.test", "Other", false, false)
	if e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, other, "POST", "/reports/schedules", b)
	if rr.Code != 403 {
		t.Fatalf("shared schedule %d", rr.Code)
	}
	rr = reportingRequest(t, h, other, "PUT", "/reports/schedules/"+saved.Schedule.ID, b)
	if rr.Code != 403 {
		t.Fatalf("foreign schedule update %d", rr.Code)
	}
	b["at"] = "25:00"
	rr = reportingRequest(t, h, h.user, "PUT", "/reports/schedules/"+saved.Schedule.ID, b)
	if rr.Code != 400 {
		t.Fatalf("invalid clock %d", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/schedules", nil)
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "DELETE", "/reports/schedules/"+saved.Schedule.ID, nil)
	if rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	all, e := h.store.ListReportSchedules(ctx)
	if e != nil || len(all) != 0 {
		t.Fatal("not deleted", e)
	}
}
func TestReportingHTTPBudgets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if e := h.store.UpdateUser(ctx, h.user.ID, "user", true); e != nil {
		t.Fatal(e)
	}
	b := map[string]any{"name": "My budget", "scope": "user", "subject_id": h.user.ID, "amount_nanousd": 100, "start": "2026-01-01T00:00:00Z", "end": "2026-02-01T00:00:00Z"}
	rr := reportingRequest(t, h, h.user, "POST", "/reports/budgets", b)
	if rr.Code != 201 {
		t.Fatalf("budget %d %s", rr.Code, rr.Body)
	}
	var saved struct {
		Budget struct {
			ID     string `json:"id"`
			Amount int64  `json:"amount_nanousd"`
		} `json:"budget"`
	}
	json.Unmarshal(rr.Body.Bytes(), &saved)
	if saved.Budget.ID == "" || saved.Budget.Amount != 100 {
		t.Fatal("budget contract", rr.Body)
	}
	b["amount_nanousd"] = -1
	rr = reportingRequest(t, h, h.user, "PUT", "/reports/budgets/"+saved.Budget.ID, b)
	if rr.Code != 400 {
		t.Fatal("negative", rr.Code)
	}
	b["amount_nanousd"] = 100
	b["scope"] = "organization"
	b["subject_id"] = ""
	rr = reportingRequest(t, h, h.user, "POST", "/reports/budgets", b)
	if rr.Code != 403 {
		t.Fatal("organization", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/budgets", nil)
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "DELETE", "/reports/budgets/"+saved.Budget.ID, nil)
	if rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	all, e := h.store.ListReportBudgets(ctx)
	if e != nil || len(all) != 0 {
		t.Fatal("not deleted", e)
	}
	h.server.Config.LocalOnly = true
	rr = reportingRequest(t, h, h.user, "GET", "/reports/budgets", nil)
	if rr.Code != 403 {
		t.Fatal("local costs", rr.Code)
	}
}
func TestReportingHTTPOptionsAndCatalog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if e := h.store.UpdateUser(ctx, h.user.ID, "user", true); e != nil {
		t.Fatal(e)
	}
	other, _, e := h.store.UpsertUserFromIdentity(ctx, "opts", "options@example.test", "Secret user", false, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []*store.UsageEvent{{UserID: h.user.ID, ModelID: h.model.ID, CreatedAt: time.Now().AddDate(-2, 0, 0)}, {UserID: other.ID, ModelID: "secret-model", CreatedAt: time.Now().Add(-time.Hour)}} {
		if e := h.store.InsertUsageEvent(ctx, v); e != nil {
			t.Fatal(e)
		}
	}
	rr := reportingRequest(t, h, h.user, "GET", "/reports/options", nil)
	if rr.Code != 200 {
		t.Fatalf("options %d %s", rr.Code, rr.Body)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(h.model.ID)) || bytes.Contains(rr.Body.Bytes(), []byte("secret-model")) || bytes.Contains(rr.Body.Bytes(), []byte("Secret user")) {
		t.Fatal("option scope/history", rr.Body)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/options?scope=organization", nil)
	if rr.Code != 403 {
		t.Fatal("options org", rr.Code)
	}
	h.server.Config.LocalOnly = true
	rr = reportingRequest(t, h, h.user, "GET", "/reports/catalog", nil)
	if rr.Code != 200 || bytes.Contains(rr.Body.Bytes(), []byte("cost_usd")) {
		t.Fatal("catalog redaction", rr.Code, rr.Body)
	}
}
func TestReportingHTTPDownloadGuards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := reportDefinitionHTTP()
	d.Scope = "organization"
	d.Metrics = append(d.Metrics, "cost_usd")
	v, e := h.store.EnqueueReport(ctx, h.user.ID, "", d)
	if e != nil {
		t.Fatal(e)
	}
	claim, e := h.store.ClaimReportRun(ctx, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	n := 3.0
	res := reporting.Result{Definition: d, Columns: []reporting.Column{{Key: "cost_usd", Label: "cost", Unit: "USD"}}, Totals: map[string]*float64{"cost_usd": &n}}
	if e = h.store.FinishReportRun(ctx, v.ID, claim.LeaseToken, &res, ""); e != nil {
		t.Fatal(e)
	}
	rr := reportingRequest(t, h, h.user, "GET", "/reports/runs/"+v.ID+"/download?format=csv", nil)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Disposition"), v.ID+".csv\"") {
		t.Fatalf("filename: %d %s", rr.Code, rr.Header().Get("Content-Disposition"))
	}
	h.server.Config.LocalOnly = true
	for _, path := range []string{"/reports/runs/" + v.ID, "/reports/runs/" + v.ID + "/download?format=json"} {
		rr = reportingRequest(t, h, h.user, "GET", path, nil)
		if rr.Code != 200 || bytes.Contains(rr.Body.Bytes(), []byte("cost_usd")) {
			t.Fatalf("redaction: %d %s", rr.Code, rr.Body)
		}
	}
	h.server.Config.LocalOnly = false
	if e = h.store.UpdateUser(ctx, h.user.ID, "user", true); e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+v.ID+"/download?format=csv", nil)
	if rr.Code != 403 {
		t.Fatal("demotion", rr.Code)
	}
	if e = h.store.UpdateUser(ctx, h.user.ID, "admin", true); e != nil {
		t.Fatal(e)
	}
	res.Columns = append(res.Columns, res.Columns[0])
	raw, _ := json.Marshal(res)
	if _, e = h.store.DB().Exec(`UPDATE report_run SET result_json=? WHERE id=?`, string(raw), v.ID); e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+v.ID+"/download?format=csv", nil)
	if rr.Code != 500 || rr.Header().Get("Content-Disposition") != "" {
		t.Fatalf("export failure: %d %s", rr.Code, rr.Body)
	}
	if _, e = h.store.DB().Exec(`UPDATE report_run SET expires_at=? WHERE id=?`, store.FormatTime(time.Now().Add(-time.Hour)), v.ID); e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+v.ID+"/download?format=json", nil)
	if rr.Code != 410 {
		t.Fatal("expired", rr.Code)
	}
}
func TestReportingHTTPSharedDefinitionsAndRevision(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other, _, e := h.store.UpsertUserFromIdentity(ctx, "shared", "shared@example.test", "Reader", false, false)
	if e != nil {
		t.Fatal(e)
	}
	d := reportDefinitionHTTP()
	d.Metrics = append(d.Metrics, "cost_usd")
	body := map[string]any{"definition": d, "shared": true}
	rr := reportingRequest(t, h, h.user, "POST", "/reports", body)
	if rr.Code != 201 {
		t.Fatal(rr.Code)
	}
	var b struct {
		Report store.ReportDefinition `json:"report"`
	}
	json.Unmarshal(rr.Body.Bytes(), &b)
	id := b.Report.ID
	rr = reportingRequest(t, h, other, "GET", "/reports", nil)
	if !bytes.Contains(rr.Body.Bytes(), []byte(id)) {
		t.Fatal("shared invisible", rr.Body)
	}
	rr = reportingRequest(t, h, other, "POST", "/reports/runs", map[string]any{"report_id": id, "definition": d})
	if rr.Code != 202 {
		t.Fatal(rr.Code, rr.Body)
	}
	var rb struct {
		Run store.ReportRun `json:"run"`
	}
	json.Unmarshal(rr.Body.Bytes(), &rb)
	if rb.Run.OwnerUserID != other.ID {
		t.Fatal("shared rerun owner")
	}
	scope, e := h.store.ResolveReportScope(ctx, rb.Run.OwnerUserID, rb.Run.Definition)
	if e != nil || scope.UserID != other.ID {
		t.Fatal("shared self changed", e)
	}
	own, e := h.store.EnqueueReport(ctx, h.user.ID, id, d)
	if e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, other, "GET", "/reports/runs", nil)
	if bytes.Contains(rr.Body.Bytes(), []byte(own.ID)) {
		t.Fatal("shared run leaked")
	}
	body["revision"] = b.Report.Revision
	rr = reportingRequest(t, h, other, "PUT", "/reports/"+id, body)
	if rr.Code != 403 {
		t.Fatal("foreign definition", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "PUT", "/reports/"+id, body)
	if rr.Code != 200 {
		t.Fatal("update", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "PUT", "/reports/"+id, body)
	if rr.Code != 409 {
		t.Fatal("revision", rr.Code)
	}
	h.server.Config.LocalOnly = true
	rr = reportingRequest(t, h, h.user, "GET", "/reports", nil)
	if bytes.Contains(rr.Body.Bytes(), []byte("cost_usd")) {
		t.Fatal("saved definition cost leaked")
	}
	rr = reportingRequest(t, h, h.user, "DELETE", "/reports/"+id, nil)
	if rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	if _, e = h.store.ReportDefinitionByID(ctx, id); e == nil {
		t.Fatal("definition retained")
	}
}
func TestReportingHTTPAuthenticatedBackpressure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rr := h.do("GET", "/api/v1/reports/catalog", nil)
	if rr.Code != 200 {
		t.Fatalf("authenticated mount: %d %s", rr.Code, rr.Body)
	}
	if e := h.store.UpdateUser(ctx, h.user.ID, "user", true); e != nil {
		t.Fatal(e)
	}
	d := reportDefinitionHTTP()
	team, e := h.store.CreateTeam(ctx, "Owned team", h.user.ID)
	if e != nil {
		t.Fatal(e)
	}
	d.Scope = "team"
	d.TeamID = team.ID
	rr = reportingRequest(t, h, h.user, "POST", "/reports/runs", map[string]any{"definition": d})
	if rr.Code != 202 {
		t.Fatalf("leader: %d %s", rr.Code, rr.Body)
	}
	var b struct {
		Run store.ReportRun `json:"run"`
	}
	json.Unmarshal(rr.Body.Bytes(), &b)
	// Keep a second leader so the demotion respects last-leader invariants.
	second, _, e := h.store.UpsertUserFromIdentity(ctx, "leader2", "leader2@example.test", "Leader two", false, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = h.store.AddTeamMember(ctx, team.ID, second.ID, "leader", "manual", ""); e != nil {
		t.Fatal(e)
	}
	if e = h.store.SetTeamMemberRole(ctx, team.ID, h.user.ID, "member"); e != nil {
		t.Fatal(e)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports/runs/"+b.Run.ID, nil)
	if rr.Code != 403 {
		t.Fatal("team demotion", rr.Code)
	}
	d = reportDefinitionHTTP()
	for i := 0; i < 9; i++ {
		if _, e = h.store.EnqueueReport(ctx, h.user.ID, "", d); e != nil {
			t.Fatal(e)
		}
	}
	rr = reportingRequest(t, h, h.user, "POST", "/reports/runs", map[string]any{"definition": d})
	if rr.Code != 429 {
		t.Fatal("queue cap", rr.Code)
	}
	rr = reportingRequest(t, h, h.user, "POST", "/reports", map[string]any{"definition": d, "owner_user_id": second.ID})
	if rr.Code != 400 {
		t.Fatal("owner injection", rr.Code)
	}
	if e = h.store.UpdateUser(ctx, h.user.ID, "user", false); e != nil {
		t.Fatal(e)
	}
	rr = h.do("GET", "/api/v1/reports/catalog", nil)
	if rr.Code != 401 && rr.Code != 403 {
		t.Fatalf("deactivated bearer: %d", rr.Code)
	}
}
func reportDefinitionHTTP() reporting.Definition { return reporting.GetCatalog().Templates[1] }
func TestReportingHTTPDefinitionScope(t *testing.T) {
	h := newHarness(t)
	if err := h.store.UpdateUser(context.Background(), h.user.ID, "user", true); err != nil {
		t.Fatal(err)
	}
	h.user.Role = "user"
	d := reportDefinitionHTTP()
	rr := reportingRequest(t, h, h.user, "POST", "/reports", map[string]any{"definition": d, "shared": true})
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	var saved struct {
		Report store.ReportDefinition `json:"report"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Report.OwnerUserID != h.user.ID {
		t.Fatal("wrong owner")
	}
	d.Scope = "organization"
	rr = reportingRequest(t, h, h.user, "POST", "/reports", map[string]any{"definition": d})
	if rr.Code != 403 {
		t.Fatalf("org: %d %s", rr.Code, rr.Body)
	}
	rr = reportingRequest(t, h, nil, "GET", "/reports", nil)
	if rr.Code != 401 {
		t.Fatalf("anonymous: %d", rr.Code)
	}
	if err := h.store.UpdateUser(context.Background(), h.user.ID, h.user.Role, false); err != nil {
		t.Fatal(err)
	}
	rr = reportingRequest(t, h, h.user, "GET", "/reports", nil)
	if rr.Code != 403 {
		t.Fatalf("disabled: %d", rr.Code)
	}
}
