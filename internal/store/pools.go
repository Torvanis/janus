package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Model pools: a managed model (alias) with several targets that the proxy
// load-balances across. Grants, quotas, the stable name and the fallback all
// stay on the alias; a pool only changes WHICH real model serves a request.
//
// Every alias has at least one member row. managed_model.target_model_id is
// kept equal to the first member (priority, then position) so every reader
// written before pools existed — the catalog card, the upstream-delete blast
// radius, reporting — keeps working on a pool of one exactly as before.

// Pool balancing policies.
const (
	// PoolPolicyFailover serves the highest-priority healthy member; the rest
	// stand by. This is what a single-target alias has always done.
	PoolPolicyFailover = "failover"
	// PoolPolicyRoundRobin spreads requests by weight.
	PoolPolicyRoundRobin = "round_robin"
	// PoolPolicyLeastLoaded sends each request to the member with the
	// shortest queue (waiting counts double, running once, plus this
	// gateway's own requests in flight).
	PoolPolicyLeastLoaded = "least_loaded"
	// PoolPolicyContext balances on context held: the tokens each member is
	// currently processing as a share of the context it can hold at once,
	// so a member serving two 100k-token conversations counts as busier than
	// one serving ten short chats.
	PoolPolicyContext = "context"
)

// PoolPolicies lists the policies in the order the admin UI offers them.
var PoolPolicies = []string{PoolPolicyFailover, PoolPolicyRoundRobin, PoolPolicyLeastLoaded, PoolPolicyContext}

// Session affinity modes. Affinity keeps a conversation on the member that
// already holds its prompt cache.
const (
	// PoolAffinityBounded keeps a conversation on its member until that
	// member is more than SpillPct above the pool average, then moves it to
	// its (equally deterministic) second choice. The default.
	PoolAffinityBounded = "bounded"
	// PoolAffinityStrict moves a conversation only when its member is out of
	// service.
	PoolAffinityStrict = "strict"
	// PoolAffinityOff balances every request independently.
	PoolAffinityOff = "off"
)

// PoolAffinities lists the modes in the order the admin UI offers them.
var PoolAffinities = []string{PoolAffinityBounded, PoolAffinityStrict, PoolAffinityOff}

// Pool limits.
const (
	MaxPoolMembers      = 32
	MaxPoolMemberWeight = 100
	DefaultPoolSpillPct = 25
)

// ManagedModelPool is an alias's balancing configuration.
type ManagedModelPool struct {
	Policy   string `json:"policy"`
	Affinity string `json:"affinity"`
	// SpillPct is how far above the pool average (in percent) a member may
	// run before bounded affinity moves new turns off it.
	SpillPct int          `json:"spill_pct"`
	Members  []PoolMember `json:"members"`
}

// PoolMember is one target of a pool.
type PoolMember struct {
	ModelID string `json:"model_id"`
	// Weight biases round robin and load comparisons (a member with weight 2
	// is expected to carry twice the traffic). 1..MaxPoolMemberWeight.
	Weight int `json:"weight"`
	// Priority orders failover: lower serves first. Members with equal
	// priority share traffic under the other policies.
	Priority int  `json:"priority"`
	Enabled  bool `json:"enabled"`
	// ContextCapacity is the number of context tokens the member can hold
	// at once, used by the context policy. 0 = read it from the server
	// (vLLM KV-cache size, llama.cpp slots x context) or, failing that,
	// assume one full context window.
	ContextCapacity int64 `json:"context_capacity"`
	Position        int   `json:"position"`

	// Read-time presentation, resolved from the catalog.
	ModelName    string `json:"model_name,omitempty"`
	PublicName   string `json:"public_name,omitempty"`
	ModelStatus  string `json:"model_status,omitempty"`
	UpstreamID   string `json:"upstream_id,omitempty"`
	UpstreamName string `json:"upstream_name,omitempty"`
	AdapterType  string `json:"adapter_type,omitempty"`
	// ContextWindow is the member model's own context window.
	ContextWindow int64 `json:"context_window,omitempty"`
	// Missing reports that the member's model no longer exists.
	Missing bool `json:"missing,omitempty"`
}

