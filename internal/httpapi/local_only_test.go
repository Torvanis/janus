package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// Local-only mode (JANUS_LOCAL_ONLY) contract: cost tracking is off — usage
// events record zero cost while token/request accounting is untouched, the
// session bootstrap and system status expose the flag to the SPA, USD-metric
// quota writes are refused, and pre-existing USD quotas become inert instead
// of blocking traffic. With the flag off, behavior is byte-for-byte today's.

// TestLocalOnlyRecordsZeroCost verifies the local-only contract: with the mode on, a proxied
// request's usage event carries cost_nanousd = 0 while token counts are the
// upstream-reported values; with the mode off the same request is costed.
func TestLocalOnlyRecordsZeroCost(t *testing.T) {
	chat := map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}

	t.Run("local-only on: zero cost, tokens intact", func(t *testing.T) {
		h := newHarness(t)
		h.server.Config.LocalOnly = true

		rec := h.do(http.MethodPost, "/v1/chat/completions", chat)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
		}
		event := h.waitForUsageEvents(1)[0]
		if event.CostNano != 0 {
			t.Errorf("cost_nanousd = %d, want 0 in local-only mode", event.CostNano)
		}
		if event.TokensIn != 100 || event.TokensOut != 40 || event.TokensCached != 20 {
			t.Errorf("tokens in/out/cached = %d/%d/%d, want 100/40/20 (token accounting must be unaffected)",
				event.TokensIn, event.TokensOut, event.TokensCached)
		}
	})

	t.Run("local-only off: cost computed as today", func(t *testing.T) {
		h := newHarness(t)

		rec := h.do(http.MethodPost, "/v1/chat/completions", chat)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
		}
		event := h.waitForUsageEvents(1)[0]
		if event.CostNano <= 0 {
			t.Errorf("cost_nanousd = %d, want > 0 with the flag off", event.CostNano)
		}
		if event.TokensIn != 100 || event.TokensOut != 40 {
			t.Errorf("tokens in/out = %d/%d, want 100/40", event.TokensIn, event.TokensOut)
		}
	})
}

// TestMeExposesLocalOnly verifies the local-only contract: /api/v1/me carries the instance mode so
// the SPA can hide every cost surface.
func TestMeExposesLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode bool
	}{
		{"flag on", true},
		{"flag off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.server.Config.LocalOnly = tc.mode
			session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			rec := h.doAsSession(session, http.MethodGet, "/api/v1/me", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("/api/v1/me returned %d: %s", rec.Code, rec.Body.String())
			}
			payload := decodeBody(t, rec)
			got, ok := payload["local_only"]
			if !ok {
				t.Fatal("/api/v1/me must include local_only")
			}
			if got != tc.mode {
				t.Fatalf("local_only = %v, want %v", got, tc.mode)
			}
		})
	}
}

// TestSystemStatusReportsLocalOnly covers the documented status surface: an admin
// can see the instance mode in one place.
func TestSystemStatusReportsLocalOnly(t *testing.T) {
	h := newHarness(t)
	h.server.Config.LocalOnly = true
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/system/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("system status returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["local_only"]; got != true {
		t.Fatalf("system status local_only = %v, want true", got)
	}
}

