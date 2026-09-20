package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const quotaColumns = `id, subject_type, subject_id, model_id, metric, limit_value, window_kind, breach_behavior, alert_thresholds, created_at`

func scanQuota(scan func(...any) error) (*Quota, error) {
	var q Quota
	var created, thresholds string
	if err := scan(&q.ID, &q.SubjectType, &q.SubjectID, &q.ModelID, &q.Metric, &q.Limit, &q.Window, &q.BreachBehavior, &thresholds, &created); err != nil {
		return nil, err
	}
	q.AlertThresholds = DecodeThresholds(thresholds)
	q.CreatedAt = ParseTime(created)
	return &q, nil
}

// EncodeThresholds serialises alert threshold percentages for storage as a
// comma-separated ascending list; nil/empty encodes to "" (defaults apply).
func EncodeThresholds(ts []int) string {
	if len(ts) == 0 {
		return ""
	}
	sorted := append([]int(nil), ts...)
	sort.Ints(sorted)
	parts := make([]string, 0, len(sorted))
	for _, t := range sorted {
		parts = append(parts, strconv.Itoa(t))
	}
	return strings.Join(parts, ",")
}

// DecodeThresholds parses a stored threshold list; malformed entries are
// dropped so a corrupted row degrades to the defaults instead of failing scans.
func DecodeThresholds(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := []int{}
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 100 {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// ListQuotas returns all quota rules, newest first.
func (s *Store) ListQuotas(ctx context.Context) ([]*Quota, error) {
	rows, err := s.query(ctx, `SELECT `+quotaColumns+` FROM quota ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Quota{}
	for rows.Next() {
		q, err := scanQuota(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan quota: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.decorateQuotas(ctx, out)
	return out, nil
}

func (s *Store) decorateQuotas(ctx context.Context, quotas []*Quota) {
	for _, q := range quotas {
		switch q.SubjectType {
		case "user":
			if u, err := s.UserByID(ctx, q.SubjectID); err == nil {
				q.SubjectName = displayName(u)
			}
		case "team":
			if t, err := s.TeamByID(ctx, q.SubjectID); err == nil {
				q.SubjectName = t.Name
			}
		case "service_token":
			if st, err := s.ServiceTokenByID(ctx, q.SubjectID); err == nil {
				q.SubjectName = st.Name
			}
		}
		if q.ModelID != "" {
			if m, err := s.ModelByID(ctx, q.ModelID); err == nil {
				q.ModelName = m.Name
			} else if mm, err := s.ManagedModelByID(ctx, q.ModelID); err == nil {
				// A quota may be scoped to a managed model. Enforcement still
				// keys on the underlying model id recorded on usage events,
				// but the label must show the alias the admin actually chose.
				q.ModelName = mm.Name
			}
		} else {
			q.ModelName = "All models"
		}
	}
}

// QuotaByID loads one rule.
func (s *Store) QuotaByID(ctx context.Context, id string) (*Quota, error) {
	q, err := scanQuota(s.queryRow(ctx, `SELECT `+quotaColumns+` FROM quota WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load quota: %w", err)
	}
	return q, nil
}

// QuotasForSubject returns the rules applying to a user, including any inherited
// from the teams the user belongs to.
func (s *Store) QuotasForSubject(ctx context.Context, userID string, teamIDs []string) ([]*Quota, error) {
	args := []any{userID}
	q := `SELECT ` + quotaColumns + ` FROM quota WHERE (subject_type = 'user' AND subject_id = ?)`
	if len(teamIDs) > 0 {
		q += ` OR (subject_type = 'team' AND subject_id IN (` + placeholders(len(teamIDs)) + `))`
		args = append(args, toArgs(teamIDs)...)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Quota{}
	for rows.Next() {
		qq, err := scanQuota(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan quota: %w", err)
		}
		out = append(out, qq)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.decorateQuotas(ctx, out)
	return out, nil
}

// QuotasForServiceToken returns the rules applying to a service token.
//
// A service token belongs to no team and no group, so — unlike a user — it
// inherits nothing: only rules written directly against it apply. That is
// deliberate. An integration credential's budget should be an explicit
// decision by an administrator, never something it picks up by proximity.
func (s *Store) QuotasForServiceToken(ctx context.Context, serviceTokenID string) ([]*Quota, error) {
	rows, err := s.query(ctx, `SELECT `+quotaColumns+` FROM quota WHERE subject_type = 'service_token' AND subject_id = ?`, serviceTokenID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Quota{}
	for rows.Next() {
		q, err := scanQuota(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan quota: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.decorateQuotas(ctx, out)
	return out, nil
}

// CreateQuota inserts a rule.
func (s *Store) CreateQuota(ctx context.Context, q *Quota) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.EnsureReportQuotaHistory(ctx); err != nil {
			return err
		}
		q.ID = NewID()
		q.CreatedAt = nowUTC()
		if err := tx.exec(ctx, `INSERT INTO quota (`+quotaColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			q.ID, q.SubjectType, q.SubjectID, q.ModelID, q.Metric, q.Limit, q.Window, q.BreachBehavior,
			EncodeThresholds(q.AlertThresholds), FormatTime(q.CreatedAt)); err != nil {
			return err
		}
		persisted, err := tx.QuotaByID(ctx, q.ID)
		if err != nil {
			return err
		}
		return tx.appendReportQuotaVersion(ctx, persisted, false, false, time.Now().UTC())
	})
}

// UpdateQuota changes a rule's limit, window, breach behaviour, and alert
// thresholds (nil/empty thresholds restore the 80/95 defaults).
func (s *Store) UpdateQuota(ctx context.Context, id string, limit int64, window, behavior string, alertThresholds []int) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.EnsureReportQuotaHistory(ctx); err != nil {
			return err
		}
		if err := tx.exec(ctx, `UPDATE quota SET limit_value = ?, window_kind = ?, breach_behavior = ?, alert_thresholds = ? WHERE id = ?`,
			limit, window, behavior, EncodeThresholds(alertThresholds), id); err != nil {
			return err
		}
		q, err := tx.QuotaByID(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.appendReportQuotaVersion(ctx, q, false, false, time.Now().UTC())
	})
}

// DeleteQuota removes a rule and its ledger rows.
func (s *Store) DeleteQuota(ctx context.Context, id string) error {
	return s.modelTx(ctx, func(tx *Store) error {
		if err := tx.EnsureReportQuotaHistory(ctx); err != nil {
			return err
		}
		q, err := tx.QuotaByID(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := tx.exec(ctx, `DELETE FROM quota_ledger WHERE quota_id = ?`, id); err != nil {
			return err
		}
		if err := tx.exec(ctx, `DELETE FROM quota_alert_state WHERE quota_id = ?`, id); err != nil {
			return err
		}
		if err := tx.exec(ctx, `DELETE FROM quota WHERE id = ?`, id); err != nil {
			return err
		}
		if q == nil {
			return nil
		}
		return tx.appendReportQuotaVersion(ctx, q, true, false, time.Now().UTC())
	})
}

// LedgerValue reads the durable counter for a quota's current window.
func (s *Store) LedgerValue(ctx context.Context, quotaID string, windowStart time.Time) (int64, error) {
	var v int64
	err := s.queryRow(ctx, `SELECT current_value FROM quota_ledger WHERE quota_id = ? AND window_start = ?`,
		quotaID, FormatTime(windowStart)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read quota ledger: %w", err)
	}
	return v, nil
}

// AddLedger increments the durable counter, creating the window row on demand.
func (s *Store) AddLedger(ctx context.Context, quotaID string, windowStart time.Time, delta int64) (int64, error) {
	ws := FormatTime(windowStart)
	now := FormatTime(nowUTC())
	// A single atomic upsert: the previous UPDATE→SELECT→INSERT sequence let
	// two concurrent writers on a brand-new (quota_id, window_start) both
	// miss the UPDATE and race the INSERT — the loser hit the primary key,
	// its delta was dropped until the next reconcile, and the quota could be
	// marginally overshot at window boundaries. Both PostgreSQL and SQLite
	// support ON CONFLICT ... DO UPDATE and RETURNING.
	var v int64
	err := s.queryRow(ctx, `INSERT INTO quota_ledger (quota_id, window_start, current_value, updated_at) VALUES (?,?,?,?)
		ON CONFLICT (quota_id, window_start) DO UPDATE SET
			current_value = quota_ledger.current_value + excluded.current_value,
			updated_at = excluded.updated_at
		RETURNING current_value`, quotaID, ws, delta, now).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("upsert quota ledger: %w", err)
	}
	return v, nil
}

// SetLedger overwrites a window counter, used when reconciling from the event log.
func (s *Store) SetLedger(ctx context.Context, quotaID string, windowStart time.Time, value int64) error {
	ws := FormatTime(windowStart)
	now := FormatTime(nowUTC())
	// Atomic for the same reason as AddLedger: concurrent first writes to a
	// fresh window must not race an INSERT after a missed UPDATE.
	return s.exec(ctx, `INSERT INTO quota_ledger (quota_id, window_start, current_value, updated_at) VALUES (?,?,?,?)
		ON CONFLICT (quota_id, window_start) DO UPDATE SET
			current_value = excluded.current_value,
			updated_at = excluded.updated_at`, quotaID, ws, value, now)
}

// MarkThresholdNotified records that an alert fired, returning false when it had
// already fired for this window so alerts are never duplicated.
func (s *Store) MarkThresholdNotified(ctx context.Context, quotaID string, windowStart time.Time, threshold int) (bool, error) {
	ws := FormatTime(windowStart)
	var existing string
	err := s.queryRow(ctx, `SELECT notified_at FROM quota_alert_state WHERE quota_id = ? AND window_start = ? AND threshold = ?`,
		quotaID, ws, threshold).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read quota alert state: %w", err)
	}
	if err := s.exec(ctx, `INSERT INTO quota_alert_state (quota_id, window_start, threshold, notified_at) VALUES (?,?,?,?)`,
		quotaID, ws, threshold, FormatTime(nowUTC())); err != nil {
		return false, err
	}
	return true, nil
}

// PurgeQuotaAlertState deletes alert-dedup rows whose notification is older
// than the cutoff. Dedup only matters within a quota's current window (at most
// ~31 days wide), and notified_at is never earlier than the row's window_start,
// so any cutoff older than the widest window is safe: rows past it can never
// belong to a window that is still deduplicating.
func (s *Store) PurgeQuotaAlertState(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM quota_alert_state WHERE notified_at < ?`), FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge quota alert state: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