// Servable reports whether the member can take traffic at all (before any
// runtime health signal is considered).
func (m PoolMember) Servable() bool {
	return m.Enabled && !m.Missing && m.ModelStatus == ModelEnabled
}

// servableMembers counts members that can take traffic right now.
func (p ManagedModelPool) servableMembers() int {
	n := 0
	for _, m := range p.Members {
		if m.Servable() {
			n++
		}
	}
	return n
}

// IsLoadBalanced reports whether the pool uses anything beyond the
// single-target behaviour every edition has: more than one enabled member,
// or a policy other than failover.
func (p ManagedModelPool) IsLoadBalanced() bool {
	enabled := 0
	for _, m := range p.Members {
		if m.Enabled {
			enabled++
		}
	}
	return enabled > 1 || (p.Policy != "" && p.Policy != PoolPolicyFailover)
}

// PoolMemberEjection is a passive ejection published by one gateway replica
// so every replica takes the member out together (otherwise replicas that
// have not seen the failures keep routing a conversation to it while the rest
// move it — the conversation's cache is split).
type PoolMemberEjection struct {
	ManagedModelID string    `json:"managed_model_id"`
	ModelID        string    `json:"model_id"`
	Until          time.Time `json:"until"`
	Reason         string    `json:"reason"`
	InstanceID     string    `json:"instance_id"`
	UpdatedAt      time.Time `json:"updated_at"`
}

var poolMigration = migration{name: "0044_model_pools", stmt: []string{
	`CREATE TABLE IF NOT EXISTS managed_model_target (
		managed_model_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		weight INTEGER NOT NULL DEFAULT 1,
		priority INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		context_capacity INTEGER NOT NULL DEFAULT 0,
		position INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		PRIMARY KEY (managed_model_id, model_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_managed_model_target_model ON managed_model_target(model_id)`,
	`INSERT INTO managed_model_target (managed_model_id, model_id, weight, priority, enabled, context_capacity, position, created_at)
		SELECT id, target_model_id, 1, 0, 1, 0, 0, created_at FROM managed_model WHERE target_model_id <> ''`,
	`ALTER TABLE managed_model ADD COLUMN lb_policy TEXT NOT NULL DEFAULT 'failover'`,
	`ALTER TABLE managed_model ADD COLUMN lb_affinity TEXT NOT NULL DEFAULT 'bounded'`,
	`ALTER TABLE managed_model ADD COLUMN lb_spill_pct INTEGER NOT NULL DEFAULT 25`,
	`CREATE TABLE IF NOT EXISTS pool_member_state (
		managed_model_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		ejected_until TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		instance_id TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY (managed_model_id, model_id)
	)`,
	// Why a pooled request went to the member it did (balancer.Reason*) and
	// how many members were tried.
	`ALTER TABLE usage_event ADD COLUMN pool_reason TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE usage_event ADD COLUMN pool_attempts INTEGER NOT NULL DEFAULT 0`,
}, down: []string{
	`DROP TABLE IF EXISTS pool_member_state`,
	`DROP INDEX IF EXISTS idx_managed_model_target_model`,
	`DROP TABLE IF EXISTS managed_model_target`,
}}

// PoolMemberInput is one member as an admin submits it.
type PoolMemberInput struct {
	ModelID         string `json:"model_id"`
	Weight          int    `json:"weight"`
	Priority        int    `json:"priority"`
	Enabled         *bool  `json:"enabled"`
	ContextCapacity int64  `json:"context_capacity"`
}

// PoolInput is a complete pool configuration as an admin submits it. The
// member list replaces the current one.
type PoolInput struct {
	Policy   string            `json:"policy"`
	Affinity string            `json:"affinity"`
	SpillPct *int              `json:"spill_pct"`
	Members  []PoolMemberInput `json:"members"`
}

