package store

import (
	"context"
	"database/sql"
	"errors"
)

// teamContextMigration is registered after membership schema by schema.go.
var teamContextMigration = migration{name: "0028_team_context", stmt: []string{
	`ALTER TABLE api_token ADD COLUMN team_id TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX idx_api_token_team ON api_token(team_id)`,
	`CREATE TABLE session_team_context (session_id TEXT PRIMARY KEY, user_id TEXT NOT NULL, team_id TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX idx_session_team_user ON session_team_context(user_id)`,
}}

// SetSessionTeam changes one browser session's context, never an API key.
func (s *Store) SetSessionTeam(ctx context.Context, sessionID, userID, teamID string) error {
	if sessionID == "" || userID == "" {
		return &ValidationError{Field: "team_id", Message: "a browser session is required"}
	}
	return s.modelTx(ctx, func(tx *Store) error {
		if teamID != "" {
			if err := tx.ValidateTeamMember(ctx, teamID, userID); err != nil {
				return err
			}
		}
		return tx.exec(ctx, `INSERT INTO session_team_context(session_id,user_id,team_id) VALUES (?,?,?) ON CONFLICT(session_id) DO UPDATE SET team_id=excluded.team_id WHERE session_team_context.user_id=excluded.user_id`, sessionID, userID, teamID)
	})
}

// SessionTeam resolves the selected team without silently substituting another.
func (s *Store) SessionTeam(ctx context.Context, sessionID, userID string) (string, error) {
	var id string
	err := s.queryRow(ctx, `SELECT team_id FROM session_team_context WHERE session_id=? AND user_id=?`, sessionID, userID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// ValidateTeamMember reads authoritative membership on every protected request.
func (s *Store) ValidateTeamMember(ctx context.Context, teamID, userID string) error {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM team_member m JOIN team t ON t.id=m.team_id JOIN app_user u ON u.id=m.user_id WHERE m.team_id=? AND m.user_id=? AND t.archived_at='' AND u.is_active=1`, teamID, userID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return &ValidationError{Field: "team_id", Message: "you are not an active member of the selected team; choose another team or personal context"}
	}
	return nil
}
