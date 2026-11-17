package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/torvanis/janus/internal/usage"
)

var metadataFields = []string{"context_window", "rate_in_nanousd", "rate_out_nanousd", "rate_cached_nanousd", "rate_cache_write_5m_nanousd", "rate_cache_write_1h_nanousd"}

// MetadataField distinguishes unknown from an explicit free price. All prices
// use the same nano-USD per million tokens as the legacy model columns.
//
// Source is the layer the value comes from: "admin" (an administrator's
// override), "upstream" (the provider's API, read at discovery),
// "reference" (the price catalog) or "unknown". SourceDetail says more for
// the value in force: the catalog version, whether it was the model's own
// entry or the default for its type, and the page the price was read from.
type MetadataField struct {
	Value           *int64        `json:"value"`
	Source          string        `json:"source"`
	SourceDetail    *SourceDetail `json:"source_detail,omitempty"`
	Override        *int64        `json:"override"`
	AutomaticValue  *int64        `json:"automatic_value"`
	AutomaticSource string        `json:"automatic_source"`
	// AutomaticDetail describes AutomaticValue (what "Use automatic"
	// returns to).
	AutomaticDetail *SourceDetail `json:"automatic_detail,omitempty"`
}

// SourceDetail is a human label for a value's source plus, for catalog
// values, where the price was read.
type SourceDetail struct {
	// Label is display text: "Admin override", "Provider API",
	// "Janus price catalog 2026.11.16.1", "Janus price catalog
	// 2026.11.16.1 (default for tts)".
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"` // catalog: "model" or "default"
	Type  string `json:"type,omitempty"` // catalog default: the model type
	URL   string `json:"url,omitempty"`
	// ReadAt is the day the price was read: from the catalog's source page,
	// or from the provider's API at the last discovery.
	ReadAt string `json:"read_at,omitempty"`
	// SetAt is the day an administrator last set the price (UTC,
	// YYYY-MM-DD); absent for prices set before it was recorded.
	SetAt string `json:"set_at,omitempty"`
}

// ReferenceSource records which catalog gave a reference value
// (pricecatalog.LineSource, stored with the value).
type ReferenceSource struct {
	Catalog string `json:"catalog"`
	Kind    string `json:"kind"`
	Type    string `json:"type,omitempty"`
	URL     string `json:"url,omitempty"`
	ReadAt  string `json:"read_at,omitempty"`
}

type AutomaticMetadata struct {
	Upstream  map[string]int64 `json:"upstream,omitempty"`
	Reference map[string]int64 `json:"reference,omitempty"`
	// ReferenceSources says, per Reference key, which price catalog gave
	// it. Values written before the catalog existed have none.
	ReferenceSources map[string]ReferenceSource `json:"reference_sources,omitempty"`
	Warnings         []string                   `json:"warnings,omitempty"`
	// UpstreamReadAt is the day the provider's API prices were last read
	// (UTC, YYYY-MM-DD), stamped by RefreshModelMetadata.
	UpstreamReadAt string `json:"upstream_read_at,omitempty"`
}
type metadataState struct {
	AutomaticMetadata
	Overrides map[string]*int64 `json:"overrides,omitempty"`
	// OverridesSetAt is the day each override was last given a new value.
	OverridesSetAt map[string]string `json:"overrides_set_at,omitempty"`
	// CatalogAligned: the model keeps no administrator prices and its
	// prices follow the Janus price catalog. Nil = not decided yet (a model
	// from before the setting existed); DecideCatalogAlignment settles it
	// without changing a price.
	CatalogAligned *bool `json:"catalog_aligned,omitempty"`
}

// metadataDay is how source dates are written: a UTC calendar day.
func metadataDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (m *Model) readMetadata(raw string) error {
	if err := json.Unmarshal([]byte(raw), &m.metadataState); err != nil {
		return fmt.Errorf("read model metadata: %w", err)
	}
	m.resolveMetadata()
	return nil
}

