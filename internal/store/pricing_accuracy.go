package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/torvanis/janus/internal/usage"
)

// quotedEstimateMigration adds Janus's own price beside a provider's quote
// (usage_event.estimated_cost_nanousd, NULL = no estimate) and a partial
// index over quoted calls only, so the pricing accuracy report reads a
// range of quoted calls without touching the rest of the table.
//
// Numbered 0085, not 0084: feat/price-catalog may take 0084, and the gap is
// harmless (migrations are keyed by name and applied in list order).
var quotedEstimateMigration = migration{name: "0085_usage_estimated_cost", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN estimated_cost_nanousd BIGINT`,
	`CREATE INDEX IF NOT EXISTS idx_usage_quoted_time ON usage_event(created_at) WHERE token_accounting_method = 'upstream_reported_cost'`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_usage_quoted_time`,
	`ALTER TABLE usage_event DROP COLUMN estimated_cost_nanousd`,
}}

// quotedClause selects calls whose cost the provider quoted. It is a
// literal, never a bind parameter, so PostgreSQL can match it to the
// partial index idx_usage_quoted_time.
const quotedClause = `token_accounting_method = '` + usage.AccountingUpstreamCost + `'`

// PriceAccuracyGroup compares what providers quoted with Janus's own price
// for one model, one provider, or every quoted call.
type PriceAccuracyGroup struct {
	UpstreamID   string `json:"upstream_id,omitempty"`
	UpstreamName string `json:"upstream_name,omitempty"`
	ModelID      string `json:"model_id,omitempty"`
	ModelName    string `json:"model,omitempty"`
	// QuotedCalls counts every call the provider quoted in the range;
	// ComparedCalls the ones that also carry Janus's estimate. The money
	// and the shares below cover compared calls only.
	QuotedCalls   int64 `json:"quoted_calls"`
	ComparedCalls int64 `json:"compared_calls"`
	QuotedNano    int64 `json:"quoted_nanousd"`
	EstimateNano  int64 `json:"estimate_nanousd"`
	// DiffNano is Janus's estimate minus the quote (positive: Janus would
	// have charged more). DiffPercent is that over the quote; nil when the
	// quotes add up to $0.
	DiffNano    int64    `json:"diff_nanousd"`
	DiffPercent *float64 `json:"diff_percent"`
	// Within1 and Within5 count compared calls whose estimate is within 1%
	// and 5% of the quote.
	Within1 int64 `json:"within_1_percent"`
	Within5 int64 `json:"within_5_percent"`
	// NoPriceCalls counts compared calls where Janus's estimate is $0 but
	// the provider charged something: Janus has no price for that model
	// (or for what was measured).
	NoPriceCalls int64 `json:"no_price_calls"`
	// HasPriceNow says whether the model has a saved price today, so the
	// report links to "Set prices" only where it still matters (models
	// only).
	HasPriceNow *bool `json:"has_price_now,omitempty"`
}

func (g *PriceAccuracyGroup) add(o PriceAccuracyGroup) {
	g.QuotedCalls += o.QuotedCalls
	g.ComparedCalls += o.ComparedCalls
	g.QuotedNano += o.QuotedNano
	g.EstimateNano += o.EstimateNano
	g.Within1 += o.Within1
	g.Within5 += o.Within5
	g.NoPriceCalls += o.NoPriceCalls
}

func (g *PriceAccuracyGroup) finish() {
	g.DiffNano = g.EstimateNano - g.QuotedNano
	g.DiffPercent = nil
	if g.QuotedNano != 0 {
		p := float64(g.DiffNano) * 100 / float64(g.QuotedNano)
		g.DiffPercent = &p
	}
}

// PriceAccuracyCall is one compared call, for the "largest differences" list.
type PriceAccuracyCall struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"request_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpstreamID   string    `json:"upstream_id"`
	UpstreamName string    `json:"upstream_name"`
	ModelID      string    `json:"model_id"`
	ModelName    string    `json:"model"`
	Modality     string    `json:"modality"`
	// Internal marks a call Janus made for itself (a classifier); the
	// request log lists those only with "show gateway internals".
	Internal     bool     `json:"internal"`
	QuotedNano   int64    `json:"quoted_nanousd"`
	EstimateNano int64    `json:"estimate_nanousd"`
	DiffNano     int64    `json:"diff_nanousd"`
	DiffPercent  *float64 `json:"diff_percent"`
}

