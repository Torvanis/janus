package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// subscriptionMigration adds personal provider subscriptions: a user's own
// vendor plan (SuperGrok, …) connected through the vendor's device sign-in.
//
// subscription_connection holds one row per (user, provider). Tokens arrive
// already encrypted; this layer never sees plaintext. subscription_pending
// holds an in-flight device sign-in: it lives in the database, not a process
// map, because the SPA's start and poll calls can land on different replicas.
//
// usage_event.subscription_id attributes a request to the connection that
// paid for it, so reports can separate "the organization's providers" from
// "a person's own plan" without inferring it from model names.
var subscriptionMigration = migration{name: "0040_personal_subscriptions", stmt: []string{
	`CREATE TABLE IF NOT EXISTS subscription_connection (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		provider TEXT NOT NULL,
		account_subject TEXT NOT NULL DEFAULT '',
		account_email TEXT NOT NULL DEFAULT '',
		account_name TEXT NOT NULL DEFAULT '',
		access_token_encrypted TEXT NOT NULL DEFAULT '',
		refresh_token_encrypted TEXT NOT NULL DEFAULT '',
		access_expires_at TEXT NOT NULL DEFAULT '',
		models_json TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL DEFAULT 'active',
		last_error TEXT NOT NULL DEFAULT '',
		last_used_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE (user_id, provider)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subscription_connection_account ON subscription_connection(provider, account_subject)`,
	`CREATE TABLE IF NOT EXISTS subscription_pending (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		provider TEXT NOT NULL,
		device_code_encrypted TEXT NOT NULL,
		user_code TEXT NOT NULL,
		verification_uri TEXT NOT NULL,
		verification_uri_complete TEXT NOT NULL,
		interval_seconds INTEGER NOT NULL,
		next_poll_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subscription_pending_user ON subscription_pending(user_id)`,
	`ALTER TABLE usage_event ADD COLUMN subscription_id TEXT NOT NULL DEFAULT ''`,
	// Partial: only personal traffic is indexed, so organization traffic
	// (almost every row) pays nothing. Serves the source filter and the
	// dashboard's "is this personal model name legitimate" lookup.
	`CREATE INDEX IF NOT EXISTS idx_usage_event_subscription ON usage_event(subscription_id, model_name) WHERE subscription_id <> ''`,
}}

// subscriptionSelectionMigration lets a user choose which of their plan's
// models Janus offers (a plan can expose dozens; most people want a few).
// Connections that existed before the column keep every model they had, so
// nothing disappears from a client that already uses one; new connections
// start with none selected.
//
// It also introduces the organization-wide master switch. An organization
// that had already enabled a provider keeps the feature on; everyone else
// starts with it off.
var subscriptionSelectionMigration = migration{name: "0041_subscription_model_selection", stmt: []string{
	`ALTER TABLE subscription_connection ADD COLUMN selected_models_json TEXT NOT NULL DEFAULT '[]'`,
	`UPDATE subscription_connection SET selected_models_json = models_json`,
	`INSERT INTO system_setting (key, value, updated_at)
		SELECT 'personal_subscriptions', '{"enabled":true}', '2026-09-30T00:00:00.000000000Z'
		WHERE EXISTS (SELECT 1 FROM system_setting WHERE key = 'subscription_providers' AND value LIKE '%true%')
		ON CONFLICT (key) DO NOTHING`,
}}

// subscriptionHealthMigration adds what the background check needs: when a
// connection was last verified (and what went wrong, if it was transient),
// the vendor's own plan-usage snapshot, and small per-account routing state
// (GitHub Copilot serves individual and business accounts from different
// hosts).
var subscriptionHealthMigration = migration{name: "0042_subscription_health", stmt: []string{
	`ALTER TABLE subscription_connection ADD COLUMN meta_json TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE subscription_connection ADD COLUMN checked_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE subscription_connection ADD COLUMN check_error TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE subscription_connection ADD COLUMN usage_json TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE subscription_connection ADD COLUMN usage_at TEXT NOT NULL DEFAULT ''`,
}}

