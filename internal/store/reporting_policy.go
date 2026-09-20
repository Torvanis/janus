package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

var reportingPolicyMigration = migration{
	name: "0033_reporting_policy",
	stmt: []string{
		`CREATE TABLE IF NOT EXISTS report_quota_history (id TEXT PRIMARY KEY, quota_id TEXT NOT NULL, effective_from TEXT NOT NULL, effective_to TEXT NOT NULL DEFAULT '', policy_json TEXT NOT NULL, deleted INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_report_quota_history_time ON report_quota_history(quota_id, effective_from)`,
		`CREATE TABLE IF NOT EXISTS report_policy_state (id TEXT PRIMARY KEY, started_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS report_budget (id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_user_id TEXT NOT NULL, scope TEXT NOT NULL, subject_id TEXT NOT NULL, amount_nanousd BIGINT NOT NULL CHECK(amount_nanousd >= 0), start_at TEXT NOT NULL, end_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	},
}

// ReportBudget is reporting-only money in integer nanodollars. It has no
// enforcement, ledger, alert, or quota association.
type ReportBudget struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id"`
	Scope       string    `json:"scope"`
	SubjectID   string    `json:"subject_id"`
	AmountNano  int64     `json:"amount_nanousd"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Store) SaveReportBudget(ctx context.Context, b *ReportBudget) error {
	if b == nil {
		return fmt.Errorf("budget is required")
	}
	if strings.TrimSpace(b.Name) == "" || strings.TrimSpace(b.OwnerUserID) == "" {
		return fmt.Errorf("budget name and owner are required")
	}
	switch b.Scope {
	case "user", "team":
		if strings.TrimSpace(b.SubjectID) == "" {
			return fmt.Errorf("budget subject is required")
		}
	case "organization":
	default:
		return fmt.Errorf("invalid budget scope")
	}
	if b.AmountNano < 0 {
		return fmt.Errorf("budget amount must be nonnegative")
	}
	if b.Start.IsZero() || b.End.IsZero() || !b.End.After(b.Start) || b.Start.Year() < 1 || b.End.Year() > 9999 {
		return fmt.Errorf("invalid budget bounds")
	}
	saved := *b
	if saved.ID == "" {
		saved.ID = NewID()
	}
	now := time.Now().UTC()
	saved.CreatedAt, saved.UpdatedAt = now, now
	var created string
	err := s.queryRow(ctx, `INSERT INTO report_budget(id,name,owner_user_id,scope,subject_id,amount_nanousd,start_at,end_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,owner_user_id=excluded.owner_user_id,scope=excluded.scope,subject_id=excluded.subject_id,amount_nanousd=excluded.amount_nanousd,start_at=excluded.start_at,end_at=excluded.end_at,updated_at=excluded.updated_at RETURNING created_at`, saved.ID, saved.Name, saved.OwnerUserID, saved.Scope, saved.SubjectID, saved.AmountNano, reportPolicyTime(saved.Start), reportPolicyTime(saved.End), reportPolicyTime(now), reportPolicyTime(now)).Scan(&created)
	if err != nil {
		return err
	}
	saved.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return err
	}
	*b = saved
	return nil
}

func (s *Store) ListReportBudgets(ctx context.Context) ([]ReportBudget, error) {
	rows, err := s.query(ctx, `SELECT id,name,owner_user_id,scope,subject_id,amount_nanousd,start_at,end_at,created_at,updated_at FROM report_budget ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportBudget{}
	for rows.Next() {
		var b ReportBudget
		var start, end, created, updated string
		if err := rows.Scan(&b.ID, &b.Name, &b.OwnerUserID, &b.Scope, &b.SubjectID, &b.AmountNano, &start, &end, &created, &updated); err != nil {
			return nil, err
		}
		for raw, target := range map[string]*time.Time{start: &b.Start, end: &b.End} {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return nil, err
			}
			*target = parsed
		}
		b.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		b.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteReportBudget(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("budget id is required")
	}
	return s.exec(ctx, `DELETE FROM report_budget WHERE id = ?`, id)
}

type ReportQuotaVersion struct {
	ID, QuotaID                string
	EffectiveFrom, EffectiveTo time.Time
	Policy                     Quota
	Deleted                    bool
	Baseline                   bool
}

// Fixed-width UTC timestamps sort chronologically on both supported databases.
func reportPolicyTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// EnsureReportQuotaHistory establishes the first observable policy snapshot.
// Baselines deliberately make no claims about policy before initialization.
// The marker write serializes initializers and quota mutations on both dialects.
func (s *Store) EnsureReportQuotaHistory(ctx context.Context) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.exec(ctx, `INSERT INTO report_policy_state(id, started_at) VALUES ('quota_history', '') ON CONFLICT(id) DO NOTHING`); err != nil {
			return err
		}
		if err := tx.exec(ctx, `UPDATE report_policy_state SET started_at = started_at WHERE id = 'quota_history'`); err != nil {
			return err
		}
		var started string
		if err := tx.queryRow(ctx, `SELECT started_at FROM report_policy_state WHERE id = 'quota_history'`).Scan(&started); err != nil {
			return err
		}
		if started != "" {
			return nil
		}
		rows, err := tx.query(ctx, `SELECT `+quotaColumns+` FROM quota`)
		if err != nil {
			return err
		}
		quotas := []*Quota{}
		for rows.Next() {
			q, err := scanQuota(rows.Scan)
			if err != nil {
				_ = rows.Close()
				return err
			}
			quotas = append(quotas, q)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		at := time.Now().UTC()
		for _, q := range quotas {
			if err := tx.appendReportQuotaVersion(ctx, q, false, true, at); err != nil {
				return err
			}
		}
		return tx.exec(ctx, `UPDATE report_policy_state SET started_at = ? WHERE id = 'quota_history'`, reportPolicyTime(at))
	})
}

