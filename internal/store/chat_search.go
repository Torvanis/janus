package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Janus Chat search: a keyed blind index over encrypted chat content.
//
// Chat bodies stay encrypted at rest, so the database cannot search them.
// Instead the HTTP layer tokenizes each message (internal/chatsearch) and
// stores only HMAC(searchKey, userID, term) per message in
// chat_search_term; a query is hashed the same way and matched by equality.
// The store never sees a plaintext word. Matching is whole word (after
// light stemming) only: substring search is intentionally unsupported.
//
// Conversation titles are indexed too, as rows whose message_id is
// ChatTitleMessageID(conversationID) ("title:<conversation id>"), so no
// extra column is needed and a title row is replaced like any message.

var chatSearchMigration = migration{name: "0055_chat_search", stmt: []string{
	`CREATE TABLE IF NOT EXISTS chat_search_term (
		user_id TEXT NOT NULL,
		term_hash TEXT NOT NULL,
		message_id TEXT NOT NULL,
		conversation_id TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_search_term_user_hash ON chat_search_term(user_id, term_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_search_term_message ON chat_search_term(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_search_term_conversation ON chat_search_term(conversation_id)`,
	`ALTER TABLE chat_message ADD COLUMN context_dropped INTEGER NOT NULL DEFAULT 0`,
}, down: []string{
	`ALTER TABLE chat_message DROP COLUMN context_dropped`,
	`DROP INDEX IF EXISTS idx_chat_search_term_conversation`,
	`DROP INDEX IF EXISTS idx_chat_search_term_message`,
	`DROP INDEX IF EXISTS idx_chat_search_term_user_hash`,
	`DROP TABLE IF EXISTS chat_search_term`,
}}

// chatTitlePrefix marks the search rows that index a conversation title.
const chatTitlePrefix = "title:"

// ChatTitleMessageID is the message_id under which a conversation's title is
// indexed in chat_search_term.
func ChatTitleMessageID(conversationID string) string { return chatTitlePrefix + conversationID }

// IsChatTitleMessageID reports whether a search row's message_id is a title.
func IsChatTitleMessageID(id string) bool { return strings.HasPrefix(id, chatTitlePrefix) }

// chatSearchInsertBatch bounds the rows per INSERT (4 parameters each), well
// under both databases' bind-parameter limits.
const chatSearchInsertBatch = 500