// subscriptionReasoningMigration stores, per connection, what Janus knows
// about each model's reasoning setting (from the vendor catalog, or learned
// when the vendor refused a value), so requests are fitted to the model.
// It also records on the usage event when Janus changed the requested
// reasoning effort, so an admin can see why a model ignored "high".
var subscriptionReasoningMigration = migration{name: "0043_subscription_reasoning", stmt: []string{
	`ALTER TABLE subscription_connection ADD COLUMN reasoning_json TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE usage_event ADD COLUMN reasoning_adjustment TEXT NOT NULL DEFAULT ''`,
}}

// Subscription connection states.
const (
	SubscriptionActive         = "active"
	SubscriptionReauthRequired = "reauth_required"
)

// SettingSubscriptionProviders is the system_setting key holding the
// administrator's per-provider enablement ({"xai": true}). Absent = every
// provider off: personal subscriptions are opt-in per organization.
const SettingSubscriptionProviders = "subscription_providers"

// SettingPersonalSubscriptions is the organization-wide master switch
// ({"enabled": true}). Off hides the feature everywhere — the Subscriptions
// page, my/* models, the Security Gateway scope — without deleting anyone's
// connection, so turning it back on restores exactly what was there.
const SettingPersonalSubscriptions = "personal_subscriptions"