// NormalizePool validates a submitted pool and fills defaults. It does not
// touch the database beyond checking that each member is a real catalog model.
func (s *Store) NormalizePool(ctx context.Context, in PoolInput, fallbackModelID string) (ManagedModelPool, error) {
	out := ManagedModelPool{
		Policy:   strings.ToLower(strings.TrimSpace(in.Policy)),
		Affinity: strings.ToLower(strings.TrimSpace(in.Affinity)),
		SpillPct: DefaultPoolSpillPct,
	}
	if out.Policy == "" {
		out.Policy = PoolPolicyFailover
	}
	if !containsString(PoolPolicies, out.Policy) {
		return out, &ValidationError{Field: "policy", Message: fmt.Sprintf("unknown balancing policy %q; choose from %s", out.Policy, strings.Join(PoolPolicies, ", "))}
	}
	if out.Affinity == "" {
		out.Affinity = PoolAffinityBounded
	}
	if !containsString(PoolAffinities, out.Affinity) {
		return out, &ValidationError{Field: "affinity", Message: fmt.Sprintf("unknown affinity mode %q; choose from %s", out.Affinity, strings.Join(PoolAffinities, ", "))}
	}
	if in.SpillPct != nil {
		if *in.SpillPct < 0 || *in.SpillPct > 400 {
			return out, &ValidationError{Field: "spill_pct", Message: "spill threshold must be between 0 and 400 percent"}
		}
		out.SpillPct = *in.SpillPct
	}
	if len(in.Members) == 0 {
		return out, &ValidationError{Field: "members", Message: "a pool needs at least one model"}
	}
	if len(in.Members) > MaxPoolMembers {
		return out, &ValidationError{Field: "members", Message: fmt.Sprintf("a pool can have at most %d models", MaxPoolMembers)}
	}
	seen := map[string]bool{}
	anyEnabled := false
	for i, m := range in.Members {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			return out, &ValidationError{Field: "members", Message: "every pool member needs a model"}
		}
		if seen[id] {
			return out, &ValidationError{Field: "members", Message: "a model can only be in a pool once"}
		}
		seen[id] = true
		if id == fallbackModelID && fallbackModelID != "" {
			return out, &ValidationError{Field: "members", Message: "the alias's fallback cannot also be a pool member; the fallback serves when every member is out"}
		}
		if _, err := s.ManagedModelByID(ctx, id); err == nil {
			return out, &ValidationError{Field: "members", Message: "pool members must be real catalog models, not other managed models"}
		} else if !errors.Is(err, ErrNotFound) {
			return out, err
		}
		if _, err := s.ModelByID(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return out, &ValidationError{Field: "members", Message: "one of the pool's models does not exist"}
			}
			return out, err
		}
		weight := m.Weight
		if weight == 0 {
			weight = 1
		}
		if weight < 1 || weight > MaxPoolMemberWeight {
			return out, &ValidationError{Field: "members", Message: fmt.Sprintf("weights must be between 1 and %d", MaxPoolMemberWeight)}
		}
		if m.Priority < 0 || m.Priority > 100 {
			return out, &ValidationError{Field: "members", Message: "priorities must be between 0 and 100"}
		}
		if m.ContextCapacity < 0 {
			return out, &ValidationError{Field: "members", Message: "context capacity cannot be negative"}
		}
		enabled := m.Enabled == nil || *m.Enabled
		anyEnabled = anyEnabled || enabled
		out.Members = append(out.Members, PoolMember{
			ModelID: id, Weight: weight, Priority: m.Priority, Enabled: enabled,
			ContextCapacity: m.ContextCapacity, Position: i,
		})
	}
	if !anyEnabled {
		return out, &ValidationError{Field: "members", Message: "enable at least one pool member (disable the alias instead to take it out of service)"}
	}
	return out, nil
}

