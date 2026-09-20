package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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

// Membership writers take their global mutex first, then this user lock. Usage
// writers take only this lock, so unrelated users do not contend on PostgreSQL.
// SQLite's writer lock provides the corresponding serialization.
func (s *Store) lockTeamAttributionUser(ctx context.Context, userID string) error {
	return s.exec(ctx, `UPDATE app_user SET id=id WHERE id=?`, userID)
}

func (s *Store) lateUsageTeam(ctx context.Context, userID string, admittedAt time.Time) (string, error) {
	var teamID string
	err := s.queryRow(ctx, `SELECT team_id FROM team_attribution_transition WHERE user_id=? AND effective_at>=? ORDER BY effective_at,id LIMIT 1`, userID, FormatTime(admittedAt)).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return teamID, err
}
