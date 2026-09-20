package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SumTeamMetricSince reads persisted event attribution, not current membership.
// History reassignment is therefore visible on every replica immediately after
// commit. CSV delimiters prevent partial-ID matches and count each event once.
func (s *Store) SumTeamMetricSince(ctx context.Context, teamID, modelID, metric string, since time.Time) (int64, error) {
	expr := map[string]string{MetricTokensIn: "COALESCE(SUM(tokens_in),0)", MetricTokensOut: "COALESCE(SUM(tokens_out),0)", MetricCostUSD: "COALESCE(SUM(cost_nanousd),0)", MetricRequests: "COUNT(*)"}[metric]
	if expr == "" {
		return 0, fmt.Errorf("unsupported quota metric %q", metric)
	}
	if teamID == "" {
		return 0, nil
	}
	query := `SELECT ` + expr + ` FROM usage_event WHERE created_at>=? AND (',' || team_ids || ',') LIKE ? ESCAPE '!'`
	pattern := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(teamID)
	args := []any{FormatTime(since), "%," + pattern + ",%"}
	if modelID != "" {
		query += ` AND model_id=?`
		args = append(args, modelID)
	}
	// Read coverage and usage in one statement so a concurrent purge cannot
	// pair a pre-purge watermark with a post-purge (incomplete) aggregate.
	query = `SELECT (` + query + `), COALESCE((SELECT started_at FROM reporting_coverage WHERE key='retention_before'), '')`
	var total int64
	var retentionBefore string
	if err := s.queryRow(ctx, query, args...).Scan(&total, &retentionBefore); err != nil {
		return 0, fmt.Errorf("sum team quota metric: %w", err)
	}
	if retentionBefore != "" && since.Before(ParseTime(retentionBefore)) {
		return 0, fmt.Errorf("missing quota usage coverage: team %q window starts at %s before retention_before %s; purged usage cannot be restored by raising retention", teamID, FormatTime(since), retentionBefore)
	}
	return total, nil
}

// ChangeTokenTeam changes an existing credential without rotating its secret.
// The returned count is the number of persisted usage rows whose attribution
// changed. Empty teamID selects Personal. History is opt-in; later usage keeps
// its admission context (no attribution transition is created here).
func (s *Store) ChangeTokenTeam(ctx context.Context, tokenID, ownerID, teamID string, moveHistory bool) (*Token, int64, error) {
	var token *Token
	var moved int64
	err := s.modelTx(ctx, func(tx *Store) error {
		// Follow membership writers' lock order and serialize empty-snapshot
		// attribution for this owner. Lock the token against revocation too.
		// Explicit-snapshot inserts need not wait: only rows visible to the
		// history UPDATE are moved, never events arriving after that statement.
		if err := tx.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := tx.lockTeamAttributionUser(ctx, ownerID); err != nil {
			return err
		}
		if err := tx.exec(ctx, `UPDATE api_token SET id=id WHERE id=?`, tokenID); err != nil {
			return err
		}
		var err error
		token, err = tx.TokenByID(ctx, tokenID)
		if err != nil {
			return err
		}
		if token.UserID != ownerID {
			return ErrNotFound
		}
		if !token.RevokedAt.IsZero() {
			return &ValidationError{Field: "token_id", Message: "revoked credentials cannot change team"}
		}
		var active int
		if err := tx.queryRow(ctx, `SELECT is_active FROM app_user WHERE id=?`, ownerID).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return &ValidationError{Field: "owner_id", Message: "credential owner is inactive"}
		}
		if teamID != "" {
			if err := tx.ValidateTeamMember(ctx, teamID, ownerID); err != nil {
				return err
			}
		}
		oldTeam := token.TeamID
		if err := tx.exec(ctx, `UPDATE api_token SET team_id=? WHERE id=?`, teamID, tokenID); err != nil {
			return err
		}
		if moveHistory {
			res, err := tx.tx.ExecContext(ctx, tx.rebind(`UPDATE usage_event SET team_ids=? WHERE token_id=? AND team_ids<>?`), teamID, tokenID, teamID)
			if err != nil {
				return err
			}
			moved, err = res.RowsAffected()
			if err != nil {
				return err
			}
		}
		oldValue, _ := json.Marshal(map[string]any{"team_id": oldTeam})
		newValue, _ := json.Marshal(map[string]any{"team_id": teamID, "move_history": moveHistory, "moved_usage_events": moved})
		if err := tx.AppendAudit(ctx, &AuditEntry{ActorUserID: ownerID, Action: "token.team_changed", ResourceType: "api_token", ResourceID: tokenID, OldValue: string(oldValue), NewValue: string(newValue)}); err != nil {
			return err
		}
		token.TeamID = teamID
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return token, moved, nil
}
