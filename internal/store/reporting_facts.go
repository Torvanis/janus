package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// No identity foreign keys: deletion or reassignment must not rewrite history.
// Existing usage is deliberately not backfilled with today's identity facts.
var reportingFactsMigration = migration{
	name: "0032_reporting_facts",
	stmt: []string{
		`CREATE TABLE IF NOT EXISTS reporting_usage_snapshot (
   usage_id TEXT PRIMARY KEY,
   recorded_at TEXT NOT NULL,
   group_ids TEXT NOT NULL DEFAULT '[]',
   groups_known INTEGER NOT NULL DEFAULT 0,
   model_family TEXT NOT NULL DEFAULT '',
   provider TEXT NOT NULL DEFAULT '',
   hosting TEXT NOT NULL DEFAULT '',
   project TEXT NOT NULL DEFAULT '',
   cost_center TEXT NOT NULL DEFAULT '',
   cost_status TEXT NOT NULL DEFAULT 'unknown',
   classification_known INTEGER NOT NULL DEFAULT 0
  )`,
		`CREATE INDEX IF NOT EXISTS idx_reporting_usage_snapshot_recorded ON reporting_usage_snapshot(recorded_at)`,
		`CREATE TABLE IF NOT EXISTS reporting_coverage (key TEXT PRIMARY KEY, started_at TEXT NOT NULL)`,
	},
	down: []string{`DROP TABLE IF EXISTS reporting_usage_snapshot`, `DROP TABLE IF EXISTS reporting_coverage`},
}

// insertReportingFact must run in the metering transaction. In particular, do
// not resolve group membership at delayed insertion time.
func (s *Store) insertReportingFact(ctx context.Context, e *UsageEvent) error {
	groups := e.GroupIDs
	known := groups != nil || e.ServiceTokenID != ""
	if groups == nil || e.ServiceTokenID != "" {
		groups = []string{}
	}
	encoded, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("encode reporting groups: %w", err)
	}
	status := e.CostStatus
	switch status {
	case "known_free", "priced", "unpriced", "disabled", "unknown":
	default:
		status = "unknown"
	}
	classified := e.ModelFamily != "" && e.ModelProvider != "" && e.ModelHosting != ""
	if err := s.exec(ctx, `INSERT INTO reporting_usage_snapshot
  (usage_id, recorded_at, group_ids, groups_known, model_family, provider, hosting, project, cost_center, cost_status, classification_known)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, FormatTime(e.CreatedAt), string(encoded), boolInt(known), e.ModelFamily, e.ModelProvider, e.ModelHosting, e.Project, e.CostCenter, status, boolInt(classified)); err != nil {
		return fmt.Errorf("insert reporting snapshot: %w", err)
	}
	// This is the earliest captured event time, not a promise that every
	// event since then has facts. Readers must still count missing snapshots.
	if err := s.exec(ctx, `INSERT INTO reporting_coverage (key, started_at) VALUES ('usage_snapshots', ?)
  ON CONFLICT (key) DO UPDATE SET started_at = excluded.started_at
  WHERE reporting_coverage.started_at > excluded.started_at`, FormatTime(e.CreatedAt)); err != nil {
		return fmt.Errorf("record reporting coverage: %w", err)
	}
	return nil
}
