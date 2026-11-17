package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/pricecatalog"
	"github.com/torvanis/janus/internal/store"
)

// Resetting a model's prices to the Janus price catalog, and keeping it
// aligned (store/model_catalog_alignment.go):
//
//	GET  /api/v1/admin/models/{id}/pricing/catalog-preview
//	POST /api/v1/admin/models/pricing/catalog-preview       {ids, only_if_unchanged?}
//	POST /api/v1/admin/models/{id}/pricing/reset-to-catalog
//	POST /api/v1/admin/models/pricing/reset-to-catalog       {ids, only_if_unchanged?}
//
// A preview returns every price line before and after and never writes (the
// bulk preview is a POST only because its id list can be too long for a
// URL). A reset clears every price an administrator set (the five legacy
// token rates included), applies the catalog's entry for the model (else the
// default for its provider and type) and keeps the model aligned. Prices the
// provider publishes through its API still apply over the catalog.

func (s *Server) mountModelCatalogPricingRoutes(r chi.Router) {
	r.Get("/admin/models/{id}/pricing/catalog-preview", s.handlePreviewModelCatalogReset)
	r.Post("/admin/models/pricing/catalog-preview", s.handleBulkPreviewModelsCatalogReset)
	r.Post("/admin/models/{id}/pricing/reset-to-catalog", s.handleResetModelToCatalog)
	r.Post("/admin/models/pricing/reset-to-catalog", s.handleBulkResetModelsToCatalog)
}

// maxBulkCatalogReset bounds one bulk request (the whole catalog of a large
// site fits).
const maxBulkCatalogReset = 5000

// catalogResetPlan is what a reset to the catalog would do to one model.
type catalogResetPlan struct {
	ModelID string `json:"model_id"`
	Model   string `json:"model"`
	// Available is false when the catalog has nothing for the model;
	// Reason ("no_catalog", "no_provider", "no_entry") and Message say why.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	// Catalog labels the catalog values used: "Janus price catalog
	// 2026.11.16.1", with "(default for chat)" when no entry names the
	// model.
	Catalog        string `json:"catalog,omitempty"`
	CatalogAligned bool   `json:"catalog_aligned"`
	// HasAdminPrices: the reset clears prices an administrator set.
	HasAdminPrices bool                `json:"has_admin_prices"`
	Changes        []store.PriceChange `json:"changes"`
	ChangedLines   int                 `json:"changed_lines"`
	// ProviderLines counts lines that, after the reset, bill the price the
	// provider publishes through its API rather than the catalog's.
	ProviderLines int `json:"provider_lines"`

	reference map[string]int64
	sources   map[string]store.ReferenceSource
}

func (s *Server) planCatalogReset(ctx context.Context, m *store.Model, up *store.Upstream) catalogResetPlan {
	p := catalogResetPlan{ModelID: m.ID, Model: m.Name, CatalogAligned: m.CatalogAligned, HasAdminPrices: m.HasAdminPrices(), Changes: []store.PriceChange{}}
	cur := s.priceCatalog().Current()
	switch {
	case cur == nil:
		p.Reason, p.Message = "no_catalog", "Janus has no usable price catalog right now"
		return p
	case up == nil || pricecatalog.PriceProvider(up.AdapterType, up.BaseURL) == "":
		p.Reason, p.Message = "no_provider", "The Janus price catalog lists prices for providers' own APIs only; this model's upstream is self-hosted or a proxy"
		return p
	}
	ref, src := pricecatalog.Reference(cur, up.AdapterType, up.BaseURL, m.Name, m.Modalities)
	priced := false
	for k := range ref {
		if k != "context_window" {
			priced = true
			if p.Catalog == "" {
				p.Catalog = catalogLabel(src[k])
			}
		}
	}
	if !priced {
		typ := pricecatalog.ModelType(m.Modalities)
		if typ == "" {
			typ = "this kind of"
		}
		p.Reason, p.Message = "no_entry", fmt.Sprintf("The Janus price catalog (%s) has no prices for this model, and no default for %s models from %s", cur.Version, typ, up.Name)
		return p
	}
	p.Available, p.reference, p.sources = true, ref, src
	p.Changes = m.CatalogResetPreview(ref, src)
	for _, c := range p.Changes {
		if c.Changed {
			p.ChangedLines++
		}
		if c.AfterSource == "upstream" && c.After != nil {
			p.ProviderLines++
		}
	}
	return p
}

func catalogLabel(src store.ReferenceSource) string {
	if src.Kind == "default" && src.Type != "" {
		return src.Catalog + " (default for " + src.Type + ")"
	}
	return src.Catalog
}

// resetModelToCatalog applies a plan and records it in the audit log.
func (s *Server) resetModelToCatalog(r *http.Request, m *store.Model, p catalogResetPlan) (*store.Model, error) {
	if err := s.Store.ResetModelPricesToCatalog(r.Context(), m.ID, p.reference, p.sources); err != nil {
		return nil, err
	}
	updated, err := s.Store.ModelByID(r.Context(), m.ID)
	if err != nil {
		return nil, err
	}
	before, after := modelMetadataAudit(m), modelMetadataAudit(updated)
	after["catalog"] = p.Catalog
	after["changed_lines"] = p.ChangedLines
	s.audit(r, "model_prices_reset_to_catalog", "model", m.ID, before, after)
	return updated, nil
}

