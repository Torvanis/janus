package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Janus Chat share links: a read-only snapshot of a conversation's
// current branch, taken when the link is made (later messages are not
// shared). The id is the unguessable link token; anyone signed in to this
// Janus can open it, the owner can revoke it. The snapshot is a Cipher
// envelope of JSON, like every other chat body.

var chatShareMigration = migration{name: "0061_chat_shares", stmt: []string{
	`CREATE TABLE IF NOT EXISTS chat_share (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		conversation_id TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		snapshot_enc TEXT NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0,
		views INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		revoked_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_share_conversation ON chat_share(user_id, conversation_id)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_chat_share_conversation`,
	`DROP TABLE IF EXISTS chat_share`,
}}

// ChatShare is one share link.
type ChatShare struct {
	ID             string
	UserID         string
	ConversationID string
	Title          string
	SnapshotEnc    string
	MessageCount   int
	Views          int
	CreatedAt      time.Time
	RevokedAt      time.Time
}

const chatShareColumns = `id, user_id, conversation_id, title, snapshot_enc, message_count, views, created_at, revoked_at`

func scanChatShare(scan func(...any) error) (*ChatShare, error) {
	var sh ChatShare
	var created, revoked string
	if err := scan(&sh.ID, &sh.UserID, &sh.ConversationID, &sh.Title, &sh.SnapshotEnc, &sh.MessageCount, &sh.Views, &created, &revoked); err != nil {
		return nil, err
	}
	sh.CreatedAt = ParseTime(created)
	if revoked != "" {
		sh.RevokedAt = ParseTime(revoked)
	}
	return &sh, nil
}

// CreateChatShare stores a new snapshot link. The caller sets ID to an
// unguessable token.
func (s *Store) CreateChatShare(ctx context.Context, sh *ChatShare) error {
	sh.CreatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO chat_share (`+chatShareColumns+`) VALUES (?,?,?,?,?,?,0,?,'')`,
		sh.ID, sh.UserID, sh.ConversationID, sh.Title, sh.SnapshotEnc, sh.MessageCount, FormatTime(sh.CreatedAt))
}

// ChatShareByID loads a live (not revoked) share for any viewer.
func (s *Store) ChatShareByID(ctx context.Context, id string) (*ChatShare, error) {
	sh, err := scanChatShare(s.queryRow(ctx, `SELECT `+chatShareColumns+` FROM chat_share WHERE id = ? AND revoked_at = ''`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sh, err
}

// CountChatShareView adds one to a share's view counter.
func (s *Store) CountChatShareView(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE chat_share SET views = views + 1 WHERE id = ?`, id)
}

// ListChatShares returns the owner's live links for a conversation, newest first.
func (s *Store) ListChatShares(ctx context.Context, userID, conversationID string) ([]*ChatShare, error) {
	rows, err := s.query(ctx, `SELECT `+chatShareColumns+` FROM chat_share
		WHERE user_id = ? AND conversation_id = ? AND revoked_at = '' ORDER BY created_at DESC`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ChatShare{}
	for rows.Next() {
		sh, err := scanChatShare(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// RevokeChatShare ends one of the owner's links. The snapshot is dropped
// at once (nothing left to read).
func (s *Store) RevokeChatShare(ctx context.Context, userID, id string) (bool, error) {
	runner := s.runner(ctx)
	res, err := runner.ExecContext(ctx, s.rebind(`UPDATE chat_share SET revoked_at = ?, snapshot_enc = '' WHERE id = ? AND user_id = ? AND revoked_at = ''`),
		FormatTime(nowUTC()), id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeChatSharesForConversation ends every link to a conversation
// (when it is deleted).
func (s *Store) RevokeChatSharesForConversation(ctx context.Context, userID, conversationID string) error {
	return s.exec(ctx, `UPDATE chat_share SET revoked_at = ?, snapshot_enc = '' WHERE user_id = ? AND conversation_id = ? AND revoked_at = ''`,
		FormatTime(nowUTC()), userID, conversationID)
}
