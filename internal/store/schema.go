package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one schema change set. stmt runs on every dialect. pgStmt runs
// only on PostgreSQL — the dialect layer in store.go rewrites `?` placeholders
// but never rewrites DDL types, so engine-specific DDL (e.g. ALTER COLUMN ...
// TYPE, which SQLite neither supports nor needs) must be declared explicitly.
// down/pgDown reverse the migration; a migration with neither is forward-only.
type migration struct {
	name   string
	stmt   []string
	pgStmt []string
	down   []string
	pgDown []string
}

// migrations are applied in order exactly once and recorded in schema_migration.
// Each entry is immutable after release; corrections ship as a new migration.
var migrations = []migration{
	{
		name: "0001_core_identity",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS app_user (
				id TEXT PRIMARY KEY,
				auth_provider_id TEXT NOT NULL UNIQUE,
				email TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				role TEXT NOT NULL DEFAULT 'user',
				is_active INTEGER NOT NULL DEFAULT 1,
				timezone TEXT NOT NULL DEFAULT '',
				locale TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				last_login_at TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_app_user_email ON app_user(email)`,
			`CREATE TABLE IF NOT EXISTS user_group (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				from_idp INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS group_member (
				group_id TEXT NOT NULL,
				user_id TEXT NOT NULL,
				PRIMARY KEY (group_id, user_id)
			)`,
			`CREATE TABLE IF NOT EXISTS team (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				lead_user_id TEXT NOT NULL DEFAULT '',
				lead_can_edit_quotas INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS team_member (
				team_id TEXT NOT NULL,
				user_id TEXT NOT NULL,
				PRIMARY KEY (team_id, user_id)
			)`,
		},
	},
	{
		name: "0002_tokens",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS api_token (
				id TEXT PRIMARY KEY,
				user_id TEXT NOT NULL,
				token_digest TEXT NOT NULL UNIQUE,
				prefix TEXT NOT NULL,
				description TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				last_used_at TEXT NOT NULL DEFAULT '',
				revoked_at TEXT NOT NULL DEFAULT ''  -- '' = active (unrevoked); Token.MarshalJSON mirrors this on the wire ("" — never Go's RFC 3339 zero time)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_api_token_user ON api_token(user_id)`,
		},
	},
	{
		name: "0003_audit",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS audit_log (
				id TEXT PRIMARY KEY,
				actor_user_id TEXT NOT NULL DEFAULT '',
				actor_label TEXT NOT NULL DEFAULT '',
				action TEXT NOT NULL,
				resource_type TEXT NOT NULL,
				resource_id TEXT NOT NULL DEFAULT '',
				old_value TEXT NOT NULL DEFAULT '',
				new_value TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor_user_id, created_at)`,
		},
	},
	{
		name: "0004_upstreams_models",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS upstream (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				adapter_type TEXT NOT NULL,
				base_url TEXT NOT NULL,
				api_key_encrypted TEXT NOT NULL DEFAULT '',
				api_key_mask TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1,
				deleted_at TEXT NOT NULL DEFAULT '',
				last_check_at TEXT NOT NULL DEFAULT '',
				last_error TEXT NOT NULL DEFAULT '',
				last_latency_ms INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS model (
				id TEXT PRIMARY KEY,
				upstream_id TEXT NOT NULL,
				name TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending_approval',
				modalities TEXT NOT NULL DEFAULT 'chat',
				rate_in_nanousd INTEGER NOT NULL DEFAULT 0,
				rate_out_nanousd INTEGER NOT NULL DEFAULT 0,
				rate_cached_nanousd INTEGER NOT NULL DEFAULT 0,
				rate_effective_from TEXT NOT NULL DEFAULT '',
				discovered_at TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_model_upstream_name ON model(upstream_id, name)`,
			`CREATE TABLE IF NOT EXISTS rate_card_version (
				id TEXT PRIMARY KEY,
				model_id TEXT NOT NULL,
				rate_in_nanousd INTEGER NOT NULL DEFAULT 0,
				rate_out_nanousd INTEGER NOT NULL DEFAULT 0,
				rate_cached_nanousd INTEGER NOT NULL DEFAULT 0,
				effective_from TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_rate_card_model ON rate_card_version(model_id, effective_from)`,
		},
	},
	{
		name: "0005_grants",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS model_grant (
				id TEXT PRIMARY KEY,
				model_id TEXT NOT NULL,
				grantee_type TEXT NOT NULL,
				grantee_id TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_grant_model ON model_grant(model_id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_grant_unique ON model_grant(model_id, grantee_type, grantee_id)`,
		},
	},
	{
		name: "0006_usage",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS usage_event (
				id TEXT PRIMARY KEY,
				created_at TEXT NOT NULL,
				user_id TEXT NOT NULL DEFAULT '',
				token_id TEXT NOT NULL DEFAULT '',
				team_ids TEXT NOT NULL DEFAULT '',
				upstream_id TEXT NOT NULL DEFAULT '',
				model_id TEXT NOT NULL DEFAULT '',
				model_name TEXT NOT NULL DEFAULT '',
				endpoint_path TEXT NOT NULL DEFAULT '',
				http_method TEXT NOT NULL DEFAULT '',
				modality TEXT NOT NULL DEFAULT 'chat',
				streaming INTEGER NOT NULL DEFAULT 0,
				request_bytes INTEGER NOT NULL DEFAULT 0,
				response_bytes INTEGER NOT NULL DEFAULT 0,
				attachment_count INTEGER NOT NULL DEFAULT 0,
				tokens_in INTEGER NOT NULL DEFAULT 0,
				tokens_out INTEGER NOT NULL DEFAULT 0,
				tokens_cached INTEGER NOT NULL DEFAULT 0,
				token_accounting_method TEXT NOT NULL DEFAULT 'upstream_reported',
				cost_nanousd INTEGER NOT NULL DEFAULT 0,
				finish_reason TEXT NOT NULL DEFAULT '',
				http_status INTEGER NOT NULL DEFAULT 0,
				latency_ms INTEGER NOT NULL DEFAULT 0,
				ttfb_ms INTEGER NOT NULL DEFAULT 0,
				upstream_latency_ms INTEGER NOT NULL DEFAULT 0,
				client_user_agent TEXT NOT NULL DEFAULT '',
				client_ip TEXT NOT NULL DEFAULT '',
				x_forwarded_for TEXT NOT NULL DEFAULT '',
				referer TEXT NOT NULL DEFAULT '',
				error_code TEXT NOT NULL DEFAULT '',
				quota_violated INTEGER NOT NULL DEFAULT 0,
				blocking_rule_id TEXT NOT NULL DEFAULT '',
				request_id TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_user_time ON usage_event(user_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_model_time ON usage_event(model_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_upstream_time ON usage_event(upstream_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_usage_time ON usage_event(created_at)`,
		},
	},
	{
		name: "0007_quotas",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS quota (
				id TEXT PRIMARY KEY,
				subject_type TEXT NOT NULL,
				subject_id TEXT NOT NULL DEFAULT '',
				model_id TEXT NOT NULL DEFAULT '',
				metric TEXT NOT NULL,
				limit_value INTEGER NOT NULL,
				window_kind TEXT NOT NULL,
				breach_behavior TEXT NOT NULL DEFAULT 'let_finish',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_quota_subject ON quota(subject_type, subject_id)`,
			`CREATE TABLE IF NOT EXISTS quota_ledger (
				quota_id TEXT NOT NULL,
				window_start TEXT NOT NULL,
				current_value INTEGER NOT NULL DEFAULT 0,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (quota_id, window_start)
			)`,
			`CREATE TABLE IF NOT EXISTS quota_alert_state (
				quota_id TEXT NOT NULL,
				window_start TEXT NOT NULL,
				threshold INTEGER NOT NULL,
				notified_at TEXT NOT NULL,
				PRIMARY KEY (quota_id, window_start, threshold)
			)`,
		},
	},
	{
		name: "0008_rules_alerts_flags",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS blocking_rule (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL DEFAULT '',
				combinator TEXT NOT NULL DEFAULT 'and',
				reason TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1,
				clauses TEXT NOT NULL DEFAULT '[]',
				hit_count INTEGER NOT NULL DEFAULT 0,
				last_hit_at TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS alert_rule (
				id TEXT PRIMARY KEY,
				trigger_kind TEXT NOT NULL,
				severity TEXT NOT NULL DEFAULT 'warning',
				channels TEXT NOT NULL DEFAULT 'in_app',
				webhook_url TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS notification (
				id TEXT PRIMARY KEY,
				user_id TEXT NOT NULL DEFAULT '',
				severity TEXT NOT NULL DEFAULT 'info',
				title TEXT NOT NULL,
				body TEXT NOT NULL DEFAULT '',
				read_at TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_notification_user ON notification(user_id, created_at)`,
			`CREATE TABLE IF NOT EXISTS feature_flag (
				name TEXT PRIMARY KEY,
				enabled INTEGER NOT NULL DEFAULT 0,
				updated_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS docs_feedback (
				id TEXT PRIMARY KEY,
				page TEXT NOT NULL,
				helpful INTEGER NOT NULL,
				note TEXT NOT NULL DEFAULT '',
				resolved_at TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS rate_limit_rule (
				id TEXT PRIMARY KEY,
				subject_type TEXT NOT NULL DEFAULT 'user',
				subject_id TEXT NOT NULL DEFAULT '',
				endpoint TEXT NOT NULL DEFAULT '*',
				requests_per_minute INTEGER NOT NULL,
				created_at TEXT NOT NULL
			)`,
		},
	},
	{
		// Browser sessions and in-flight OIDC sign-ins live in the shared
		// database so any replica can serve any request: sessions survive a
		// pod restart and the OIDC callback may land on a replica other than
		// the one that started the flow.
		name: "0009_sessions_oidc_state",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS web_session (
				id TEXT PRIMARY KEY,
				user_id TEXT NOT NULL,
				csrf_token TEXT NOT NULL,
				created_at TEXT NOT NULL,
				last_seen_at TEXT NOT NULL,
				absolute_ends TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_web_session_user ON web_session(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_web_session_ends ON web_session(absolute_ends)`,
			`CREATE TABLE IF NOT EXISTS oidc_state (
				state TEXT PRIMARY KEY,
				verifier TEXT NOT NULL,
				nonce TEXT NOT NULL,
				redirect_to TEXT NOT NULL DEFAULT '',
				redirect_uri TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				expires_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_oidc_state_expires ON oidc_state(expires_at)`,
		},
	},
	{
		// admins may configure the warning threshold percentages per
		// quota rule. Stored as a comma-separated ascending list (e.g.
		// "50,90"); empty string means the 80/95 defaults.
		name: "0010_quota_alert_thresholds",
		stmt: []string{
			`ALTER TABLE quota ADD COLUMN alert_thresholds TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Administrator capability now composes from three sources with OR:
		// bootstrap email (sticky role promotion), explicit grant via the
		// admin UI (sticky role write), and IdP admin-group membership. The
		// group-derived signal is stored separately from role so it can be
		// re-evaluated on every sign-in (leaving the group revokes it) without
		// ever touching the sticky role column. Additive with a safe default:
		// every pre-existing row backfills to FALSE, so nobody gains or loses
		// admin by applying this migration. BOOLEAN + FALSE parse identically
		// on SQLite (NUMERIC affinity, FALSE == 0) and PostgreSQL.
		name: "0011_admin_via_group",
		stmt: []string{
			`ALTER TABLE app_user ADD COLUMN admin_via_group BOOLEAN NOT NULL DEFAULT FALSE`,
		},
	},
	{
		// Admins may rename a model for downstream presentation ("display
		// name"). Empty means the native upstream name is presented.
		// Uniqueness against other enabled models' names and display names
		// is application-enforced in SetModelDisplayName — not a schema
		// constraint — so renames on disabled/stale rows never block
		// discovery or curation.
		name: "0012_model_display_name",
		stmt: []string{
			`ALTER TABLE model ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_model_display_name ON model(display_name)`,
		},
	},
	{
		// 0013: 64-bit money columns + cache-write rate/token columns.
		//
		// Root cause of the "cannot save Fable-class rates" 500: the DDL
		// above declares every money/counter column as INTEGER, which SQLite
		// stores as a 64-bit integer but PostgreSQL as 32-bit int4. Any value
		// above 2,147,483,647 nano-USD (~$2.147/MTok) — e.g. a $50/MTok rate
		// = 50,000,000,000 nano-USD — overflows on insert and surfaces as a
		// 500. The store.go dialect layer rewrites only `?` placeholders,
		// never types, so the widening must be explicit PostgreSQL DDL
		// (pgStmt). On SQLite the widening is a no-op: its INTEGER is already
		// 64-bit and it does not support ALTER COLUMN TYPE.
		//
		// The same migration adds cache-write rate columns to model and
		// rate_card_version, and cache-write token counts to usage_event, so
		// later releases can price Anthropic-style 5-minute/1-hour prompt
		// cache writes and reprice historical usage when those rates change.
		// BIGINT has INTEGER affinity on SQLite, so the ADD COLUMN statements
		// are shared across dialects.
		//
		// Down migration: drops the six added columns on both dialects, then
		// narrows the seven widened columns back to INTEGER on PostgreSQL.
		// The narrowing is LOSSY for out-of-range values: anything outside
		// ±2,147,483,647 is clamped to the int4 bounds by the USING clause.
		name: "0013_bigint_money_cache_write_rates",
		stmt: []string{
			`ALTER TABLE model ADD COLUMN rate_cache_write_5m_nanousd BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE model ADD COLUMN rate_cache_write_1h_nanousd BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE rate_card_version ADD COLUMN rate_cache_write_5m_nanousd BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE rate_card_version ADD COLUMN rate_cache_write_1h_nanousd BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_event ADD COLUMN tokens_cache_write_5m BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_event ADD COLUMN tokens_cache_write_1h BIGINT NOT NULL DEFAULT 0`,
		},
		pgStmt: []string{
			`ALTER TABLE model ALTER COLUMN rate_in_nanousd TYPE BIGINT`,
			`ALTER TABLE model ALTER COLUMN rate_out_nanousd TYPE BIGINT`,
			`ALTER TABLE model ALTER COLUMN rate_cached_nanousd TYPE BIGINT`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_in_nanousd TYPE BIGINT`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_out_nanousd TYPE BIGINT`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_cached_nanousd TYPE BIGINT`,
			// usage_event.cost_nanousd is mode-independent: local-only
			// deployments (JANUS_LOCAL_ONLY) record 0 into the same column,
			// so toggling the mode never requires a schema migration.
			`ALTER TABLE usage_event ALTER COLUMN cost_nanousd TYPE BIGINT`,
			`ALTER TABLE quota ALTER COLUMN limit_value TYPE BIGINT`,
			`ALTER TABLE quota_ledger ALTER COLUMN current_value TYPE BIGINT`,
		},
		down: []string{
			`ALTER TABLE model DROP COLUMN rate_cache_write_5m_nanousd`,
			`ALTER TABLE model DROP COLUMN rate_cache_write_1h_nanousd`,
			`ALTER TABLE rate_card_version DROP COLUMN rate_cache_write_5m_nanousd`,
			`ALTER TABLE rate_card_version DROP COLUMN rate_cache_write_1h_nanousd`,
			`ALTER TABLE usage_event DROP COLUMN tokens_cache_write_5m`,
			`ALTER TABLE usage_event DROP COLUMN tokens_cache_write_1h`,
		},
		pgDown: []string{
			`ALTER TABLE model ALTER COLUMN rate_in_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_in_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE model ALTER COLUMN rate_out_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_out_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE model ALTER COLUMN rate_cached_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_cached_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_in_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_in_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_out_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_out_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE rate_card_version ALTER COLUMN rate_cached_nanousd TYPE INTEGER USING LEAST(GREATEST(rate_cached_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE usage_event ALTER COLUMN cost_nanousd TYPE INTEGER USING LEAST(GREATEST(cost_nanousd, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE quota ALTER COLUMN limit_value TYPE INTEGER USING LEAST(GREATEST(limit_value, -2147483648), 2147483647)::INTEGER`,
			`ALTER TABLE quota_ledger ALTER COLUMN current_value TYPE INTEGER USING LEAST(GREATEST(current_value, -2147483648), 2147483647)::INTEGER`,
		},
	},
	{
		// 0014: per-model context window, in tokens. The catalog never
		// modeled how much input a model accepts, so the UI could not show
		// it. 0 means "unknown" — pre-existing rows and models the seed does
		// not cover read back 0 and the frontend renders a neutral
		// placeholder instead of a misleading number. INTEGER has 64-bit
		// affinity on SQLite and is int4 on PostgreSQL, which comfortably
		// holds any realistic token count (current maximum ~2M tokens), so
		// the ADD COLUMN statement is shared across dialects.
		name: "0014_model_context_window",
		stmt: []string{
			`ALTER TABLE model ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0`,
		},
		down: []string{
			`ALTER TABLE model DROP COLUMN context_window`,
		},
	},
	{
		// 0015: service tokens — a second kind of authenticated principal.
		//
		// A service token belongs to no user. It authenticates non-human
		// integrations (AI applications, websites, batch jobs) on the proxy
		// surface only, and its usage is attributed to the token's NAME the
		// way a user's usage is attributed to their identity.
		//
		// usage_event.service_token_id is the structural basis for the
		// reporting rule: service-token traffic records an EMPTY user_id and
		// a non-empty service_token_id, so people-oriented reports (top
		// users, per-user dashboards) exclude it by construction rather than
		// by every query remembering to filter. Org-wide reports (admin
		// overview, org pulse, analytics) apply no principal filter and
		// therefore include it automatically.
		//
		// Names are unique and immutable-by-convention (renaming is allowed
		// but audited) because they are the reporting key users will see.
		// expires_at gives unattended credentials an optional clock;
		// revoked_at is the manual withdrawal. Rows are never deleted —
		// usage history references them.
		name: "0015_service_tokens",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS service_token (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				description TEXT NOT NULL DEFAULT '',
				token_digest TEXT NOT NULL UNIQUE,
				prefix TEXT NOT NULL,
				created_by_user_id TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				last_used_at TEXT NOT NULL DEFAULT '',
				expires_at TEXT NOT NULL DEFAULT '',
				revoked_at TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_service_token_digest ON service_token(token_digest)`,
			`ALTER TABLE usage_event ADD COLUMN service_token_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_usage_service_token_time ON usage_event(service_token_id, created_at)`,
		},
		down: []string{
			`DROP INDEX IF EXISTS idx_usage_service_token_time`,
			`ALTER TABLE usage_event DROP COLUMN service_token_id`,
			`DROP INDEX IF EXISTS idx_service_token_digest`,
			`DROP TABLE IF EXISTS service_token`,
		},
	},
	{
		// 0016: managed models — admin-defined stable aliases that point at a
		// real catalog model and can be repointed without users noticing.
		//
		// The alias is a first-class grantable entity (model_grant rows carry
		// model_kind = 'managed' and reference managed_model.id), but it is
		// NOT a billing or reporting entity: a proxied request resolves the
		// alias to its target and records the TARGET's model_id/model_name on
		// usage_event, so every model report keeps reflecting real models.
		//
		// usage_event.requested_model_name records which alias the caller
		// actually asked for. It is deliberately a separate column from
		// model_name: it gives admins visibility into alias adoption without
		// contaminating model reporting, which continues to read model_name.
		// Direct (non-alias) requests leave it empty.
		//
		// model_grant.model_kind distinguishes a grant on a real model from a
		// grant on an alias. It defaults to 'model' so every pre-existing
		// grant row keeps its exact meaning.
		name: "0016_managed_models",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS managed_model (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				description TEXT NOT NULL DEFAULT '',
				target_model_id TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'enabled',
				created_by_user_id TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_managed_model_target ON managed_model(target_model_id)`,
			`ALTER TABLE model_grant ADD COLUMN model_kind TEXT NOT NULL DEFAULT 'model'`,
			`ALTER TABLE usage_event ADD COLUMN requested_model_name TEXT NOT NULL DEFAULT ''`,
		},
		down: []string{
			`ALTER TABLE usage_event DROP COLUMN requested_model_name`,
			`ALTER TABLE model_grant DROP COLUMN model_kind`,
			`DROP INDEX IF EXISTS idx_managed_model_target`,
			`DROP TABLE IF EXISTS managed_model`,
		},
	},
	{
		// admin_group holds IdP group names whose members are promoted to
		// admin on sign-in. This is the third and broadest source of admin
		// capability, after the bootstrap email list and an explicit
		// per-user grant. It lives in the database rather than the
		// environment so a bootstrap admin can manage it from the UI without
		// a redeploy; JANUS_ADMIN_GROUPS is still honoured and unioned in, so
		// an existing deployment keeps working unchanged.
		name: "0018_admin_group",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS admin_group (
				name TEXT PRIMARY KEY,
				created_by_user_id TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
		},
		down: []string{
			`DROP TABLE IF EXISTS admin_group`,
		},
	},
	{
		// client_app names the calling application — an agent harness, IDE
		// plugin, or app — when it identifies itself via an attribution
		// header. User-Agent only ever reveals the SDK, so without this a
		// fleet of different agents is indistinguishable in the request log.
		// Additive with a constant default, so on Postgres this is a
		// metadata-only change even on a large usage_event table.
		name: "0017_usage_event_client_app",
		stmt: []string{
			`ALTER TABLE usage_event ADD COLUMN client_app TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// system_setting is a small key/value table for instance-wide
		// operational settings an administrator may change at runtime
		// without a redeploy. The first tenant is upstream_timeouts: the
		// connect / time-to-first-byte / total upstream bounds, which until
		// now could only be changed by restarting with different
		// JANUS_UPSTREAM_*_TIMEOUT_SECONDS values. Living in the shared
		// database means the change survives restarts and reaches every
		// replica through the config cache, exactly like feature flags.
		// Values are JSON documents so each setting can evolve its own shape.
		name: "0019_system_setting",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS system_setting (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
		down: []string{
			`DROP TABLE IF EXISTS system_setting`,
		},
	},
	{
		// Troubleshooting mode. Janus never stores request or response
		// bodies in usage_event, which is right for a metering log but
		// leaves an administrator blind when a request fails. A
		// troubleshooting session is an explicit, time-boxed opt-in: while
		// one is enabled, requests matching its filter (model, principal,
		// upstream, error code, outcome, token size) have their headers and
		// bodies captured under a retention policy the session declares.
		//
		// troubleshooting_session keeps one row per session (the newest is
		// the current one) so the audit of "what was captured when, under
		// which rules" survives the session being switched off. config is
		// a JSON document (filter + retention + storage options) so the
		// rules can evolve without a migration per knob.
		//
		// troubleshooting_capture holds one row per captured request. The
		// filter-relevant columns are denormalised from usage_event so the
		// captures can be listed and purged by criteria without a join, and
		// so a capture stays self-describing after its usage_event row is
		// purged by the ordinary retention job. Bodies are TEXT (base64, or
		// an AES-GCM envelope of the base64 when the session asked for
		// encryption at rest) so the same DDL serves SQLite and PostgreSQL;
		// a disk-backed session leaves them empty and points at files via
		// storage_ref.
		name: "0020_troubleshooting",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS troubleshooting_session (
				id TEXT PRIMARY KEY,
				enabled INTEGER NOT NULL DEFAULT 1,
				config TEXT NOT NULL,
				created_by TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				expires_at TEXT NOT NULL DEFAULT '',
				disabled_at TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_session_created ON troubleshooting_session(created_at)`,
			`CREATE TABLE IF NOT EXISTS troubleshooting_capture (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL,
				usage_event_id TEXT NOT NULL,
				request_id TEXT NOT NULL DEFAULT '',
				captured_at TEXT NOT NULL,
				user_id TEXT NOT NULL DEFAULT '',
				service_token_id TEXT NOT NULL DEFAULT '',
				model_name TEXT NOT NULL DEFAULT '',
				upstream_id TEXT NOT NULL DEFAULT '',
				endpoint_path TEXT NOT NULL DEFAULT '',
				http_status INTEGER NOT NULL DEFAULT 0,
				error_code TEXT NOT NULL DEFAULT '',
				tokens_in BIGINT NOT NULL DEFAULT 0,
				tokens_out BIGINT NOT NULL DEFAULT 0,
				streaming INTEGER NOT NULL DEFAULT 0,
				request_headers TEXT NOT NULL DEFAULT '',
				response_headers TEXT NOT NULL DEFAULT '',
				request_body TEXT NOT NULL DEFAULT '',
				response_body TEXT NOT NULL DEFAULT '',
				request_body_bytes BIGINT NOT NULL DEFAULT 0,
				response_body_bytes BIGINT NOT NULL DEFAULT 0,
				size_bytes BIGINT NOT NULL DEFAULT 0,
				storage_backend TEXT NOT NULL DEFAULT 'database',
				storage_ref TEXT NOT NULL DEFAULT '',
				encrypted INTEGER NOT NULL DEFAULT 0,
				truncated INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_session ON troubleshooting_capture(session_id)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_event ON troubleshooting_capture(usage_event_id)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_captured ON troubleshooting_capture(captured_at)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_model ON troubleshooting_capture(model_name)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_user ON troubleshooting_capture(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_status ON troubleshooting_capture(http_status, error_code)`,
			`CREATE INDEX IF NOT EXISTS idx_troubleshooting_capture_tokens ON troubleshooting_capture(tokens_in, tokens_out)`,
		},
		down: []string{
			`DROP TABLE IF EXISTS troubleshooting_capture`,
			`DROP TABLE IF EXISTS troubleshooting_session`,
		},
	},
	{
		// Throughput per request. tokens_*_per_second are what the proxy
		// returned to the caller in the X-Janus-*Tokens-*-Per-Second
		// headers; throughput_source records whether the provider measured
		// them ('upstream') or the gateway derived them from its own clock
		// ('calculated'), because the two are not comparable. REAL is
		// accepted by both SQLite and PostgreSQL.
		name: "0021_usage_event_throughput",
		stmt: []string{
			`ALTER TABLE usage_event ADD COLUMN tokens_in_per_second REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_event ADD COLUMN tokens_out_per_second REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_event ADD COLUMN throughput_source TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Optional automatic fallback for a managed model. fallback_model_id
		// names a real catalog model (never another alias, so a chain can
		// never form) served when the primary target is unavailable;
		// fallback_triggers is a JSON array of the failure modes that count
		// as "unavailable" (see FallbackTrigger* in types.go). Empty
		// fallback_model_id means no fallback — today's behaviour.
		name: "0022_managed_model_fallback",
		stmt: []string{
			`ALTER TABLE managed_model ADD COLUMN fallback_model_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE managed_model ADD COLUMN fallback_triggers TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_managed_model_fallback ON managed_model(fallback_model_id)`,
		},
		down: []string{
			`DROP INDEX IF EXISTS idx_managed_model_fallback`,
		},
	},
	{
		// fallback_reason records, on the usage event, the failure mode
		// (FallbackTrigger*) that sent a managed-model request to its
		// fallback — '' when the target served. Together with model_name
		// (the model that actually served) and requested_model_name (the
		// alias) it lets the request log and analytics audit every
		// substitution without consulting server logs.
		name: "0023_usage_event_fallback_reason",
		stmt: []string{
			`ALTER TABLE usage_event ADD COLUMN fallback_reason TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Security Gateway: policies (named bundles of checks), bindings
		// (policy → scope, a sibling of grants rather than a field on them),
		// customer term lists (encrypted: the list is itself confidential)
		// and violations (one row per matched check per request; match text
		// is NULL unless the kind is capture-eligible — secrets and term
		// matches are hash-only by construction). model.classifier_role
		// marks a catalog model as a guard classifier, which removes it from
		// the servable catalog and from grant targeting. usage_event gains
		// the summary columns the request log needs without a join.
		name: "0024_security_gateway",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS secgw_policy (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				description TEXT NOT NULL DEFAULT '',
				enabled INTEGER NOT NULL DEFAULT 1,
				mandatory INTEGER NOT NULL DEFAULT 0,
				checks TEXT NOT NULL,
				capture TEXT NOT NULL DEFAULT '{}',
				synthetic_refusal INTEGER NOT NULL DEFAULT 0,
				refusal_text TEXT NOT NULL DEFAULT '',
				created_by TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secgw_binding (
				id TEXT PRIMARY KEY,
				policy_id TEXT NOT NULL REFERENCES secgw_policy(id) ON DELETE CASCADE,
				scope_type TEXT NOT NULL,
				scope_id TEXT NOT NULL DEFAULT '',
				created_by TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				UNIQUE(scope_type, scope_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_secgw_binding_policy ON secgw_binding(policy_id)`,
			`CREATE TABLE IF NOT EXISTS secgw_term_list (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL UNIQUE,
				match_mode TEXT NOT NULL,
				severity TEXT NOT NULL,
				terms_encrypted TEXT NOT NULL,
				allow_encrypted TEXT NOT NULL DEFAULT '',
				term_count INTEGER NOT NULL DEFAULT 0,
				created_by TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS secgw_violation (
				id TEXT PRIMARY KEY,
				request_id TEXT NOT NULL,
				usage_event_id TEXT NOT NULL DEFAULT '',
				user_id TEXT NOT NULL DEFAULT '',
				service_token_id TEXT NOT NULL DEFAULT '',
				model_name TEXT NOT NULL DEFAULT '',
				policy_id TEXT NOT NULL,
				binding_id TEXT NOT NULL DEFAULT '',
				kind TEXT NOT NULL,
				rule_id TEXT NOT NULL,
				severity TEXT NOT NULL,
				direction TEXT NOT NULL,
				action TEXT NOT NULL,
				match_offset INTEGER NOT NULL DEFAULT 0,
				match_length INTEGER NOT NULL DEFAULT 0,
				match_hash TEXT NOT NULL DEFAULT '',
				match_text_encrypted TEXT,
				classifier_score REAL NOT NULL DEFAULT 0,
				classifier_model TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_secgw_violation_request ON secgw_violation(request_id)`,
			`CREATE INDEX IF NOT EXISTS idx_secgw_violation_kind_time ON secgw_violation(kind, created_at)`,
			`ALTER TABLE model ADD COLUMN classifier_role TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE usage_event ADD COLUMN secgw_action TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE usage_event ADD COLUMN secgw_violations INTEGER NOT NULL DEFAULT 0`,
		},
		down: []string{
			`DROP INDEX IF EXISTS idx_secgw_violation_kind_time`,
			`DROP INDEX IF EXISTS idx_secgw_violation_request`,
			`DROP TABLE IF EXISTS secgw_violation`,
			`DROP TABLE IF EXISTS secgw_term_list`,
			`DROP INDEX IF EXISTS idx_secgw_binding_policy`,
			`DROP TABLE IF EXISTS secgw_binding`,
			`DROP TABLE IF EXISTS secgw_policy`,
		},
	},
	{
		// Each running gateway process heartbeats one row. The live count
		// is what lets per-process rate-limit buckets divide the configured
		// rate so the cluster-wide ceiling equals the rule, without a shared
		// counter on the request path.
		name: "0025_gateway_instance",
		stmt: []string{
			`CREATE TABLE IF NOT EXISTS gateway_instance (
				id TEXT PRIMARY KEY,
				started_at TEXT NOT NULL,
				seen_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_gateway_instance_seen ON gateway_instance(seen_at)`,
		},
		down: []string{
			`DROP INDEX IF EXISTS idx_gateway_instance_seen`,
			`DROP TABLE IF EXISTS gateway_instance`,
		},
	},
	{name: "0026_model_metadata", stmt: []string{
		`ALTER TABLE model ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}'`,
		`UPDATE model SET metadata_json = '{"overrides":{' || '"context_window":' || CASE WHEN context_window > 0 THEN CAST(context_window AS TEXT) ELSE 'null' END || ',' || '"rate_in_nanousd":' || CASE WHEN rate_effective_from <> '' OR rate_in_nanousd <> 0 THEN CAST(rate_in_nanousd AS TEXT) ELSE 'null' END || ',' || '"rate_out_nanousd":' || CASE WHEN rate_effective_from <> '' OR rate_out_nanousd <> 0 THEN CAST(rate_out_nanousd AS TEXT) ELSE 'null' END || ',' || '"rate_cached_nanousd":' || CASE WHEN rate_effective_from <> '' OR rate_cached_nanousd <> 0 THEN CAST(rate_cached_nanousd AS TEXT) ELSE 'null' END || ',' || '"rate_cache_write_5m_nanousd":' || CASE WHEN rate_effective_from <> '' OR rate_cache_write_5m_nanousd <> 0 THEN CAST(rate_cache_write_5m_nanousd AS TEXT) ELSE 'null' END || ',' || '"rate_cache_write_1h_nanousd":' || CASE WHEN rate_effective_from <> '' OR rate_cache_write_1h_nanousd <> 0 THEN CAST(rate_cache_write_1h_nanousd AS TEXT) ELSE 'null' END || '}}'`,
	}},
	{name: "0027_team_membership", stmt: []string{
		`ALTER TABLE team ADD COLUMN listed INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE team ADD COLUMN archived_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE team_member ADD COLUMN role TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('member','moderator','leader'))`,
		`INSERT INTO team_member(team_id,user_id,role) SELECT id,lead_user_id,'leader' FROM team WHERE lead_user_id <> '' ON CONFLICT(team_id,user_id) DO UPDATE SET role='leader'`,
		`CREATE TABLE team_membership_source (team_id TEXT NOT NULL, user_id TEXT NOT NULL, source_type TEXT NOT NULL CHECK (source_type IN ('manual','group')), source_id TEXT NOT NULL DEFAULT '', PRIMARY KEY(team_id,user_id,source_type,source_id))`,
		`INSERT INTO team_membership_source(team_id,user_id,source_type,source_id) SELECT team_id,user_id,'manual','' FROM team_member`,
		`CREATE INDEX idx_team_member_user ON team_member(user_id)`,
		`CREATE TABLE team_group_mapping (team_id TEXT NOT NULL, group_id TEXT NOT NULL, PRIMARY KEY(team_id,group_id))`,
		`CREATE INDEX idx_team_group_mapping_group ON team_group_mapping(group_id)`,
		`CREATE TABLE team_join_request (id TEXT PRIMARY KEY, team_id TEXT NOT NULL, user_id TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','cancelled')), created_at TEXT NOT NULL, updated_at TEXT NOT NULL, reviewer_user_id TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', UNIQUE(team_id,user_id))`,
	}},
	teamContextMigration,
	scimMigration,
	teamAttributionMigration,
	{name: "0031_team_request_decision_reason", stmt: []string{`ALTER TABLE team_join_request ADD COLUMN decision_reason TEXT NOT NULL DEFAULT ''`}},
	reportingFactsMigration,
	reportingPolicyMigration,
	reportingJobsMigration,
	reportingClassificationMigration,
	localAuthMigration,
	identityProviderMigration,
	pendingLoginMigration,
}

// migrationAdvisoryLockKey is the pg_advisory_lock key that serialises
// Migrate across replicas. Arbitrary but fixed: any two Janus processes
// pointed at the same database must agree on it forever.
const migrationAdvisoryLockKey int64 = 0x6a616e75735f6d69 // "janus_mi"

// Migrate applies every outstanding migration. It is safe to run on every
// boot and on every replica: on PostgreSQL, application is serialised by a
// session-scoped advisory lock held for the duration of the run, so replicas
// booting concurrently (the Helm default is replicaCount 2) apply migrations
// one at a time and the losers find them already recorded in the ledger.
// SQLite deployments are single-process by definition (embedded file
// database), so the file lock suffices there.
func (s *Store) Migrate(ctx context.Context) error {
	if s.dialect == DialectPostgres {
		// The lock is session-scoped, so it must be taken on one pinned
		// connection that stays open until migrations finish; the migration
		// statements themselves may run on any pool connection.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire migration lock connection: %w", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLockKey); err != nil {
			return fmt.Errorf("acquire migration advisory lock: %w", err)
		}
		defer func() {
			// Unlock on the same session, even when ctx is already done.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockKey)
		}()
	}

	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	applied := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migration`)
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan migration ledger: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration ledger: %w", err)
	}

	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		// Dialect-specific DDL (e.g. Postgres column-type widenings) runs
		// first, then the shared statements; RollbackMigration reverses in
		// the opposite order.
		stmts := m.stmt
		if s.dialect == DialectPostgres && len(m.pgStmt) > 0 {
			stmts = append(append([]string{}, m.pgStmt...), m.stmt...)
		}
		if err := s.InTx(ctx, func(tx *sql.Tx) error {
			for _, stmt := range stmts {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply migration %s: %w", m.name, err)
				}
			}
			_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO schema_migration (name, applied_at) VALUES (?, ?)`), m.name, FormatTime(nowUTC()))
			return err
		}); err != nil {
			return fmt.Errorf("migrate %s: %w", m.name, err)
		}
	}
	return nil
}

// RollbackMigration reverses one applied migration by name and removes it
// from the schema_migration ledger, so a later Migrate re-applies it. Only
// migrations that declare down statements are reversible; everything shipped
// before 0011 is forward-only. Rolling back 0011 on PostgreSQL narrows the
// money columns back to 32-bit INTEGER, which is LOSSY: values outside
// ±2,147,483,647 nano-USD are clamped to the int4 bounds.
func (s *Store) RollbackMigration(ctx context.Context, name string) error {
	var m *migration
	for i := range migrations {
		if migrations[i].name == name {
			m = &migrations[i]
			break
		}
	}
	if m == nil {
		return fmt.Errorf("rollback migration %s: unknown migration", name)
	}
	if len(m.down) == 0 && len(m.pgDown) == 0 {
		return fmt.Errorf("rollback migration %s: forward-only (no down statements declared)", name)
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM schema_migration WHERE name = ?`, name).Scan(&n); err != nil {
		return fmt.Errorf("rollback migration %s: read ledger: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("rollback migration %s: not recorded as applied", name)
	}
	// Reverse of Migrate's order: shared down statements first, then the
	// dialect-specific reversals.
	stmts := append([]string{}, m.down...)
	if s.dialect == DialectPostgres {
		stmts = append(stmts, m.pgDown...)
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rollback migration %s: %w", name, err)
		}
	}
	if err := s.exec(ctx, `DELETE FROM schema_migration WHERE name = ?`, name); err != nil {
		return fmt.Errorf("rollback migration %s: unrecord: %w", name, err)
	}
	return nil
}

// AppliedMigrations lists the migrations recorded as applied, oldest first.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT name FROM schema_migration ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan migration name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
