package store

// Input tokens as billed.
//
// Owner ruling: "We should be recording the cached tokens the same way that
// they are billed by both of these entities" (Anthropic and AWS Bedrock).
// Both bill every prompt token: fresh input at the input rate, cache reads at
// the cache-read rate, cache writes at the 5-minute / 1-hour write rates.
// OpenAI-style providers do the same with cached tokens counted inside
// prompt_tokens. So usage_event.tokens_in is the whole prompt for every
// provider, and tokens_cached / tokens_cache_write_5m / tokens_cache_write_1h
// are parts of it billed at their own rates. The adapters produce it that way
// (adapter.Usage); every figure that counts input tokens (quotas, tiles,
// request log, CSV, Prometheus, reports, cache hit rate = cached / tokens_in)
// reads tokens_in as it is.
//
// Before, Anthropic and Bedrock rows stored only the fresh part in tokens_in.
// Migration 0082 flagged those rows (usage_event.cached_disjoint) and gave
// the rollup a separate whole-prompt sum (usage_rollup.tokens_prompt), with
// read-side corrections. 0083 supersedes both: it moves the cache parts into
// tokens_in on the flagged rows and in the rollup, and nothing reads or
// writes the two columns any more.

// cacheDisjointMigration is 0082 exactly as it shipped (migrations are
// immutable once released). Its columns are unused since 0083.
var cacheDisjointMigration = migration{name: "0082_usage_cached_disjoint", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN cached_disjoint INTEGER NOT NULL DEFAULT 0`,
	`UPDATE usage_event SET cached_disjoint = 1
		WHERE upstream_id IN (SELECT id FROM upstream WHERE adapter_type IN ('anthropic', 'bedrock'))
		AND modality NOT IN ('classification', 'decision')
		AND (tokens_cached > 0 OR tokens_cache_write_5m > 0 OR tokens_cache_write_1h > 0)`,
	`ALTER TABLE usage_rollup ADD COLUMN tokens_prompt BIGINT NOT NULL DEFAULT 0`,
	`UPDATE usage_rollup SET tokens_prompt = tokens_in`,
	`UPDATE usage_rollup SET tokens_prompt = usage_rollup.tokens_prompt + d.extra
		FROM (SELECT substr(created_at, 1, 13) || ':00:00.000000000Z' AS bucket, user_id, service_token_id, token_id, team_ids,
				model_name, requested_model_name, http_status,
				SUM(CASE WHEN cached_disjoint <> 0 THEN tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h
					ELSE tokens_cached - tokens_in END) AS extra
			FROM usage_event
			WHERE created_at < (SELECT value FROM usage_rollup_state WHERE key = 'watermark')
			AND ((cached_disjoint <> 0 AND tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h > 0)
				OR (cached_disjoint = 0 AND tokens_cached > tokens_in))
			GROUP BY substr(created_at, 1, 13) || ':00:00.000000000Z', user_id, service_token_id, token_id, team_ids,
				model_name, requested_model_name, http_status) d
		WHERE usage_rollup.bucket = d.bucket AND usage_rollup.user_id = d.user_id
		AND usage_rollup.service_token_id = d.service_token_id AND usage_rollup.token_id = d.token_id
		AND usage_rollup.team_ids = d.team_ids AND usage_rollup.model_name = d.model_name
		AND usage_rollup.requested_model_name = d.requested_model_name AND usage_rollup.http_status = d.http_status`,
}, down: []string{
	`ALTER TABLE usage_rollup DROP COLUMN tokens_prompt`,
	`ALTER TABLE usage_event DROP COLUMN cached_disjoint`,
}}

// tokensInAsBilledStmts move the cache parts into tokens_in on every row
// still flagged cached_disjoint, rollup first (it finds the affected rows by
// the flag), then the rows themselves, clearing the flag so a second run
// changes nothing. Only rows that change are touched: flagged rows with cache
// reads or writes, and the rollup rows (hour × dimensions) they were folded
// into before the watermark; events after the watermark are folded later from
// the already-corrected rows. Costs are stored per row and are not touched:
// billing never depended on this representation.
var tokensInAsBilledStmts = []string{
	`UPDATE usage_rollup SET tokens_in = usage_rollup.tokens_in + d.extra
		FROM (SELECT substr(created_at, 1, 13) || ':00:00.000000000Z' AS bucket, user_id, service_token_id, token_id, team_ids,
				model_name, requested_model_name, http_status,
				SUM(tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h) AS extra
			FROM usage_event
			WHERE cached_disjoint <> 0 AND tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h > 0
			AND created_at < (SELECT value FROM usage_rollup_state WHERE key = 'watermark')
			GROUP BY substr(created_at, 1, 13) || ':00:00.000000000Z', user_id, service_token_id, token_id, team_ids,
				model_name, requested_model_name, http_status) d
		WHERE usage_rollup.bucket = d.bucket AND usage_rollup.user_id = d.user_id
		AND usage_rollup.service_token_id = d.service_token_id AND usage_rollup.token_id = d.token_id
		AND usage_rollup.team_ids = d.team_ids AND usage_rollup.model_name = d.model_name
		AND usage_rollup.requested_model_name = d.requested_model_name AND usage_rollup.http_status = d.http_status`,
	`UPDATE usage_event SET tokens_in = tokens_in + tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h,
		cached_disjoint = 0
		WHERE cached_disjoint <> 0 AND tokens_cached + tokens_cache_write_5m + tokens_cache_write_1h > 0`,
}

// tokensInAsBilledMigration (0083) applies tokensInAsBilledStmts. On
// PostgreSQL it first takes the rollup writer's lock (rollupLockKey) for the
// transaction, so no replica folds events into the rollup between the rollup
// correction and the row correction.
//
// The two superseded columns stay in the schema for one release, unused: the
// gateway rolls out with RollingUpdate (maxUnavailable 0), so pods of the
// previous release keep inserting usage_event.cached_disjoint and rolling
// usage_rollup.tokens_prompt while the new pod migrates; dropping them here
// would make those inserts fail and lose their usage records. The rows those
// pods write during the rollout are still flagged, so the release after this
// one runs tokensInAsBilledStmts again and then drops both columns
// (SQLite 3.35+ supports DROP COLUMN; modernc.org/sqlite bundles 3.46).
var tokensInAsBilledMigration = migration{name: "0083_usage_tokens_in_as_billed",
	pgStmt: []string{`SELECT pg_advisory_xact_lock(7665529491487158895)`}, // rollupLockKey
	stmt:   tokensInAsBilledStmts,
}
