package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Janus Chat attachments.
//
// A row describes one uploaded file; the bytes live in file storage
// (internal/blobstore), which encrypts them, and are referenced by
// StorageRef (the original) and TextRef (extracted text, documents only).
// The filename is a Cipher envelope like message bodies. An attachment is
// uploaded unbound (conversation_id and message_id empty) and bound to the
// user message it was sent with; unbound uploads are purged after a day.

// Attachment kinds and statuses.
const (
	ChatAttachmentImage    = "image"
	ChatAttachmentDocument = "document"
	// Made for a reply by a Janus-run tool: a file the model handed over
	// (create_file, deep research reports), a video, a music track.
	ChatAttachmentFile  = "file"
	ChatAttachmentVideo = "video"
	ChatAttachmentAudio = "audio"

	ChatAttachmentReady  = "ready"
	ChatAttachmentFailed = "failed"
)

var chatAttachmentMigration = migration{name: "0057_chat_attachments", stmt: []string{
	`CREATE TABLE IF NOT EXISTS chat_attachment (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		conversation_id TEXT NOT NULL DEFAULT '',
		message_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		filename_enc TEXT NOT NULL DEFAULT '',
		mime TEXT NOT NULL DEFAULT '',
		size_bytes BIGINT NOT NULL DEFAULT 0,
		sha256 TEXT NOT NULL DEFAULT '',
		storage_ref TEXT NOT NULL DEFAULT '',
		text_ref TEXT NOT NULL DEFAULT '',
		text_chars BIGINT NOT NULL DEFAULT 0,
		est_tokens BIGINT NOT NULL DEFAULT 0,
		truncated INTEGER NOT NULL DEFAULT 0,
		width INTEGER NOT NULL DEFAULT 0,
		height INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'ready',
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_attachment_user ON chat_attachment(user_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_attachment_message ON chat_attachment(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_attachment_conversation ON chat_attachment(conversation_id)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_chat_attachment_conversation`,
	`DROP INDEX IF EXISTS idx_chat_attachment_message`,
	`DROP INDEX IF EXISTS idx_chat_attachment_user`,
	`DROP TABLE IF EXISTS chat_attachment`,
}}

// ChatAttachment is one uploaded chat file.
type ChatAttachment struct {
	ID             string
	UserID         string
	ConversationID string
	MessageID      string
	Kind           string
	FilenameEnc    string
	MIME           string
	SizeBytes      int64
	SHA256         string
	StorageRef     string
	TextRef        string
	TextChars      int64
	EstTokens      int64
	Truncated      bool
	Width          int
	Height         int
	Status         string
	Error          string
	CreatedAt      time.Time
}

const chatAttachmentColumns = `id, user_id, conversation_id, message_id, kind, filename_enc, mime, size_bytes, sha256,
	storage_ref, text_ref, text_chars, est_tokens, truncated, width, height, status, error, created_at`

func scanChatAttachment(scan func(...any) error) (*ChatAttachment, error) {
	var a ChatAttachment
	var truncated int
	var created string
	if err := scan(&a.ID, &a.UserID, &a.ConversationID, &a.MessageID, &a.Kind, &a.FilenameEnc, &a.MIME, &a.SizeBytes,
		&a.SHA256, &a.StorageRef, &a.TextRef, &a.TextChars, &a.EstTokens, &truncated, &a.Width, &a.Height, &a.Status,
		&a.Error, &created); err != nil {
		return nil, err
	}
	a.Truncated = truncated == 1
	a.CreatedAt = ParseTime(created)
	return &a, nil
}

