package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Usage rollups.
//
// Every console list that shows "last 30 days" figures (API tokens, service
// tokens, managed models, the admin overview) used to aggregate usage_event,
// the raw request log, on every page load, often once per row. Cost grew
// with total traffic: at 1.3 M events one page load took 15-130 s.
//
// usage_rollup holds the same sums per UTC hour and per reporting dimension
// (user, service token, API token, team snapshot, model, requested model,
// HTTP status). Reads combine it with the raw log:
//
//	whole hours inside the window, before the watermark -> usage_rollup
//	the ragged first hour and everything after the watermark -> usage_event
//
// so results are exactly what a raw scan returns, while the raw part stays a
// few minutes of events regardless of history size.
//
// Writers. Nothing on the request path touches the rollup: metering stays
// one INSERT per event (or a batch in performance mode) with no shared row.
// One background roller per cluster (an advisory lock elects it per run)
// folds events older than rollupGrace into the rollup and advances the
// watermark. rollupGrace exceeds the longest time between an event's
// created_at and its commit (a slow insert or a performance-mode batch), so
// an event is never committed behind the watermark. ReconcileUsageRollup
// re-checks recent hours against the raw log as a backstop (e.g. writes that
// were retried after a database outage longer than the grace).
//
// The two operations that rewrite raw history keep the rollup consistent in
// the same transaction: ChangeTokenTeam with move_history rebuilds that
// token's rows, and PurgeUsageEvents drops purged hours.
//
// Before the roller first runs (fresh install, tests), there is no watermark
// and every read uses the raw log, exactly as before.

const (
	// rollupGrace is how far behind wall-clock time the watermark stays.
	rollupGrace = 5 * time.Minute
	// rollupChunk bounds the event-time span folded per transaction, so the
	// first backfill of a large log proceeds in small steps.
	rollupChunk = 6 * time.Hour
	// rollupReconcileSpan is how many hours behind the watermark a
	// reconcile pass re-checks.
	rollupReconcileSpan = 3 * time.Hour
	// rollupLockKey serialises rollup writers across replicas ("janus_ro").
	rollupLockKey int64 = 0x6a616e75735f726f
)

