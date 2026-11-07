package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/headerpolicy"
)

// Header policies rewrite the caller's headers before a request is forwarded
// to an upstream provider. A policy is a named, ordered rule set; an
// assignment binds one policy to a target. The most specific assignment wins:
// an upstream (or one personal-subscription provider) beats the org default.
//
// Rule values marked secret are stored encrypted in the same envelope as
// upstream API keys and never returned by the API.

// Header policy assignment targets.
const (
	HeaderTargetDefault  = "default"
	HeaderTargetUpstream = "upstream"
	// HeaderTargetPersonal is one personal-subscription provider ("openai",
	// "xai", ...), applied to every user's connection to it.
	HeaderTargetPersonal = "personal_subscription"
)

// SettingHeaderLogging stores HeaderLogSettings.
const SettingHeaderLogging = "header_logging"

// BuiltinEdgePolicyID is the policy the migration seeds and assigns as the
// org default: strip the CDN / tunnel / proxy headers that describe the
// caller. It is an ordinary policy: admins can edit, unassign or delete it.
const BuiltinEdgePolicyID = "hp_builtin_edge"

var headerPolicyMigration = migration{name: "0071_header_policy", stmt: []string{
	`CREATE TABLE IF NOT EXISTS header_policy (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		allow_only TEXT NOT NULL DEFAULT '[]',
		rules TEXT NOT NULL DEFAULT '[]',
		created_by TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS header_policy_assignment (
		target_type TEXT NOT NULL,
		target_id TEXT NOT NULL DEFAULT '',
		policy_id TEXT NOT NULL REFERENCES header_policy(id) ON DELETE CASCADE,
		created_by TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		PRIMARY KEY (target_type, target_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_header_policy_assignment_policy ON header_policy_assignment(policy_id)`,
	// One row per logged request: caller headers, what went upstream, and
	// the policy's changes. Kept out of usage_event (hot, wide, batched).
	`CREATE TABLE IF NOT EXISTS request_header_log (
		usage_id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		policy_id TEXT NOT NULL DEFAULT '',
		policy_name TEXT NOT NULL DEFAULT '',
		target TEXT NOT NULL DEFAULT '',
		inbound TEXT NOT NULL DEFAULT '[]',
		outbound TEXT NOT NULL DEFAULT '[]',
		changes TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_request_header_log_created ON request_header_log(created_at)`,
	// Built-in default: strip edge / proxy headers for every upstream. A
	// leak fix (Cloudflare Access identity tokens reached providers) and a
	// compatibility fix (OpenAI's edge refuses requests carrying them).
	`INSERT INTO header_policy (id, name, description, allow_only, rules, created_by, created_at, updated_at)
	 VALUES ('` + BuiltinEdgePolicyID + `', 'Strip edge & proxy headers',
	 'Removes the headers a CDN, tunnel or reverse proxy in front of Janus adds about the caller (Cloudflare, X-Forwarded-*, Via, ...). They identify the caller''s network, can carry a signed identity token, and some providers refuse requests that include them.',
	 '[]', '` + builtinEdgeRulesJSON() + `', 'system', '2026-10-31T00:00:00.000000000Z', '2026-10-31T00:00:00.000000000Z')`,
	`INSERT INTO header_policy_assignment (target_type, target_id, policy_id, created_by, created_at)
	 VALUES ('default', '', '` + BuiltinEdgePolicyID + `', 'system', '2026-10-31T00:00:00.000000000Z')`,
}, down: []string{
	`DROP TABLE IF EXISTS request_header_log`,
	`DROP TABLE IF EXISTS header_policy_assignment`,
	`DROP TABLE IF EXISTS header_policy`,
}}

func builtinEdgeRulesJSON() string {
	rules := make([]headerpolicy.Rule, 0, len(headerpolicy.EdgeHeaders))
	for _, n := range headerpolicy.EdgeHeaders {
		rules = append(rules, headerpolicy.Rule{Action: headerpolicy.ActionRemove, Name: n})
	}
	b, _ := json.Marshal(rules)
	return strings.ReplaceAll(string(b), "'", "''")
}

