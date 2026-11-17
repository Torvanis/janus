package store

import (
	"context"

	"github.com/torvanis/janus/internal/usage"
)

// attachQuotingUpstreamNames names the upstream on every row whose cost the
// provider quoted (AccountingUpstreamCost), so the request log can say
// "Cost quoted by OpenRouter". Soft-deleted upstreams keep their name: the
// quote was theirs when the request ran.
func (s *Store) attachQuotingUpstreamNames(ctx context.Context, events []*UsageEvent) error {
	ids := map[string]bool{}
	args := []any{}
	for _, e := range events {
		if e.AccountingMode == usage.AccountingUpstreamCost && e.UpstreamID != "" && !ids[e.UpstreamID] {
			ids[e.UpstreamID] = true
			args = append(args, e.UpstreamID)
		}
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := s.query(ctx, `SELECT id,name FROM upstream WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range events {
		if e.AccountingMode == usage.AccountingUpstreamCost {
			e.QuotedBy = names[e.UpstreamID]
		}
	}
	return nil
}
