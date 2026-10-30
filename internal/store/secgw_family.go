package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Family safety (migration 0058): the one built-in, non-editable Security
// Gateway policy every edition may switch on. It is deliberately NOT a row
// in secgw_policy: the ordinary policy API can never create, edit, bind or
// delete it, so its license exemption (it always blocks, Community
// included) is keyed on something no admin can forge, and it applies in
// addition to every Business policy instead of competing with them in
// resolution. See ~/audits/janus-chat/family-filter-design.md.

// SecgwFamilyPolicyID is the policy_id family violations carry.
const SecgwFamilyPolicyID = "family_safety"

// SecgwFamilyPolicyName is how the admin UI names it.
const SecgwFamilyPolicyName = "Child Safety"

// Family assignment scopes.
const (
	SecgwFamilyScopeUser = "user"
	SecgwFamilyScopeTeam = "team"
)

// SecgwActionSupported records a family-safety "support" outcome: the
// child's message was answered with a supportive reply instead of being
// forwarded, and the parent was notified.
const SecgwActionSupported = "supported"

// SecgwActionWithheld records Child Safety's tutor holding back a
// schoolwork answer: the reply ended with a tutor's note. It is the tutor
// working, not a security incident, so Chat shows it as a plain note.
const SecgwActionWithheld = "withheld"

// SecgwFamilyDefaultTimeoutMs bounds each family classifier call.
const SecgwFamilyDefaultTimeoutMs = 4000

// SecgwFamilySafetySlugs are the critical safety categories the family
// policy always asks, whatever the admin chooses: child exploitation,
// sexual content and crimes, violence, weapons, self-harm, hate, and
// drugs/criminal planning (non-violent crimes).
var SecgwFamilySafetySlugs = []string{
	"child_sexual_exploitation", "sexual_content", "sex_crimes", "violent_crimes",
	"indiscriminate_weapons", "self_harm", "hate", "nonviolent_crimes",
}

// SecgwFamilySupportSlugs are the categories answered with support (a kind
// reply pointing to a trusted adult and crisis lines, plus a parent
// notification) instead of a refusal, on the child's own message.
var SecgwFamilySupportSlugs = map[string]bool{"feeling_low": true, "self_harm": true}

// SecgwFamilyCategory returns the SHIPPED definition of a built-in
// category. The family policy reads these, never the editable rows, so
// editing or disabling a category on the Categories tab cannot change it.
func SecgwFamilyCategory(slug string) (*SecgwCategory, bool) {
	c, ok := builtinSecgwCategory(slug)
	if !ok {
		return nil, false
	}
	return &c, true
}

// SecgwFamilyConfig is the family policy's whole configuration.
type SecgwFamilyConfig struct {
	Enabled           bool   `json:"enabled"`
	ClassifierModelID string `json:"classifier_model_id"`
	// Profiles are the age ranges (Child Safety): built-ins first.
	Profiles    []*ChildProfile         `json:"profiles"`
	Ask         string                  `json:"ask"`
	TimeoutMs   int                     `json:"timeout_ms"`
	Assignments []SecgwFamilyAssignment `json:"assignments"`
	UpdatedBy   string                  `json:"updated_by_user_id"`
	UpdatedAt   time.Time               `json:"updated_at"`
}

// SecgwFamilyAssignment puts a user or a team under the family policy.
type SecgwFamilyAssignment struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	// ProfileID is the age range (Child Safety profile) it gets.
	ProfileID string `json:"profile_id"`
	// Name is resolved at read time.
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Profile returns a profile by id.
func (c *SecgwFamilyConfig) Profile(id string) *ChildProfile {
	for _, p := range c.Profiles {
		if p.ID == id {
			return p
		}
	}
	return nil
}

