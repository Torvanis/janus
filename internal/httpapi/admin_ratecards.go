package httpapi

import (
	"net/http"
	"time"

	"github.com/torvanis/janus/internal/ratecards"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Bundled reference rate cards. Fresh installs have no rates, and an
// unpriced model cannot be enabled — these endpoints expose the seed shipped
// inside the binary and apply it to discovered models that are still unpriced.
//
// Local-only mode (JANUS_LOCAL_ONLY) deliberately leaves these endpoints
// functional: pricing data is retained (never deleted) so that disabling the
// mode later resumes cost tracking with the rates already in place. The UI
// hides every pricing surface while the mode is on, so the endpoints are
// simply unreachable through the product.

const ratecardCaveat = "Reference list prices at the time the seed was authored — verify against the provider's current pricing before relying on cost attribution."

// handleReferenceRatecards returns the bundled seed for display in the UI or
// for download.
func (s *Server) handleReferenceRatecards(w http.ResponseWriter, r *http.Request) {
	entries, updated, err := ratecards.Reference()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ratecards": entries,
		"updated":   updated,
		"caveat":    ratecardCaveat,
	})
}

// handleApplyReferenceRatecards prices every discovered model that (a) has no
// rate card yet and (b) matches a seed entry by name or alias. Models an
// administrator already priced are never touched. Independently of pricing,
// it also copies the seed's context window onto every matching model that
// still has none (context_window 0) — the no-touch guarantee is about rate
// cards, and a context window an administrator already set is never
// overwritten.
func (s *Server) handleApplyReferenceRatecards(w http.ResponseWriter, r *http.Request) {
	models, err := s.Store.ListModels(r.Context(), store.ModelFilter{})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	applied := []map[string]any{}
	contextWindowsSet := 0
	for _, m := range models {
		entry := ratecards.Match(m.Name)
		if entry == nil {
			continue
		}
		priced, contextSet, err := s.Store.ApplyReferenceMetadata(r.Context(), m.ID, entry.ContextWindowTokens, store.RateCard{
			RateInNano: usage.NanoFromUSD(entry.InUSDPerMTok), RateOutNano: usage.NanoFromUSD(entry.OutUSDPerMTok), RateCachedNano: usage.NanoFromUSD(entry.CachedUSDPerMTok),
			RateCacheWrite5mNano: usage.NanoFromUSD(entry.CacheWrite5mUSDPerMTok), RateCacheWrite1hNano: usage.NanoFromUSD(entry.CacheWrite1hUSDPerMTok), EffectiveFrom: now,
		})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if contextSet {
			s.audit(r, "model_context_window_seeded", "model", m.ID,
				map[string]any{"context_window": int64(0)},
				map[string]any{
					"model": m.Name, "context_window": entry.ContextWindowTokens,
					"source": "bundled_reference_seed",
				})
			contextWindowsSet++
		}
		if !priced {
			continue
		}
		s.audit(r, "model_rates_seeded", "model", m.ID, nil, map[string]any{
			"model": m.Name, "rate_in_usd_per_mtok": entry.InUSDPerMTok,
			"rate_out_usd_per_mtok": entry.OutUSDPerMTok, "rate_cached_usd_per_mtok": entry.CachedUSDPerMTok,
			"rate_cache_write_5m_usd_per_mtok": entry.CacheWrite5mUSDPerMTok,
			"rate_cache_write_1h_usd_per_mtok": entry.CacheWrite1hUSDPerMTok,
			"source":                           "bundled_reference_seed",
		})
		applied = append(applied, map[string]any{
			"model_id": m.ID, "model": m.Name,
			"rate_in_usd_per_mtok": entry.InUSDPerMTok, "rate_out_usd_per_mtok": entry.OutUSDPerMTok,
			"rate_cached_usd_per_mtok":         entry.CachedUSDPerMTok,
			"rate_cache_write_5m_usd_per_mtok": entry.CacheWrite5mUSDPerMTok,
			"rate_cache_write_1h_usd_per_mtok": entry.CacheWrite1hUSDPerMTok,
			"context_window_tokens":            entry.ContextWindowTokens,
		})
	}
	if len(applied) > 0 || contextWindowsSet > 0 {
		s.InvalidateConfigCache()
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"applied":             applied,
		"applied_count":       len(applied),
		"context_windows_set": contextWindowsSet,
		"caveat":              ratecardCaveat,
	})
}
