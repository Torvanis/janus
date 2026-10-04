package store

// No current-roster migration: historical explicit snapshots are authoritative.
var teamAttributionMigration = migration{name: "0030_team_attribution", stmt: []string{
	`CREATE TABLE IF NOT EXISTS team_attribution_transition (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL,
 team_id TEXT NOT NULL,
 effective_at TEXT NOT NULL
 )`,
	`CREATE INDEX IF NOT EXISTS idx_team_attribution_transition_user_time ON team_attribution_transition(user_id,effective_at)`,
}}

// Joining a first team no longer moves Personal usage; only an explicit token
// team change with move_history reattributes history. The transition table and
// the per-request app_user row lock that consulted it are gone, and so is the
// global reporting_coverage 'usage_snapshots' marker every usage write upserted
// (it fed only a report footnote but serialized all metering on one row).
var dropTeamAttributionMigration = migration{name: "0045_drop_team_attribution", stmt: []string{
	`DROP INDEX IF EXISTS idx_team_attribution_transition_user_time`,
	`DROP TABLE IF EXISTS team_attribution_transition`,
	`DELETE FROM reporting_coverage WHERE key='usage_snapshots'`,
}, down: []string{
	`CREATE TABLE IF NOT EXISTS team_attribution_transition (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL,
 team_id TEXT NOT NULL,
 effective_at TEXT NOT NULL
 )`,
	`CREATE INDEX IF NOT EXISTS idx_team_attribution_transition_user_time ON team_attribution_transition(user_id,effective_at)`,
}}
