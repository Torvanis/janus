package store

// secgwMatchHashIndexMigration lets "every request that carried this same
// text" be answered from the index: match_hash is the keyed hash of what a
// check matched.
var secgwMatchHashIndexMigration = migration{name: "0077_secgw_match_hash_index", stmt: []string{
	`CREATE INDEX IF NOT EXISTS idx_secgw_violation_match_hash ON secgw_violation(match_hash, created_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_secgw_violation_match_hash`,
}}
