package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// doAsSession issues an app-API request with a session cookie + CSRF header,
// the way the web UI calls /api/v1/*.
func (h *harness) doAsSession(session *auth.Session, method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session.ID})
	req.Header.Set(auth.CSRFHeader, session.CSRFToken)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestLeadDelegatedQuotaManagement locks delegation: an admin can delegate a
// team's quota management to its lead via lead_can_edit_quotas, and the lead
// endpoints enforce (a) lead identity, (b) the delegation toggle, and (c) team
// scope on every quota they touch.
func TestLeadDelegatedQuotaManagement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	lead := h.user
	member, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-member", "member@example.com", "Member", false, false)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	team, err := h.store.CreateTeam(ctx, "Research", lead.ID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := h.store.SetTeamMembers(ctx, team.ID, []string{lead.ID, member.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}

	leadSession, err := h.server.Sessions.Create(ctx, lead.ID)
	if err != nil {
		t.Fatalf("create lead session: %v", err)
	}
	memberSession, err := h.server.Sessions.Create(ctx, member.ID)
	if err != nil {
		t.Fatalf("create member session: %v", err)
	}

	quotaBody := map[string]any{
		"metric": store.MetricRequests, "limit_value": 100.0,
		"window": store.WindowDaily, "breach_behavior": store.BreachLetFinish,
	}

	// Delegation off: even the lead is refused.
	rec := h.doAsSession(leadSession, http.MethodPost, "/api/v1/lead/teams/"+team.ID+"/quotas", quotaBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("lead created a quota without delegation: %d %s", rec.Code, rec.Body.String())
	}

	if err := h.store.UpdateTeam(ctx, team.ID, team.Name, lead.ID, true); err != nil {
		t.Fatalf("enable delegation: %v", err)
	}

	// A non-lead member is refused even with delegation on.
	rec = h.doAsSession(memberSession, http.MethodPost, "/api/v1/lead/teams/"+team.ID+"/quotas", quotaBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-lead member created a quota: %d %s", rec.Code, rec.Body.String())
	}

	// The lead can create a quota; the subject is forced to the team.
	rec = h.doAsSession(leadSession, http.MethodPost, "/api/v1/lead/teams/"+team.ID+"/quotas", quotaBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("lead create quota = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Quota store.Quota `json:"quota"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created quota: %v", err)
	}
	if created.Quota.SubjectType != "team" || created.Quota.SubjectID != team.ID {
		t.Fatalf("quota subject = %s/%s, want team/%s", created.Quota.SubjectType, created.Quota.SubjectID, team.ID)
	}

	// The listing shows the team, delegation state, and the quota.
	rec = h.doAsSession(leadSession, http.MethodGet, "/api/v1/lead/teams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lead teams list = %d: %s", rec.Code, rec.Body.String())
	}
	var listing struct {
		Teams []struct {
			ID            string           `json:"id"`
			CanEditQuotas bool             `json:"can_edit_quotas"`
			Quotas        []map[string]any `json:"quotas"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Teams) != 1 || listing.Teams[0].ID != team.ID || !listing.Teams[0].CanEditQuotas || len(listing.Teams[0].Quotas) != 1 {
		t.Fatalf("unexpected listing: %s", rec.Body.String())
	}

	// The member sees no led teams at all.
	rec = h.doAsSession(memberSession, http.MethodGet, "/api/v1/lead/teams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member lead-teams list = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode member listing: %v", err)
	}
	if len(listing.Teams) != 0 {
		t.Fatalf("member should lead no teams, got %s", rec.Body.String())
	}

	// Update and delete within scope succeed and a foreign quota 404s.
	rec = h.doAsSession(leadSession, http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/quotas/"+created.Quota.ID,
		map[string]any{"limit_value": 250.0})
	if rec.Code != http.StatusOK {
		t.Fatalf("lead update quota = %d: %s", rec.Code, rec.Body.String())
	}

	foreign := &store.Quota{SubjectType: "user", SubjectID: member.ID, Metric: store.MetricRequests,
		Limit: 5, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := h.store.CreateQuota(ctx, foreign); err != nil {
		t.Fatalf("create foreign quota: %v", err)
	}
	rec = h.doAsSession(leadSession, http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/quotas/"+foreign.ID,
		map[string]any{"limit_value": 9999.0})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("out-of-scope quota edit = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	rec = h.doAsSession(leadSession, http.MethodDelete, "/api/v1/lead/teams/"+team.ID+"/quotas/"+created.Quota.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lead delete quota = %d: %s", rec.Code, rec.Body.String())
	}
}
