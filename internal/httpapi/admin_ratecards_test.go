package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/ratecards"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// TestApplyReferenceRatecards locks rate-card seeding: the bundled seed prices unpriced
// discovered models by name, never overwrites administrator-set rates, and is
// idempotent.
func TestApplyReferenceRatecards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "gpt-4o-mini", []string{"chat"}); err != nil {
		t.Fatalf("seed unpriced model: %v", err)
	}

	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/ratecards/reference", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reference ratecards = %d: %s", rec.Code, rec.Body.String())
	}
	var reference struct {
		Ratecards []map[string]any `json:"ratecards"`
		Caveat    string           `json:"caveat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reference); err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	if len(reference.Ratecards) < 10 || reference.Caveat == "" {
		t.Fatalf("reference seed looks wrong: %d entries, caveat %q", len(reference.Ratecards), reference.Caveat)
	}

	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		AppliedCount int `json:"applied_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode apply result: %v", err)
	}
	if result.AppliedCount != 1 {
		t.Fatalf("applied_count = %d, want 1 (only the unpriced gpt-4o-mini)", result.AppliedCount)
	}

	models, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	for _, m := range models {
		switch m.Name {
		case "gpt-4o-mini":
			if m.RateInNano != usage.NanoFromUSD(0.15) || m.RateOutNano != usage.NanoFromUSD(0.6) {
				t.Errorf("gpt-4o-mini rates = %d/%d, want the seed values", m.RateInNano, m.RateOutNano)
			}
		case "test-model":
			// Administrator-set rates (10/30/1 USD per MTok) must be untouched.
			if m.RateInNano != 10*usage.NanoPerUSD {
				t.Errorf("test-model rates were overwritten: %d", m.RateInNano)
			}
		}
	}

	// Re-applying changes nothing.
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("second apply = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode second apply: %v", err)
	}
	if result.AppliedCount != 0 {
		t.Fatalf("second apply touched %d models, want 0", result.AppliedCount)
	}
}

// TestRatecardsSeedFiveDimensions locks the bundled seed shape: it parses,
// current Anthropic models carry all five billing dimensions (input, 5m
// cache write, 1h cache write, cache hit, output), and providers that do not
// bill cache writes default those dimensions to zero.
func TestRatecardsSeedFiveDimensions(t *testing.T) {
	entries, updated, err := ratecards.Reference()
	if err != nil {
		t.Fatalf("bundled seed does not parse: %v", err)
	}
	if updated < "2026-08-23" {
		t.Errorf("seed updated date %q was not bumped with the new pricing", updated)
	}
	for _, e := range entries {
		if e.CacheWrite5mUSDPerMTok < 0 || e.CacheWrite1hUSDPerMTok < 0 {
			t.Errorf("%s has a negative cache-write rate", e.Model)
		}
	}

	// USD per MTok in in/5m-write/1h-write/cached/out order.
	anthropic := []struct {
		model string
		rates [5]float64
	}{
		{"claude-fable-5", [5]float64{10, 12.50, 20, 1, 50}},
		{"claude-mythos-5", [5]float64{10, 12.50, 20, 1, 50}},
		{"claude-opus-5", [5]float64{5, 6.25, 10, 0.50, 25}},
		{"claude-sonnet-5", [5]float64{2, 2.50, 4, 0.20, 10}},
		{"claude-haiku-4.5", [5]float64{1, 1.25, 2, 0.10, 5}},
	}
	for _, tc := range anthropic {
		t.Run(tc.model, func(t *testing.T) {
			e := ratecards.Match(tc.model)
			if e == nil {
				t.Fatalf("seed has no entry for %s", tc.model)
			}
			got := [5]float64{e.InUSDPerMTok, e.CacheWrite5mUSDPerMTok, e.CacheWrite1hUSDPerMTok, e.CachedUSDPerMTok, e.OutUSDPerMTok}
			if got != tc.rates {
				t.Errorf("%s rates = %v, want %v (in/5m-write/1h-write/cached/out)", tc.model, got, tc.rates)
			}
		})
	}

	// Non-Anthropic entries never bill cache writes: the fields default to 0.
	for _, name := range []string{"gpt-4o", "mistral-large-latest", "text-embedding-3-small"} {
		e := ratecards.Match(name)
		if e == nil {
			t.Fatalf("seed has no entry for %s", name)
		}
		if e.CacheWrite5mUSDPerMTok != 0 || e.CacheWrite1hUSDPerMTok != 0 {
			t.Errorf("%s cache-write rates = %v/%v, want 0/0", name, e.CacheWrite5mUSDPerMTok, e.CacheWrite1hUSDPerMTok)
		}
	}
}