// SetManagedModelPool replaces an alias's balancing configuration and member
// list in one transaction, and repoints target_model_id at the new first
// member so pre-pool readers see a sensible primary.
func (s *Store) SetManagedModelPool(ctx context.Context, id string, pool ManagedModelPool) error {
	if len(pool.Members) == 0 {
		return &ValidationError{Field: "members", Message: "a pool needs at least one model"}
	}
	primary := primaryMember(pool.Members)
	now := FormatTime(nowUTC())
	return s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(
			`UPDATE managed_model SET lb_policy = ?, lb_affinity = ?, lb_spill_pct = ?, target_model_id = ?, updated_at = ? WHERE id = ?`),
			pool.Policy, pool.Affinity, pool.SpillPct, primary.ModelID, now, id)
		if err != nil {
			return fmt.Errorf("update pool policy: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM managed_model_target WHERE managed_model_id = ?`), id); err != nil {
			return fmt.Errorf("clear pool members: %w", err)
		}
		for i, m := range pool.Members {
			if _, err := tx.ExecContext(ctx, s.rebind(
				`INSERT INTO managed_model_target (managed_model_id, model_id, weight, priority, enabled, context_capacity, position, created_at)
				 VALUES (?,?,?,?,?,?,?,?)`),
				id, m.ModelID, m.Weight, m.Priority, boolInt(m.Enabled), m.ContextCapacity, i, now); err != nil {
				return fmt.Errorf("insert pool member: %w", err)
			}
		}
		// Ejections of members that left the pool are meaningless now.
		if _, err := tx.ExecContext(ctx, s.rebind(
			`DELETE FROM pool_member_state WHERE managed_model_id = ? AND model_id NOT IN (SELECT model_id FROM managed_model_target WHERE managed_model_id = ?)`),
			id, id); err != nil {
			return fmt.Errorf("prune pool ejections: %w", err)
		}
		return nil
	})
}

// primaryMember is the member pre-pool readers see as "the target": the
// lowest-priority-number enabled member, earliest position first.
func primaryMember(members []PoolMember) PoolMember {
	sorted := append([]PoolMember{}, members...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Enabled != sorted[j].Enabled {
			return sorted[i].Enabled
		}
		return sorted[i].Priority < sorted[j].Priority
	})
	return sorted[0]
}