// TestLocalOnlyRejectsUSDQuotaWrites verifies the local-only API: creating or
// updating a Spend (USD) quota is refused with a 400 naming local-only mode,
// the metric list offered to the create dialogs omits the USD option, and
// token/request quotas keep working. With the flag off, USD quotas create
// exactly as before.
func TestLocalOnlyRejectsUSDQuotaWrites(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A USD quota that predates the mode being switched on.
	preexisting := &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID,
		Metric: store.MetricCostUSD, Limit: 5_000_000_000, Window: store.WindowDaily,
		BreachBehavior: store.BreachLetFinish,
	}
	if err := h.store.CreateQuota(ctx, preexisting); err != nil {
		t.Fatalf("seed usd quota: %v", err)
	}

	h.server.Config.LocalOnly = true

	create := func(metric string) map[string]any {
		return map[string]any{
			"subject_type": "user", "subject_id": h.user.ID, "metric": metric,
			"limit_value": 100, "window": "daily",
		}
	}

	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/quotas", create(store.MetricCostUSD))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("USD quota create with local-only on returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local-only") {
		t.Fatalf("refusal must name local-only mode: %s", rec.Body.String())
	}

	if rec := h.doAsSession(session, http.MethodPut, "/api/v1/admin/quotas/"+preexisting.ID, map[string]any{
		"limit_value": 200,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("USD quota update with local-only on returned %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Token quotas are unaffected by the mode.
	if rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/quotas", create(store.MetricTokensOut)); rec.Code != http.StatusCreated {
		t.Fatalf("token quota create with local-only on returned %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// The metric options served to the create dialog omit Spend (USD).
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/admin/quotas", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list quotas returned %d: %s", rec.Code, rec.Body.String())
	}
	for _, m := range decodeBody(t, rec)["metrics"].([]any) {
		if m.(map[string]any)["value"] == store.MetricCostUSD {
			t.Fatal("metric options must omit cost_usd in local-only mode")
		}
	}

	// The user-facing quota dashboard omits the inert USD quota entirely
	// (it is a cost surface), while non-USD quotas keep showing.
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/dashboard/quota", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("quota dashboard returned %d: %s", rec.Code, rec.Body.String())
	}
	userQuotas := decodeBody(t, rec)["quotas"].([]any)
	if len(userQuotas) == 0 {
		t.Fatal("token quota must remain visible on the user quota dashboard")
	}
	for _, q := range userQuotas {
		if q.(map[string]any)["metric"] == store.MetricCostUSD {
			t.Fatal("user quota dashboard must omit USD quotas in local-only mode")
		}
	}

	// Flag off: USD quota creation works exactly as before, and the metric
	// option is offered again.
	h.server.Config.LocalOnly = false
	if rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/quotas", create(store.MetricCostUSD)); rec.Code != http.StatusCreated {
		t.Fatalf("USD quota create with local-only off returned %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/admin/quotas", nil)
	found := false
	for _, m := range decodeBody(t, rec)["metrics"].([]any) {
		if m.(map[string]any)["value"] == store.MetricCostUSD {
			found = true
		}
	}
	if !found {
		t.Fatal("metric options must include cost_usd with the flag off")
	}
}

// TestLocalOnlyUSDQuotasAreInert locks the traffic guarantee: a USD quota
// breached by consumption recorded BEFORE the mode was enabled must not block
// new requests while local-only is on (it is inert, not enforced).
func TestLocalOnlyUSDQuotasAreInert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A breached USD quota: $0.000001 limit against pre-recorded spend.
	q := &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID,
		Metric: store.MetricCostUSD, Limit: 1_000, Window: store.WindowDaily,
		BreachBehavior: store.BreachLetFinish,
	}
	if err := h.store.CreateQuota(ctx, q); err != nil {
		t.Fatalf("seed usd quota: %v", err)
	}
	chat := map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}

	// Sanity: with the mode off, the first request is costed and the second
	// is rejected by the breached USD quota.
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chat); rec.Code != http.StatusOK {
		t.Fatalf("first request returned %d: %s", rec.Code, rec.Body.String())
	}
	h.waitForUsageEvents(1)
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chat); rec.Code == http.StatusOK {
		t.Fatal("breached USD quota must block traffic with the flag off")
	}

	// Mode on: the same breached quota no longer blocks anything.
	h.server.Config.LocalOnly = true
	h.server.Quota.SetLocalOnly(true)
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chat); rec.Code != http.StatusOK {
		t.Fatalf("breached USD quota must be inert in local-only mode, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLocalOnlyEnablesUnpricedModel: local-only mode hides every rate field,
// so the rate-card requirement for enabling a model must not apply there —
// otherwise a discovered model can never be enabled. Outside local-only mode
// the requirement stands (TestAdminPatchModelEnableRequiresRateCard).
func TestLocalOnlyEnablesUnpricedModel(t *testing.T) {
	h := newHarness(t)
	h.server.Config.LocalOnly = true
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	unpriced := h.seedUnpricedModel("local-only-model")
	rec := h.doAsSession(session, http.MethodPatch, "/api/v1/admin/models/"+unpriced.ID, map[string]any{"status": "enabled"})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable unpriced model in local-only mode = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	reloaded, err := h.store.ModelByID(ctx, unpriced.ID)
	if err != nil || reloaded.Status != store.ModelEnabled {
		t.Fatalf("status after enable = %v (err %v), want enabled", reloaded.Status, err)
	}
	if !reloaded.RateEffectiveFrom.IsZero() {
		t.Fatalf("enabling must not invent a rate card; rate_effective_from = %v", reloaded.RateEffectiveFrom)
	}

	// The same request without local-only mode is still refused.
	h.server.Config.LocalOnly = false
	other := h.seedUnpricedModel("billed-model")
	rec = h.doAsSession(session, http.MethodPatch, "/api/v1/admin/models/"+other.ID, map[string]any{"status": "enabled"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable unpriced model with cost tracking = %d, want 400", rec.Code)
	}
}
