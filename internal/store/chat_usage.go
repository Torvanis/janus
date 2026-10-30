package store

import (
	"context"
	"database/sql"
	"errors"
)

// Chat accounting on the usage log. One Chat reply can make many gateway
// calls (tool rounds, a no-tools retry, deep-research rounds, image, video
// and music calls, media conversion, the auto-title). Each is a normal
// metered request; chat_message_id ties them to the reply they belong to,
// so the reply's footer can show the whole reply's tokens and cost.
// chat_subcall marks every call after the first of one user action: it
// counts in full for tokens and cost, but not again as a request (rate
// limits and request-count quotas count the user's message once).
var usageChatMessageMigration = migration{name: "0069_usage_chat_message", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN chat_message_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE usage_event ADD COLUMN chat_subcall INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX IF NOT EXISTS idx_usage_chat_message ON usage_event (chat_message_id)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_usage_chat_message`,
	`ALTER TABLE usage_event DROP COLUMN chat_subcall`,
	`ALTER TABLE usage_event DROP COLUMN chat_message_id`,
}}

// requestCountExpr counts requests for request-count quotas: a Chat
// sub-call is not a request of its own.
const requestCountExpr = "COALESCE(SUM(CASE WHEN chat_subcall = 0 THEN 1 ELSE 0 END),0)"

// chatOwnUsageClause keeps the rows a Chat reply's notes and footer may
// read: never the gateway's own calls that share the request id (Security
// Gateway classifier calls, auto-routing evaluation and judge calls).
const chatOwnUsageClause = "client_app NOT IN ('" + InternalClientApp + "','" + InternalRoutingClientApp + "')"

// ChatUsageTotals is the sum of every usage event recorded for one Chat
// reply (all of its rounds and sub-calls).
type ChatUsageTotals struct {
	Events       int
	TokensIn     int64
	TokensOut    int64
	TokensCached int64
	CostNano     int64
}

// ChatMessageUsage sums the usage events tagged with a Chat message id.
func (s *Store) ChatMessageUsage(ctx context.Context, messageID string) (ChatUsageTotals, error) {
	var t ChatUsageTotals
	if messageID == "" {
		return t, nil
	}
	err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
		COALESCE(SUM(tokens_cached),0), COALESCE(SUM(cost_nanousd),0) FROM usage_event
		WHERE chat_message_id = ? AND `+chatOwnUsageClause, messageID).Scan(&t.Events, &t.TokensIn, &t.TokensOut, &t.TokensCached, &t.CostNano)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	return t, err
}
