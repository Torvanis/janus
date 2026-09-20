package httpapi

import (
	"context"
	"encoding/json"

	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/jobs"
	"github.com/torvanis/janus/internal/reporting"
	"github.com/torvanis/janus/internal/store"
)

// Exercise the same catalog -> enqueue -> worker -> HTTP result path as Reports.
func TestTeamUsageReportHTTPScopes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rr := h.do(http.MethodGet, "/api/v1/reports/catalog", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rr.Code, rr.Body)
	}
	var catalog reporting.Catalog
	if err := json.Unmarshal(rr.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	var definition reporting.Definition
	for _, d := range catalog.Templates {
		if d.Template == "team_usage" {
			definition = d
		}
	}
	if definition.Template == "" {
		t.Fatal("team_usage missing from HTTP catalog")
	}
	if definition.Scope != "self" {
		t.Fatalf("unsafe default scope: %s", definition.Scope)
	}

	other, _, err := h.store.UpsertUserFromIdentity(ctx, "team-usage-other", "other-usage@example.test", "Other", false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*store.User{h.user, other} {
		if err := h.store.UpdateUser(ctx, u.ID, store.RoleUser, true); err != nil {
			t.Fatal(err)
		}
	}
	a, err := h.store.CreateTeam(ctx, "Usage Alpha", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.store.CreateTeam(ctx, "Usage Beta", other.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A's current roster includes Other, but their B and personal traffic is not A's.
	if err := h.store.AddTeamMember(ctx, a.ID, other.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "usage-second-model", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var modelB *store.Model
	for _, model := range models {
		if model.ID != h.model.ID {
			modelB = model
		}
	}
	if modelB == nil {
		t.Fatal("second model missing")
	}
	if err := h.store.AddTeamMember(ctx, b.ID, h.user.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	token, _, err := h.store.CreateTeamToken(ctx, h.user.ID, "Historical attribution", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RemoveTeamMemberSource(ctx, b.ID, h.user.ID, "manual", ""); err != nil {
		t.Fatal(err)
	}
	// Deliberately store A-attributed history against a now B-bound key: reports
	// must read event attribution, never the key's current team or user's roster.
	events := []store.UsageEvent{
		{UserID: h.user.ID, TeamIDs: a.ID, ModelID: h.model.ID, TokensIn: 10, TokensOut: 1, CostNano: 1000000000, TokenID: token.ID},
		{UserID: h.user.ID, TeamIDs: a.ID, ModelID: modelB.ID, TokensIn: 20, TokensOut: 2, CostNano: 2000000000},
		{UserID: other.ID, TeamIDs: a.ID, ModelID: h.model.ID, TokensIn: 30, TokensOut: 3, CostNano: 3000000000},
		{UserID: other.ID, TeamIDs: b.ID, ModelID: h.model.ID, TokensIn: 40, TokensOut: 4, CostNano: 4000000000},
		{UserID: other.ID, TeamIDs: b.ID, ModelID: modelB.ID, TokensIn: 50, TokensOut: 5, CostNano: 5000000000},
		{UserID: other.ID, ModelID: modelB.ID, TokensIn: 60, TokensOut: 6, CostNano: 6000000000},
	}
	for i := range events {
		events[i].ID = store.NewID()
		events[i].CreatedAt = time.Now().UTC()
		events[i].HTTPStatus = 200
		events[i].CostStatus = "priced"
		if err := h.store.InsertUsageEvent(ctx, &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	type amounts struct{ requests, in, out, cost float64 }
	key := func(team, model string) string { return team + "/" + model }
	selfRows := map[string]amounts{key(a.ID, h.model.ID): {1, 10, 1, 1}, key(a.ID, modelB.ID): {1, 20, 2, 2}}
	teamRows := map[string]amounts{key(a.ID, h.model.ID): {2, 40, 4, 4}, key(a.ID, modelB.ID): {1, 20, 2, 2}}
	orgRows := map[string]amounts{key(a.ID, h.model.ID): {2, 40, 4, 4}, key(a.ID, modelB.ID): {1, 20, 2, 2}, key(b.ID, h.model.ID): {1, 40, 4, 4}, key(b.ID, modelB.ID): {1, 50, 5, 5}, key("(unknown)", modelB.ID): {1, 60, 6, 6}}
	generate := func(d reporting.Definition, want map[string]amounts, total amounts, local bool) {
		t.Helper()
		h.server.Config.LocalOnly = local
		rr := h.do(http.MethodPost, "/api/v1/reports/runs", map[string]any{"definition": d})
		if rr.Code != http.StatusAccepted {
			t.Fatalf("enqueue %s: %d %s", d.Scope, rr.Code, rr.Body)
		}
		var body struct {
			Run store.ReportRun `json:"run"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		id := body.Run.ID
		worked, err := jobs.NewReportWorker(h.store, local, h.server.Logger).RunOnce(ctx)
		if err != nil || !worked {
			t.Fatalf("worker: worked=%v error=%v", worked, err)
		}
		rr = h.do(http.MethodGet, "/api/v1/reports/runs/"+id, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("result: %d %s", rr.Code, rr.Body)
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		result := body.Run.Result
		if result == nil {
			t.Fatalf("no generated result: %s", rr.Body)
		}
		if result.Definition.Name != "Team usage" || len(result.Rows) != len(want) {
			t.Fatalf("unexpected report: %+v", result)
		}
		check := func(values map[string]*float64, want amounts) {
			t.Helper()
			for metric, expected := range map[string]float64{"requests": want.requests, "tokens_in": want.in, "tokens_out": want.out, "cost_usd": want.cost} {
				if local && metric == "cost_usd" {
					if _, ok := values[metric]; ok {
						t.Fatal("cost leaked")
					}
					continue
				}
				if value := values[metric]; value == nil || *value != expected {
					t.Fatalf("%s: got %v want %v", metric, value, expected)
				}
			}
		}
		seen := map[string]bool{}
		for _, row := range result.Rows {
			k := key(row.Dimensions["team"], row.Dimensions["model"])
			expected, ok := want[k]
			if !ok || seen[k] {
				t.Fatalf("unexpected or duplicate team/model row: %+v", row)
			}
			seen[k] = true
			check(row.Values, expected)
		}
		check(result.Totals, total)
		if result.SourceRows != int64(total.requests) {
			t.Fatalf("source rows: %d", result.SourceRows)
		}
		if local && strings.Contains(rr.Body.String(), "cost_usd") {
			t.Fatal("local-only result leaked monetary schema")
		}
	}
	generate(definition, selfRows, amounts{2, 30, 3, 3}, false)
	definition.Scope, definition.TeamID = "team", a.ID
	generate(definition, teamRows, amounts{3, 60, 6, 6}, false)
	// Removing the event owner from A must not rewrite historical attribution.
	if err := h.store.RemoveTeamMemberSource(ctx, a.ID, other.ID, "manual", ""); err != nil {
		t.Fatal(err)
	}
	generate(definition, teamRows, amounts{3, 60, 6, 6}, false)
	generate(definition, teamRows, amounts{3, 60, 6, 6}, true)
	h.server.Config.LocalOnly = false
	for _, tc := range []struct{ scope, team string }{{"organization", ""}, {"team", b.ID}} {
		definition.Scope, definition.TeamID = tc.scope, tc.team
		rr := h.do(http.MethodPost, "/api/v1/reports/runs", map[string]any{"definition": definition})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized %s: %d %s", tc.scope, rr.Code, rr.Body)
		}
	}
	// Ordinary members (not just nonmembers) must not gain team report access.
	if err := h.store.AddTeamMember(ctx, b.ID, h.user.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	rr = h.do(http.MethodPost, "/api/v1/reports/runs", map[string]any{"definition": definition})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member team scope: %d %s", rr.Code, rr.Body)
	}
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	definition.Scope, definition.TeamID = "organization", ""
	generate(definition, orgRows, amounts{6, 210, 21, 21}, false)
	// Even an administrator's self scope remains self, never organization.
	definition.Scope = "self"
	generate(definition, selfRows, amounts{2, 30, 3, 3}, false)
	t.Logf("Verified self/team/organization breakdowns across %d historical events", len(events))
}
