package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Janus Chat memory: durable facts a person asked the assistant to keep
// (or that the model saved with the remember tool). Each memory is user
// text, stored as a Cipher envelope like messages; the store never sees
// plaintext. Every non-temporary chat reads them into its system message.
// A person can turn memory off, and list, edit and delete memories.
//
// The same migration records which assistant replies were made side by
// side (Compare): replies sharing a compare_group answered one message.

var chatRound10Migration = migration{name: "0064_chat_memory_compare", stmt: []string{
	`CREATE TABLE IF NOT EXISTS chat_memory (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		content_enc TEXT NOT NULL,
		source_conversation_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_memory_user ON chat_memory (user_id, created_at)`,
	`ALTER TABLE chat_user_setting ADD COLUMN memory_off INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_message ADD COLUMN compare_group TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE chat_message DROP COLUMN compare_group`,
	`ALTER TABLE chat_user_setting DROP COLUMN memory_off`,
	`DROP TABLE IF EXISTS chat_memory`,
}}

// usageRequestIndexMigration indexes usage_event by request_id. Chat reads
// each reply's metering row by request id (UsageEventByRequestID); without
// the index that is a full scan (about 2 s on 1.3M rows), slower than the
// lookup's own deadline, so replies showed 0 tokens / $0.
var usageRequestIndexMigration = migration{name: "0065_usage_request_index", stmt: []string{
	`CREATE INDEX IF NOT EXISTS idx_usage_request ON usage_event (request_id)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_usage_request`,
}}

// ChatMaxMemories bounds one person's memories; ChatMaxMemoryChars one memory.
const (
	ChatMaxMemories    = 200
	ChatMaxMemoryChars = 500
)

// ErrChatMemoryFull is returned when a person already has ChatMaxMemories.
var ErrChatMemoryFull = errors.New("chat memory full")

// ChatMemory is one remembered fact.
type ChatMemory struct {
	ID                   string
	UserID               string
	ContentEnc           string
	SourceConversationID string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ChatMemories lists a person's memories, oldest first.
func (s *Store) ChatMemories(ctx context.Context, userID string) ([]*ChatMemory, error) {
	rows, err := s.query(ctx, `SELECT id, user_id, content_enc, source_conversation_id, created_at, updated_at
		FROM chat_memory WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ChatMemory
	for rows.Next() {
		var m ChatMemory
		var created, updated string
		if err := rows.Scan(&m.ID, &m.UserID, &m.ContentEnc, &m.SourceConversationID, &created, &updated); err != nil {
			return nil, err
		}
		m.CreatedAt, m.UpdatedAt = ParseTime(created), ParseTime(updated)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// InsertChatMemory saves a new memory (ErrChatMemoryFull past the limit).
func (s *Store) InsertChatMemory(ctx context.Context, m *ChatMemory) error {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM chat_memory WHERE user_id = ?`, m.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= ChatMaxMemories {
		return ErrChatMemoryFull
	}
	if m.ID == "" {
		m.ID = NewID()
	}
	m.CreatedAt = nowUTC()
	m.UpdatedAt = m.CreatedAt
	return s.exec(ctx, `INSERT INTO chat_memory (id, user_id, content_enc, source_conversation_id, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		m.ID, m.UserID, m.ContentEnc, m.SourceConversationID, FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt))
}

// UpdateChatMemory rewrites one of the person's memories.
func (s *Store) UpdateChatMemory(ctx context.Context, userID, id, contentEnc string) error {
	return s.execOne(ctx, `UPDATE chat_memory SET content_enc = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		contentEnc, FormatTime(nowUTC()), id, userID)
}

// DeleteChatMemory removes one of the person's memories.
func (s *Store) DeleteChatMemory(ctx context.Context, userID, id string) error {
	return s.execOne(ctx, `DELETE FROM chat_memory WHERE id = ? AND user_id = ?`, id, userID)
}

// DeleteAllChatMemories removes every memory the person has.
func (s *Store) DeleteAllChatMemories(ctx context.Context, userID string) error {
	return s.exec(ctx, `DELETE FROM chat_memory WHERE user_id = ?`, userID)
}

// ChatMemoryOff reports whether the person turned memory off.
func (s *Store) ChatMemoryOff(ctx context.Context, userID string) (bool, error) {
	var off int
	err := s.queryRow(ctx, `SELECT memory_off FROM chat_user_setting WHERE user_id = ?`, userID).Scan(&off)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return off != 0, err
}

// SetChatMemoryOff records the person's memory switch.
func (s *Store) SetChatMemoryOff(ctx context.Context, userID string, off bool) error {
	v := 0
	if off {
		v = 1
	}
	return s.exec(ctx, `INSERT INTO chat_user_setting (user_id, instructions_enc, default_model, updated_at, memory_off) VALUES (?,'','',?,?)
		ON CONFLICT (user_id) DO UPDATE SET memory_off = excluded.memory_off, updated_at = excluded.updated_at`,
		userID, FormatTime(nowUTC()), v)
}

// execOne runs a statement that must touch exactly one row (ErrNotFound
// otherwise: the id is not the caller's).
func (s *Store) execOne(ctx context.Context, query string, args ...any) error {
	runner := s.runner(ctx)
	res, err := runner.ExecContext(ctx, s.rebind(query), args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetChatMessageCompareGroup marks replies made side by side.
func (s *Store) SetChatMessageCompareGroup(ctx context.Context, messageID, group string) error {
	return s.exec(ctx, `UPDATE chat_message SET compare_group = ? WHERE id = ?`, group, messageID)
}

// ChatCompareGroups maps message id → compare group for a conversation's
// side-by-side replies (only messages that have one).
func (s *Store) ChatCompareGroups(ctx context.Context, conversationID string) (map[string]string, error) {
	rows, err := s.query(ctx, `SELECT id, compare_group FROM chat_message WHERE conversation_id = ? AND compare_group <> ''`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, g string
		if err := rows.Scan(&id, &g); err != nil {
			return nil, err
		}
		out[id] = g
	}
	return out, rows.Err()
}