func (s *Store) listChatAttachments(ctx context.Context, query string, args ...any) ([]*ChatAttachment, error) {
	rows, err := s.query(ctx, `SELECT `+chatAttachmentColumns+` FROM chat_attachment `+query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ChatAttachment{}
	for rows.Next() {
		a, err := scanChatAttachment(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertChatAttachment records an uploaded file.
func (s *Store) InsertChatAttachment(ctx context.Context, a *ChatAttachment) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = nowUTC()
	}
	if a.Status == "" {
		a.Status = ChatAttachmentReady
	}
	truncated := 0
	if a.Truncated {
		truncated = 1
	}
	return s.exec(ctx, `INSERT INTO chat_attachment (`+chatAttachmentColumns+`) VALUES (`+placeholders(19)+`)`,
		a.ID, a.UserID, a.ConversationID, a.MessageID, a.Kind, a.FilenameEnc, a.MIME, a.SizeBytes, a.SHA256,
		a.StorageRef, a.TextRef, a.TextChars, a.EstTokens, truncated, a.Width, a.Height, a.Status, a.Error,
		FormatTime(a.CreatedAt))
}

// SetChatAttachmentText records the text Janus made of a sound or video
// file (a transcript or a description) so later turns reuse it.
func (s *Store) SetChatAttachmentText(ctx context.Context, userID, id, ref string, chars, est int64) error {
	return s.exec(ctx, `UPDATE chat_attachment SET text_ref = ?, text_chars = ?, est_tokens = ? WHERE id = ? AND user_id = ?`,
		ref, chars, est, id, userID)
}

// ChatAttachment loads one attachment owned by userID.
func (s *Store) ChatAttachment(ctx context.Context, userID, id string) (*ChatAttachment, error) {
	a, err := scanChatAttachment(s.queryRow(ctx, `SELECT `+chatAttachmentColumns+` FROM chat_attachment
		WHERE id = ? AND user_id = ?`, id, userID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ChatAttachmentsByIDs loads the user's attachments with the given ids (any
// order; ids the user does not own are simply absent).
func (s *Store) ChatAttachmentsByIDs(ctx context.Context, userID string, ids []string) ([]*ChatAttachment, error) {
	if len(ids) == 0 {
		return []*ChatAttachment{}, nil
	}
	args := []any{userID}
	for _, id := range ids {
		args = append(args, id)
	}
	return s.listChatAttachments(ctx, `WHERE user_id = ? AND id IN (`+placeholders(len(ids))+`) ORDER BY created_at, id`, args...)
}

// ChatAttachmentsForMessages returns the attachments bound to any of the
// given messages, oldest first.
func (s *Store) ChatAttachmentsForMessages(ctx context.Context, messageIDs []string) ([]*ChatAttachment, error) {
	out := []*ChatAttachment{}
	const batch = 500
	for start := 0; start < len(messageIDs); start += batch {
		end := min(start+batch, len(messageIDs))
		args := make([]any, 0, end-start)
		for _, id := range messageIDs[start:end] {
			args = append(args, id)
		}
		part, err := s.listChatAttachments(ctx, `WHERE message_id IN (`+placeholders(end-start)+`) ORDER BY created_at, id`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

// ChatAttachmentsForConversation returns every attachment bound to a
// conversation of userID.
func (s *Store) ChatAttachmentsForConversation(ctx context.Context, userID, conversationID string) ([]*ChatAttachment, error) {
	return s.listChatAttachments(ctx, `WHERE user_id = ? AND conversation_id = ? ORDER BY created_at, id`, userID, conversationID)
}

// BindChatAttachments attaches the given uploads to a message. Only the
// user's attachments that are unbound, or already in this conversation,
// are touched; it returns how many rows it bound.
func (s *Store) BindChatAttachments(ctx context.Context, userID, conversationID, messageID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{conversationID, messageID, userID, conversationID}
	for _, id := range ids {
		args = append(args, id)
	}
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	res, err := runner.ExecContext(ctx, s.rebind(`UPDATE chat_attachment SET conversation_id = ?, message_id = ?
		WHERE user_id = ? AND (conversation_id = '' OR conversation_id = ?) AND id IN (`+placeholders(len(ids))+`)`), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteChatAttachment removes one unbound attachment row of userID and
// reports whether a row went; a row bound to a message is left alone.
func (s *Store) DeleteChatAttachment(ctx context.Context, userID, id string) (bool, error) {
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	res, err := runner.ExecContext(ctx, s.rebind(`DELETE FROM chat_attachment WHERE id = ? AND user_id = ? AND message_id = ''`), id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteChatAttachmentRows removes attachment rows by id (blobs are the
// caller's job, before this).
// DeleteBoundChatAttachment removes one of userID's attachments whatever it
// is bound to (a reply replacing a file it made).
func (s *Store) DeleteBoundChatAttachment(ctx context.Context, userID, id string) error {
	return s.execOne(ctx, `DELETE FROM chat_attachment WHERE id = ? AND user_id = ?`, id, userID)
}

func (s *Store) DeleteChatAttachmentRows(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return s.exec(ctx, `DELETE FROM chat_attachment WHERE id IN (`+placeholders(len(ids))+`)`, args...)
}

// StaleUnboundChatAttachments lists uploads never sent with a message and
// older than before, oldest first, at most limit.
func (s *Store) StaleUnboundChatAttachments(ctx context.Context, before time.Time, limit int) ([]*ChatAttachment, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.listChatAttachments(ctx, `WHERE message_id = '' AND created_at < ? ORDER BY created_at LIMIT ?`, FormatTime(before), limit)
}