var secgwFamilyMigration = migration{name: "0058_family_safety", stmt: []string{
	`CREATE TABLE IF NOT EXISTS secgw_family (
		id TEXT PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0,
		classifier_model_id TEXT NOT NULL DEFAULT '',
		categories TEXT NOT NULL DEFAULT '[]',
		ask TEXT NOT NULL DEFAULT 'together',
		timeout_ms INTEGER NOT NULL DEFAULT 4000,
		updated_by TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS secgw_family_assignment (
		scope_type TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		created_by TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		PRIMARY KEY (scope_type, scope_id)
	)`,
	`INSERT INTO secgw_family (id, enabled, classifier_model_id, categories, ask, timeout_ms, updated_by, updated_at)
		VALUES ('family_safety', 0, '', '["homework_shortcut","not_for_children","bypass_controls","personal_info","feeling_low"]', 'together', 4000, '', '2026-10-19T00:00:00.000000000Z')
		ON CONFLICT (id) DO NOTHING`,
}, down: []string{
	`DROP TABLE IF EXISTS secgw_family_assignment`,
	`DROP TABLE IF EXISTS secgw_family`,
}}

// SecgwFamily loads the configuration with assignments (names resolved).
func (s *Store) SecgwFamily(ctx context.Context) (*SecgwFamilyConfig, error) {
	var c SecgwFamilyConfig
	var enabled int
	var updated string
	err := s.queryRow(ctx, `SELECT enabled, classifier_model_id, ask, timeout_ms, updated_by, updated_at FROM secgw_family WHERE id = ?`, SecgwFamilyPolicyID).
		Scan(&enabled, &c.ClassifierModelID, &c.Ask, &c.TimeoutMs, &c.UpdatedBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		c = SecgwFamilyConfig{Ask: SecgwAskTogether, TimeoutMs: SecgwFamilyDefaultTimeoutMs}
	} else if err != nil {
		return nil, fmt.Errorf("load family safety: %w", err)
	} else {
		c.Enabled = enabled == 1
		c.UpdatedAt = ParseTime(updated)
	}
	if c.Profiles, err = s.ChildProfiles(ctx); err != nil {
		return nil, err
	}
	if c.TimeoutMs <= 0 {
		c.TimeoutMs = SecgwFamilyDefaultTimeoutMs
	}
	rows, err := s.query(ctx, `SELECT scope_type, scope_id, profile_id, created_at FROM secgw_family_assignment ORDER BY scope_type, created_at`)
	if err != nil {
		return nil, fmt.Errorf("load family assignments: %w", err)
	}
	c.Assignments = []SecgwFamilyAssignment{}
	for rows.Next() {
		var a SecgwFamilyAssignment
		var created string
		if err := rows.Scan(&a.ScopeType, &a.ScopeID, &a.ProfileID, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		a.CreatedAt = ParseTime(created)
		c.Assignments = append(c.Assignments, a)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range c.Assignments {
		a := &c.Assignments[i]
		var name string
		switch a.ScopeType {
		case SecgwFamilyScopeUser:
			_ = s.queryRow(ctx, `SELECT CASE WHEN name <> '' THEN name ELSE email END FROM app_user WHERE id = ?`, a.ScopeID).Scan(&name)
		case SecgwFamilyScopeTeam:
			_ = s.queryRow(ctx, `SELECT name FROM team WHERE id = ?`, a.ScopeID).Scan(&name)
		}
		a.Name = name
	}
	return &c, nil
}

// SecgwFamilyInput is what the family endpoint accepts.
type SecgwFamilyInput struct {
	Enabled           bool   `json:"enabled"`
	ClassifierModelID string `json:"classifier_model_id"`
	Ask               string `json:"ask"`
	// Profiles, when set, replaces the age ranges (built-ins are kept;
	// custom ones not listed are removed). Nil leaves them unchanged.
	Profiles    []ChildProfile          `json:"profiles"`
	Assignments []SecgwFamilyAssignment `json:"assignments"`
}

// SetSecgwFamily validates and replaces the configuration and assignments
// in one transaction. The classifier's capability is checked by the
// caller (it owns the protocol rules); this checks shape and references.
func (s *Store) SetSecgwFamily(ctx context.Context, in SecgwFamilyInput, by string) error {
	switch in.Ask {
	case "":
		in.Ask = SecgwAskTogether
	case SecgwAskTogether, SecgwAskSeparately:
	default:
		return &ValidationError{Field: "ask", Message: "ask must be together or separately"}
	}
	if len(in.Assignments) > 500 {
		return &ValidationError{Field: "assignments", Message: "Assign at most 500 users and teams"}
	}
	assign := map[string]SecgwFamilyAssignment{}
	for _, a := range in.Assignments {
		a.ScopeID = strings.TrimSpace(a.ScopeID)
		if a.ScopeID == "" {
			return &ValidationError{Field: "assignments", Message: "Each assignment needs a user or team"}
		}
		var n int
		switch a.ScopeType {
		case SecgwFamilyScopeUser:
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM app_user WHERE id = ?`, a.ScopeID).Scan(&n); err != nil {
				return err
			}
		case SecgwFamilyScopeTeam:
			if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team WHERE id = ? AND archived_at = ''`, a.ScopeID).Scan(&n); err != nil {
				return err
			}
		default:
			return &ValidationError{Field: "assignments", Message: "scope_type must be user or team"}
		}
		if n == 0 {
			return &ValidationError{Field: "assignments", Message: "That " + a.ScopeType + " does not exist"}
		}
		if a.ProfileID == "" {
			a.ProfileID = ChildProfileKids
		}
		assign[a.ScopeType+"\x00"+a.ScopeID] = a
	}
	existing, err := s.ChildProfiles(ctx)
	if err != nil {
		return err
	}
	now := FormatTime(nowUTC())
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		profiles := map[string]bool{}
		for _, p := range existing {
			profiles[p.ID] = true
		}
		if in.Profiles != nil {
			kept, err := setChildProfiles(ctx, ts, in.Profiles, existing)
			if err != nil {
				return err
			}
			profiles = kept
		}
		for _, a := range assign {
			if !profiles[a.ProfileID] {
				return &ValidationError{Field: "assignments", Message: "That age range does not exist"}
			}
		}
		if err := ts.exec(ctx, `INSERT INTO secgw_family (id, enabled, classifier_model_id, ask, timeout_ms, updated_by, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled, classifier_model_id = excluded.classifier_model_id,
			ask = excluded.ask, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			SecgwFamilyPolicyID, boolInt(in.Enabled), strings.TrimSpace(in.ClassifierModelID), in.Ask,
			SecgwFamilyDefaultTimeoutMs, by, now); err != nil {
			return err
		}
		return childAssignmentsTx(ctx, ts, assign, by, now)
	})
}

// SecgwFamilySnapshot is the family policy as the proxy hot path sees it.
type SecgwFamilySnapshot struct {
	Config *SecgwFamilyConfig
	// Covered is every user the policy applies to right now, with the
	// profile (age range) that applies to them.
	Covered map[string]*ChildProfile
}

// Active reports whether the policy is on and has a classifier (with no
// classifier the admin UI shows "Not protecting" and traffic passes, so a
// parent half-way through setup does not lock the children out).
func (f *SecgwFamilySnapshot) Active() bool {
	return f != nil && f.Config != nil && f.Config.Enabled && f.Config.ClassifierModelID != ""
}

// Covers reports whether the policy applies to a user.
func (f *SecgwFamilySnapshot) Covers(userID string) bool {
	return f.Active() && userID != "" && f.Covered[userID] != nil
}

// Profile is the age range that applies to a covered user (nil if none).
func (f *SecgwFamilySnapshot) Profile(userID string) *ChildProfile {
	if !f.Covers(userID) {
		return nil
	}
	return f.Covered[userID]
}

// LoadSecgwFamilySnapshot reads the family policy for the engine.
func (s *Store) LoadSecgwFamilySnapshot(ctx context.Context) (*SecgwFamilySnapshot, error) {
	cfg, err := s.SecgwFamily(ctx)
	if err != nil {
		return nil, err
	}
	snap := &SecgwFamilySnapshot{Config: cfg, Covered: map[string]*ChildProfile{}}
	if !cfg.Enabled {
		return snap, nil
	}
	if snap.Covered, err = s.childCoverage(ctx, cfg.Profiles); err != nil {
		return nil, err
	}
	return snap, nil
}

// SecgwFamilyCovers answers "is this user under Family safety" straight
// from the database, for the admin effective-policy explainer.
func (s *Store) SecgwFamilyCovers(ctx context.Context, userID string) (bool, error) {
	f, err := s.LoadSecgwFamilySnapshot(ctx)
	if err != nil {
		return false, err
	}
	return f.Covers(userID), nil
}
