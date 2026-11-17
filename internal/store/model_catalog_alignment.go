package store

import (
	"context"
	"encoding/json"
	"maps"
	"sort"
)

// Keeping a model aligned to the Janus price catalog (internal/pricecatalog):
// an aligned model has no administrator prices, so what it bills is the
// provider's API price where the provider publishes one, else the catalog's.
// A catalog update rewrites the catalog layer of every model
// (RefreshModelReference); on an aligned model nothing sits above it.

// hasAdminPrices reports whether an administrator set any price (the five
// token rates, which older releases copied from the old rate cards as
// administrator values, included). The context window is not a price.
func (st *metadataState) hasAdminPrices() bool {
	for k, v := range st.Overrides {
		if v != nil && k != "context_window" {
			return true
		}
	}
	return false
}

// HasAdminPrices reports whether an administrator set any of the model's
// prices.
func (m *Model) HasAdminPrices() bool { return m.metadataState.hasAdminPrices() }

// CatalogAlignmentDecided is false for a model from before the "keep
// aligned" setting existed, until DecideCatalogAlignment settles it.
func (m *Model) CatalogAlignmentDecided() bool { return m.metadataState.CatalogAligned != nil }

// PriceChange is one price line before and after a reset to the catalog.
// A nil value is "not priced".
type PriceChange struct {
	Line         string        `json:"line"`
	Before       *int64        `json:"before"`
	BeforeSource string        `json:"before_source"`
	After        *int64        `json:"after"`
	AfterSource  string        `json:"after_source"`
	AfterDetail  *SourceDetail `json:"after_detail,omitempty"`
	Changed      bool          `json:"changed"`
}

// withCatalogPrices is the model as a reset to the catalog leaves it: no
// administrator prices (the context window keeps its override), the given
// catalog layer, aligned.
func (m *Model) withCatalogPrices(reference map[string]int64, sources map[string]ReferenceSource) *Model {
	st := m.metadataState
	st.Overrides = map[string]*int64{}
	st.OverridesSetAt = map[string]string{}
	if v := m.metadataState.Overrides["context_window"]; v != nil {
		st.Overrides["context_window"] = v
		if at, ok := m.metadataState.OverridesSetAt["context_window"]; ok {
			st.OverridesSetAt["context_window"] = at
		}
	}
	st.Reference = maps.Clone(reference)
	st.ReferenceSources = maps.Clone(sources)
	on := true
	st.CatalogAligned = &on
	out := &Model{metadataState: st}
	out.resolveMetadata()
	return out
}

// CatalogResetPreview lists every price line the model has now or would
// have after a reset to the given catalog values, in line order.
func (m *Model) CatalogResetPreview(reference map[string]int64, sources map[string]ReferenceSource) []PriceChange {
	after := m.withCatalogPrices(reference, sources)
	keys := map[string]bool{}
	for k := range m.Metadata {
		keys[k] = true
	}
	for k := range after.Metadata {
		keys[k] = true
	}
	delete(keys, "context_window")
	out := []PriceChange{}
	for k := range keys {
		b, a := m.Metadata[k], after.Metadata[k]
		if b.Value == nil && a.Value == nil {
			continue
		}
		c := PriceChange{Line: k, Before: b.Value, BeforeSource: b.Source, After: a.Value, AfterSource: a.Source, AfterDetail: a.SourceDetail,
			Changed: !equalMetadataValue(b.Value, a.Value)}
		if c.BeforeSource == "" {
			c.BeforeSource = "unknown"
		}
		if c.AfterSource == "" {
			c.AfterSource = "unknown"
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// ResetModelPricesToCatalog clears every administrator price of the model
// (the five legacy token rates included), installs the given catalog
// layer and keeps the model aligned to the catalog. A price that changes
// appends a price history entry, as any price change does.
func (s *Store) ResetModelPricesToCatalog(ctx context.Context, id string, reference map[string]int64, sources map[string]ReferenceSource) error {
	m, err := s.ModelByID(ctx, id)
	if err != nil {
		return err
	}
	cleared := map[string]*int64{}
	for k := range m.metadataState.Overrides {
		if k != "context_window" {
			cleared[k] = nil
		}
	}
	on := true
	return s.updateModelMetadata(ctx, id, ModelPatch{Overrides: cleared, CatalogAligned: &on}, func(st *metadataState) {
		st.Reference = reference
		st.ReferenceSources = sources
	})
}

// DecideCatalogAlignment settles "keep aligned" for a model from before the
// setting existed, without changing anything billed: the model is aligned
// only when no administrator set any of its prices AND the prices it bills
// now (its rate card in force) are exactly what a reset to the given
// catalog values would give. Otherwise it keeps its prices and is not
// aligned. Only the setting is written. decided is false when the model
// already had one.
func (s *Store) DecideCatalogAlignment(ctx context.Context, id string, reference map[string]int64, sources map[string]ReferenceSource) (decided, aligned bool, err error) {
	err = s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `UPDATE model SET id=id WHERE id=?`, id); err != nil {
			return err
		}
		m, err := tx.ModelByID(ctx, id)
		if err != nil {
			return err
		}
		if m.metadataState.CatalogAligned != nil {
			aligned = *m.metadataState.CatalogAligned
			return nil
		}
		decided = true
		if !m.metadataState.hasAdminPrices() {
			billed, err := tx.RateCardAt(ctx, id, nowUTC())
			if err != nil {
				return err
			}
			aligned = sameBilledPrices(billed, m.withCatalogPrices(reference, sources))
		}
		m.metadataState.CatalogAligned = &aligned
		raw, err := json.Marshal(m.metadataState)
		if err != nil {
			return err
		}
		return tx.exec(ctx, `UPDATE model SET metadata_json=? WHERE id=?`, string(raw), id)
	})
	return decided, aligned, err
}

// sameBilledPrices reports whether a model would bill exactly what the rate
// card says: the same five token rates and price lines, and priced or not
// alike.
func sameBilledPrices(billed *RateCard, m *Model) bool {
	value := func(k string) int64 {
		if v := m.Metadata[k].Value; v != nil {
			return *v
		}
		return 0
	}
	priced := false
	for k, f := range m.Metadata {
		if k != "context_window" && f.Value != nil {
			priced = true
		}
	}
	if billed == nil || billed.EffectiveFrom.IsZero() {
		return !priced
	}
	if !priced {
		return false
	}
	if billed.RateInNano != value("rate_in_nanousd") || billed.RateOutNano != value("rate_out_nanousd") ||
		billed.RateCachedNano != value("rate_cached_nanousd") || billed.RateCacheWrite5mNano != value("rate_cache_write_5m_nanousd") ||
		billed.RateCacheWrite1hNano != value("rate_cache_write_1h_nanousd") {
		return false
	}
	lines := m.PriceLines()
	if len(lines) != len(billed.Lines) {
		return false
	}
	for k, v := range lines {
		if w, ok := billed.Lines[k]; !ok || w != v {
			return false
		}
	}
	return true
}
