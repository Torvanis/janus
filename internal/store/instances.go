package store

import (
	"context"
	"fmt"
	"time"
)

// InstanceStaleAfter is how long a gateway_instance row may go without a
// heartbeat before it stops counting as live. Three missed 15-second
// heartbeats: long enough to ride out a slow database, short enough that a
// pod that was killed stops inflating the divisor within a minute.
const InstanceStaleAfter = 45 * time.Second

// HeartbeatInstance records that this process is alive and returns the
// number of live processes including itself. One round-trip: the upsert
// and the count run in the same statement so the answer is consistent with
// the write.
func (s *Store) HeartbeatInstance(ctx context.Context, id string, now time.Time) (int, error) {
	ts := FormatTime(now)
	if err := s.exec(ctx, `INSERT INTO gateway_instance (id, started_at, seen_at) VALUES (?,?,?)
		ON CONFLICT (id) DO UPDATE SET seen_at = excluded.seen_at`, id, ts, ts); err != nil {
		return 0, fmt.Errorf("heartbeat instance: %w", err)
	}
	return s.LiveInstanceCount(ctx, now)
}

// LiveInstanceCount is the number of processes that heartbeated within
// InstanceStaleAfter. Never below 1: this process is one of them.
func (s *Store) LiveInstanceCount(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM gateway_instance WHERE seen_at > ?`,
		FormatTime(now.Add(-InstanceStaleAfter))).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live instances: %w", err)
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// RemoveInstance deletes this process's row on clean shutdown so the
// divisor drops immediately instead of after InstanceStaleAfter.
func (s *Store) RemoveInstance(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM gateway_instance WHERE id = ?`, id)
}

// PruneInstances removes rows that have been stale for well over the
// liveness window, so a churny deployment does not accumulate history.
func (s *Store) PruneInstances(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM gateway_instance WHERE seen_at < ?`),
		FormatTime(now.Add(-24*time.Hour)))
	if err != nil {
		return 0, fmt.Errorf("prune instances: %w", err)
	}
	return res.RowsAffected()
}