var usageRollupMigration = migration{name: "0046_usage_rollup", stmt: []string{
	`CREATE TABLE IF NOT EXISTS usage_rollup (
		bucket TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		service_token_id TEXT NOT NULL DEFAULT '',
		token_id TEXT NOT NULL DEFAULT '',
		team_ids TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		requested_model_name TEXT NOT NULL DEFAULT '',
		http_status INTEGER NOT NULL DEFAULT 0,
		request_count BIGINT NOT NULL DEFAULT 0,
		error_count BIGINT NOT NULL DEFAULT 0,
		tokens_in BIGINT NOT NULL DEFAULT 0,
		tokens_out BIGINT NOT NULL DEFAULT 0,
		tokens_cached BIGINT NOT NULL DEFAULT 0,
		tokens_cache_write_5m BIGINT NOT NULL DEFAULT 0,
		tokens_cache_write_1h BIGINT NOT NULL DEFAULT 0,
		cost_nanousd BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS usage_rollup_key ON usage_rollup(bucket, user_id, service_token_id, token_id, team_ids, model_name, requested_model_name, http_status)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_rollup_token ON usage_rollup(token_id, bucket)`,
	`CREATE TABLE IF NOT EXISTS usage_rollup_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	// Rebuilding one token's history (move_history) reads it by token.
	`CREATE INDEX IF NOT EXISTS idx_usage_token_time ON usage_event(token_id, created_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_usage_token_time`,
	`DROP TABLE IF EXISTS usage_rollup_state`,
	`DROP INDEX IF EXISTS idx_usage_rollup_token`,
	`DROP INDEX IF EXISTS usage_rollup_key`,
	`DROP TABLE IF EXISTS usage_rollup`,
}}

const rollupDims = `user_id, service_token_id, token_id, team_ids, model_name, requested_model_name, http_status`

const rollupSums = `request_count, error_count, tokens_in, tokens_out, tokens_cached, tokens_cache_write_5m, tokens_cache_write_1h, cost_nanousd`

// rollupHourExpr truncates a stored created_at ("2006-01-02T15:04:05.000000000Z")
// to its UTC hour in the same fixed-width layout, so buckets compare as text.
const rollupHourExpr = `substr(created_at, 1, 13) || ':00:00.000000000Z'`

const rollupSelect = rollupHourExpr + `, ` + rollupDims + `,
	COUNT(*), COALESCE(SUM(CASE WHEN http_status >= 400 THEN 1 ELSE 0 END),0),
	COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(tokens_cached),0),
	COALESCE(SUM(tokens_cache_write_5m),0), COALESCE(SUM(tokens_cache_write_1h),0), COALESCE(SUM(cost_nanousd),0)`

const rollupUpsert = ` ON CONFLICT (bucket, ` + rollupDims + `) DO UPDATE SET
	request_count = usage_rollup.request_count + excluded.request_count,
	error_count = usage_rollup.error_count + excluded.error_count,
	tokens_in = usage_rollup.tokens_in + excluded.tokens_in,
	tokens_out = usage_rollup.tokens_out + excluded.tokens_out,
	tokens_cached = usage_rollup.tokens_cached + excluded.tokens_cached,
	tokens_cache_write_5m = usage_rollup.tokens_cache_write_5m + excluded.tokens_cache_write_5m,
	tokens_cache_write_1h = usage_rollup.tokens_cache_write_1h + excluded.tokens_cache_write_1h,
	cost_nanousd = usage_rollup.cost_nanousd + excluded.cost_nanousd`

func floorHour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

func ceilHour(t time.Time) time.Time {
	f := floorHour(t)
	if f.Equal(t.UTC()) {
		return f
	}
	return f.Add(time.Hour)
}

// rollupLock takes the cluster-wide rollup writer lock for this transaction.
// wait=false returns false when another writer holds it. SQLite has a single
// writer connection, so the transaction itself is the lock.
func (s *Store) rollupLock(ctx context.Context, wait bool) (bool, error) {
	if s.dialect != DialectPostgres {
		return true, nil
	}
	if wait {
		return true, s.exec(ctx, `SELECT pg_advisory_xact_lock(?)`, rollupLockKey)
	}
	var ok bool
	if err := s.queryRow(ctx, `SELECT pg_try_advisory_xact_lock(?)`, rollupLockKey).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// rollupWatermark is the instant before which usage_rollup is complete.
// ok=false means the roller has never run: reads use the raw log only.
func (s *Store) rollupWatermark(ctx context.Context) (time.Time, bool, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM usage_rollup_state WHERE key = 'watermark'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read rollup watermark: %w", err)
	}
	return ParseTime(v), true, nil
}

func (s *Store) setRollupWatermark(ctx context.Context, wm time.Time) error {
	return s.exec(ctx, `INSERT INTO usage_rollup_state (key, value) VALUES ('watermark', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, FormatTime(wm))
}

// foldRaw adds the raw events in [from, to) matching extra into the rollup.
func (s *Store) foldRaw(ctx context.Context, from, to time.Time, extra string, extraArgs ...any) error {
	where := `created_at >= ? AND created_at < ?`
	args := []any{FormatTime(from), FormatTime(to)}
	if extra != "" {
		where += " AND " + extra
		args = append(args, extraArgs...)
	}
	return s.exec(ctx, `INSERT INTO usage_rollup (bucket, `+rollupDims+`, `+rollupSums+`)
		SELECT `+rollupSelect+` FROM usage_event WHERE `+where+`
		GROUP BY `+rollupHourExpr+`, `+rollupDims+rollupUpsert, args...)
}

// RollupUsage folds one chunk of settled events into usage_rollup and
// advances the watermark. done reports that the watermark reached
// now-rollupGrace (or another replica is rolling); call again until done.
func (s *Store) RollupUsage(ctx context.Context, now time.Time) (done bool, err error) {
	err = s.writeTx(ctx, func(s *Store) error {
		ok, err := s.rollupLock(ctx, false)
		if err != nil || !ok {
			done = true
			return err
		}
		target := now.UTC().Add(-rollupGrace)
		wm, have, err := s.rollupWatermark(ctx)
		if err != nil {
			return err
		}
		if !have {
			var first sql.NullString
			if err := s.queryRow(ctx, `SELECT MIN(created_at) FROM usage_event`).Scan(&first); err != nil {
				return fmt.Errorf("find first usage event: %w", err)
			}
			if !first.Valid || first.String == "" {
				done = true
				return s.setRollupWatermark(ctx, target)
			}
			wm = floorHour(ParseTime(first.String))
		}
		if !target.After(wm) {
			done = true
			return nil
		}
		next := wm.Add(rollupChunk)
		if !next.Before(target) {
			next, done = target, true
		}
		if err := s.foldRaw(ctx, wm, next, ""); err != nil {
			return fmt.Errorf("fold usage into rollup: %w", err)
		}
		return s.setRollupWatermark(ctx, next)
	})
	return done, err
}

// ReconcileUsageRollup re-checks the hours just behind the watermark against
// the raw log and rebuilds any whose request count differs. It returns how
// many hours were rebuilt. Cheap when nothing is wrong (one indexed count).
func (s *Store) ReconcileUsageRollup(ctx context.Context) (int, error) {
	rebuilt := 0
	err := s.writeTx(ctx, func(s *Store) error {
		ok, err := s.rollupLock(ctx, false)
		if err != nil || !ok {
			return err
		}
		wm, have, err := s.rollupWatermark(ctx)
		if err != nil || !have {
			return err
		}
		from := floorHour(wm).Add(-rollupReconcileSpan)
		raw := map[string]int64{}
		rows, err := s.query(ctx, `SELECT `+rollupHourExpr+`, COUNT(*) FROM usage_event
			WHERE created_at >= ? AND created_at < ? GROUP BY `+rollupHourExpr, FormatTime(from), FormatTime(wm))
		if err != nil {
			return err
		}
		for rows.Next() {
			var b string
			var n int64
			if err := rows.Scan(&b, &n); err != nil {
				_ = rows.Close()
				return err
			}
			raw[b] = n
		}
		if err := rows.Close(); err != nil {
			return err
		}
		rolled := map[string]int64{}
		rows, err = s.query(ctx, `SELECT bucket, SUM(request_count) FROM usage_rollup
			WHERE bucket >= ? AND bucket < ? GROUP BY bucket`, FormatTime(from), FormatTime(wm))
		if err != nil {
			return err
		}
		for rows.Next() {
			var b string
			var n int64
			if err := rows.Scan(&b, &n); err != nil {
				_ = rows.Close()
				return err
			}
			rolled[b] = n
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for h := from; h.Before(wm); h = h.Add(time.Hour) {
			key := FormatTime(h)
			if raw[key] == rolled[key] {
				continue
			}
			if err := s.rebuildRollupHour(ctx, h, wm, ""); err != nil {
				return err
			}
			rebuilt++
		}
		return nil
	})
	return rebuilt, err
}

// rebuildRollupHour replaces hour h's rollup rows (matching extra) from the
// raw log, up to the watermark.
func (s *Store) rebuildRollupHour(ctx context.Context, h, wm time.Time, extra string, extraArgs ...any) error {
	del := `DELETE FROM usage_rollup WHERE bucket = ?`
	args := []any{FormatTime(h)}
	if extra != "" {
		del += " AND " + extra
		args = append(args, extraArgs...)
	}
	if err := s.exec(ctx, del, args...); err != nil {
		return fmt.Errorf("clear rollup hour: %w", err)
	}
	end := h.Add(time.Hour)
	if wm.Before(end) {
		end = wm
	}
	if !end.After(h) {
		return nil
	}
	return s.foldRaw(ctx, h, end, extra, extraArgs...)
}

// rebuildRollupToken rebuilds one API token's rollup rows from the raw log,
// after its history was re-attributed. Runs inside the caller's transaction.
func (s *Store) rebuildRollupToken(ctx context.Context, tokenID string) error {
	if _, err := s.rollupLock(ctx, true); err != nil {
		return err
	}
	wm, have, err := s.rollupWatermark(ctx)
	if err != nil || !have {
		return err
	}
	if err := s.exec(ctx, `DELETE FROM usage_rollup WHERE token_id = ?`, tokenID); err != nil {
		return fmt.Errorf("clear token rollup: %w", err)
	}
	return s.foldRaw(ctx, time.Time{}, wm, `token_id = ?`, tokenID)
}

// purgeRollupBefore drops rolled-up hours wholly before a purge cutoff and
// rebuilds the hour the cutoff falls in. Runs inside the caller's transaction,
// after the raw rows are gone.
func (s *Store) purgeRollupBefore(ctx context.Context, before time.Time) error {
	if _, err := s.rollupLock(ctx, true); err != nil {
		return err
	}
	wm, have, err := s.rollupWatermark(ctx)
	if err != nil || !have {
		return err
	}
	h := floorHour(before)
	if err := s.exec(ctx, `DELETE FROM usage_rollup WHERE bucket < ?`, FormatTime(h)); err != nil {
		return fmt.Errorf("purge rollup: %w", err)
	}
	if h.Before(wm) && !h.Equal(before.UTC()) {
		return s.rebuildRollupHour(ctx, h, wm, "")
	}
	return nil
}

// --- reads -------------------------------------------------------------------

// rollupDimension maps a breakdown dimension to its column when the rollup
// carries it. Dimensions it does not carry (modality, upstream, endpoint)
// are read from the raw log.
var rollupDimension = map[string]string{
	"model":           "model_name",
	"token":           "token_id",
	"user":            "user_id",
	"status":          "http_status",
	"service_token":   "service_token_id",
	"requested_model": "requested_model_name",
	// team_snapshot groups by the recorded team list; callers split it.
	"team_snapshot": "team_ids",
}

// srcTotalsExpr aggregates a usageSource into Totals scan order.
const srcTotalsExpr = `COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(SUM(tokens_cached),0),
	COALESCE(SUM(tokens_cache_write_5m),0), COALESCE(SUM(tokens_cache_write_1h),0),
	COALESCE(SUM(cost_nanousd),0), COALESCE(SUM(request_count),0), COALESCE(SUM(error_count),0)`

const srcCols = `user_id, service_token_id, token_id, team_ids, model_name, requested_model_name, http_status,
	tokens_in, tokens_out, tokens_cached, tokens_cache_write_5m, tokens_cache_write_1h, cost_nanousd`

// usageSource returns a derived table "(...) u" with one row per raw event
// or rollup row in scope, columns: t (event time or hour start), srcCols,
// request_count, error_count. forceRaw skips the rollup (sub-hour series).
func (s *Store) usageSource(ctx context.Context, scope UsageScope, forceRaw bool) (string, []any, error) {
	dim, dimArgs := scope.dimClause()
	rawSel := `SELECT created_at AS t, ` + srcCols + `, 1 AS request_count,
		CASE WHEN http_status >= 400 THEN 1 ELSE 0 END AS error_count FROM usage_event WHERE `
	rawOnly := func() (string, []any, error) {
		where, args := dim, append([]any{}, dimArgs...)
		if !scope.Start.IsZero() {
			where += " AND created_at >= ?"
			args = append(args, FormatTime(scope.Start))
		}
		if !scope.End.IsZero() {
			where += " AND created_at <= ?"
			args = append(args, FormatTime(scope.End))
		}
		return "(" + rawSel + where + ") u", args, nil
	}
	if forceRaw {
		return rawOnly()
	}
	wm, have, err := s.rollupWatermark(ctx)
	if err != nil {
		return "", nil, err
	}
	if !have {
		return rawOnly()
	}
	var h1 time.Time
	if !scope.Start.IsZero() {
		h1 = ceilHour(scope.Start)
	}
	cut := wm
	if !scope.End.IsZero() && scope.End.Before(wm) {
		cut = floorHour(scope.End)
	}
	if !h1.IsZero() && !cut.After(h1) {
		return rawOnly()
	}

	rollWhere, rollArgs := dim, append([]any{}, dimArgs...)
	if !h1.IsZero() {
		rollWhere += " AND bucket >= ?"
		rollArgs = append(rollArgs, FormatTime(h1))
	}
	rollWhere += " AND bucket < ?"
	rollArgs = append(rollArgs, FormatTime(cut))

	segments := []string{}
	rawArgs := append([]any{}, dimArgs...)
	if !scope.Start.IsZero() && h1.After(scope.Start) {
		segments = append(segments, "(created_at >= ? AND created_at < ?)")
		rawArgs = append(rawArgs, FormatTime(scope.Start), FormatTime(h1))
	}
	tail := "created_at >= ?"
	rawArgs = append(rawArgs, FormatTime(cut))
	if !scope.End.IsZero() {
		tail += " AND created_at <= ?"
		rawArgs = append(rawArgs, FormatTime(scope.End))
	}
	segments = append(segments, "("+tail+")")

	sqlText := "(SELECT bucket AS t, " + srcCols + ", request_count, error_count FROM usage_rollup WHERE " + rollWhere +
		" UNION ALL " + rawSel + dim + " AND (" + strings.Join(segments, " OR ") + ")) u"
	return sqlText, append(rollArgs, rawArgs...), nil
}

// AggregateUsageBy returns totals per value of one dimension (e.g. one row
// per API token) in a single query: list pages call this instead of one
// AggregateUsage per row.
func (s *Store) AggregateUsageBy(ctx context.Context, scope UsageScope, dimension string) (map[string]Totals, error) {
	col, ok := rollupDimension[dimension]
	if !ok {
		return nil, fmt.Errorf("unsupported grouping dimension %q", dimension)
	}
	src, args, err := s.usageSource(ctx, scope, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT `+col+`, `+srcTotalsExpr+` FROM `+src+` GROUP BY `+col, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage by %s: %w", dimension, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Totals{}
	for rows.Next() {
		var key any
		var t Totals
		if err := rows.Scan(&key, &t.TokensIn, &t.TokensOut, &t.TokensCached, &t.TokensCacheWrite5m, &t.TokensCacheWrite1h,
			&t.CostNano, &t.Requests, &t.ErrorCount); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%v", key)] = t
	}
	return out, rows.Err()
}

// BreakdownUsageBy is BreakdownUsage for every value of groupDim at once:
// for each group key, the dimension's rows ranked by tokens_out then
// requests (BreakdownUsage's default), at most 50, with the same model
// catalog filtering.
func (s *Store) BreakdownUsageBy(ctx context.Context, scope UsageScope, groupDim, dimension string) (map[string][]Breakdown, error) {
	gcol, ok := rollupDimension[groupDim]
	col, ok2 := rollupDimension[dimension]
	if !ok || !ok2 {
		return nil, fmt.Errorf("unsupported breakdown %q by %q", dimension, groupDim)
	}
	// Load the catalog before opening the result set: SQLite has one
	// connection, so a query inside the row loop would wait forever.
	var valid map[string]struct{}
	if dimension == "model" {
		var cerr error
		if valid, cerr = s.catalogModelNames(ctx); cerr != nil {
			s.logger().WarnContext(ctx, "grouped model breakdown: could not load the model catalog; returning unfiltered results", "error", cerr.Error())
			valid = nil
		}
	}
	src, args, err := s.usageSource(ctx, scope, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT `+gcol+`, `+col+`, `+srcTotalsExpr+` FROM `+src+` GROUP BY `+gcol+`, `+col, args...)
	if err != nil {
		return nil, fmt.Errorf("breakdown %s by %s: %w", dimension, groupDim, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]Breakdown{}
	for rows.Next() {
		var g, key any
		var b Breakdown
		if err := rows.Scan(&g, &key, &b.Totals.TokensIn, &b.Totals.TokensOut, &b.Totals.TokensCached,
			&b.Totals.TokensCacheWrite5m, &b.Totals.TokensCacheWrite1h, &b.Totals.CostNano, &b.Totals.Requests, &b.Totals.ErrorCount); err != nil {
			return nil, err
		}
		b.Key = fmt.Sprintf("%v", key)
		b.Label = b.Key
		if valid != nil {
			if _, ok := valid[b.Key]; !ok {
				continue
			}
		}
		gk := fmt.Sprintf("%v", g)
		out[gk] = append(out[gk], b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, items := range out {
		sort.Slice(items, func(i, j int) bool {
			if items[i].Totals.TokensOut != items[j].Totals.TokensOut {
				return items[i].Totals.TokensOut > items[j].Totals.TokensOut
			}
			if items[i].Totals.Requests != items[j].Totals.Requests {
				return items[i].Totals.Requests > items[j].Totals.Requests
			}
			return items[i].Key < items[j].Key
		})
		if len(items) > 50 {
			out[k] = items[:50]
		}
	}
	return out, nil
}

// CountDistinctPrincipalsBy is CountDistinctPrincipals per value of one
// dimension, in one query.
func (s *Store) CountDistinctPrincipalsBy(ctx context.Context, scope UsageScope, dimension string) (map[string]int64, error) {
	col, ok := rollupDimension[dimension]
	if !ok {
		return nil, fmt.Errorf("unsupported grouping dimension %q", dimension)
	}
	src, args, err := s.usageSource(ctx, scope, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, `SELECT `+col+`,
		COUNT(DISTINCT CASE WHEN user_id <> '' THEN user_id END),
		COUNT(DISTINCT CASE WHEN service_token_id <> '' THEN service_token_id END)
		FROM `+src+` GROUP BY `+col, args...)
	if err != nil {
		return nil, fmt.Errorf("count principals by %s: %w", dimension, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var key any
		var users, tokens int64
		if err := rows.Scan(&key, &users, &tokens); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%v", key)] = users + tokens
	}
	return out, rows.Err()
}