// PriceAccuracyReport is the pricing accuracy report over one range.
type PriceAccuracyReport struct {
	Start     time.Time            `json:"start"`
	End       time.Time            `json:"end"`
	Totals    PriceAccuracyGroup   `json:"totals"`
	Models    []PriceAccuracyGroup `json:"models"`
	Providers []PriceAccuracyGroup `json:"providers"`
	Worst     []PriceAccuracyCall  `json:"worst"`
}

// PriceAccuracy compares, for every call in [start, end) whose cost the
// provider quoted, the quote with Janus's own price recorded beside it.
// Two reads, both over the partial index of quoted calls: one grouped by
// model (providers and totals are sums of it), one for the worst calls.
func (s *Store) PriceAccuracy(ctx context.Context, start, end time.Time, worst int) (*PriceAccuracyReport, error) {
	if worst <= 0 || worst > 50 {
		worst = 10
	}
	from, to := FormatTime(start), FormatTime(end)
	rows, err := s.query(ctx, `SELECT upstream_id, model_id, model_name,
		COUNT(*),
		SUM(CASE WHEN estimated_cost_nanousd IS NULL THEN 0 ELSE 1 END),
		COALESCE(SUM(CASE WHEN estimated_cost_nanousd IS NULL THEN 0 ELSE cost_nanousd END), 0),
		COALESCE(SUM(estimated_cost_nanousd), 0),
		SUM(CASE WHEN estimated_cost_nanousd IS NOT NULL AND ABS(estimated_cost_nanousd - cost_nanousd) * 100 <= cost_nanousd THEN 1 ELSE 0 END),
		SUM(CASE WHEN estimated_cost_nanousd IS NOT NULL AND ABS(estimated_cost_nanousd - cost_nanousd) * 20 <= cost_nanousd THEN 1 ELSE 0 END),
		SUM(CASE WHEN estimated_cost_nanousd = 0 AND cost_nanousd > 0 THEN 1 ELSE 0 END)
		FROM usage_event WHERE `+quotedClause+` AND created_at >= ? AND created_at < ?
		GROUP BY upstream_id, model_id, model_name`, from, to)
	if err != nil {
		return nil, fmt.Errorf("price accuracy: %w", err)
	}
	report := &PriceAccuracyReport{Start: start, End: end, Models: []PriceAccuracyGroup{}, Providers: []PriceAccuracyGroup{}, Worst: []PriceAccuracyCall{}}
	for rows.Next() {
		var g PriceAccuracyGroup
		if err := rows.Scan(&g.UpstreamID, &g.ModelID, &g.ModelName, &g.QuotedCalls, &g.ComparedCalls, &g.QuotedNano,
			&g.EstimateNano, &g.Within1, &g.Within5, &g.NoPriceCalls); err != nil {
			_ = rows.Close()
			return nil, err
		}
		report.Models = append(report.Models, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	rows, err = s.query(ctx, `SELECT id, request_id, created_at, upstream_id, model_id, model_name, modality, client_app,
		cost_nanousd, estimated_cost_nanousd
		FROM usage_event WHERE `+quotedClause+` AND created_at >= ? AND created_at < ? AND estimated_cost_nanousd IS NOT NULL
		ORDER BY ABS(estimated_cost_nanousd - cost_nanousd) DESC, created_at DESC, id DESC LIMIT ?`, from, to, worst)
	if err != nil {
		return nil, fmt.Errorf("price accuracy calls: %w", err)
	}
	for rows.Next() {
		var c PriceAccuracyCall
		var created, app string
		if err := rows.Scan(&c.ID, &c.RequestID, &created, &c.UpstreamID, &c.ModelID, &c.ModelName, &c.Modality, &app,
			&c.QuotedNano, &c.EstimateNano); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.CreatedAt = ParseTime(created)
		c.Internal = app == InternalClientApp || app == InternalRoutingClientApp || app == InternalProbeClientApp
		c.DiffNano = c.EstimateNano - c.QuotedNano
		if c.QuotedNano != 0 {
			p := float64(c.DiffNano) * 100 / float64(c.QuotedNano)
			c.DiffPercent = &p
		}
		report.Worst = append(report.Worst, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	names, err := s.upstreamNamesFor(ctx, report)
	if err != nil {
		return nil, err
	}
	providers := map[string]*PriceAccuracyGroup{}
	for i := range report.Models {
		m := &report.Models[i]
		m.UpstreamName = names[m.UpstreamID]
		if m.NoPriceCalls > 0 && m.ModelID != "" {
			// Only flagged models are looked up: a few at most.
			if model, err := s.ModelByID(ctx, m.ModelID); err == nil {
				priced := modelHasPrice(model)
				m.HasPriceNow = &priced
			}
		}
		m.finish()
		report.Totals.add(*m)
		p := providers[m.UpstreamID]
		if p == nil {
			p = &PriceAccuracyGroup{UpstreamID: m.UpstreamID, UpstreamName: m.UpstreamName}
			providers[m.UpstreamID] = p
		}
		p.add(*m)
	}
	for _, p := range providers {
		p.finish()
		report.Providers = append(report.Providers, *p)
	}
	report.Totals.finish()
	for i := range report.Worst {
		report.Worst[i].UpstreamName = names[report.Worst[i].UpstreamID]
	}
	// Most money quoted first; ties by name so the order is stable.
	sort.Slice(report.Models, func(i, j int) bool {
		a, b := report.Models[i], report.Models[j]
		if a.QuotedNano != b.QuotedNano {
			return a.QuotedNano > b.QuotedNano
		}
		if a.ModelName != b.ModelName {
			return a.ModelName < b.ModelName
		}
		return a.UpstreamName < b.UpstreamName
	})
	sort.Slice(report.Providers, func(i, j int) bool {
		a, b := report.Providers[i], report.Providers[j]
		if a.QuotedNano != b.QuotedNano {
			return a.QuotedNano > b.QuotedNano
		}
		return a.UpstreamName < b.UpstreamName
	})
	return report, nil
}

// modelHasPrice reports whether a model has any saved price: a rate card or
// a price line.
func modelHasPrice(m *Model) bool {
	return !m.RateEffectiveFrom.IsZero() || m.RateInNano > 0 || m.RateOutNano > 0 || len(m.PriceLines()) > 0
}

// upstreamNamesFor names every upstream in the report, soft-deleted ones
// included: the quote was theirs when the call ran.
func (s *Store) upstreamNamesFor(ctx context.Context, r *PriceAccuracyReport) (map[string]string, error) {
	seen := map[string]bool{}
	args := []any{}
	for _, m := range r.Models {
		if m.UpstreamID != "" && !seen[m.UpstreamID] {
			seen[m.UpstreamID] = true
			args = append(args, m.UpstreamID)
		}
	}
	names := map[string]string{}
	if len(args) == 0 {
		return names, nil
	}
	rows, err := s.query(ctx, `SELECT id, name FROM upstream WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// QuotedEstimateBackfill says what one backfill pass did.
type QuotedEstimateBackfill struct {
	Scanned int // quoted calls without an estimate looked at
	Filled  int // given Janus's price from the price history
}

// cardVersion is one rate_card_version row as the backfill needs it.
type cardVersion struct {
	from  string
	rates usage.Rates
}

// BackfillQuotedEstimates gives quoted calls recorded before estimates
// existed Janus's price at the time, from the price history
// (rate_card_version, effective-dated, lines included since 0079). It is
// conservative: a call stays without an estimate (NULL) unless the history
// prices it exactly as the proxy would have:
//
//   - its model had a price version in force when the call ran (no
//     fallback to today's price: an older price is not known);
//   - it carried tokens. Older quoted media calls (images, video) did not
//     record what was measured, so they have nothing to price;
//   - it is not a batch result (priced at a batch discount factor);
//   - for a decision call, its model's per-call fee is $0 today, since the
//     history does not keep the fee.
//
// Idempotent (it only fills NULLs) and safe to run on several replicas at
// once; it pages by (created_at, id) so a call it cannot price is read once.
func (s *Store) BackfillQuotedEstimates(ctx context.Context, batch int) (QuotedEstimateBackfill, error) {
	var res QuotedEstimateBackfill
	if batch <= 0 {
		batch = 500
	}
	versions := map[string][]cardVersion{}
	fees := map[string]int64{}
	feesLoaded := false
	afterAt, afterID := "", ""
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		type row struct {
			id, created, modelID, modality, endpoint, subscription string
			counts                                                 usage.TokenCounts
		}
		rows, err := s.query(ctx, `SELECT id, created_at, model_id, modality, endpoint_path, subscription_id,
			tokens_in, tokens_out, tokens_cached, tokens_cache_write_5m, tokens_cache_write_1h
			FROM usage_event WHERE `+quotedClause+` AND estimated_cost_nanousd IS NULL
			AND (created_at > ? OR (created_at = ? AND id > ?))
			ORDER BY created_at, id LIMIT ?`, afterAt, afterAt, afterID, batch)
		if err != nil {
			return res, fmt.Errorf("backfill quoted estimates: %w", err)
		}
		var page []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.created, &r.modelID, &r.modality, &r.endpoint, &r.subscription,
				&r.counts.In, &r.counts.Out, &r.counts.Cached, &r.counts.CacheWrite5m, &r.counts.CacheWrite1h); err != nil {
				_ = rows.Close()
				return res, err
			}
			page = append(page, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return res, err
		}
		_ = rows.Close()
		if len(page) == 0 {
			return res, nil
		}
		last := page[len(page)-1]
		afterAt, afterID = last.created, last.id
		res.Scanned += len(page)

		type fill struct {
			id  string
			est int64
		}
		var fills []fill
		for _, r := range page {
			if r.modelID == "" || r.subscription != "" || r.endpoint == "/v1/batches" || (r.counts.In == 0 && r.counts.Out == 0) {
				continue
			}
			vs, ok := versions[r.modelID]
			if !ok {
				if vs, err = s.cardVersions(ctx, r.modelID); err != nil {
					return res, err
				}
				versions[r.modelID] = vs
			}
			rates, ok := versionAt(vs, r.created)
			if !ok {
				continue
			}
			var est int64
			if r.modality == "decision" || r.modality == "classification" {
				// Decision calls are priced by three token rates and a
				// per-call fee (httpapi decisionCost).
				if !feesLoaded {
					if fees, err = s.decisionFees(ctx); err != nil {
						return res, err
					}
					feesLoaded = true
				}
				if fees[r.modelID] != 0 {
					continue
				}
				est = usage.ComputeCostAll(usage.TokenCounts{In: r.counts.In, Out: r.counts.Out, Cached: r.counts.Cached},
					usage.Rates{InNano: rates.InNano, OutNano: rates.OutNano, CachedNano: rates.CachedNano})
			} else {
				est = usage.TokenPrice(r.counts, rates)
			}
			fills = append(fills, fill{r.id, est})
		}
		if len(fills) > 0 {
			err := s.writeTx(ctx, func(s *Store) error {
				for _, f := range fills {
					if err := s.exec(ctx, `UPDATE usage_event SET estimated_cost_nanousd = ? WHERE id = ? AND estimated_cost_nanousd IS NULL`, f.est, f.id); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return res, fmt.Errorf("backfill quoted estimates: %w", err)
			}
			res.Filled += len(fills)
		}
		if len(page) < batch {
			return res, nil
		}
	}
}

// cardVersions lists a model's price history, oldest first.
func (s *Store) cardVersions(ctx context.Context, modelID string) ([]cardVersion, error) {
	rows, err := s.query(ctx, `SELECT effective_from, rate_in_nanousd, rate_out_nanousd, rate_cached_nanousd,
		rate_cache_write_5m_nanousd, rate_cache_write_1h_nanousd, lines_json
		FROM rate_card_version WHERE model_id = ? ORDER BY effective_from`, modelID)
	if err != nil {
		return nil, fmt.Errorf("price history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []cardVersion
	for rows.Next() {
		var v cardVersion
		var lines string
		if err := rows.Scan(&v.from, &v.rates.InNano, &v.rates.OutNano, &v.rates.CachedNano,
			&v.rates.CacheWrite5mNano, &v.rates.CacheWrite1hNano, &lines); err != nil {
			return nil, err
		}
		if lines != "" && lines != "{}" {
			if err := json.Unmarshal([]byte(lines), &v.rates.Lines); err != nil {
				return nil, fmt.Errorf("read price history lines: %w", err)
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// versionAt is the price in force at a moment (both ISO texts compare as
// times, as in RateCardAt); false before the first version.
func versionAt(vs []cardVersion, at string) (usage.Rates, bool) {
	at = FormatTime(ParseTime(at))
	for i := len(vs) - 1; i >= 0; i-- {
		if FormatTime(ParseTime(vs[i].from)) <= at {
			return vs[i].rates, true
		}
	}
	return usage.Rates{}, false
}

// decisionFees maps decision models to their per-call fee.
func (s *Store) decisionFees(ctx context.Context) (map[string]int64, error) {
	settings, err := s.ListDecisionSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(settings))
	for id, d := range settings {
		out[id] = d.RequestFeeNano
	}
	return out, nil
}