// SubscriptionConnection is one user's connected vendor plan.
type SubscriptionConnection struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	UserEmail      string `json:"user_email,omitempty"`
	Provider       string `json:"provider"`
	AccountSubject string `json:"-"`
	AccountEmail   string `json:"account_email"`
	AccountName    string `json:"account_name"`
	// AccessExpiresAt is when the current access credential stops working.
	// Zero means it does not expire on a timer (GitHub OAuth-app tokens,
	// pasted API keys).
	AccessExpiresAt time.Time `json:"access_expires_at,omitzero"`
	// AutoRenews is true when Janus holds a refresh token and renews the
	// access credential itself.
	AutoRenews bool `json:"auto_renews"`
	// Meta is provider routing state (never secrets).
	Meta map[string]string `json:"-"`
	// CheckedAt is the last successful or failed background verification;
	// CheckError is set when the last one failed for a reason that did not
	// (yet) require reconnecting.
	CheckedAt  time.Time `json:"checked_at,omitzero"`
	CheckError string    `json:"check_error,omitempty"`
	// PlanUsage is the vendor's plan-usage snapshot (subscription.PlanUsage
	// JSON), as of UsageAt. Empty when the vendor publishes none.
	PlanUsage json.RawMessage `json:"plan_usage,omitempty"`
	UsageAt   time.Time       `json:"usage_at,omitzero"`
	// Reasoning is the per-model reasoning facts (subscription.Reasoning
	// keyed by model), JSON.
	Reasoning json.RawMessage `json:"-"`
	Models    []string        `json:"models"`
	// SelectedModels is the subset of Models the owner chose to use. Only
	// these are listed in /v1/models and accepted by the proxy.
	SelectedModels []string  `json:"selected_models"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error"`
	LastUsedAt     time.Time `json:"last_used_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	encryptedAccess  string
	encryptedRefresh string
}

// EncryptedAccessToken returns the stored ciphertext of the access token.
func (c *SubscriptionConnection) EncryptedAccessToken() string { return c.encryptedAccess }

// EncryptedRefreshToken returns the stored ciphertext of the refresh token.
func (c *SubscriptionConnection) EncryptedRefreshToken() string { return c.encryptedRefresh }

// SubscriptionPending is an in-flight device sign-in.
type SubscriptionPending struct {
	ID                      string
	UserID                  string
	Provider                string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	NextPollAt              time.Time
	ExpiresAt               time.Time
	CreatedAt               time.Time

	encryptedDeviceCode string
}

// EncryptedDeviceCode returns the stored ciphertext of the device code.
func (p *SubscriptionPending) EncryptedDeviceCode() string { return p.encryptedDeviceCode }

const subscriptionColumns = `c.id, c.user_id, COALESCE(u.email,''), c.provider, c.account_subject, c.account_email, c.account_name,
	c.access_token_encrypted, c.refresh_token_encrypted, c.access_expires_at, c.models_json, c.selected_models_json, c.status, c.last_error,
	c.last_used_at, c.created_at, c.updated_at, c.meta_json, c.checked_at, c.check_error, c.usage_json, c.usage_at, c.reasoning_json`

const subscriptionFrom = ` FROM subscription_connection c LEFT JOIN app_user u ON u.id = c.user_id `

func scanSubscription(scan func(...any) error) (*SubscriptionConnection, error) {
	var c SubscriptionConnection
	var expires, models, selected, used, created, updated, meta, checked, usage, usageAt, reasoning string
	if err := scan(&c.ID, &c.UserID, &c.UserEmail, &c.Provider, &c.AccountSubject, &c.AccountEmail, &c.AccountName,
		&c.encryptedAccess, &c.encryptedRefresh, &expires, &models, &selected, &c.Status, &c.LastError,
		&used, &created, &updated, &meta, &checked, &c.CheckError, &usage, &usageAt, &reasoning); err != nil {
		return nil, err
	}
	if reasoning != "" && json.Valid([]byte(reasoning)) {
		c.Reasoning = json.RawMessage(reasoning)
	}
	c.AutoRenews = c.encryptedRefresh != ""
	c.Meta = map[string]string{}
	_ = json.Unmarshal([]byte(meta), &c.Meta)
	c.CheckedAt = ParseTime(checked)
	c.UsageAt = ParseTime(usageAt)
	if usage != "" && json.Valid([]byte(usage)) {
		c.PlanUsage = json.RawMessage(usage)
	}
	c.AccessExpiresAt = ParseTime(expires)
	c.LastUsedAt = ParseTime(used)
	c.CreatedAt = ParseTime(created)
	c.UpdatedAt = ParseTime(updated)
	c.Models = []string{}
	_ = json.Unmarshal([]byte(models), &c.Models)
	c.SelectedModels = []string{}
	_ = json.Unmarshal([]byte(selected), &c.SelectedModels)
	return &c, nil
}

func (s *Store) listSubscriptions(ctx context.Context, where string, args ...any) ([]*SubscriptionConnection, error) {
	rows, err := s.query(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+where+` ORDER BY c.provider, u.email, c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SubscriptionConnection{}
	for rows.Next() {
		c, err := scanSubscription(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListUserSubscriptions returns the caller's own connections.
func (s *Store) ListUserSubscriptions(ctx context.Context, userID string) ([]*SubscriptionConnection, error) {
	return s.listSubscriptions(ctx, `WHERE c.user_id = ?`, userID)
}

// ListAllSubscriptions returns every connection (administrator view).
func (s *Store) ListAllSubscriptions(ctx context.Context) ([]*SubscriptionConnection, error) {
	return s.listSubscriptions(ctx, `WHERE 1=1`)
}

// SubscriptionByID loads one connection.
func (s *Store) SubscriptionByID(ctx context.Context, id string) (*SubscriptionConnection, error) {
	c, err := scanSubscription(s.queryRow(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+`WHERE c.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// UserSubscription loads the caller's connection to one provider.
func (s *Store) UserSubscription(ctx context.Context, userID, provider string) (*SubscriptionConnection, error) {
	c, err := scanSubscription(s.queryRow(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+`WHERE c.user_id = ? AND c.provider = ?`, userID, provider).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// SubscriptionByAccount finds the connection (by any user) to a specific
// vendor account. Used to keep one vendor account bound to one Janus user.
func (s *Store) SubscriptionByAccount(ctx context.Context, provider, subject string) (*SubscriptionConnection, error) {
	c, err := scanSubscription(s.queryRow(ctx, `SELECT `+subscriptionColumns+subscriptionFrom+`WHERE c.provider = ? AND c.account_subject = ?`, provider, subject).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// SubscriptionUpsert is the full credential state written on connect.
type SubscriptionUpsert struct {
	UserID, Provider                   string
	AccountSubject, AccountEmail, Name string
	EncryptedAccess, EncryptedRefresh  string
	AccessExpiresAt                    time.Time
	Meta                               map[string]string
	Models                             []string
	// SelectedModels must be a subset of Models; the caller decides it
	// (empty on a first connect, the surviving choices on a reconnect).
	SelectedModels []string
	// Reasoning is the per-model reasoning facts JSON (may be empty).
	Reasoning []byte
}

// UpsertSubscription stores (or replaces) a user's connection to a provider.
// Reconnecting the same provider replaces the old credentials in place, so a
// user never holds two live connections to one vendor.
func (s *Store) UpsertSubscription(ctx context.Context, in SubscriptionUpsert) (*SubscriptionConnection, error) {
	models, err := json.Marshal(nonNilStrings(in.Models))
	if err != nil {
		return nil, err
	}
	selected, err := json.Marshal(nonNilStrings(in.SelectedModels))
	if err != nil {
		return nil, err
	}
	if in.Meta == nil {
		in.Meta = map[string]string{}
	}
	meta, err := json.Marshal(in.Meta)
	if err != nil {
		return nil, err
	}
	reasoning := string(in.Reasoning)
	if reasoning == "" || !json.Valid(in.Reasoning) {
		reasoning = "{}"
	}
	now := FormatTime(nowUTC())
	if err := s.exec(ctx, `INSERT INTO subscription_connection
		(id, user_id, provider, account_subject, account_email, account_name, access_token_encrypted, refresh_token_encrypted,
		 access_expires_at, models_json, selected_models_json, status, last_error, last_used_at, created_at, updated_at,
		 meta_json, checked_at, check_error, usage_json, usage_at, reasoning_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'','',?,?,?,?,'','','',?)
		ON CONFLICT (user_id, provider) DO UPDATE SET
		 account_subject = excluded.account_subject, account_email = excluded.account_email, account_name = excluded.account_name,
		 access_token_encrypted = excluded.access_token_encrypted, refresh_token_encrypted = excluded.refresh_token_encrypted,
		 access_expires_at = excluded.access_expires_at, models_json = excluded.models_json,
		 selected_models_json = excluded.selected_models_json, meta_json = excluded.meta_json,
		 checked_at = excluded.checked_at, check_error = '', usage_json = '', usage_at = '', reasoning_json = excluded.reasoning_json,
		 status = excluded.status, last_error = '', updated_at = excluded.updated_at`,
		NewID(), in.UserID, in.Provider, in.AccountSubject, in.AccountEmail, in.Name, in.EncryptedAccess, in.EncryptedRefresh,
		formatExpiry(in.AccessExpiresAt), string(models), string(selected), SubscriptionActive, now, now, string(meta), now, reasoning); err != nil {
		return nil, fmt.Errorf("store subscription: %w", err)
	}
	return s.UserSubscription(ctx, in.UserID, in.Provider)
}

// RotateSubscriptionTokens stores refreshed credentials, but only if the
// refresh token is still the one the caller refreshed from. Vendors rotate
// refresh tokens, so two replicas refreshing at once must not let the loser
// overwrite the winner's (now only valid) token. ok is false when another
// writer got there first; the caller re-reads and uses theirs.
func (s *Store) RotateSubscriptionTokens(ctx context.Context, id, previousRefresh, encryptedAccess, encryptedRefresh string, expires time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE subscription_connection
		SET access_token_encrypted = ?, refresh_token_encrypted = ?, access_expires_at = ?, status = ?, last_error = '', updated_at = ?
		WHERE id = ? AND refresh_token_encrypted = ?`),
		encryptedAccess, encryptedRefresh, formatExpiry(expires), SubscriptionActive, FormatTime(nowUTC()), id, previousRefresh)
	if err != nil {
		return false, fmt.Errorf("rotate subscription tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ExpireSubscriptionAccess forces the next request to refresh (the vendor
// rejected the access token before its advertised expiry).
func (s *Store) ExpireSubscriptionAccess(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE subscription_connection SET access_expires_at = ? WHERE id = ?`, FormatTime(time.Unix(0, 0)), id)
}

// MarkSubscriptionReauth records that the connection can no longer be used
// until the user connects again, and why.
func (s *Store) MarkSubscriptionReauth(ctx context.Context, id, reason string) error {
	return s.exec(ctx, `UPDATE subscription_connection SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		SubscriptionReauthRequired, reason, FormatTime(nowUTC()), id)
}

// SetSubscriptionModels replaces the cached model list and the selection
// together, so a model the vendor withdrew can never stay selected.
func (s *Store) SetSubscriptionModels(ctx context.Context, id string, models, selected []string) error {
	encoded, err := json.Marshal(nonNilStrings(models))
	if err != nil {
		return err
	}
	chosen, err := json.Marshal(nonNilStrings(selected))
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE subscription_connection SET models_json = ?, selected_models_json = ?, updated_at = ? WHERE id = ?`,
		string(encoded), string(chosen), FormatTime(nowUTC()), id)
}

// SetSubscriptionReasoning replaces a connection's per-model reasoning
// facts (the caller merges catalog and learned facts).
func (s *Store) SetSubscriptionReasoning(ctx context.Context, id string, reasoning []byte) error {
	if len(reasoning) == 0 || !json.Valid(reasoning) {
		reasoning = []byte("{}")
	}
	return s.exec(ctx, `UPDATE subscription_connection SET reasoning_json = ? WHERE id = ?`, string(reasoning), id)
}

// SetSubscriptionSelectedModels stores the owner's choice of models. The
// caller validates it against the connection's available models.
func (s *Store) SetSubscriptionSelectedModels(ctx context.Context, id string, selected []string) error {
	chosen, err := json.Marshal(nonNilStrings(selected))
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE subscription_connection SET selected_models_json = ?, updated_at = ? WHERE id = ?`,
		string(chosen), FormatTime(nowUTC()), id)
}

// ClaimSubscriptionCheck reserves the background verification of one
// connection. It succeeds only if checked_at is still what the caller read,
// so two replicas never check (and refresh) the same connection at once.
func (s *Store) ClaimSubscriptionCheck(ctx context.Context, id string, previous time.Time) (bool, error) {
	prev := ""
	if !previous.IsZero() {
		prev = FormatTime(previous)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE subscription_connection SET checked_at = ? WHERE id = ? AND checked_at = ?`),
		FormatTime(nowUTC()), id, prev)
	if err != nil {
		return false, fmt.Errorf("claim subscription check: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RecordSubscriptionCheck stores the outcome of a verification. checkError
// is empty on success; usage is the vendor's plan-usage JSON, or empty to
// keep the previous snapshot.
func (s *Store) RecordSubscriptionCheck(ctx context.Context, id, checkError string, usage []byte) error {
	now := FormatTime(nowUTC())
	if len(usage) > 0 {
		return s.exec(ctx, `UPDATE subscription_connection SET checked_at = ?, check_error = ?, usage_json = ?, usage_at = ? WHERE id = ?`,
			now, checkError, string(usage), now, id)
	}
	return s.exec(ctx, `UPDATE subscription_connection SET checked_at = ?, check_error = ? WHERE id = ?`, now, checkError, id)
}

// SubscriptionActivity is the organization-side view of one connection's
// traffic through Janus.
type SubscriptionActivity struct {
	Requests  int64 `json:"requests"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

// SubscriptionActivitySince totals the successful and failed requests Janus
// sent through a connection since the given instant.
func (s *Store) SubscriptionActivitySince(ctx context.Context, id string, since time.Time) (SubscriptionActivity, error) {
	var a SubscriptionActivity
	err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM usage_event WHERE subscription_id = ? AND created_at >= ?`, id, FormatTime(since)).Scan(&a.Requests, &a.TokensIn, &a.TokensOut)
	return a, err
}

// formatExpiry stores a zero expiry (credential that does not expire on a
// timer) as the empty string rather than year one.
func formatExpiry(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return FormatTime(t)
}

// TouchSubscription records use. Best-effort bookkeeping for the UI.
func (s *Store) TouchSubscription(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE subscription_connection SET last_used_at = ? WHERE id = ?`, FormatTime(nowUTC()), id)
}

// DeleteSubscription removes a connection. Historical usage keeps its
// subscription_id so reports still attribute past traffic correctly.
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM subscription_connection WHERE id = ?`, id)
}

// CreateSubscriptionPending records an in-flight device sign-in. A user has
// at most one pending sign-in per provider; starting again replaces it.
func (s *Store) CreateSubscriptionPending(ctx context.Context, p *SubscriptionPending, encryptedDeviceCode string) error {
	p.ID = NewID()
	p.CreatedAt = nowUTC()
	return s.InTx(ctx, func(tx *sql.Tx) error {
		s := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		if err := s.exec(ctx, `DELETE FROM subscription_pending WHERE (user_id = ? AND provider = ?) OR expires_at < ?`,
			p.UserID, p.Provider, FormatTime(nowUTC())); err != nil {
			return err
		}
		return s.exec(ctx, `INSERT INTO subscription_pending
			(id, user_id, provider, device_code_encrypted, user_code, verification_uri, verification_uri_complete, interval_seconds, next_poll_at, expires_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.UserID, p.Provider, encryptedDeviceCode, p.UserCode, p.VerificationURI, p.VerificationURIComplete,
			int(p.Interval/time.Second), FormatTime(p.NextPollAt), FormatTime(p.ExpiresAt), FormatTime(p.CreatedAt))
	})
}

// SubscriptionPendingByID loads a pending sign-in owned by userID.
func (s *Store) SubscriptionPendingByID(ctx context.Context, id, userID string) (*SubscriptionPending, error) {
	var p SubscriptionPending
	var interval int
	var next, expires, created string
	err := s.queryRow(ctx, `SELECT id, user_id, provider, device_code_encrypted, user_code, verification_uri, verification_uri_complete,
		interval_seconds, next_poll_at, expires_at, created_at FROM subscription_pending WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&p.ID, &p.UserID, &p.Provider, &p.encryptedDeviceCode, &p.UserCode, &p.VerificationURI, &p.VerificationURIComplete,
			&interval, &next, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Interval = time.Duration(interval) * time.Second
	p.NextPollAt, p.ExpiresAt, p.CreatedAt = ParseTime(next), ParseTime(expires), ParseTime(created)
	return &p, nil
}

// ClaimSubscriptionPoll reserves the right to poll the vendor now. It
// enforces the vendor's polling interval across replicas: the update only
// succeeds when next_poll_at has passed, so two tabs or two replicas cannot
// both hit the token endpoint inside one interval (which earns slow_down).
func (s *Store) ClaimSubscriptionPoll(ctx context.Context, id string, interval time.Duration) (bool, error) {
	now := nowUTC()
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE subscription_pending SET next_poll_at = ?, interval_seconds = ?
		WHERE id = ? AND next_poll_at <= ?`), FormatTime(now.Add(interval)), int(interval/time.Second), id, FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("claim subscription poll: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteSubscriptionPending removes a finished or abandoned sign-in.
func (s *Store) DeleteSubscriptionPending(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM subscription_pending WHERE id = ?`, id)
}

// SubscriptionProviderSettings returns the administrator's per-provider
// enablement. Missing providers are disabled.
func (s *Store) SubscriptionProviderSettings(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	raw, _, err := s.systemSetting(ctx, SettingSubscriptionProviders)
	if errors.Is(err, ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode %s setting: %w", SettingSubscriptionProviders, err)
	}
	return out, nil
}

// PersonalSubscriptionsEnabled reads the organization-wide master switch.
// Absent = off.
func (s *Store) PersonalSubscriptionsEnabled(ctx context.Context) (bool, error) {
	raw, _, err := s.systemSetting(ctx, SettingPersonalSubscriptions)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var doc struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return false, fmt.Errorf("decode %s setting: %w", SettingPersonalSubscriptions, err)
	}
	return doc.Enabled, nil
}

// SetPersonalSubscriptionsEnabled stores the master switch.
func (s *Store) SetPersonalSubscriptionsEnabled(ctx context.Context, enabled bool) error {
	encoded, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingPersonalSubscriptions, string(encoded))
}

// SetSubscriptionProviderSettings stores the per-provider enablement.
func (s *Store) SetSubscriptionProviderSettings(ctx context.Context, enabled map[string]bool) error {
	encoded, err := json.Marshal(enabled)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingSubscriptionProviders, string(encoded))
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
