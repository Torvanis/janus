package httpapi

import (
	"context"
	"encoding/csv"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestRequestExportIncludesChargedTeam(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Accounting Team", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{UserID: h.user.ID, TeamIDs: team.ID}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(http.MethodGet, "/api/v1/requests.csv", nil)
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("export rows %v", rows)
	}
	col := -1
	for i, s := range rows[0] {
		if s == "charged_team" {
			col = i
		}
	}
	if col < 0 || rows[1][col] != "Accounting Team" {
		t.Fatalf("charged team export missing: %v", rows)
	}
}

func TestTokenAffinityHTTPPreservesKeyAndChangesGrants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, err := h.store.CreateTeam(ctx, "Affinity A", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.store.CreateTeam(ctx, "Affinity B", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeTeam, a.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := h.store.TokenByDigest(ctx, store.HashToken(h.token))
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/tokens/" + tok.ID + "/team"
	// A browser-only mutation prevents a leaked inference key changing its own grants.
	if rec := h.do(http.MethodPut, path, map[string]any{"team_id": a.ID, "move_history": false}); rec.Code != 403 {
		t.Fatalf("bearer mutation: %d %s", rec.Code, rec.Body.String())
	}
	for _, target := range []string{a.ID, b.ID, ""} {
		rec := h.doAsSession(session, http.MethodPut, path, map[string]any{"team_id": target, "move_history": false})
		if rec.Code != 200 {
			t.Fatalf("change affinity: %d %s", rec.Code, rec.Body.String())
		}
		current, _, err := h.server.resolveToken(ctx, h.token)
		if err != nil || current.TeamID != target {
			t.Fatalf("warm credential changed incorrectly: %#v %v", current, err)
		}
		result := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}})
		expected := 200
		if target == b.ID {
			expected = 403
		}
		if result.Code != expected {
			t.Fatalf("context %q: status=%d want=%d %s", target, result.Code, expected, result.Body.String())
		}
		h.server.pending.Wait()
	}
	rows, _, err := h.store.ListRequests(ctx, store.RequestFilter{TokenID: tok.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("usage count %d", len(rows))
	}
	seen := map[string]bool{}
	for _, e := range rows {
		seen[e.TeamIDs] = true
	}
	if !seen[a.ID] || !seen[b.ID] || !seen[""] {
		t.Fatalf("lost admission attribution: %v", seen)
	}
	if rec := h.doAsSession(session, http.MethodPut, path, map[string]any{"team_id": a.ID}); rec.Code != 400 {
		t.Fatalf("missing history choice: %d", rec.Code)
	}
}
