package store

import (
	"context"
	"strings"
)

// Labels decorate only the IDs already recorded on authorized request rows.
// Current token affinity and current team membership never change attribution.
func (s *Store) attachUsageTeamNames(ctx context.Context, events []*UsageEvent) error {
	ids := map[string]bool{}
	args := []any{}
	for _, e := range events {
		e.TeamNames = []string{}
		for _, id := range strings.Split(e.TeamIDs, ",") {
			if id != "" && !ids[id] {
				ids[id] = true
				args = append(args, id)
			}
		}
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := s.query(ctx, `SELECT id,name FROM team WHERE id IN (`+placeholders(len(args))+`)`, args...)
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
		for _, id := range strings.Split(e.TeamIDs, ",") {
			if id == "" {
				continue
			}
			name := names[id]
			if name == "" {
				name = "Unavailable team"
			}
			e.TeamNames = append(e.TeamNames, name)
		}
	}
	return nil
}