// metadataKeys is every key the model has a value for: the fixed fields,
// and any price line (usage.ParseLine) one of the three sources carries.
func (m *Model) metadataKeys() []string {
	keys := append([]string{}, metadataFields...)
	seen := map[string]bool{}
	for _, k := range keys {
		seen[k] = true
	}
	var lines []string
	add := func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		if _, err := usage.ParseLine(k); err == nil {
			lines = append(lines, k)
		}
	}
	for k := range m.metadataState.Upstream {
		add(k)
	}
	for k := range m.metadataState.Reference {
		add(k)
	}
	for k, v := range m.metadataState.Overrides {
		if v != nil {
			add(k)
		}
	}
	sort.Strings(lines)
	return append(keys, lines...)
}

// PriceLines is the model's resolved price lines (everything it is priced
// by beyond the five token rates), nil when it has none.
func (m *Model) PriceLines() map[string]int64 {
	var out map[string]int64
	for k, f := range m.Metadata {
		if f.Value == nil || isFixedMetadataField(k) {
			continue
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[k] = *f.Value
	}
	return out
}

func isFixedMetadataField(key string) bool {
	for _, k := range metadataFields {
		if k == key {
			return true
		}
	}
	return false
}

func (m *Model) resolveMetadata() {
	m.CatalogAligned = m.metadataState.CatalogAligned != nil && *m.metadataState.CatalogAligned
	m.Metadata = map[string]MetadataField{}
	m.MetadataWarnings = append([]string{}, m.metadataState.Warnings...)
	for _, key := range m.metadataKeys() {
		f := MetadataField{Source: "unknown", AutomaticSource: "unknown"}
		if v, ok := m.metadataState.Reference[key]; ok {
			f.AutomaticValue = &v
			f.AutomaticSource = "reference"
			f.AutomaticDetail = referenceDetail(m.metadataState.ReferenceSources[key])
		}
		if v, ok := m.metadataState.Upstream[key]; ok {
			f.AutomaticValue = &v
			f.AutomaticSource = "upstream"
			f.AutomaticDetail = &SourceDetail{Label: "Provider API", ReadAt: m.metadataState.UpstreamReadAt}
		}
		f.Value = f.AutomaticValue
		f.Source = f.AutomaticSource
		f.SourceDetail = f.AutomaticDetail
		if v := m.metadataState.Overrides[key]; v != nil {
			f.Override = v
			f.Value = v
			f.Source = "admin"
			f.SourceDetail = &SourceDetail{Label: "Admin override", SetAt: m.metadataState.OverridesSetAt[key]}
		}
		m.Metadata[key] = f
	}
}

// referenceDetail labels a reference value: the catalog that gave it, or
// the bundled list that came before the catalog.
func referenceDetail(src ReferenceSource) *SourceDetail {
	if src.Catalog == "" {
		return &SourceDetail{Label: "Bundled reference list"}
	}
	d := &SourceDetail{Label: src.Catalog, Kind: src.Kind, Type: src.Type, URL: src.URL, ReadAt: src.ReadAt}
	if src.Kind == "default" {
		d.Label += " (default for " + src.Type + ")"
	}
	return d
}

type ModelPatch struct {
	DisplayName   *string
	Status        *string
	Overrides     map[string]*int64
	EffectiveFrom time.Time
	// AllowUnpriced lets a never-priced model be enabled. Set only in
	// local-only mode (JANUS_LOCAL_ONLY), where cost tracking is off and the
	// UI hides every rate field, so requiring a rate card would leave the
	// administrator no way to enable a model.
	AllowUnpriced bool
	// CatalogAligned sets the "keep aligned to the Janus price catalog"
	// switch. Turning it on is ResetModelPricesToCatalog; a patch that
	// sets a price turns it off by itself.
	CatalogAligned *bool
}

func (s *Store) PatchModel(ctx context.Context, id string, patch ModelPatch) error {
	return s.updateModelMetadata(ctx, id, patch, nil)
}
func (s *Store) RefreshModelMetadata(ctx context.Context, id string, automatic AutomaticMetadata) error {
	if len(automatic.Upstream) > 0 && automatic.UpstreamReadAt == "" {
		automatic.UpstreamReadAt = metadataDay(nowUTC())
	}
	return s.updateModelMetadata(ctx, id, ModelPatch{}, func(st *metadataState) { st.AutomaticMetadata = automatic })
}

// RefreshModelReference replaces only the model's price-catalog layer (a
// catalog update), leaving administrator overrides, the provider's API
// prices and warnings exactly as they are. It returns how many catalog
// values were added, changed or removed.
func (s *Store) RefreshModelReference(ctx context.Context, id string, reference map[string]int64, sources map[string]ReferenceSource) (int, error) {
	changed := 0
	err := s.updateModelMetadata(ctx, id, ModelPatch{}, func(st *metadataState) {
		changed = countChanged(st.Reference, reference)
		st.Reference = reference
		st.ReferenceSources = sources
	})
	return changed, err
}

func countChanged(before, after map[string]int64) int {
	n := 0
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			n++
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			n++
		}
	}
	return n
}

