package httpapi

import (
	"net/http"
	"testing"
)

// Every Business feature that already existed in the product now refuses
// its *enabling* write on Community with 402 feature_not_licensed, while
// the Community shape of the same call still works. Reads are never gated.
func TestBusinessFeatureGatesOnCommunity(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	is402 := func(name string, rec interface{ Result() *http.Response }) {
		t.Helper()
		if rec.Result().StatusCode != http.StatusPaymentRequired {
			t.Fatalf("%s: want 402, got %d", name, rec.Result().StatusCode)
		}
	}

	// guardrails_enforce: observe is fine, block is not.
	if rec := h.do(http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{"name": "obs", "checks": []map[string]any{{"kind": "secrets", "mode": "observe", "enabled": true}}}); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("observe policy on community: %d %s", rec.Code, rec.Body)
	}
	is402("block policy", h.do(http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{"name": "blk", "checks": []map[string]any{{"kind": "secrets", "mode": "block", "enabled": true}}}))

	// captures: enabling recording.
	is402("captures on", h.do(http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{"enabled": true, "config": map[string]any{"filter": map[string]any{"match": "all", "outcome": "failure"}, "retention": map[string]any{"max_age_hours": 1, "max_count": 10}}}))
	if rec := h.do(http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{"enabled": false, "config": map[string]any{"filter": map[string]any{"match": "all", "outcome": "failure"}, "retention": map[string]any{"max_age_hours": 1, "max_count": 10}}}); rec.Code == http.StatusPaymentRequired {
		t.Fatal("disabling captures must never be gated")
	}

	// email_alerts: email channel.
	is402("email alert", h.do(http.MethodPost, "/api/v1/admin/alerts", map[string]any{"trigger": "quota_exhausted", "channels": []string{"email"}}))
	if rec := h.do(http.MethodPost, "/api/v1/admin/alerts", map[string]any{"trigger": "quota_exhausted", "channels": []string{"in_app"}}); rec.Code == http.StatusPaymentRequired {
		t.Fatal("in-app alert gated")
	}

	// model_fallbacks: creating with a fallback.
	is402("fallback", h.do(http.MethodPost, "/api/v1/admin/managed-models", map[string]any{"name": "mm", "target_model_id": h.model.ID, "fallback_model_id": h.model.ID}))
	if rec := h.do(http.MethodPost, "/api/v1/admin/managed-models", map[string]any{"name": "mm2", "target_model_id": h.model.ID}); rec.Code == http.StatusPaymentRequired {
		t.Fatal("plain managed model gated")
	}

	// audit_export: the CSV; the list is not.
	is402("audit export", h.do(http.MethodGet, "/api/v1/admin/audit/export", nil))
	if rec := h.do(http.MethodGet, "/api/v1/admin/audit", nil); rec.Code != http.StatusOK {
		t.Fatalf("audit list gated: %d", rec.Code)
	}

	// With a Business key everything above opens, and the export is CSV.
	bizLicense(t, h)
	rec := h.do(http.MethodGet, "/api/v1/admin/audit/export?action=license.install", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("export: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if body := rec.Body.String(); len(body) < 20 || body[:10] != "created_at" {
		t.Fatalf("csv body: %q", body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/alerts", map[string]any{"trigger": "quota_exhausted", "channels": []string{"email"}}); rec.Code == http.StatusPaymentRequired {
		t.Fatal("email alert still gated on business")
	}
}
