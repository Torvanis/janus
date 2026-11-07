package store

import (
	"context"
	"fmt"
	"time"
)

// Cluster-wide alert deduplication. Every replica runs the same detectors
// (discovery, classifier breakers, Child Safety), so an alert must be
// claimed in the database before it is sent: only the replica whose claim
// succeeds delivers it. Claims live in system_setting rows keyed
// alertDedupPrefix+key whose value is the claim's expiry as zero-padded
// Unix nanoseconds (so a plain string comparison orders them on SQLite and
// Postgres alike). No migration is needed.
const alertDedupPrefix = "alert_dedup:"

func alertDedupValue(t time.Time) string { return fmt.Sprintf("%020d", t.UnixNano()) }

// ClaimAlert claims key for window. It reports true when this caller won the
// claim (no live claim existed), false when another caller (or replica) holds
// an unexpired claim. The claim is one atomic upsert, so two replicas racing
// for the same key never both win.
func (s *Store) ClaimAlert(ctx context.Context, key string, window time.Duration) (bool, error) {
	now := nowUTC()
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`INSERT INTO system_setting (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		WHERE system_setting.value < ?`),
		alertDedupPrefix+key, alertDedupValue(now.Add(window)), FormatTime(now), alertDedupValue(now))
	if err != nil {
		return false, fmt.Errorf("claim alert %s: %w", key, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseAlert drops the claim on key and reports whether one existed (live
// or expired). Exactly one concurrent caller sees true, which is how a
// recovery notice is sent once for an outage that was announced once.
func (s *Store) ReleaseAlert(ctx context.Context, key string) (bool, error) {
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`DELETE FROM system_setting WHERE key = ?`), alertDedupPrefix+key)
	if err != nil {
		return false, fmt.Errorf("release alert %s: %w", key, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PurgeAlertClaims deletes alert claims that expired before the cutoff.
func (s *Store) PurgeAlertClaims(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.runner(ctx).ExecContext(ctx, s.rebind(`DELETE FROM system_setting WHERE key LIKE ? AND value < ?`),
		alertDedupPrefix+"%", alertDedupValue(before))
	if err != nil {
		return 0, fmt.Errorf("purge alert claims: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