// replaceSingleMember keeps the member table in step with a classic repoint
// (SetManagedModelTarget) of a pool of one.
func (s *Store) replaceSingleMember(ctx context.Context, tx *sql.Tx, id, modelID string) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM managed_model_target WHERE managed_model_id = ?`), id); err != nil {
		return fmt.Errorf("clear pool member: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM pool_member_state WHERE managed_model_id = ?`), id); err != nil {
		return fmt.Errorf("clear pool ejections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO managed_model_target (managed_model_id, model_id, weight, priority, enabled, context_capacity, position, created_at)
		 VALUES (?,?,1,0,1,0,0,?)`), id, modelID, FormatTime(nowUTC())); err != nil {
		return fmt.Errorf("insert pool member: %w", err)
	}
	return nil
}

// loadPool reads an alias's members and presentation.
func (s *Store) loadPool(ctx context.Context, m *ManagedModel) error {
	rows, err := s.query(ctx, `SELECT model_id, weight, priority, enabled, context_capacity, position
		FROM managed_model_target WHERE managed_model_id = ? ORDER BY position ASC, model_id ASC`, m.ID)
	if err != nil {
		return fmt.Errorf("load pool members: %w", err)
	}
	members := []PoolMember{}
	for rows.Next() {
		var pm PoolMember
		var enabled int
		if err := rows.Scan(&pm.ModelID, &pm.Weight, &pm.Priority, &enabled, &pm.ContextCapacity, &pm.Position); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan pool member: %w", err)
		}
		pm.Enabled = enabled != 0
		members = append(members, pm)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(members) == 0 && m.TargetModelID != "" {
		// A row written by a pre-pool binary during a rolling upgrade.
		members = []PoolMember{{ModelID: m.TargetModelID, Weight: 1, Enabled: true}}
	}
	for i := range members {
		model, err := s.ModelByID(ctx, members[i].ModelID)
		if errors.Is(err, ErrNotFound) {
			members[i].Missing = true
			continue
		}
		if err != nil {
			return err
		}
		members[i].ModelName = model.Name
		members[i].PublicName = model.PublicName()
		members[i].ModelStatus = model.Status
		members[i].UpstreamID = model.UpstreamID
		members[i].UpstreamName = model.UpstreamName
		members[i].ContextWindow = model.ContextWindow
		if up, err := s.UpstreamByID(ctx, model.UpstreamID); err == nil {
			members[i].AdapterType = up.AdapterType
		}
	}
	m.Pool.Members = members
	return nil
}

// PoolMemberEjections returns every live (not yet expired) ejection.
// It is read through the config cache on the hot path.
func (s *Store) PoolMemberEjections(ctx context.Context, now time.Time) ([]PoolMemberEjection, error) {
	rows, err := s.query(ctx, `SELECT managed_model_id, model_id, ejected_until, reason, instance_id, updated_at
		FROM pool_member_state WHERE ejected_until > ?`, FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("load pool ejections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []PoolMemberEjection{}
	for rows.Next() {
		var e PoolMemberEjection
		var until, updated string
		if err := rows.Scan(&e.ManagedModelID, &e.ModelID, &until, &e.Reason, &e.InstanceID, &updated); err != nil {
			return nil, fmt.Errorf("scan pool ejection: %w", err)
		}
		e.Until, e.UpdatedAt = ParseTime(until), ParseTime(updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EjectPoolMember publishes (or extends) an ejection. A shorter ejection
// never shortens a longer one another replica already published.
func (s *Store) EjectPoolMember(ctx context.Context, e PoolMemberEjection) error {
	now := FormatTime(nowUTC())
	until := FormatTime(e.Until)
	return s.InTx(ctx, func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx, s.rebind(
			`SELECT ejected_until FROM pool_member_state WHERE managed_model_id = ? AND model_id = ?`),
			e.ManagedModelID, e.ModelID).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, s.rebind(
				`INSERT INTO pool_member_state (managed_model_id, model_id, ejected_until, reason, instance_id, updated_at) VALUES (?,?,?,?,?,?)`),
				e.ManagedModelID, e.ModelID, until, e.Reason, e.InstanceID, now)
			return err
		case err != nil:
			return err
		case current >= until:
			return nil
		}
		_, err = tx.ExecContext(ctx, s.rebind(
			`UPDATE pool_member_state SET ejected_until = ?, reason = ?, instance_id = ?, updated_at = ? WHERE managed_model_id = ? AND model_id = ?`),
			until, e.Reason, e.InstanceID, now, e.ManagedModelID, e.ModelID)
		return err
	})
}

// ClearPoolMemberEjection lifts an ejection (a half-open trial succeeded, or
// an admin reinstated the member).
func (s *Store) ClearPoolMemberEjection(ctx context.Context, managedModelID, modelID string) error {
	return s.exec(ctx, `DELETE FROM pool_member_state WHERE managed_model_id = ? AND model_id = ?`, managedModelID, modelID)
}

// PooledUpstreamIDs lists the upstreams hosting a member of an enabled pool
// with more than one enabled member: the set the balancer must watch.
func (s *Store) PooledUpstreamIDs(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT m.upstream_id FROM managed_model_target t
		JOIN managed_model mm ON mm.id = t.managed_model_id
		JOIN model m ON m.id = t.model_id
		WHERE mm.status = ? AND t.enabled = 1 AND t.managed_model_id IN (
			SELECT managed_model_id FROM managed_model_target WHERE enabled = 1 GROUP BY managed_model_id HAVING COUNT(*) > 1)`,
		ManagedModelEnabled)
	if err != nil {
		return nil, fmt.Errorf("list pooled upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
