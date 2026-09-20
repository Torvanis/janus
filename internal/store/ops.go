package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --- Audit ------------------------------------------------------------------

// AppendAudit writes one immutable administrative record.
func (s *Store) AppendAudit(ctx context.Context, e *AuditEntry) error {
	e.ID = NewID()
	e.CreatedAt = nowUTC()
	return s.exec(ctx,
		`INSERT INTO audit_log (id, actor_user_id, actor_label, action, resource_type, resource_id, old_value, new_value, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.ActorUserID, e.ActorLabel, e.Action, e.ResourceType, e.ResourceID, e.OldValue, e.NewValue, FormatTime(e.CreatedAt))
}

// AuditFilter narrows an audit query.
type AuditFilter struct {
	Actor        string
	Action       string
	ResourceType string
	Start        time.Time
	End          time.Time
	Limit        int
	Offset       int
}

// ListAudit returns a page of audit entries plus the total match count.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Actor != "" {
		where = append(where, "actor_user_id = ?")
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.ResourceType != "" {
		where = append(where, "resource_type = ?")
		args = append(args, f.ResourceType)
	}
	if !f.Start.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, FormatTime(f.Start))
	}
	if !f.End.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, FormatTime(f.End))
	}
	clause := strings.Join(where, " AND ")
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit entries: %w", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT id, actor_user_id, actor_label, action, resource_type, resource_id, old_value, new_value, created_at
		FROM audit_log WHERE `+clause+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []*AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var created string
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorLabel, &e.Action, &e.ResourceType, &e.ResourceID, &e.OldValue, &e.NewValue, &created); err != nil {
			return nil, 0, fmt.Errorf("scan audit entry: %w", err)
		}
		e.CreatedAt = ParseTime(created)
		out = append(out, &e)
	}
	return out, total, rows.Err()
}

