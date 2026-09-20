package store

import (
	"context"
	"github.com/torvanis/janus/internal/reporting"
	"sort"
	"time"
)

type ReportFilterOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ReportFilterOptions reads the complete retained historical domain, not a rolling
// window. It reuses the report fact reader's server-side authorization, snapshots,
// and hard resource ceiling: an oversized domain fails explicitly, never truncates.
// Names are resolved only for IDs already observed in authorized facts.
func (s *Store) ReportFilterOptions(ctx context.Context, actorID string, d reporting.Definition) (map[string][]ReportFilterOption, error) {
	if err := reporting.Validate(d); err != nil {
		return nil, err
	}
	scope, err := s.ResolveReportScope(ctx, actorID, d)
	if err != nil {
		return nil, err
	}
	facts, err := s.reportFacts(ctx, time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC().Add(time.Nanosecond), scope, d)
	if err != nil {
		return nil, err
	}
	out := map[string][]ReportFilterOption{}
	catalog := reporting.GetCatalog()
	result := reporting.Result{}
	for _, dim := range catalog.Dimensions {
		out[dim.ID] = []ReportFilterOption{}
		seen := map[string]bool{}
		for _, f := range facts {
			for _, id := range reportDimension(f, dim.ID, time.UTC) {
				seen[id] = true
			}
		}
		ids := []string{}
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			result.Rows = append(result.Rows, reporting.Row{Dimensions: map[string]string{dim.ID: id}})
		}
	}
	if err = s.reportLabels(ctx, &result); err != nil {
		return nil, err
	}
	for _, row := range result.Rows {
		for _, dim := range catalog.Dimensions {
			if id, ok := row.Dimensions[dim.ID]; ok {
				label := row.Dimensions[dim.ID+"_label"]
				if label == "" {
					label = id
				}
				out[dim.ID] = append(out[dim.ID], ReportFilterOption{ID: id, Label: label})
			}
		}
	}
	return out, nil
}
