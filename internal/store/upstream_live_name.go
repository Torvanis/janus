package store

// upstreamLiveNameMigration lets a name be reused once its upstream is deleted.
//
// Deleting an upstream is a soft delete: the row stays so historical usage
// keeps its upstream attribution. The original schema declared the name
// column UNIQUE across ALL rows, so recreating "vLLM on K8s" after deleting
// it failed with `duplicate key value violates unique constraint
// "upstream_name_key"` — a name that no longer appears anywhere in the UI.
//
// Uniqueness now applies only to live upstreams (a partial unique index on
// deleted_at = ”). The inline UNIQUE constraint cannot be dropped on SQLite,
// so the table is rebuilt with identical columns — a statement sequence that
// is valid on both SQLite and PostgreSQL. No other table has a foreign key to
// upstream, and the table is tiny, so the rebuild is cheap. Forward-only.
var upstreamLiveNameMigration = migration{name: "0039_upstream_live_name", stmt: []string{
	`CREATE TABLE upstream_v39 (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
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
	`INSERT INTO upstream_v39 (id, name, adapter_type, base_url, api_key_encrypted, api_key_mask, enabled, deleted_at, last_check_at, last_error, last_latency_ms, created_at)
		SELECT id, name, adapter_type, base_url, api_key_encrypted, api_key_mask, enabled, deleted_at, last_check_at, last_error, last_latency_ms, created_at FROM upstream`,
	`DROP TABLE upstream`,
	`ALTER TABLE upstream_v39 RENAME TO upstream`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_live_name ON upstream(name) WHERE deleted_at = ''`,
}}
