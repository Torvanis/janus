package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestPersonalRequestTokenFilterBeforePagingAndExport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []struct{ user, token, model string }{
		{h.user.ID, "selected", "selected-model"},
		{h.user.ID, "selected", "selected-model"},
		{h.user.ID, "different", "different-model"},
		{"other-user", "foreign", "private-model"},
	} {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{CreatedAt: now.Add(-time.Minute), UserID: e.user, TokenID: e.token, ModelName: e.model, Modality: "chat", HTTPStatus: 200}); err != nil {
			t.Fatal(err)
		}
	}
	for _, offset := range []string{"0", "1"} {
		rec := h.do(http.MethodGet, "/api/v1/requests?range=day&token_id=selected&limit=1&offset="+offset, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		rows := body["requests"].([]any)
		if body["total_count"] != float64(2) || len(rows) != 1 {
			t.Fatalf("filter before paging: %v", body)
		}
		if rows[0].(map[string]any)["model"] != "selected-model" {
			t.Fatalf("wrong token: %v", rows)
		}
	}
	rec := h.do(http.MethodGet, "/api/v1/requests.csv?range=day&token_id=selected", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "selected-model") || strings.Contains(rec.Body.String(), "different-model") || strings.Contains(rec.Body.String(), "private-model") {
		t.Fatalf("filtered CSV: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(http.MethodGet, "/api/v1/requests?token_id=foreign", nil)
	if rec.Code != http.StatusOK || decodeBody(t, rec)["total_count"] != float64(0) {
		t.Fatalf("foreign token widened user scope: %d %s", rec.Code, rec.Body.String())
	}
}