// modelCatalogPlan loads a model and its upstream and works out the plan; it
// writes the error response and returns ok=false when it cannot.
func (s *Server) modelCatalogPlan(w http.ResponseWriter, r *http.Request) (*store.Model, catalogResetPlan, bool) {
	m, err := s.Store.ModelByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That model"))
		return nil, catalogResetPlan{}, false
	}
	if err != nil {
		WriteError(w, r, err)
		return nil, catalogResetPlan{}, false
	}
	up, err := s.Store.UpstreamByID(r.Context(), m.UpstreamID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, err)
		return nil, catalogResetPlan{}, false
	}
	return m, s.planCatalogReset(r.Context(), m, up), true
}

// handlePreviewModelCatalogReset is the read-only before and after of a
// reset, shown when the model drawer opens.
func (s *Server) handlePreviewModelCatalogReset(w http.ResponseWriter, r *http.Request) {
	if _, p, ok := s.modelCatalogPlan(w, r); ok {
		WriteJSON(w, http.StatusOK, map[string]any{"plan": p})
	}
}

func (s *Server) handleResetModelToCatalog(w http.ResponseWriter, r *http.Request) {
	// No body fields: an old client's {"dry_run": true} is refused as an
	// unknown field rather than applied as a reset.
	if r.ContentLength != 0 {
		var body struct{}
		if err := decodeJSON(r, &body); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	m, p, ok := s.modelCatalogPlan(w, r)
	if !ok {
		return
	}
	if !p.Available {
		WriteError(w, r, ErrConflict(p.Message+"."))
		return
	}
	updated, err := s.resetModelToCatalog(r, m, p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.markCostQuoted(r, updated)
	WriteJSON(w, http.StatusOK, map[string]any{"plan": p, "applied": true, "model": updated})
}

// bulkCatalogResult is one model's outcome in a bulk request.
type bulkCatalogResult struct {
	ModelID string `json:"model_id"`
	Model   string `json:"model"`
	// Outcome: "reset" (prices reset, aligned), "skipped" (see Reason),
	// "not_found"; in a preview, "would_reset".
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	ChangedLines int    `json:"changed_lines"`
	// Plan is included in a preview.
	Plan *catalogResetPlan `json:"plan,omitempty"`
}

func (s *Server) handleBulkPreviewModelsCatalogReset(w http.ResponseWriter, r *http.Request) {
	s.bulkCatalogReset(w, r, false)
}

func (s *Server) handleBulkResetModelsToCatalog(w http.ResponseWriter, r *http.Request) {
	s.bulkCatalogReset(w, r, true)
}

// bulkCatalogReset previews (apply false: writes nothing) or applies a
// reset to the catalog for many models.
func (s *Server) bulkCatalogReset(w http.ResponseWriter, r *http.Request, apply bool) {
	var body struct {
		IDs []string `json:"ids"`
		// OnlyIfUnchanged turns "keep aligned" on only for models whose
		// prices stay exactly the same; the others are skipped with reason
		// "prices_would_change".
		OnlyIfUnchanged bool `json:"only_if_unchanged"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(body.IDs) == 0 {
		WriteError(w, r, ErrInvalidRequest("Name at least one model in ids.").WithParam("ids"))
		return
	}
	if len(body.IDs) > maxBulkCatalogReset {
		WriteError(w, r, ErrInvalidRequest(fmt.Sprintf("At most %d models per request.", maxBulkCatalogReset)).WithParam("ids"))
		return
	}
	ups, err := s.Store.ListUpstreams(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byID := make(map[string]*store.Upstream, len(ups))
	for _, u := range ups {
		byID[u.ID] = u
	}
	results := []bulkCatalogResult{}
	counts := map[string]int{}
	seen := map[string]bool{}
	changed := false
	for _, id := range body.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m, err := s.Store.ModelByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			results = append(results, bulkCatalogResult{ModelID: id, Outcome: "not_found"})
			counts["not_found"]++
			continue
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := s.planCatalogReset(r.Context(), m, byID[m.UpstreamID])
		res := bulkCatalogResult{ModelID: m.ID, Model: m.Name, ChangedLines: p.ChangedLines}
		switch {
		case !p.Available:
			res.Outcome, res.Reason, res.Message = "skipped", p.Reason, p.Message
		case body.OnlyIfUnchanged && p.ChangedLines > 0:
			res.Outcome, res.Reason, res.Message = "skipped", "prices_would_change", "Its prices would change; reset it to the Janus price catalog instead"
		case !apply:
			res.Outcome = "would_reset"
		default:
			if _, err := s.resetModelToCatalog(r, m, p); err != nil {
				WriteError(w, r, err)
				return
			}
			res.Outcome = "reset"
			changed = true
		}
		if !apply {
			plan := p
			res.Plan = &plan
		}
		counts[res.Outcome]++
		results = append(results, res)
	}
	if changed {
		s.InvalidateConfigCache()
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": results, "counts": counts, "applied": apply})
}
