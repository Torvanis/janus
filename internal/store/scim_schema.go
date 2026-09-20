package store

// Registered by the application's migration ledger after 0028.
var scimMigration = migration{name: "0029_scim", stmt: []string{
	`CREATE TABLE IF NOT EXISTS scim_token (id TEXT PRIMARY KEY, name TEXT NOT NULL, token_digest TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', revoked_at TEXT NOT NULL DEFAULT '', last_used_at TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS scim_resource (id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('Users','Groups')), external_id TEXT NOT NULL DEFAULT '', username_norm TEXT NOT NULL DEFAULT '', body_json TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, deleted_at TEXT NOT NULL DEFAULT '')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_scim_username ON scim_resource(username_norm) WHERE kind='Users'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_scim_external ON scim_resource(kind,external_id) WHERE external_id<>''`,
}, down: []string{`DROP TABLE IF EXISTS scim_resource`, `DROP TABLE IF EXISTS scim_token`}}
