package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// A rate-limit rule can name one service token: that integration is
// throttled, other callers are not.
func TestRateLimitRuleTargetsServiceToken(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	tok, plaintext := h.newServiceToken(t, "looping-agent", true)
	other, otherPlain := h.newServiceToken(t, "other-bot", true)
	_ = other

	if rec := h.do(http.MethodPost, "/api/v1/admin/rate-limits", map[string]any{
		"subject_type": "service_token", "subject_id": "no-such-token", "endpoint": "*", "requests_per_minute": 1,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown service token = %d %s, want 400", rec.Code, rec.Body.String())
	}
	rec := h.do(http.MethodPost, "/api/v1/admin/rate-limits", map[string]any{
		"subject_type": "service_token", "subject_id": tok.ID, "endpoint": "/v1/chat/completions", "requests_per_minute": 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service-token rule = %d %s, want 201", rec.Code, rec.Body.String())
	}
	rule := decodeBody(t, rec)["rule"].(map[string]any)
	if rule["subject_type"] != "service_token" {
		t.Fatalf("stored subject_type = %v", rule["subject_type"])
	}
	list := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/rate-limits", nil))["rules"].([]any)
	if got := list[0].(map[string]any)["subject_name"]; got != "looping-agent" {
		t.Errorf("listed subject_name = %v, want the token name", got)
	}
	if err := h.store.SetFeatureFlag(ctx, "per_user_rate_limits_enabled", true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("first call = %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call within the minute = %d, want 429", rec.Code)
	}
	if rec := h.doAsServiceToken(otherPlain, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("other service token was throttled: %d", rec.Code)
	}
}