// TestRatecardsApplyLandsAllFiveRates proves the apply path writes every
// billing dimension — including output rates far beyond the old 32-bit
// ~$2.147/MTok ceiling — and leaves non-Anthropic write rates at zero.
func TestRatecardsApplyLandsAllFiveRates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, name := range []string{"claude-fable-5", "gpt-4o"} {
		if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed unpriced model %s: %v", name, err)
		}
	}

	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Applied []map[string]any `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode apply result: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Fatalf("applied %d models, want 2: %s", len(result.Applied), rec.Body.String())
	}
	for _, entry := range result.Applied {
		for _, key := range []string{"rate_cache_write_5m_usd_per_mtok", "rate_cache_write_1h_usd_per_mtok"} {
			if _, ok := entry[key]; !ok {
				t.Errorf("apply response for %v is missing %s", entry["model"], key)
			}
		}
	}

	models, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	byName := map[string]*store.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}

	fable := byName["claude-fable-5"]
	if fable == nil {
		t.Fatal("claude-fable-5 missing from listing")
	}
	want := store.RateCard{
		RateInNano:           usage.NanoFromUSD(10),
		RateCacheWrite5mNano: usage.NanoFromUSD(12.50),
		RateCacheWrite1hNano: usage.NanoFromUSD(20),
		RateCachedNano:       usage.NanoFromUSD(1),
		RateOutNano:          usage.NanoFromUSD(50),
	}
	if want.RateOutNano <= math.MaxInt32 {
		t.Fatalf("test premise broken: $50/MTok = %d nano must exceed int32", want.RateOutNano)
	}
	got := store.RateCard{
		RateInNano:           fable.RateInNano,
		RateCacheWrite5mNano: fable.RateCacheWrite5mNano,
		RateCacheWrite1hNano: fable.RateCacheWrite1hNano,
		RateCachedNano:       fable.RateCachedNano,
		RateOutNano:          fable.RateOutNano,
	}
	if got != want {
		t.Errorf("claude-fable-5 model rates = %+v, want %+v", got, want)
	}
	if fable.RateEffectiveFrom.IsZero() {
		t.Error("claude-fable-5 has no rate_effective_from — seed apply must record a rate card")
	}

	// The versioned rate card carries the same five dimensions.
	rc, err := h.store.RateCardAt(ctx, fable.ID, fable.RateEffectiveFrom)
	if err != nil {
		t.Fatalf("rate card at effective_from: %v", err)
	}
	if rc.RateInNano != want.RateInNano || rc.RateOutNano != want.RateOutNano ||
		rc.RateCachedNano != want.RateCachedNano ||
		rc.RateCacheWrite5mNano != want.RateCacheWrite5mNano ||
		rc.RateCacheWrite1hNano != want.RateCacheWrite1hNano {
		t.Errorf("versioned rate card = %+v, want the five seeded dimensions %+v", rc, want)
	}

	gpt := byName["gpt-4o"]
	if gpt == nil {
		t.Fatal("gpt-4o missing from listing")
	}
	if gpt.RateInNano != usage.NanoFromUSD(2.5) || gpt.RateOutNano != usage.NanoFromUSD(10) {
		t.Errorf("gpt-4o base rates = %d/%d, want the seed values", gpt.RateInNano, gpt.RateOutNano)
	}
	if gpt.RateCacheWrite5mNano != 0 || gpt.RateCacheWrite1hNano != 0 {
		t.Errorf("gpt-4o cache-write rates = %d/%d, want 0/0 (provider does not bill cache writes)",
			gpt.RateCacheWrite5mNano, gpt.RateCacheWrite1hNano)
	}
}

// TestApplyReferenceRatecardsContextWindows proves the apply path copies the
// seed's context window onto matching models that still have none — including
// models an administrator already priced, since the no-touch guarantee is
// about rate cards — never overwrites a value that is already set, audits each
// seeding, and is idempotent.
func TestApplyReferenceRatecardsContextWindows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// gpt-4o-mini: unpriced and unknown window — gets both from the seed.
	// gpt-4o: admin-priced but unknown window — keeps its rates, gains the window.
	for _, name := range []string{"gpt-4o-mini", "gpt-4o"} {
		if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed model %s: %v", name, err)
		}
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{Search: "gpt-4o"})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	byName := map[string]*store.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	adminRates := store.RateCard{RateInNano: usage.NanoFromUSD(9), RateOutNano: usage.NanoFromUSD(27), EffectiveFrom: time.Now().UTC().Add(-time.Hour)}
	if err := h.store.SetModelRates(ctx, byName["gpt-4o"].ID, adminRates); err != nil {
		t.Fatalf("admin-price gpt-4o: %v", err)
	}

	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Applied           []map[string]any `json:"applied"`
		AppliedCount      int              `json:"applied_count"`
		ContextWindowsSet int              `json:"context_windows_set"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode apply result: %v", err)
	}
	if result.AppliedCount != 1 {
		t.Fatalf("applied_count = %d, want 1 (only the unpriced gpt-4o-mini gets rates)", result.AppliedCount)
	}
	if result.ContextWindowsSet != 2 {
		t.Fatalf("context_windows_set = %d, want 2 (gpt-4o-mini and the already-priced gpt-4o)", result.ContextWindowsSet)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("applied has %d entries, want 1", len(result.Applied))
	}
	if got, ok := result.Applied[0]["context_window_tokens"].(float64); !ok || int64(got) != 128000 {
		t.Errorf("applied entry context_window_tokens = %v, want 128000", result.Applied[0]["context_window_tokens"])
	}

	mini, err := h.store.ModelByID(ctx, byName["gpt-4o-mini"].ID)
	if err != nil {
		t.Fatalf("reload gpt-4o-mini: %v", err)
	}
	if mini.ContextWindow != 128000 {
		t.Errorf("gpt-4o-mini context_window = %d, want the seeded 128000", mini.ContextWindow)
	}
	full, err := h.store.ModelByID(ctx, byName["gpt-4o"].ID)
	if err != nil {
		t.Fatalf("reload gpt-4o: %v", err)
	}
	if full.ContextWindow != 128000 {
		t.Errorf("admin-priced gpt-4o context_window = %d, want the seeded 128000", full.ContextWindow)
	}
	if full.RateInNano != adminRates.RateInNano || full.RateOutNano != adminRates.RateOutNano {
		t.Errorf("gpt-4o admin rates were overwritten: %d/%d", full.RateInNano, full.RateOutNano)
	}

	// Each seeding is audited with the source.
	audits, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "model_context_window_seeded"})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audits) != 2 {
		t.Fatalf("model_context_window_seeded audit rows = %d, want 2", len(audits))
	}
	for _, a := range audits {
		if !strings.Contains(a.NewValue, "context_window") || !strings.Contains(a.NewValue, "bundled_reference_seed") {
			t.Errorf("audit payload missing context_window/source: %s", a.NewValue)
		}
	}

	// Admin-corrected values survive a re-apply: set one, re-apply, no change.
	if err := h.store.SetModelContextWindow(ctx, mini.ID, 64000); err != nil {
		t.Fatalf("admin-correct context window: %v", err)
	}
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/ratecards/apply", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("second apply = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode second apply: %v", err)
	}
	if result.AppliedCount != 0 || result.ContextWindowsSet != 0 {
		t.Fatalf("second apply = %d rates / %d windows, want 0/0 (idempotent, already-set values untouched)",
			result.AppliedCount, result.ContextWindowsSet)
	}
	if again, _ := h.store.ModelByID(ctx, mini.ID); again.ContextWindow != 64000 {
		t.Errorf("re-apply overwrote the admin-corrected context window: %d", again.ContextWindow)
	}

	// The admin model listing carries the field for the UI.
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/admin/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin models = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"context_window":128000`) {
		t.Errorf("admin model listing must include context_window: %s", rec.Body.String())
	}
}