func (s *Store) appendReportQuotaVersion(ctx context.Context, q *Quota, deleted, baseline bool, at time.Time) error {
	// Store the baseline flag with the snapshot, not with the current quota.
	snapshot := struct {
		Quota
		Baseline bool `json:"reporting_baseline,omitempty"`
	}{*q, baseline}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	rows, err := s.query(ctx, `SELECT effective_from FROM report_quota_history WHERE quota_id = ? AND effective_to = ''`, q.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			_ = rows.Close()
			return err
		}
		previous, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if !at.After(previous) {
			at = previous.Add(time.Nanosecond)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	when := reportPolicyTime(at)
	if err := s.exec(ctx, `UPDATE report_quota_history SET effective_to = ? WHERE quota_id = ? AND effective_to = ''`, when, q.ID); err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO report_quota_history(id,quota_id,effective_from,policy_json,deleted) VALUES (?,?,?,?,?)`, NewID(), q.ID, when, string(encoded), boolInt(deleted))
}

// ListReportQuotaHistory returns versions overlapping the half-open [start,end)
// range, including deletion tombstones. It never joins against current quotas.
func (s *Store) ListReportQuotaHistory(ctx context.Context, start, end time.Time) ([]ReportQuotaVersion, error) {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, fmt.Errorf("invalid quota history bounds")
	}
	rows, err := s.query(ctx, `SELECT id,quota_id,effective_from,effective_to,policy_json,deleted FROM report_quota_history WHERE effective_from < ? AND (effective_to = '' OR effective_to > ?) ORDER BY effective_from,id`, reportPolicyTime(end), reportPolicyTime(start))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportQuotaVersion{}
	for rows.Next() {
		var v ReportQuotaVersion
		var from, to, encoded string
		var deleted int
		if err := rows.Scan(&v.ID, &v.QuotaID, &from, &to, &encoded, &deleted); err != nil {
			return nil, err
		}
		var snapshot struct {
			Quota
			Baseline bool `json:"reporting_baseline,omitempty"`
		}
		if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
			return nil, err
		}
		v.Policy, v.Baseline, v.Deleted = snapshot.Quota, snapshot.Baseline, deleted != 0
		v.EffectiveFrom, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return nil, err
		}
		if to != "" {
			v.EffectiveTo, err = time.Parse(time.RFC3339Nano, to)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