// IndexChatMessage replaces the search terms of one message (or title row)
// with termHashes. An empty list just removes the message from the index.
func (s *Store) IndexChatMessage(ctx context.Context, userID, conversationID, messageID string, termHashes []string) error {
	seen := map[string]bool{}
	uniq := make([]string, 0, len(termHashes))
	for _, h := range termHashes {
		if h != "" && !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		if err := ts.exec(ctx, `DELETE FROM chat_search_term WHERE message_id = ?`, messageID); err != nil {
			return err
		}
		// Temporary conversations are never searchable, whoever asks.
		var temporary int
		if err := ts.queryRow(ctx, `SELECT temporary FROM chat_conversation WHERE id = ?`, conversationID).Scan(&temporary); err == nil && temporary != 0 {
			return nil
		}
		for start := 0; start < len(uniq); start += chatSearchInsertBatch {
			end := min(start+chatSearchInsertBatch, len(uniq))
			var b strings.Builder
			args := make([]any, 0, (end-start)*4)
			b.WriteString(`INSERT INTO chat_search_term (user_id, term_hash, message_id, conversation_id) VALUES `)
			for i, h := range uniq[start:end] {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString("(?,?,?,?)")
				args = append(args, userID, h, messageID, conversationID)
			}
			if err := ts.exec(ctx, b.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetChatConversationTitleIf renames a conversation only while its title is
// still oldTitle (so an automatic title never overwrites a user's rename),
// and reports whether it did.
func (s *Store) SetChatConversationTitleIf(ctx context.Context, userID, id, oldTitle, newTitle string) (bool, error) {
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	res, err := runner.ExecContext(ctx, s.rebind(`UPDATE chat_conversation SET title = ? WHERE id = ? AND user_id = ? AND title = ? AND deleted_at = ''`),
		newTitle, id, userID, oldTitle)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ChatSearchHit is one conversation matching a search.
type ChatSearchHit struct {
	ConversationID string
	Title          string
	UpdatedAt      time.Time
	// Matches is how many messages (titles excluded) hold any query term.
	Matches int
	// BestMessageID is the message holding the most distinct query terms
	// (a real message preferred over the title row on ties); the caller
	// decrypts it for the snippet. May be a title row id.
	BestMessageID string
}

// SearchChat returns userID's live conversations that contain ALL of
// termHashes (anywhere in the conversation, title included), ranked by the
// number of matching messages and then by most recently updated.
func (s *Store) SearchChat(ctx context.Context, userID string, termHashes []string, limit int) ([]ChatSearchHit, error) {
	seen := map[string]bool{}
	var hashes []string
	for _, h := range termHashes {
		if h != "" && !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return []ChatSearchHit{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	in := placeholders(len(hashes))
	args := []any{userID}
	args = append(args, toArgs(hashes)...)
	args = append(args, len(hashes), userID)
	args = append(args, toArgs(hashes)...)
	args = append(args, userID, limit)
	rows, err := s.query(ctx, `SELECT t.conversation_id, c.title, c.updated_at,
			COUNT(DISTINCT CASE WHEN t.message_id LIKE 'title:%' THEN NULL ELSE t.message_id END) AS matches
		FROM chat_search_term t
		JOIN (SELECT conversation_id FROM chat_search_term
			WHERE user_id = ? AND term_hash IN (`+in+`)
			GROUP BY conversation_id HAVING COUNT(DISTINCT term_hash) = ?) hit ON hit.conversation_id = t.conversation_id
		JOIN chat_conversation c ON c.id = t.conversation_id
		WHERE t.user_id = ? AND t.term_hash IN (`+in+`) AND c.user_id = ? AND c.deleted_at = '' AND c.temporary = 0
		GROUP BY t.conversation_id, c.title, c.updated_at
		ORDER BY matches DESC, c.updated_at DESC, t.conversation_id
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	out := []ChatSearchHit{}
	for rows.Next() {
		var h ChatSearchHit
		var updated string
		if err := rows.Scan(&h.ConversationID, &h.Title, &updated, &h.Matches); err != nil {
			_ = rows.Close()
			return nil, err
		}
		h.UpdatedAt = ParseTime(updated)
		out = append(out, h)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		bargs := []any{userID, out[i].ConversationID}
		bargs = append(bargs, toArgs(hashes)...)
		if err := s.queryRow(ctx, `SELECT message_id FROM chat_search_term
			WHERE user_id = ? AND conversation_id = ? AND term_hash IN (`+in+`)
			GROUP BY message_id
			ORDER BY COUNT(DISTINCT term_hash) DESC, CASE WHEN message_id LIKE 'title:%' THEN 1 ELSE 0 END, message_id DESC
			LIMIT 1`, bargs...).Scan(&out[i].BestMessageID); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}

// ChatReindexMessage is one message with its owner, for rebuilding the
// search index.
type ChatReindexMessage struct {
	UserID  string
	Message *ChatMessage
}

// ChatMessagesForReindex pages through every message of live conversations
// in id order (keyset: pass the last id seen).
func (s *Store) ChatMessagesForReindex(ctx context.Context, afterID string, limit int) ([]ChatReindexMessage, error) {
	cols := strings.Split(chatMessageColumns, ",")
	for i, c := range cols {
		cols[i] = "m." + strings.TrimSpace(c)
	}
	rows, err := s.query(ctx, `SELECT `+strings.Join(cols, ", ")+`, c.user_id FROM chat_message m
		JOIN chat_conversation c ON c.id = m.conversation_id
		WHERE c.deleted_at = '' AND m.id > ? ORDER BY m.id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ChatReindexMessage{}
	for rows.Next() {
		var item ChatReindexMessage
		m, err := scanChatMessage(func(dest ...any) error {
			return rows.Scan(append(dest, &item.UserID)...)
		})
		if err != nil {
			return nil, err
		}
		item.Message = m
		out = append(out, item)
	}
	return out, rows.Err()
}

// ChatConversationsForReindex pages through every live conversation in id
// order (keyset: pass the last id seen).
func (s *Store) ChatConversationsForReindex(ctx context.Context, afterID string, limit int) ([]*ChatConversation, error) {
	rows, err := s.query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE deleted_at = '' AND id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ChatConversation{}
	for rows.Next() {
		c, err := scanChatConversation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneChatSearchIndex removes index rows that belong to deleted (or
// missing) conversations and returns how many it removed.
func (s *Store) PruneChatSearchIndex(ctx context.Context) (int64, error) {
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	res, err := runner.ExecContext(ctx, s.rebind(`DELETE FROM chat_search_term WHERE conversation_id NOT IN
		(SELECT id FROM chat_conversation WHERE deleted_at = '')`))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
