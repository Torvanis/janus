package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var metadataFields = []string{"context_window", "rate_in_nanousd", "rate_out_nanousd", "rate_cached_nanousd", "rate_cache_write_5m_nanousd", "rate_cache_write_1h_nanousd"}

// MetadataField distinguishes unknown from an explicit free price. All prices
// use the same nano-USD per million tokens as the legacy model columns.
type MetadataField struct {
	Value           *int64 `json:"value"`
	Source          string `json:"source"`
	Override        *int64 `json:"override"`
	AutomaticValue  *int64 `json:"automatic_value"`
	AutomaticSource string `json:"automatic_source"`
}
type AutomaticMetadata struct {
	Upstream  map[string]int64 `json:"upstream,omitempty"`
	Reference map[string]int64 `json:"reference,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
}
type metadataState struct {
	AutomaticMetadata
	Overrides map[string]*int64 `json:"overrides,omitempty"`
}

func (m *Model) readMetadata(raw string) error {
	if err := json.Unmarshal([]byte(raw), &m.metadataState); err != nil {
		return fmt.Errorf("read model metadata: %w", err)
	}
	m.resolveMetadata()
	return nil
}
func (m *Model) resolveMetadata() {
	m.Metadata = map[string]MetadataField{}
	m.MetadataWarnings = append([]string{}, m.metadataState.Warnings...)
	for _, key := range metadataFields {
		f := MetadataField{Source: "unknown", AutomaticSource: "unknown"}
		if v, ok := m.metadataState.Reference[key]; ok {
			f.AutomaticValue = &v
			f.AutomaticSource = "reference"
		}
		if v, ok := m.metadataState.Upstream[key]; ok {
			f.AutomaticValue = &v
			f.AutomaticSource = "upstream"
		}
		f.Value = f.AutomaticValue
		f.Source = f.AutomaticSource
		if v := m.metadataState.Overrides[key]; v != nil {
			f.Override = v
			f.Value = v
			f.Source = "admin"
		}
		m.Metadata[key] = f
	}
}

type ModelPatch struct {
	DisplayName   *string
	Status        *string
	Overrides     map[string]*int64
	EffectiveFrom time.Time
}

func (s *Store) PatchModel(ctx context.Context, id string, patch ModelPatch) error {
	return s.updateModelMetadata(ctx, id, patch, nil)
}
func (s *Store) RefreshModelMetadata(ctx context.Context, id string, automatic AutomaticMetadata) error {
	return s.updateModelMetadata(ctx, id, ModelPatch{}, &automatic)
}
func (s *Store) updateModelMetadata(ctx context.Context, id string, patch ModelPatch, automatic *AutomaticMetadata) error {
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
			if _, ok := before[k]; !ok {
				return &ValidationError{Field: k, Message: "unsupported metadata field"}
			}
			if v != nil && *v < 0 {
				return &ValidationError{Field: k, Message: "must be zero or positive"}
			}
			if v == nil {
				delete(m.metadataState.Overrides, k)
			} else {
				m.metadataState.Overrides[k] = v
			}
		}
		if automatic != nil {
			m.metadataState.AutomaticMetadata = *automatic
		}
		m.resolveMetadata()
		rateChanged := false
		rateKnown := false
		for _, key := range metadataFields[1:] {
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
			if *patch.Status == ModelEnabled && effective.IsZero() {
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
			return tx.exec(ctx, `INSERT INTO rate_card_version (id,model_id,rate_in_nanousd,rate_out_nanousd,rate_cached_nanousd,rate_cache_write_5m_nanousd,rate_cache_write_1h_nanousd,effective_from,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, NewID(), id, in, out, cached, w5, w1, FormatTime(at), FormatTime(nowUTC()))
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
		return tx.RefreshModelMetadata(ctx, id, automatic)
	})
	return created, err
}

// ApplyReferenceMetadata retains the explicit seed endpoint's semantics while
// checking eligibility under the write lock. A concurrent admin edit wins.
func (s *Store) ApplyReferenceMetadata(ctx context.Context, id string, tokens int64, rc RateCard) (priced, contextSet bool, err error) {
	err = s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `UPDATE model SET id=id WHERE id=?`, id); err != nil {
			return err
		}
		m, err := tx.ModelByID(ctx, id)
		if err != nil {
			return err
		}
		p := ModelPatch{EffectiveFrom: rc.EffectiveFrom, Overrides: map[string]*int64{}}
		contextSet = tokens > 0 && m.ContextWindow == 0 && m.Metadata["context_window"].Override == nil
		if contextSet {
			p.Overrides["context_window"] = &tokens
		}
		priced = m.RateEffectiveFrom.IsZero()
		if priced {
			p.Overrides["rate_in_nanousd"] = &rc.RateInNano
			p.Overrides["rate_out_nanousd"] = &rc.RateOutNano
			p.Overrides["rate_cached_nanousd"] = &rc.RateCachedNano
			p.Overrides["rate_cache_write_5m_nanousd"] = &rc.RateCacheWrite5mNano
			p.Overrides["rate_cache_write_1h_nanousd"] = &rc.RateCacheWrite1hNano
		}
		if len(p.Overrides) == 0 {
			return nil
		}
		return tx.PatchModel(ctx, id, p)
	})
	return
}
