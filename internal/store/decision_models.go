package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Decision models (migration 0050).
//
// A catalog model with classifier_role "decision" answers typed questions
// instead of generating text. Janus asks them itself (guards, Usage
// insights, Auto-routing) and, since this migration, serves them to callers
// on /v1/decisions (OpenAI dialect), /v1/systemone and /api/alpha/decisions
// (System One dialect).
//
//   - classifier_profile.dialect: the wire format the provider speaks
//     ("" = detected from the base URL: OpenAI's API speaks "openai", every
//     other known server "systemone").
//   - decision_model_setting: how calls are priced (cost source and a
//     per-request fee on top of the rate card) and whether a model that
//     backs a blocking security check may still be granted to callers.

var decisionModelsMigration = migration{name: "0050_decision_models", stmt: []string{
	`ALTER TABLE classifier_profile ADD COLUMN dialect TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS decision_model_setting (
		model_id TEXT PRIMARY KEY,
		cost_source TEXT NOT NULL DEFAULT '',
		request_fee_nanousd INTEGER NOT NULL DEFAULT 0,
		grant_when_guarding INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	)`,
}, down: []string{
	`DROP TABLE IF EXISTS decision_model_setting`,
	`ALTER TABLE classifier_profile DROP COLUMN dialect`,
}}

// decisionStaleStatusMigration remembers a model's approval state when
// discovery marks it stale, so a decision model that drops out of a listing
// and comes back resumes as it was (enabled stays enabled) instead of
// landing in pending_approval. Chat models keep the old rule (see
// UpsertDiscoveredModel).
var decisionStaleStatusMigration = migration{name: "0052_model_status_before_stale", stmt: []string{
	`ALTER TABLE model ADD COLUMN status_before_stale TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE model DROP COLUMN status_before_stale`,
}}

// Decision dialects stored on a classifier profile.
const (
	DecisionDialectAuto      = ""
	DecisionDialectSystemOne = "systemone"
	DecisionDialectOpenAI    = "openai"
)

// Cost sources for decision calls.
const (
	// CostSourceAuto: the provider's reported charge when it sends one
	// (OpenRouter), else the rate card.
	CostSourceAuto = ""
	// CostSourceProvider: only the provider's reported charge; a call the
	// provider does not price costs nothing.
	CostSourceProvider = "provider"
	// CostSourceCard: always the rate card (plus the request fee), even
	// when the provider reports a charge.
	CostSourceCard = "card"
	// CostSourceNone: free (self-hosted with no chargeback).
	CostSourceNone = "none"
)

// DecisionSetting is how one decision model is priced and granted.
type DecisionSetting struct {
	ModelID    string `json:"model_id"`
	CostSource string `json:"cost_source"`
	// RequestFeeNano is charged per call on top of the token price
	// (per-request fees, or a flat chargeback for a self-hosted model).
	RequestFeeNano int64 `json:"request_fee_nanousd"`
	// GrantWhenGuarding lets callers be granted this model even while it
	// backs a blocking security check (they can then probe the judge).
	GrantWhenGuarding bool `json:"grant_when_guarding"`
}

// DecisionSettingFor returns the stored setting, or the defaults.
func (s *Store) DecisionSettingFor(ctx context.Context, modelID string) (DecisionSetting, error) {
	d := DecisionSetting{ModelID: modelID}
	var grant int
	err := s.queryRow(ctx, `SELECT cost_source, request_fee_nanousd, grant_when_guarding FROM decision_model_setting WHERE model_id = ?`, modelID).
		Scan(&d.CostSource, &d.RequestFeeNano, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("decision setting: %w", err)
	}
	d.GrantWhenGuarding = grant == 1
	return d, nil
}

// ListDecisionSettings returns every stored setting by model id.
func (s *Store) ListDecisionSettings(ctx context.Context) (map[string]DecisionSetting, error) {
	rows, err := s.query(ctx, `SELECT model_id, cost_source, request_fee_nanousd, grant_when_guarding FROM decision_model_setting`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]DecisionSetting{}
	for rows.Next() {
		var d DecisionSetting
		var grant int
		if err := rows.Scan(&d.ModelID, &d.CostSource, &d.RequestFeeNano, &grant); err != nil {
			return nil, err
		}
		d.GrantWhenGuarding = grant == 1
		out[d.ModelID] = d
	}
	return out, rows.Err()
}