func (s *Store) updateModelMetadata(ctx context.Context, id string, patch ModelPatch, automatic func(*metadataState)) error {
	return s.modelTx(ctx, func(tx *Store) error {
		// Acquire the row's write lock before reading overrides, on both engines.
		if err := tx.exec(ctx, `UPDATE model SET id=id WHERE id=?`, id); err != nil {
			return err
		}
		m, err := tx.ModelByID(ctx, id)
		if err != nil {
			return err
		}
		before := m.Metadata
		if m.metadataState.Overrides == nil {
			m.metadataState.Overrides = map[string]*int64{}
		}
		for k, v := range patch.Overrides {
			if !isFixedMetadataField(k) {
				// Anything else must be a well-formed price line.
				if _, err := usage.ParseLine(k); err != nil {
					return &ValidationError{Field: k, Message: "unsupported metadata field: " + err.Error()}
				}
			}
			if v != nil && *v < 0 {
				return &ValidationError{Field: k, Message: "must be zero or positive"}
			}
			if v == nil {
				delete(m.metadataState.Overrides, k)
				delete(m.metadataState.OverridesSetAt, k)
				continue
			}
			if old := m.metadataState.Overrides[k]; old == nil || *old != *v {
				if m.metadataState.OverridesSetAt == nil {
					m.metadataState.OverridesSetAt = map[string]string{}
				}
				m.metadataState.OverridesSetAt[k] = metadataDay(nowUTC())
			}
			m.metadataState.Overrides[k] = v
		}
		// An administrator's price is not the catalog's: setting one stops
		// the model following the catalog.
		for k, v := range patch.Overrides {
			if v != nil && k != "context_window" {
				off := false
				m.metadataState.CatalogAligned = &off
				break
			}
		}
		if patch.CatalogAligned != nil {
			v := *patch.CatalogAligned
			m.metadataState.CatalogAligned = &v
			if v && m.metadataState.hasAdminPrices() {
				return &ValidationError{Field: "catalog_aligned", Message: "A model with prices set by an administrator follows the Janus price catalog only after a reset to it"}
			}
		}
		if automatic != nil {
			automatic(&m.metadataState)
		}
		m.resolveMetadata()
		rateChanged := false
		rateKnown := false
		rateKeys := map[string]bool{}
		for key := range before {
			rateKeys[key] = true
		}
		for key := range m.Metadata {
			rateKeys[key] = true
		}
		delete(rateKeys, "context_window")
		for key := range rateKeys {
			a, b := before[key], m.Metadata[key]
			if !equalMetadataValue(a.Value, b.Value) {
				rateChanged = true
			}
			if b.Value != nil {
				rateKnown = true
			}
			if _, ok := patch.Overrides[key]; ok {
				rateChanged = true
			}
		}
		effective := m.RateEffectiveFrom
		if rateChanged {
			effective = patch.EffectiveFrom
			if effective.IsZero() {
				effective = nowUTC()
			}
		}
		// Preserve the historical PATCH contract: saving any dimension is a card;
		// if all dimensions become unknown it is no longer a priced model.
		if !rateKnown {
			effective = time.Time{}
		}
		if patch.Status != nil {
			switch *patch.Status {
			case ModelEnabled, ModelDisabled, ModelPending:
			default:
				return &ValidationError{Field: "status", Message: "Status must be enabled, disabled, or pending_approval"}
			}
			if *patch.Status == ModelEnabled && effective.IsZero() && !patch.AllowUnpriced {
				return &ValidationError{Field: "status", Message: "Save a rate card first — $0 is allowed for self-hosted models"}
			}
		}
		if patch.DisplayName != nil {
			if err := tx.SetModelDisplayName(ctx, id, *patch.DisplayName); err != nil {
				return err
			}
		}
		if patch.Status != nil {
			if err := tx.SetModelStatus(ctx, id, *patch.Status); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(m.metadataState)
		if err != nil {
			return err
		}
		value := func(k string) int64 {
			if v := m.Metadata[k].Value; v != nil {
				return *v
			}
			return 0
		}
		in, out, cached, w5, w1 := value("rate_in_nanousd"), value("rate_out_nanousd"), value("rate_cached_nanousd"), value("rate_cache_write_5m_nanousd"), value("rate_cache_write_1h_nanousd")
		stamp := ""
		if !effective.IsZero() {
			stamp = FormatTime(effective)
		}
		if err := tx.exec(ctx, `UPDATE model SET metadata_json=?,context_window=?,rate_in_nanousd=?,rate_out_nanousd=?,rate_cached_nanousd=?,rate_cache_write_5m_nanousd=?,rate_cache_write_1h_nanousd=?,rate_effective_from=? WHERE id=?`, string(raw), value("context_window"), in, out, cached, w5, w1, stamp, id); err != nil {
			return err
		}
		if rateChanged {
			at := effective
			if at.IsZero() {
				at = nowUTC()
			}
			lines := "{}"
			if l := m.PriceLines(); len(l) > 0 {
				b, err := json.Marshal(l)
				if err != nil {
					return err
				}
				lines = string(b)
			}
			return tx.exec(ctx, `INSERT INTO rate_card_version (id,model_id,rate_in_nanousd,rate_out_nanousd,rate_cached_nanousd,rate_cache_write_5m_nanousd,rate_cache_write_1h_nanousd,lines_json,effective_from,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, NewID(), id, in, out, cached, w5, w1, lines, FormatTime(at), FormatTime(nowUTC()))
		}
		return nil
	})
}
func equalMetadataValue(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// UpsertDiscoveredMetadata atomically merges curation, discovery and resolved
// billing metadata; existing overrides are read under the same write lock.
func (s *Store) UpsertDiscoveredMetadata(ctx context.Context, upstreamID, name string, modalities []string, automatic AutomaticMetadata) (bool, error) {
	created := false
	err := s.modelTx(ctx, func(tx *Store) error {
		var err error
		created, err = tx.UpsertDiscoveredModel(ctx, upstreamID, name, modalities)
		if err != nil {
			return err
		}
		var id string
		if err := tx.queryRow(ctx, `SELECT id FROM model WHERE upstream_id=? AND name=?`, upstreamID, name).Scan(&id); err != nil {
			return err
		}
		if len(automatic.Upstream) > 0 && automatic.UpstreamReadAt == "" {
			automatic.UpstreamReadAt = metadataDay(nowUTC())
		}
		// A model Janus has just met has no administrator prices: it
		// follows the price catalog from the start.
		return tx.updateModelMetadata(ctx, id, ModelPatch{}, func(st *metadataState) {
			st.AutomaticMetadata = automatic
			if created {
				on := true
				st.CatalogAligned = &on
			}
		})
	})
	return created, err
}

// priceLinesMigration lets a rate card version carry the price lines in
// force with it, so a past request is priced by the lines of its day.
var priceLinesMigration = migration{name: "0079_rate_card_lines", stmt: []string{
	`ALTER TABLE rate_card_version ADD COLUMN lines_json TEXT NOT NULL DEFAULT '{}'`,
}, down: []string{
	`ALTER TABLE rate_card_version DROP COLUMN lines_json`,
}}

// measuredUsageMigration records what a request billed by a measured unit
// used (images, seconds of video, characters) and the line that priced it.
var measuredUsageMigration = migration{name: "0080_usage_measured_units", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN billed_unit TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE usage_event ADD COLUMN billed_quantity DOUBLE PRECISION NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_event ADD COLUMN billed_line TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE usage_event DROP COLUMN billed_line`,
	`ALTER TABLE usage_event DROP COLUMN billed_quantity`,
	`ALTER TABLE usage_event DROP COLUMN billed_unit`,
}}
