package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Janus Chat saved prompts: a user's own library, inserted into the
// composer with "/" + name. Bodies may contain {{variables}} the composer
// asks for. Bodies are user text and stored as Cipher envelopes; the name
// (the slash command) is plaintext so it can be unique per user.

var chatPromptMigration = migration{name: "0060_chat_prompts", stmt: []string{
	`CREATE TABLE IF NOT EXISTS chat_prompt (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		body_enc TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_prompt_user_name ON chat_prompt(user_id, name)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_chat_prompt_user_name`,
	`DROP TABLE IF EXISTS chat_prompt`,
}}

// ChatPromptMax bounds a user's library.
const ChatPromptMax = 200

// ErrChatPromptNameTaken is returned when the user already has a prompt
// with that command name; ErrChatPromptLimit when the library is full.
var (
	ErrChatPromptNameTaken = errors.New("chat prompt name taken")
	ErrChatPromptLimit     = errors.New("chat prompt library full")
)

// ChatPrompt is one saved prompt.
type ChatPrompt struct {
	ID        string
	UserID    string
	Name      string
	Title     string
	BodyEnc   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const chatPromptColumns = `id, user_id, name, title, body_enc, created_at, updated_at`

func scanChatPrompt(scan func(...any) error) (*ChatPrompt, error) {
	var p ChatPrompt
	var created, updated string
	if err := scan(&p.ID, &p.UserID, &p.Name, &p.Title, &p.BodyEnc, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = ParseTime(created), ParseTime(updated)
	return &p, nil
}

// ListChatPrompts returns the user's prompts by name.
func (s *Store) ListChatPrompts(ctx context.Context, userID string) ([]*ChatPrompt, error) {
	rows, err := s.query(ctx, `SELECT `+chatPromptColumns+` FROM chat_prompt WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ChatPrompt{}
	for rows.Next() {
		p, err := scanChatPrompt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ChatPrompt loads one of the user's prompts.
func (s *Store) ChatPrompt(ctx context.Context, userID, id string) (*ChatPrompt, error) {
	p, err := scanChatPrompt(s.queryRow(ctx, `SELECT `+chatPromptColumns+` FROM chat_prompt WHERE id = ? AND user_id = ?`, id, userID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) chatPromptNameTaken(ctx context.Context, userID, name, exceptID string) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM chat_prompt WHERE user_id = ? AND name = ? AND id <> ?`, userID, name, exceptID).Scan(&n)
	return n > 0, err
}

// CreateChatPrompt saves a new prompt. Names are unique per user.
func (s *Store) CreateChatPrompt(ctx context.Context, p *ChatPrompt) error {
	p.Name = strings.ToLower(p.Name)
	if taken, err := s.chatPromptNameTaken(ctx, p.UserID, p.Name, ""); err != nil {
		return err
	} else if taken {
		return ErrChatPromptNameTaken
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM chat_prompt WHERE user_id = ?`, p.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= ChatPromptMax {
		return ErrChatPromptLimit
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	now := nowUTC()
	p.CreatedAt, p.UpdatedAt = now, now
	return s.exec(ctx, `INSERT INTO chat_prompt (`+chatPromptColumns+`) VALUES (?,?,?,?,?,?,?)`,
		p.ID, p.UserID, p.Name, p.Title, p.BodyEnc, FormatTime(now), FormatTime(now))
}

// UpdateChatPrompt saves name, title and body.
func (s *Store) UpdateChatPrompt(ctx context.Context, p *ChatPrompt) error {
	p.Name = strings.ToLower(p.Name)
	if taken, err := s.chatPromptNameTaken(ctx, p.UserID, p.Name, p.ID); err != nil {
		return err
	} else if taken {
		return ErrChatPromptNameTaken
	}
	p.UpdatedAt = nowUTC()
	return s.exec(ctx, `UPDATE chat_prompt SET name = ?, title = ?, body_enc = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		p.Name, p.Title, p.BodyEnc, FormatTime(p.UpdatedAt), p.ID, p.UserID)
}

// DeleteChatPrompt removes one of the user's prompts.
func (s *Store) DeleteChatPrompt(ctx context.Context, userID, id string) error {
	return s.exec(ctx, `DELETE FROM chat_prompt WHERE id = ? AND user_id = ?`, id, userID)
}
