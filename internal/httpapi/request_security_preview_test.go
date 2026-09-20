package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func TestRequestSecurityPreviewCompleteness(t *testing.T) {
	for _, count := range []int{0, 199, 200, 201} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			session := adminSession(t, h)
			event := &store.UsageEvent{RequestID: "preview-request", UserID: h.user.ID, CreatedAt: time.Now().UTC(), ModelName: h.model.Name, HTTPStatus: 200}
			if err := h.store.InsertUsageEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
			vs := []*store.SecgwViolation{}
			for i := 0; i < count; i++ {
				v := &store.SecgwViolation{ID: fmt.Sprintf("preview-%03d", i), RequestID: event.RequestID, Kind: "prompt_injection", Action: "observed", MatchText: "sensitive preview text", CreatedAt: time.Now().UTC()}
				// Classifier rows consume the sample too: has_more must be
				// computed BEFORE excluding them from other_violations.
				if i%2 == 0 {
					v.ClassifierModel = h.model.ID
				}
				vs = append(vs, v)
			}
			vs = append(vs, &store.SecgwViolation{ID: "unrelated", RequestID: "other-request", Kind: "pii", Action: "observed", CreatedAt: time.Now().UTC()})
			if err := h.store.InsertSecgwViolations(ctx, vs); err != nil {
				t.Fatal(err)
			}
			rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/requests/"+event.ID+"/security", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				HasMore   bool                   `json:"violations_has_more"`
				Limit     int                    `json:"violations_limit"`
				Scanned   int                    `json:"violations_scanned"`
				Available bool                   `json:"violations_scope_available"`
				Other     []store.SecgwViolation `json:"other_violations"`
			}
			decodeInto(t, rec, &body)
			if body.HasMore != (count > 200) || body.Limit != 200 || body.Scanned != min(count, 200) || !body.Available {
				t.Fatalf("bad preview contract: %+v", body)
			}
			for _, v := range body.Other {
				if v.RequestID != event.RequestID || v.ClassifierModel != "" || v.MatchText != "" {
					t.Fatalf("unexpected violation: %+v", v)
				}
			}
			if strings.Contains(rec.Body.String(), "sensitive preview text") {
				t.Fatal("preview revealed captured text")
			}

			// Missing correlation IDs must never turn an equality filter into
			// an unscoped inventory of other people's violations.
			uncorrelated := &store.UsageEvent{UserID: h.user.ID, CreatedAt: time.Now().UTC(), HTTPStatus: 200}
			if err := h.store.InsertUsageEvent(ctx, uncorrelated); err != nil {
				t.Fatal(err)
			}
			rec = h.doAsSession(session, http.MethodGet, "/api/v1/admin/requests/"+uncorrelated.ID+"/security", nil)
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Body.String())
			}
			decodeInto(t, rec, &body)
			if body.Available || body.HasMore || body.Scanned != 0 || len(body.Other) != 0 {
				t.Fatalf("unscoped preview: %+v", body)
			}
		})
	}
}
