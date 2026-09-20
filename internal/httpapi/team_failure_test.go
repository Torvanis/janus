package httpapi

import (
	"context"
	"errors"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

type brokenTeamBody struct{}

func (brokenTeamBody) Read([]byte) (int, error) { return 0, errors.New("client disconnected") }
func (brokenTeamBody) Close() error             { return nil }
func TestTeamUnreadableRequestRetainsAttribution(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Failure accounting", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := h.store.CreateTeamToken(ctx, h.user.ID, "team", team.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", brokenTeamBody{})
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	h.server.pending.Wait()
	events, _, err := h.store.ListRequests(ctx, store.RequestFilter{UserID: h.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 400 || len(events) != 1 {
		t.Fatalf("body failure status %d produced %d events", rec.Code, len(events))
	}
	if events[0].TeamIDs != team.ID || events[0].HTTPStatus != 400 || events[0].CostNano != 0 {
		t.Fatalf("wrong failed call attribution: %+v", events[0])
	}
}