// HeaderPolicy is a stored policy. Secret rule values are never in Rules
// when returned by the API (Value is blanked, HasValue set).
type HeaderPolicy struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	AllowOnly   []string           `json:"allow_only"`
	Rules       []HeaderPolicyRule `json:"rules"`
	Assignments []HeaderAssignment `json:"assignments"`
	CreatedBy   string             `json:"created_by,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// HeaderPolicyRule is a rule as stored and exchanged with the API.
type HeaderPolicyRule struct {
	headerpolicy.Rule
	// HasValue reports a secret rule's stored value without revealing it.
	// On update, a secret rule with an empty Value and KeepValue=true keeps
	// the stored one.
	HasValue  bool `json:"has_value,omitempty"`
	KeepValue bool `json:"keep_value,omitempty"`
}

// HeaderAssignment binds a policy to a target.
type HeaderAssignment struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	PolicyID   string `json:"policy_id"`
	PolicyName string `json:"policy_name,omitempty"`
	TargetName string `json:"target_name,omitempty"`
}

// HeaderLogSettings controls request-log header capture.
type HeaderLogSettings struct {
	Mode headerpolicy.LogMode `json:"mode"`
}

// DefaultHeaderLogSettings: names and values, sensitive values masked.
func DefaultHeaderLogSettings() HeaderLogSettings {
	return HeaderLogSettings{Mode: headerpolicy.LogValues}
}

// HeaderLogSettings reads the setting (default when never saved).
func (s *Store) HeaderLogSettings(ctx context.Context) (HeaderLogSettings, error) {
	raw, _, err := s.systemSetting(ctx, SettingHeaderLogging)
	if errors.Is(err, ErrNotFound) {
		return DefaultHeaderLogSettings(), nil
	}
	if err != nil {
		return HeaderLogSettings{}, err
	}
	out := DefaultHeaderLogSettings()
	if err := json.Unmarshal([]byte(raw), &out); err != nil || !headerpolicy.ValidLogMode(out.Mode) {
		return DefaultHeaderLogSettings(), nil
	}
	return out, nil
}

// SetHeaderLogSettings stores the setting.
func (s *Store) SetHeaderLogSettings(ctx context.Context, v HeaderLogSettings) error {
	if !headerpolicy.ValidLogMode(v.Mode) {
		return &ValidationError{Field: "mode", Message: "mode must be values, names or off"}
	}
	b, _ := json.Marshal(v)
	return s.putSystemSetting(ctx, SettingHeaderLogging, string(b))
}

const headerPolicyColumns = `id, name, description, allow_only, rules, created_by, created_at, updated_at`

// scanHeaderPolicy decodes a row. With cipher != nil secret values are
// decrypted (for the proxy); otherwise they are blanked (for the API).
func scanHeaderPolicy(scan func(...any) error, cipher SecgwCipher) (*HeaderPolicy, error) {
	var p HeaderPolicy
	var allow, rules, created, updated string
	if err := scan(&p.ID, &p.Name, &p.Description, &allow, &rules, &p.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = ParseTime(created), ParseTime(updated)
	if err := json.Unmarshal([]byte(allow), &p.AllowOnly); err != nil {
		return nil, fmt.Errorf("header policy %s: allow_only: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(rules), &p.Rules); err != nil {
		return nil, fmt.Errorf("header policy %s: rules: %w", p.ID, err)
	}
	if p.AllowOnly == nil {
		p.AllowOnly = []string{}
	}
	if p.Rules == nil {
		p.Rules = []HeaderPolicyRule{}
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.Secret {
			continue
		}
		r.HasValue = r.Value != ""
		if cipher == nil || r.Value == "" {
			r.Value = ""
			continue
		}
		plain, err := cipher.Decrypt(r.Value)
		if err != nil {
			return nil, fmt.Errorf("header policy %s rule %d: %w", p.Name, i+1, ErrSecgwUndecryptable)
		}
		r.Value = plain
	}
	p.Assignments = []HeaderAssignment{}
	return &p, nil
}

// ListHeaderPolicies returns every policy with its assignments, secret
// values blanked.
func (s *Store) ListHeaderPolicies(ctx context.Context) ([]*HeaderPolicy, error) {
	rows, err := s.query(ctx, `SELECT `+headerPolicyColumns+` FROM header_policy ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	out := []*HeaderPolicy{}
	byID := map[string]*HeaderPolicy{}
	for rows.Next() {
		p, err := scanHeaderPolicy(rows.Scan, nil)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, p)
		byID[p.ID] = p
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	assigns, err := s.ListHeaderAssignments(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range assigns {
		if p := byID[a.PolicyID]; p != nil {
			p.Assignments = append(p.Assignments, a)
		}
	}
	return out, nil
}

