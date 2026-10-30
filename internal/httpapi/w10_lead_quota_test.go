package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

func TestLeadQuotaCreateLicenseGateAndArchivedTeam(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lead := h.user
	team, err := h.store.CreateTeam(ctx, "Research", lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTeamMembers(ctx, team.ID, []string{lead.ID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateTeam(ctx, team.ID, team.Name, lead.ID, true); err != nil {
		t.Fatal(err)
	}
	sess, err := h.server.Sessions.Create(ctx, lead.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"metric": store.MetricRequests, "limit_value": 100.0,
		"window": store.WindowDaily, "breach_behavior": store.BreachLetFinish}
	path := "/api/v1/lead/teams/" + team.ID + "/quotas"

	// A quota created while licensed, to edit later.
	rec := h.doAsSession(sess, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("lead create: %d %s", rec.Code, rec.Body.String())
	}
	quotaID := decodeBody(t, rec)["quota"].(map[string]any)["id"].(string)

	// License expired past grace: creating is paused for leads too.
	exp := time.Now().Add(-60 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-BUS-EXP", Org: "Acme", IssuedTo: "a@b", Edition: license.EditionBusiness,
		Seats: 100, Nodes: 3, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	if rec := h.doAsSession(sess, http.MethodPost, path, body); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("lead create with restricted license = %d %s, want 402", rec.Code, rec.Body.String())
	}
	// Edits are never license-gated.
	if rec := h.doAsSession(sess, http.MethodPut, path+"/"+quotaID, body); rec.Code != http.StatusOK {
		t.Fatalf("lead edit with restricted license = %d %s, want 200", rec.Code, rec.Body.String())
	}
	withBusinessLicense(t, h)

	// Archived team: no creating or changing; removing still works.
	if err := h.store.DeleteTeam(ctx, team.ID); err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(sess, http.MethodPost, path, body); rec.Code != http.StatusForbidden {
		t.Fatalf("lead create on archived team = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPut, path+"/"+quotaID, body); rec.Code != http.StatusForbidden {
		t.Fatalf("lead edit on archived team = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodDelete, path+"/"+quotaID, nil); rec.Code/100 != 2 {
		t.Fatalf("lead delete on archived team = %d %s, want 2xx", rec.Code, rec.Body.String())
	}
}
