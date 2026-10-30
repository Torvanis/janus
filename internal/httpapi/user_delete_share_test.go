package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// A deleted person's share links stop opening at once.
func TestDeletedUsersShareStopsServing(t *testing.T) {
	h, admin := chatHarness(t)
	other := h.sessionFor(t, "sharer@example.com")
	u, err := h.store.UserByAuthID(context.Background(), "sub-sharer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	conv := chatCreate(t, h, other, map[string]any{"model": "test-model"})
	sent := chatSend(t, h, other, conv, map[string]any{"content": "hello"})
	chatEvents(t, h, other, sent["run_id"].(string), "")
	var sh chatShareView
	decodeInto(t, h.doAsSession(other, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/shares", nil), &sh)
	viewer := h.sessionFor(t, "reader@example.com")
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("share before delete: %d", rec.Code)
	}
	if rec := h.doAsSession(admin, http.MethodDelete, "/api/v1/admin/users/"+u.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a deleted person's share still opens: %d", rec.Code)
	}
}