// HeaderPolicyByID returns one policy, secret values blanked.
func (s *Store) HeaderPolicyByID(ctx context.Context, id string) (*HeaderPolicy, error) {
	return s.headerPolicyByID(ctx, id, nil)
}

func (s *Store) headerPolicyByID(ctx context.Context, id string, cipher SecgwCipher) (*HeaderPolicy, error) {
	p, err := scanHeaderPolicy(s.queryRow(ctx, `SELECT `+headerPolicyColumns+` FROM header_policy WHERE id = ?`, id).Scan, cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// rawRules returns a policy's rules exactly as stored (secrets still
// encrypted), so an update can keep a secret value it was not sent.
func (s *Store) rawHeaderRules(ctx context.Context, id string) ([]HeaderPolicyRule, error) {
	var raw string
	err := s.queryRow(ctx, `SELECT rules FROM header_policy WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out []HeaderPolicyRule
	return out, json.Unmarshal([]byte(raw), &out)
}

// prepareHeaderRules validates p and returns the rules JSON to store, with
// secret values encrypted. prev holds the stored rules on update.
func prepareHeaderRules(p *HeaderPolicy, prev []HeaderPolicyRule, cipher SecgwCipher) (string, string, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 120 {
		return "", "", &ValidationError{Field: "name", Message: "Give the policy a name (up to 120 characters)."}
	}
	if len(p.Description) > 2000 {
		return "", "", &ValidationError{Field: "description", Message: "The description is longer than 2000 characters."}
	}
	if p.AllowOnly == nil {
		p.AllowOnly = []string{}
	}
	plain := make([]headerpolicy.Rule, len(p.Rules))
	for i, r := range p.Rules {
		plain[i] = r.Rule
		if r.Secret && r.KeepValue && r.Value == "" {
			plain[i].Value = "kept"
		}
	}
	if err := headerpolicy.Validate(p.AllowOnly, plain); err != nil {
		var fe *headerpolicy.FieldError
		if errors.As(err, &fe) {
			return "", "", &ValidationError{Field: fe.Field, Message: fe.Message}
		}
		return "", "", err
	}
	stored := make([]headerpolicy.Rule, len(p.Rules))
	for i, r := range p.Rules {
		stored[i] = r.Rule
		if !r.Secret || r.Action == headerpolicy.ActionRemove {
			stored[i].Secret = r.Secret && r.Action != headerpolicy.ActionRemove
			continue
		}
		if r.Value == "" && r.KeepValue {
			// Keep the stored ciphertext of the same-named secret rule.
			kept := ""
			for _, old := range prev {
				if old.Secret && strings.EqualFold(old.Name, r.Name) && old.Value != "" {
					kept = old.Value
					break
				}
			}
			if kept == "" {
				return "", "", &ValidationError{Field: fmt.Sprintf("rules[%d]", i), Message: fmt.Sprintf("Rule %d: enter the secret value.", i+1)}
			}
			stored[i].Value = kept
			continue
		}
		if cipher == nil {
			return "", "", &ValidationError{Field: fmt.Sprintf("rules[%d]", i), Message: "Secret values need JANUS_ENCRYPTION_KEY to be configured."}
		}
		enc, err := cipher.Encrypt(r.Value)
		if err != nil {
			return "", "", fmt.Errorf("encrypt header value: %w", err)
		}
		stored[i].Value = enc
	}
	rb, _ := json.Marshal(stored)
	ab, _ := json.Marshal(p.AllowOnly)
	return string(ab), string(rb), nil
}

// CreateHeaderPolicy validates and stores a new policy.
func (s *Store) CreateHeaderPolicy(ctx context.Context, p *HeaderPolicy, cipher SecgwCipher) error {
	allow, rules, err := prepareHeaderRules(p, nil, cipher)
	if err != nil {
		return err
	}
	p.ID = "hp_" + NewID()
	now := nowUTC()
	p.CreatedAt, p.UpdatedAt = now, now
	err = s.exec(ctx, `INSERT INTO header_policy (`+headerPolicyColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, allow, rules, p.CreatedBy, FormatTime(now), FormatTime(now))
	if isUniqueViolation(err) {
		return &ValidationError{Field: "name", Message: "A header policy with that name already exists."}
	}
	return err
}

// UpdateHeaderPolicy replaces a policy's name, description and rules.
func (s *Store) UpdateHeaderPolicy(ctx context.Context, p *HeaderPolicy, cipher SecgwCipher) error {
	prev, err := s.rawHeaderRules(ctx, p.ID)
	if err != nil {
		return err
	}
	allow, rules, err := prepareHeaderRules(p, prev, cipher)
	if err != nil {
		return err
	}
	p.UpdatedAt = nowUTC()
	err = s.exec(ctx, `UPDATE header_policy SET name = ?, description = ?, allow_only = ?, rules = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, allow, rules, FormatTime(p.UpdatedAt), p.ID)
	if isUniqueViolation(err) {
		return &ValidationError{Field: "name", Message: "A header policy with that name already exists."}
	}
	return err
}

// DeleteHeaderPolicy removes a policy and its assignments.
func (s *Store) DeleteHeaderPolicy(ctx context.Context, id string) error {
	return s.writeTx(ctx, func(s *Store) error {
		if err := s.exec(ctx, `DELETE FROM header_policy_assignment WHERE policy_id = ?`, id); err != nil {
			return err
		}
		return s.exec(ctx, `DELETE FROM header_policy WHERE id = ?`, id)
	})
}

// ListHeaderAssignments returns every assignment, policy names resolved.
func (s *Store) ListHeaderAssignments(ctx context.Context) ([]HeaderAssignment, error) {
	rows, err := s.query(ctx, `SELECT a.target_type, a.target_id, a.policy_id, p.name FROM header_policy_assignment a
		JOIN header_policy p ON p.id = a.policy_id ORDER BY a.target_type, a.target_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HeaderAssignment{}
	for rows.Next() {
		var a HeaderAssignment
		if err := rows.Scan(&a.TargetType, &a.TargetID, &a.PolicyID, &a.PolicyName); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetHeaderAssignment binds policyID to a target; an empty policyID removes
// the target's assignment (it then inherits the default).
func (s *Store) SetHeaderAssignment(ctx context.Context, targetType, targetID, policyID, by string) error {
	switch targetType {
	case HeaderTargetDefault:
		targetID = ""
	case HeaderTargetUpstream:
		var n int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM upstream WHERE id = ? AND deleted_at = ''`, targetID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &ValidationError{Field: "target_id", Message: "That upstream does not exist."}
		}
	case HeaderTargetPersonal:
		if strings.TrimSpace(targetID) == "" {
			return &ValidationError{Field: "target_id", Message: "Choose a personal-subscription provider."}
		}
	default:
		return &ValidationError{Field: "target_type", Message: "target_type must be default, upstream or personal_subscription"}
	}
	if policyID == "" {
		return s.exec(ctx, `DELETE FROM header_policy_assignment WHERE target_type = ? AND target_id = ?`, targetType, targetID)
	}
	if _, err := s.HeaderPolicyByID(ctx, policyID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ValidationError{Field: "policy_id", Message: "That header policy does not exist."}
		}
		return err
	}
	return s.exec(ctx, `INSERT INTO header_policy_assignment (target_type, target_id, policy_id, created_by, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT (target_type, target_id) DO UPDATE SET policy_id = excluded.policy_id, created_by = excluded.created_by, created_at = excluded.created_at`,
		targetType, targetID, policyID, by, FormatTime(nowUTC()))
}

// HeaderPolicySnapshot is everything the proxy needs, secrets decrypted.
type HeaderPolicySnapshot struct {
	Policies map[string]*headerpolicy.Policy
	// Secret lists, per policy, the lower-cased names whose values are
	// secret (masked in the log even if not otherwise sensitive).
	Secret  map[string]map[string]bool
	Assign  map[string]string // "type\x00id" -> policy id
	LogMode headerpolicy.LogMode
}

// Resolve returns the policy that applies to a target: its own assignment,
// else the default, else nil. target is "upstream:<id>" or
// "personal_subscription:<provider>".
func (snap *HeaderPolicySnapshot) Resolve(targetType, targetID string) (*headerpolicy.Policy, string) {
	if snap == nil {
		return nil, ""
	}
	if id, ok := snap.Assign[targetType+"\x00"+targetID]; ok {
		return snap.Policies[id], targetType
	}
	if id, ok := snap.Assign[HeaderTargetDefault+"\x00"]; ok {
		return snap.Policies[id], HeaderTargetDefault
	}
	return nil, ""
}

// LoadHeaderPolicySnapshot reads every policy and assignment.
func (s *Store) LoadHeaderPolicySnapshot(ctx context.Context, cipher SecgwCipher) (*HeaderPolicySnapshot, error) {
	snap := &HeaderPolicySnapshot{Policies: map[string]*headerpolicy.Policy{}, Secret: map[string]map[string]bool{}, Assign: map[string]string{}}
	rows, err := s.query(ctx, `SELECT `+headerPolicyColumns+` FROM header_policy`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p, err := scanHeaderPolicy(rows.Scan, cipher)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		cp := &headerpolicy.Policy{ID: p.ID, Name: p.Name, AllowOnly: p.AllowOnly}
		secret := map[string]bool{}
		for _, r := range p.Rules {
			cp.Rules = append(cp.Rules, r.Rule)
			if r.Secret {
				secret[strings.ToLower(r.Name)] = true
			}
		}
		snap.Policies[p.ID] = cp
		snap.Secret[p.ID] = secret
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	assigns, err := s.ListHeaderAssignments(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range assigns {
		snap.Assign[a.TargetType+"\x00"+a.TargetID] = a.PolicyID
	}
	ls, err := s.HeaderLogSettings(ctx)
	if err != nil {
		return nil, err
	}
	snap.LogMode = ls.Mode
	return snap, nil
}

// RequestHeaderLog is the stored header record of one request.
type RequestHeaderLog struct {
	UsageID    string                `json:"usage_id"`
	CreatedAt  time.Time             `json:"created_at"`
	PolicyID   string                `json:"policy_id"`
	PolicyName string                `json:"policy_name"`
	Target     string                `json:"target"`
	Inbound    []headerpolicy.Logged `json:"inbound"`
	Outbound   []headerpolicy.Logged `json:"outbound"`
	Changes    []headerpolicy.Change `json:"changes"`
}

// InsertRequestHeaderLog stores one request's headers.
func (s *Store) InsertRequestHeaderLog(ctx context.Context, l *RequestHeaderLog) error {
	in, _ := json.Marshal(nonNilLogged(l.Inbound))
	out, _ := json.Marshal(nonNilLogged(l.Outbound))
	ch := l.Changes
	if ch == nil {
		ch = []headerpolicy.Change{}
	}
	cb, _ := json.Marshal(ch)
	if l.CreatedAt.IsZero() {
		l.CreatedAt = nowUTC()
	}
	return s.exec(ctx, `INSERT INTO request_header_log (usage_id, created_at, policy_id, policy_name, target, inbound, outbound, changes)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (usage_id) DO NOTHING`,
		l.UsageID, FormatTime(l.CreatedAt), l.PolicyID, l.PolicyName, l.Target, string(in), string(out), string(cb))
}

func nonNilLogged(v []headerpolicy.Logged) []headerpolicy.Logged {
	if v == nil {
		return []headerpolicy.Logged{}
	}
	return v
}

// RequestHeaderLogByUsageID returns a request's header record.
func (s *Store) RequestHeaderLogByUsageID(ctx context.Context, id string) (*RequestHeaderLog, error) {
	var l RequestHeaderLog
	var created, in, out, ch string
	err := s.queryRow(ctx, `SELECT usage_id, created_at, policy_id, policy_name, target, inbound, outbound, changes FROM request_header_log WHERE usage_id = ?`, id).
		Scan(&l.UsageID, &created, &l.PolicyID, &l.PolicyName, &l.Target, &in, &out, &ch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.CreatedAt = ParseTime(created)
	_ = json.Unmarshal([]byte(in), &l.Inbound)
	_ = json.Unmarshal([]byte(out), &l.Outbound)
	_ = json.Unmarshal([]byte(ch), &l.Changes)
	return &l, nil
}

// PurgeRequestHeaderLogs deletes header records older than before. Called
// with the usage retention cutoff so headers never outlive their request.
func (s *Store) PurgeRequestHeaderLogs(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`DELETE FROM request_header_log WHERE created_at < ?`), FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge request header log: %w", err)
	}
	return res.RowsAffected()
}
