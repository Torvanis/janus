package httpapi

import (
	"context"
	"fmt"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
	"time"
)

func TestCollectionServerContinuation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := adminSession(t, h)
	for i := 0; i < 205; i++ {
		if err := h.store.CreateNotification(ctx, &store.Notification{UserID: h.user.ID, Title: fmt.Sprint(i), Severity: "info"}); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated inboxes never enter the list or its continuation calculation.
	if err := h.store.CreateNotification(ctx, &store.Notification{UserID: "other", Title: "private", Severity: "info"}); err != nil {
		t.Fatal(err)
	}
	var vs []*store.SecgwViolation
	for i := 0; i < 205; i++ {
		vs = append(vs, &store.SecgwViolation{ID: fmt.Sprintf("v-%03d", i), UserID: h.user.ID, Kind: "pii", Action: "observed", CreatedAt: time.Now().UTC()})
	}
	vs = append(vs, &store.SecgwViolation{ID: "excluded", Kind: "secrets", Action: "blocked", CreatedAt: time.Now().UTC()})
	if err := h.store.InsertSecgwViolations(ctx, vs); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/v1/notifications?unread=true", "/api/v1/admin/secgw/violations?kind=pii&action=observed"} {
		seen := map[string]bool{}
		for offset := 0; offset < 205; offset += 100 {
			rec := h.doAsSession(session, http.MethodGet, fmt.Sprintf("%s&limit=100&offset=%d", endpoint, offset), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", endpoint, rec.Code, rec.Body.String())
			}
			var body struct {
				Notifications []store.Notification   `json:"notifications"`
				Violations    []store.SecgwViolation `json:"violations"`
				HasMore       bool                   `json:"has_more"`
				Offset        int                    `json:"offset"`
				Limit         int                    `json:"limit"`
			}
			decodeInto(t, rec, &body)
			if body.HasMore != (offset < 200) || body.Offset != offset || body.Limit != 100 {
				t.Fatalf("incorrect continuation: %+v", body)
			}
			ids := []string{}
			for _, n := range body.Notifications {
				if n.UserID != h.user.ID {
					t.Fatal("cross-user notification")
				}
				ids = append(ids, n.ID)
			}
			for _, v := range body.Violations {
				if v.Kind != "pii" {
					t.Fatal("filter applied after paging")
				}
				ids = append(ids, v.ID)
			}
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("duplicate %s", id)
				}
				seen[id] = true
			}
		}
		if len(seen) != 205 {
			t.Fatalf("%s got %d rows", endpoint, len(seen))
		}
	}
}

func TestCollectionSortStableAndBlankLast(t *testing.T) {
	for _, sorter := range []func([]map[string]any, string){sortServiceTokenRows, sortManagedModelRows} {
		for _, dir := range []string{"asc", "desc"} {
			rows := []map[string]any{{"id": "blank", "name": ""}, {"id": "first", "name": "same"}, {"id": "second", "name": "same"}}
			sorter(rows, "name_"+dir)
			if rows[0]["id"] != "first" || rows[1]["id"] != "second" || rows[2]["id"] != "blank" {
				t.Fatalf("unstable/blank first: %v", rows)
			}
		}
	}
	for _, dir := range []string{"asc", "desc"} {
		rows := []map[string]any{{"id": "blank", "last_used_at": time.Time{}}, {"id": "used", "last_used_at": time.Now()}}
		sortServiceTokenRows(rows, "last_used_"+dir)
		if rows[1]["id"] != "blank" {
			t.Fatalf("blank timestamp first: %v", rows)
		}
	}
}
