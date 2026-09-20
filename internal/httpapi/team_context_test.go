package httpapi

import (
	"context"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
)

func TestDisabledUserCannotUseWarmTokenCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.server.resolveToken(ctx, h.token); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateUser(ctx, h.user.ID, h.user.Role, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.server.resolveToken(ctx, h.token); err == nil {
		t.Fatal("disabled account still authenticates from cache")
	}
}

func TestTeamKeyUsesOnlySelectedTeamGrantsAndUsage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, err := h.store.CreateTeam(ctx, "A", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.store.CreateTeam(ctx, "B", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := h.store.CreateTeamToken(ctx, h.user.ID, "A", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.token = key
	body := map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}}
	denied := h.do("POST", "/v1/chat/completions", body)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("personal all-users grant bypassed team: %d %s", denied.Code, denied.Body.String())
	}
	if _, err = h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeTeam, b.ID); err != nil {
		t.Fatal(err)
	}
	denied = h.do("POST", "/v1/chat/completions", body)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("other team's grant leaked: %d", denied.Code)
	}
	if _, err = h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeTeam, a.ID); err != nil {
		t.Fatal(err)
	}
	accepted := h.do("POST", "/v1/chat/completions", body)
	if accepted.Code != 200 {
		t.Fatalf("team call denied: %d %s", accepted.Code, accepted.Body.String())
	}
	h.server.pending.Wait()
	rows, _, err := h.store.ListRequests(ctx, store.RequestFilter{UserID: h.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("missing metering rows: %d", len(rows))
	}
	for _, row := range rows {
		if row.TeamIDs != a.ID {
			t.Fatalf("usage mixed memberships: %q", row.TeamIDs)
		}
	}
	// Directly remove membership to test every-request validation even with hot token cache.
	if _, err = h.store.DB().Exec(`DELETE FROM team_member WHERE team_id=? AND user_id=?`, a.ID, h.user.ID); err != nil {
		t.Fatal(err)
	}
	denied = h.do("POST", "/v1/chat/completions", body)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cached removed member permitted: %d %s", denied.Code, denied.Body.String())
	}
}
