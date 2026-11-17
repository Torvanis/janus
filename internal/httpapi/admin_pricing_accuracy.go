package httpapi

import (
	"net/http"
	"time"
)

// handlePricingAccuracy serves GET /api/v1/admin/pricing/accuracy: for the
// calls whose cost the provider quoted (xAI, OpenRouter, Requesty,
// Perplexity, DeepInfra, LiteLLM), the quote beside Janus's own price for
// the same call, per model and per provider, plus the calls that differ
// most. range=day|week|month|quarter|custom (start/end RFC 3339), default
// month; worst=N (1-50, default 10). Administrators only (mounted under
// RequireAdmin).
func (s *Server) handlePricingAccuracy(w http.ResponseWriter, r *http.Request) {
	start, end, label := rangeBounds(r)
	if r.URL.Query().Get("range") == "" {
		end = time.Now().UTC()
		start, label = end.AddDate(0, -1, 0), "month"
	}
	report, err := s.Store.PriceAccuracy(r.Context(), start, end, queryInt(r, "worst", 10))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"range": label, "report": report})
}
