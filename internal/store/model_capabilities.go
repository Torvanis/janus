package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/torvanis/janus/internal/capability"
)

// modelCapabilitiesMigration adds what Janus knows about each model's
// capabilities, by source (see internal/capability).
var modelCapabilitiesMigration = migration{name: "0078_model_capabilities", stmt: []string{
	`ALTER TABLE model ADD COLUMN capabilities_json TEXT NOT NULL DEFAULT '{}'`,
}, down: []string{
	`ALTER TABLE model DROP COLUMN capabilities_json`,
}}

func (m *Model) readCapabilities(raw string) error {
	state, err := capability.Parse(raw)
	if err != nil {
		return err
	}
	m.setCapabilityState(state)
	return nil
}

func (m *Model) setCapabilityState(state capability.State) {
	state.Settle(time.Now().UTC())
	m.capabilityState = state
	m.Capabilities = state.Resolve()
	m.CapabilityProbe = state.Probed
}

// CapabilityState is everything known about the model's capabilities, by
// source.
func (m *Model) CapabilityState() capability.State { return m.capabilityState }

// CapabilityProfile is the capabilities in force, for shaping requests.
func (m *Model) CapabilityProfile() capability.Profile { return m.capabilityState.Profile() }

// UpdateModelCapabilities changes a model's capability state under the
// row's write lock: change receives the stored state and edits it. It
// returns the model as stored afterwards. change returning false leaves the
// row untouched.
func (s *Store) UpdateModelCapabilities(ctx context.Context, id string, change func(*capability.State) (bool, error)) (*Model, error) {
	var out *Model
	err := s.modelTx(ctx, func(tx *Store) error {
		// Take the write lock before reading, on both engines.
		if err := tx.exec(ctx, `UPDATE model SET id=id WHERE id=?`, id); err != nil {
			return err
		}
		m, err := tx.ModelByID(ctx, id)
		if err != nil {
			return err
		}
		state := m.capabilityState
		changed, err := change(&state)
		if err != nil {
			return err
		}
		if changed {
			if err := tx.exec(ctx, `UPDATE model SET capabilities_json=? WHERE id=?`, state.String(), id); err != nil {
				return err
			}
			m.setCapabilityState(state)
		}
		out = m
		return nil
	})
	return out, err
}

// SetDiscoveredCapabilities records what the provider's model list said
// about one model. What a probe measured and what an administrator set are
// left as they are.
func (s *Store) SetDiscoveredCapabilities(ctx context.Context, upstreamID, name string, facts capability.Facts) error {
	var id string
	if err := s.queryRow(ctx, `SELECT id FROM model WHERE upstream_id=? AND name=?`, upstreamID, name).Scan(&id); err != nil {
		return err
	}
	_, err := s.UpdateModelCapabilities(ctx, id, func(state *capability.State) (bool, error) {
		return state.SetProvider(facts), nil
	})
	return err
}

// SettingCapabilityProbe is the system_setting key that holds whether a
// model is measured when it is enabled.
const SettingCapabilityProbe = "capability_probe"

// CapabilityProbeSetting says when Janus measures a model's capabilities on
// its own. The manual button works either way.
type CapabilityProbeSetting struct {
	// OnEnable: measure a chat model when an administrator enables it.
	OnEnable bool `json:"on_enable"`
}

// CapabilityProbeSetting returns the stored setting; measuring on enable is
// on until an administrator turns it off.
func (s *Store) CapabilityProbeSetting(ctx context.Context) (CapabilityProbeSetting, error) {
	raw, _, err := s.systemSetting(ctx, SettingCapabilityProbe)
	if errors.Is(err, ErrNotFound) {
		return CapabilityProbeSetting{OnEnable: true}, nil
	}
	if err != nil {
		return CapabilityProbeSetting{}, err
	}
	out := CapabilityProbeSetting{OnEnable: true}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return CapabilityProbeSetting{}, fmt.Errorf("decode %s setting: %w", SettingCapabilityProbe, err)
	}
	return out, nil
}

// SetCapabilityProbeSetting stores the setting.
func (s *Store) SetCapabilityProbeSetting(ctx context.Context, v CapabilityProbeSetting) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingCapabilityProbe, string(raw))
}