// SetDecisionSetting stores a setting; the defaults delete the row.
func (s *Store) SetDecisionSetting(ctx context.Context, d DecisionSetting) error {
	switch d.CostSource {
	case CostSourceAuto, CostSourceProvider, CostSourceCard, CostSourceNone:
	default:
		return &ValidationError{Field: "cost_source", Message: "cost_source must be empty (automatic), provider, card or none"}
	}
	if d.RequestFeeNano < 0 || d.RequestFeeNano > 100_000_000_000 {
		return &ValidationError{Field: "request_fee_nanousd", Message: "the per-request fee must be between $0 and $100"}
	}
	m, err := s.ModelByID(ctx, d.ModelID)
	if err != nil {
		return err
	}
	if m.ClassifierRole != ClassifierRoleDecision {
		return &ValidationError{Field: "model_id", Message: m.PublicName() + " is not a decision model"}
	}
	if d.CostSource == CostSourceAuto && d.RequestFeeNano == 0 && !d.GrantWhenGuarding {
		return s.exec(ctx, `DELETE FROM decision_model_setting WHERE model_id = ?`, d.ModelID)
	}
	return s.exec(ctx, `INSERT INTO decision_model_setting (model_id, cost_source, request_fee_nanousd, grant_when_guarding, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT (model_id) DO UPDATE SET cost_source = excluded.cost_source, request_fee_nanousd = excluded.request_fee_nanousd,
		grant_when_guarding = excluded.grant_when_guarding, updated_at = excluded.updated_at`,
		d.ModelID, d.CostSource, d.RequestFeeNano, boolInt(d.GrantWhenGuarding), FormatTime(nowUTC()))
}

// DecisionModelGuarding names the enabled policies in which modelID is a
// blocking judge. Such a model is the judge of the callers those policies
// cover; granting it lets them test inputs against it.
//
// A check makes the model a blocking judge when it names the model and
// either blocks itself, or its kind is held at block by a mandatory policy
// bound at org scope: a floor keeps its block mode when a more specific
// policy layers on, but takes that policy's classifier (secgw.tighten), so a
// group policy naming the model in observe mode still makes it the judge
// that blocks the group.
func (s *Store) DecisionModelGuarding(ctx context.Context, modelID string) ([]string, error) {
	policies, err := s.ListSecgwPolicies(ctx)
	if err != nil {
		return nil, err
	}
	orgBound := map[string]bool{}
	rows, err := s.query(ctx, `SELECT DISTINCT policy_id FROM secgw_binding WHERE scope_type = ?`, SecgwScopeOrg)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		orgBound[id] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Check kinds a mandatory org floor blocks, and the floor's name.
	floorBlocks := map[SecgwCheckKind]string{}
	for _, p := range policies {
		if !p.Enabled || !p.Mandatory || !orgBound[p.ID] {
			continue
		}
		for _, c := range p.Checks {
			if c.Enabled && c.Mode == SecgwModeBlock {
				if _, ok := floorBlocks[c.Kind]; !ok {
					floorBlocks[c.Kind] = p.Name
				}
			}
		}
	}
	var names []string
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		for _, c := range p.Checks {
			if c.ClassifierModelID != modelID {
				continue
			}
			// A disabled check cannot switch a floor off, but its
			// classifier still replaces the floor's (tighten), so only the
			// floor case counts it.
			if c.Enabled && c.Mode == SecgwModeBlock {
				names = append(names, p.Name)
				break
			}
			if floor, ok := floorBlocks[c.Kind]; ok {
				names = append(names, p.Name+" (blocking through the mandatory policy "+floor+")")
				break
			}
		}
	}
	return names, nil
}

// DecisionGrantable reports whether callers may be granted a classifier
// model, and why not. The reason is written for administrators (it names
// the policies); callers get DecisionReservedReason instead. Decision models are grantable unless they back a
// blocking security check and the admin has not allowed it; every other
// classifier (encoders, generative guards) never is.
func (s *Store) DecisionGrantable(ctx context.Context, m *Model) (bool, string, error) {
	if m.ClassifierRole == "" {
		return true, "", nil
	}
	if m.ClassifierRole != ClassifierRoleDecision {
		return false, "this model is a security classifier and cannot be granted to callers", nil
	}
	guarding, err := s.DecisionModelGuarding(ctx, m.ID)
	if err != nil {
		return false, "", err
	}
	if len(guarding) == 0 {
		return true, "", nil
	}
	set, err := s.DecisionSettingFor(ctx, m.ID)
	if err != nil {
		return false, "", err
	}
	if set.GrantWhenGuarding {
		return true, "", nil
	}
	return false, m.PublicName() + " blocks requests in " + strings.Join(guarding, ", ") +
		"; callers granted it could test what gets past it. Allow it in the model's decision settings first.", nil
}

// DecisionReservedReason is what a caller is told when a decision model
// they were granted is reserved for security checks. It names no policy:
// which judge backs which policy is what the guard rule hides.
const DecisionReservedReason = "this model is reserved for security checks; ask an administrator"

// DecisionModelsByName resolves a caller's model name against enabled
// decision models (the same name tiers as ModelsByName). ErrNotFound when
// none matches.
func (s *Store) DecisionModelsByName(ctx context.Context, name string) ([]*Model, error) {
	models, err := s.ListModels(ctx, ModelFilter{Status: ModelEnabled, ClassifierOnly: true})
	if err != nil {
		return nil, err
	}
	var dec []*Model
	for _, m := range models {
		if m.ClassifierRole == ClassifierRoleDecision {
			dec = append(dec, m)
		}
	}
	return matchModelName(dec, name)
}
