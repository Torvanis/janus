package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// TestAdminRateLimitUpdate locks the documented edit path: an existing rule's cap
// can be changed in place (single audit entry, bucket identity preserved)
// instead of delete+recreate.
func TestAdminRateLimitUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The harness user was bootstrapped as admin.
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/rate-limits",
		map[string]any{"endpoint": "/v1/chat/completions", "requests_per_minute": 60})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rate limit = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Rule store.RateLimitRule `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created rule: %v", err)
	}

	rec = h.doAsSession(session, http.MethodPut, "/api/v1/admin/rate-limits/"+created.Rule.ID,
		map[string]any{"requests_per_minute": 120})
	if rec.Code != http.StatusOK {
		t.Fatalf("update rate limit = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Rule store.RateLimitRule `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated rule: %v", err)
	}
	if updated.Rule.ID != created.Rule.ID || updated.Rule.RequestsPerMinute != 120 {
		t.Fatalf("update result = %+v, want same rule id with rpm 120", updated.Rule)
	}

	// The stored row changed, not a new row.
	reloaded, err := h.store.RateLimitRuleByID(ctx, created.Rule.ID)
	if err != nil || reloaded.RequestsPerMinute != 120 {
		t.Fatalf("reloaded rule = %+v err=%v, want rpm 120", reloaded, err)
	}

	// Validation still applies.
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/admin/rate-limits/"+created.Rule.ID,
		map[string]any{"requests_per_minute": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zero rpm accepted: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/admin/rate-limits/nope",
		map[string]any{"requests_per_minute": 10})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown rule update = %d, want 404", rec.Code)
	}

	// One update produced exactly one rate_limit_updated audit entry with
	// before/after values.
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	updates := 0
	for _, e := range entries {
		if e.Action == "rate_limit_updated" {
			updates++
			if e.OldValue == "" || e.NewValue == "" {
				t.Errorf("rate_limit_updated audit entry missing old/new values: %+v", e)
			}
		}
	}
	if updates != 1 {
		t.Fatalf("rate_limit_updated audit entries = %d, want 1", updates)
	}
}