// PurgeAudit deletes audit entries past the retention window.
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM audit_log WHERE created_at < ?`), FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge audit log: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- Blocking rules ---------------------------------------------------------

// ListBlockingRules returns every policy rule.
func (s *Store) ListBlockingRules(ctx context.Context) ([]*BlockingRule, error) {
	rows, err := s.query(ctx, `SELECT id, name, combinator, reason, enabled, clauses, hit_count, last_hit_at, created_at FROM blocking_rule ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*BlockingRule{}
	for rows.Next() {
		var r BlockingRule
		var enabled int
		var clauses, lastHit, created string
		if err := rows.Scan(&r.ID, &r.Name, &r.Combinator, &r.Reason, &enabled, &clauses, &r.HitCount, &lastHit, &created); err != nil {
			return nil, fmt.Errorf("scan blocking rule: %w", err)
		}
		r.Enabled = enabled == 1
		r.LastHitAt = ParseTime(lastHit)
		r.CreatedAt = ParseTime(created)
		if err := json.Unmarshal([]byte(clauses), &r.Clauses); err != nil {
			r.Clauses = []RuleClause{}
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CreateBlockingRule persists a policy rule.
func (s *Store) CreateBlockingRule(ctx context.Context, r *BlockingRule) error {
	r.ID = NewID()
	r.CreatedAt = nowUTC()
	clauses, err := json.Marshal(r.Clauses)
	if err != nil {
		return fmt.Errorf("encode rule clauses: %w", err)
	}
	return s.exec(ctx, `INSERT INTO blocking_rule (id, name, combinator, reason, enabled, clauses, hit_count, last_hit_at, created_at)
		VALUES (?,?,?,?,?,?,0,'',?)`,
		r.ID, r.Name, r.Combinator, r.Reason, boolInt(r.Enabled), string(clauses), FormatTime(r.CreatedAt))
}

// BlockingRuleByID loads one policy rule.
func (s *Store) BlockingRuleByID(ctx context.Context, id string) (*BlockingRule, error) {
	var r BlockingRule
	var enabled int
	var clauses, lastHit, created string
	err := s.queryRow(ctx, `SELECT id, name, combinator, reason, enabled, clauses, hit_count, last_hit_at, created_at FROM blocking_rule WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Combinator, &r.Reason, &enabled, &clauses, &r.HitCount, &lastHit, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load blocking rule: %w", err)
	}
	r.Enabled = enabled == 1
	r.LastHitAt = ParseTime(lastHit)
	r.CreatedAt = ParseTime(created)
	if err := json.Unmarshal([]byte(clauses), &r.Clauses); err != nil {
		r.Clauses = []RuleClause{}
	}
	return &r, nil
}

// UpdateBlockingRule edits a policy rule in place so the rule's identity — and
// with it the audit trail and hit counter — survives the edit.
func (s *Store) UpdateBlockingRule(ctx context.Context, r *BlockingRule) error {
	clauses, err := json.Marshal(r.Clauses)
	if err != nil {
		return fmt.Errorf("encode rule clauses: %w", err)
	}
	return s.exec(ctx, `UPDATE blocking_rule SET name = ?, combinator = ?, reason = ?, enabled = ?, clauses = ? WHERE id = ?`,
		r.Name, r.Combinator, r.Reason, boolInt(r.Enabled), string(clauses), r.ID)
}

// DeleteBlockingRule removes a policy rule.
func (s *Store) DeleteBlockingRule(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM blocking_rule WHERE id = ?`, id)
}

// RecordRuleHit increments a rule's match counter.
func (s *Store) RecordRuleHit(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE blocking_rule SET hit_count = hit_count + 1, last_hit_at = ? WHERE id = ?`, FormatTime(nowUTC()), id)
}

// --- Alert rules & notifications -------------------------------------------

// ListAlertRules returns configured alert deliveries.
func (s *Store) ListAlertRules(ctx context.Context) ([]*AlertRule, error) {
	rows, err := s.query(ctx, `SELECT id, trigger_kind, severity, channels, webhook_url, enabled, created_at FROM alert_rule ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*AlertRule{}
	for rows.Next() {
		var r AlertRule
		var channels, created string
		var enabled int
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Severity, &channels, &r.WebhookURL, &enabled, &created); err != nil {
			return nil, fmt.Errorf("scan alert rule: %w", err)
		}
		r.Channels = splitCSV(channels)
		r.Enabled = enabled == 1
		r.CreatedAt = ParseTime(created)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CreateAlertRule persists an alert delivery rule.
func (s *Store) CreateAlertRule(ctx context.Context, r *AlertRule) error {
	r.ID = NewID()
	r.CreatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO alert_rule (id, trigger_kind, severity, channels, webhook_url, enabled, created_at) VALUES (?,?,?,?,?,?,?)`,
		r.ID, r.Trigger, r.Severity, strings.Join(r.Channels, ","), r.WebhookURL, boolInt(r.Enabled), FormatTime(r.CreatedAt))
}

// UpdateAlertRule edits an alert delivery rule in place, preserving its id.
func (s *Store) UpdateAlertRule(ctx context.Context, r *AlertRule) error {
	return s.exec(ctx, `UPDATE alert_rule SET trigger_kind = ?, severity = ?, channels = ?, webhook_url = ?, enabled = ? WHERE id = ?`,
		r.Trigger, r.Severity, strings.Join(r.Channels, ","), r.WebhookURL, boolInt(r.Enabled), r.ID)
}

// DeleteAlertRule removes an alert delivery rule.
func (s *Store) DeleteAlertRule(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM alert_rule WHERE id = ?`, id)
}

// AlertRuleByID loads one alert delivery rule.
func (s *Store) AlertRuleByID(ctx context.Context, id string) (*AlertRule, error) {
	rules, err := s.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

// CreateNotification delivers an in-app message. An empty userID broadcasts to
// administrators, who read it from the same inbox.
func (s *Store) CreateNotification(ctx context.Context, n *Notification) error {
	n.ID = NewID()
	n.CreatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO notification (id, user_id, severity, title, body, read_at, created_at) VALUES (?,?,?,?,?,'',?)`,
		n.ID, n.UserID, n.Severity, n.Title, n.Body, FormatTime(n.CreatedAt))
}

// ListNotifications returns a user's inbox, newest first.
func (s *Store) ListNotifications(ctx context.Context, userID string, unreadOnly bool, limit int, offsets ...int) ([]*Notification, int, error) {
	where := "user_id = ?"
	args := []any{userID}
	if unreadOnly {
		where += " AND read_at = ''"
	}
	var unread int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM notification WHERE user_id = ? AND read_at = ''`, userID).Scan(&unread); err != nil {
		return nil, 0, fmt.Errorf("count unread notifications: %w", err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := 0
	if len(offsets) > 0 && offsets[0] > 0 {
		offset = offsets[0]
	}
	rows, err := s.query(ctx, `SELECT id, user_id, severity, title, body, read_at, created_at FROM notification WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Notification{}
	for rows.Next() {
		var n Notification
		var read, created string
		if err := rows.Scan(&n.ID, &n.UserID, &n.Severity, &n.Title, &n.Body, &read, &created); err != nil {
			return nil, 0, fmt.Errorf("scan notification: %w", err)
		}
		n.ReadAt = ParseTime(read)
		n.CreatedAt = ParseTime(created)
		out = append(out, &n)
	}
	return out, unread, rows.Err()
}

// MarkNotificationsRead marks one message, or the whole inbox when id is empty.
func (s *Store) MarkNotificationsRead(ctx context.Context, userID, id string) error {
	if id == "" {
		return s.exec(ctx, `UPDATE notification SET read_at = ? WHERE user_id = ? AND read_at = ''`, FormatTime(nowUTC()), userID)
	}
	return s.exec(ctx, `UPDATE notification SET read_at = ? WHERE user_id = ? AND id = ?`, FormatTime(nowUTC()), userID, id)
}

// AdminUserIDs lists every active administrator, used for system alert fan-out.
func (s *Store) AdminUserIDs(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT id FROM app_user WHERE role = ? AND is_active = 1`, RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan admin id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- Feature flags ----------------------------------------------------------

// DefaultFeatureFlags are the shipped defaults. Every user-facing feature is
// off until an admin turns it on. otel_enabled defaults to on so that setting
// OTEL_EXPORTER_OTLP_ENDPOINT alone activates export; the flag exists
// to pause export at runtime without unsetting the endpoint, and it
// is inert while no endpoint is configured. spend_emphasis chooses which
// metric dashboard graphs open on: off (the default) emphasises usage, so
// graphs default to the Tokens metric; on emphasises spend, so they default
// to the Spend/cost metric. Viewers can always switch metrics per graph, and
// an explicit ?metric= URL value keeps winning either way. SetFeatureFlag
// below validates against this map, so it is the single registry of flags.
var DefaultFeatureFlags = map[string]bool{
	"leaderboards_enabled":         false,
	"per_user_rate_limits_enabled": false,
	"reduce_motion_preferred":      false,
	"otel_enabled":                 true,
	"spend_emphasis":               false,
}

// FeatureFlags returns the current flag set, merged over the defaults, so a
// flag added to DefaultFeatureFlags (like spend_emphasis) is reported with
// its shipped default even before any admin has ever toggled it.
func (s *Store) FeatureFlags(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for k, v := range DefaultFeatureFlags {
		out[k] = v
	}
	rows, err := s.query(ctx, `SELECT name, enabled FROM feature_flag`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var enabled int
		if err := rows.Scan(&name, &enabled); err != nil {
			return nil, fmt.Errorf("scan feature flag: %w", err)
		}
		out[name] = enabled == 1
	}
	return out, rows.Err()
}

// SetFeatureFlag turns one flag on or off with immediate effect.
func (s *Store) SetFeatureFlag(ctx context.Context, name string, enabled bool) error {
	if _, ok := DefaultFeatureFlags[name]; !ok {
		return fmt.Errorf("unknown feature flag %q", name)
	}
	var existing int
	err := s.queryRow(ctx, `SELECT enabled FROM feature_flag WHERE name = ?`, name).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return s.exec(ctx, `INSERT INTO feature_flag (name, enabled, updated_at) VALUES (?,?,?)`, name, boolInt(enabled), FormatTime(nowUTC()))
	}
	if err != nil {
		return fmt.Errorf("read feature flag: %w", err)
	}
	return s.exec(ctx, `UPDATE feature_flag SET enabled = ?, updated_at = ? WHERE name = ?`, boolInt(enabled), FormatTime(nowUTC()), name)
}

// --- System settings --------------------------------------------------------
//
// system_setting is a key/value table of instance-wide operational settings
// an administrator may change at runtime. Each value is a JSON document owned
// by one typed accessor pair below; the raw helpers are deliberately
// unexported so every setting gets a validated, typed surface.

// SettingUpstreamTimeouts is the system_setting key under which runtime
// upstream timeout overrides are stored.
const SettingUpstreamTimeouts = "upstream_timeouts"

// MaxUpstreamTimeoutSeconds bounds any single upstream timeout override (24
// hours). It mirrors config.MaxUpstreamTimeout; the store cannot import config.
const MaxUpstreamTimeoutSeconds = 24 * 60 * 60

// UpstreamTimeoutOverrides are the administrator-set replacements for the
// environment-derived upstream timeouts (JANUS_UPSTREAM_*_TIMEOUT_SECONDS).
// Each field is in whole seconds; zero means "no override, use the
// environment default" for that hop, so a partially overridden set is
// representable. UpdatedAt is zero when nothing has ever been stored.
type UpstreamTimeoutOverrides struct {
	ConnectSeconds int `json:"connect_seconds"`
	TTFBSeconds    int `json:"ttfb_seconds"`
	TotalSeconds   int `json:"total_seconds"`
	// UpdatedAt comes from the row's updated_at column, never from the JSON
	// document, so it is excluded from encoding.
	UpdatedAt time.Time `json:"-"`
}

// IsZero reports whether no hop is overridden.
func (o UpstreamTimeoutOverrides) IsZero() bool {
	return o.ConnectSeconds == 0 && o.TTFBSeconds == 0 && o.TotalSeconds == 0
}

// Validate rejects negative values and values past the sanity ceiling.
// Cross-field consistency (TTFB ≤ total) needs the environment defaults and
// is checked by the handler on the effective set.
func (o UpstreamTimeoutOverrides) Validate() error {
	for _, hop := range []struct {
		name  string
		value int
	}{{"connect_seconds", o.ConnectSeconds}, {"ttfb_seconds", o.TTFBSeconds}, {"total_seconds", o.TotalSeconds}} {
		if hop.value < 0 {
			return fmt.Errorf("%s must be zero (use the environment default) or a positive number of seconds", hop.name)
		}
		if hop.value > MaxUpstreamTimeoutSeconds {
			return fmt.Errorf("%s must not exceed %d seconds (24 hours)", hop.name, MaxUpstreamTimeoutSeconds)
		}
	}
	return nil
}

// UpstreamTimeoutOverrides returns the stored overrides, or the zero value
// (every hop inheriting its environment default) when none have been set.
func (s *Store) UpstreamTimeoutOverrides(ctx context.Context) (UpstreamTimeoutOverrides, error) {
	raw, updatedAt, err := s.systemSetting(ctx, SettingUpstreamTimeouts)
	if errors.Is(err, ErrNotFound) {
		return UpstreamTimeoutOverrides{}, nil
	}
	if err != nil {
		return UpstreamTimeoutOverrides{}, err
	}
	var out UpstreamTimeoutOverrides
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return UpstreamTimeoutOverrides{}, fmt.Errorf("decode %s setting: %w", SettingUpstreamTimeouts, err)
	}
	out.UpdatedAt = updatedAt
	return out, nil
}

// SetUpstreamTimeoutOverrides replaces the stored override set. Storing an
// all-zero set removes the row entirely, so "revert to the environment" and
// "never overridden" are the same state and the status document can say
// honestly that nothing is overridden.
func (s *Store) SetUpstreamTimeoutOverrides(ctx context.Context, o UpstreamTimeoutOverrides) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.IsZero() {
		return s.deleteSystemSetting(ctx, SettingUpstreamTimeouts)
	}
	encoded, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("encode %s setting: %w", SettingUpstreamTimeouts, err)
	}
	return s.putSystemSetting(ctx, SettingUpstreamTimeouts, string(encoded))
}

// ClearUpstreamTimeoutOverrides reverts every hop to its environment default.
func (s *Store) ClearUpstreamTimeoutOverrides(ctx context.Context) error {
	return s.deleteSystemSetting(ctx, SettingUpstreamTimeouts)
}

// SettingDiscoveryInterval is the system_setting key under which the runtime
// override of the upstream model-discovery polling interval is stored.
const SettingDiscoveryInterval = "discovery_interval"

// Bounds on the discovery interval override, in minutes. The floor stops an
// admin from turning discovery into a hot loop against every upstream; the
// ceiling (one week) keeps "effectively never" expressible without allowing
// a value the ticker arithmetic cannot represent.
const (
	MinDiscoveryIntervalMinutes = 1
	MaxDiscoveryIntervalMinutes = 7 * 24 * 60
)

// DiscoveryIntervalOverride is the administrator-set replacement for the
// JANUS_DISCOVERY_INTERVAL_MINUTES environment default. Minutes == 0 means
// "no override" and is never stored (see SetDiscoveryIntervalOverride).
type DiscoveryIntervalOverride struct {
	Minutes   int       `json:"minutes"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Validate bounds the override.
func (o DiscoveryIntervalOverride) Validate() error {
	if o.Minutes < 0 {
		return fmt.Errorf("discovery interval must not be negative")
	}
	if o.Minutes != 0 && o.Minutes < MinDiscoveryIntervalMinutes {
		return fmt.Errorf("discovery interval must be at least %d minute", MinDiscoveryIntervalMinutes)
	}
	if o.Minutes > MaxDiscoveryIntervalMinutes {
		return fmt.Errorf("discovery interval must not exceed %d minutes (7 days)", MaxDiscoveryIntervalMinutes)
	}
	return nil
}

// DiscoveryIntervalOverride returns the stored override, or the zero value
// (Minutes 0: inherit the environment default) when none has been set.
func (s *Store) DiscoveryIntervalOverride(ctx context.Context) (DiscoveryIntervalOverride, error) {
	raw, updatedAt, err := s.systemSetting(ctx, SettingDiscoveryInterval)
	if errors.Is(err, ErrNotFound) {
		return DiscoveryIntervalOverride{}, nil
	}
	if err != nil {
		return DiscoveryIntervalOverride{}, err
	}
	var out DiscoveryIntervalOverride
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return DiscoveryIntervalOverride{}, fmt.Errorf("decode %s setting: %w", SettingDiscoveryInterval, err)
	}
	out.UpdatedAt = updatedAt
	return out, nil
}

// SetDiscoveryIntervalOverride replaces the stored override. Minutes == 0
// removes the row, so "revert to the environment" and "never overridden" are
// the same state.
func (s *Store) SetDiscoveryIntervalOverride(ctx context.Context, o DiscoveryIntervalOverride) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Minutes == 0 {
		return s.deleteSystemSetting(ctx, SettingDiscoveryInterval)
	}
	encoded, err := json.Marshal(DiscoveryIntervalOverride{Minutes: o.Minutes})
	if err != nil {
		return fmt.Errorf("encode %s setting: %w", SettingDiscoveryInterval, err)
	}
	return s.putSystemSetting(ctx, SettingDiscoveryInterval, string(encoded))
}

// ClearDiscoveryIntervalOverride reverts to the environment default.
func (s *Store) ClearDiscoveryIntervalOverride(ctx context.Context) error {
	return s.deleteSystemSetting(ctx, SettingDiscoveryInterval)
}

func (s *Store) systemSetting(ctx context.Context, key string) (string, time.Time, error) {
	var value, updated string
	err := s.queryRow(ctx, `SELECT value, updated_at FROM system_setting WHERE key = ?`, key).Scan(&value, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read system setting %s: %w", key, err)
	}
	return value, ParseTime(updated), nil
}

func (s *Store) putSystemSetting(ctx context.Context, key, value string) error {
	// Atomic upsert: both PostgreSQL and SQLite support ON CONFLICT ... DO
	// UPDATE, and two admins saving concurrently must never race a
	// SELECT-then-INSERT into a primary-key violation.
	if err := s.exec(ctx, `INSERT INTO system_setting (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, FormatTime(nowUTC())); err != nil {
		return fmt.Errorf("write system setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) deleteSystemSetting(ctx context.Context, key string) error {
	if err := s.exec(ctx, `DELETE FROM system_setting WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete system setting %s: %w", key, err)
	}
	return nil
}

// --- Docs feedback ----------------------------------------------------------

// AddDocsFeedback stores anonymous documentation feedback.
func (s *Store) AddDocsFeedback(ctx context.Context, page string, helpful bool, note string) error {
	return s.exec(ctx, `INSERT INTO docs_feedback (id, page, helpful, note, resolved_at, created_at) VALUES (?,?,?,?,'',?)`,
		NewID(), page, boolInt(helpful), note, FormatTime(nowUTC()))
}

// ListDocsFeedback returns recent unresolved feedback for the admin panel.
func (s *Store) ListDocsFeedback(ctx context.Context) ([]*DocsFeedback, error) {
	rows, err := s.query(ctx, `SELECT id, page, helpful, note, resolved_at, created_at FROM docs_feedback WHERE resolved_at = '' ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*DocsFeedback{}
	for rows.Next() {
		var f DocsFeedback
		var helpful int
		var resolved, created string
		if err := rows.Scan(&f.ID, &f.Page, &helpful, &f.Note, &resolved, &created); err != nil {
			return nil, fmt.Errorf("scan docs feedback: %w", err)
		}
		f.Helpful = helpful == 1
		f.ResolvedAt = ParseTime(resolved)
		f.CreatedAt = ParseTime(created)
		out = append(out, &f)
	}
	return out, rows.Err()
}

// ResolveDocsFeedback marks a feedback item handled.
func (s *Store) ResolveDocsFeedback(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE docs_feedback SET resolved_at = ? WHERE id = ?`, FormatTime(nowUTC()), id)
}

// PurgeDocsFeedback deletes feedback past its retention window.
func (s *Store) PurgeDocsFeedback(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM docs_feedback WHERE created_at < ?`), FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge docs feedback: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
