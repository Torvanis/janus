package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Janus Chat: per-conversation settings (pin, instructions, thinking
// effort, temporary) and the user's own chat preferences (instructions
// for every chat, default model).
//
// Instructions are user text and are stored as Cipher envelopes like
// message bodies (the store never sees plaintext). A temporary
// conversation is never listed or indexed and is purged by the reaper
// once idle for ChatTemporaryTTL.

// ChatTemporaryTTL is how long an idle temporary conversation is kept.
const ChatTemporaryTTL = 24 * time.Hour

// Chat reasoning effort values the SPA offers; "" = model default.
var ChatReasoningEfforts = []string{"", "low", "medium", "high"}

var chatSettingsMigration = migration{name: "0059_chat_settings", stmt: []string{
	`ALTER TABLE chat_conversation ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_conversation ADD COLUMN temporary INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE chat_conversation ADD COLUMN instructions_enc TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_conversation ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS chat_user_setting (
		user_id TEXT PRIMARY KEY,
		instructions_enc TEXT NOT NULL DEFAULT '',
		default_model TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	)`,
}, down: []string{
	`DROP TABLE IF EXISTS chat_user_setting`,
	`ALTER TABLE chat_conversation DROP COLUMN reasoning_effort`,
	`ALTER TABLE chat_conversation DROP COLUMN instructions_enc`,
	`ALTER TABLE chat_conversation DROP COLUMN temporary`,
	`ALTER TABLE chat_conversation DROP COLUMN pinned`,
}}

// ChatUserSetting is a user's own chat preferences.
type ChatUserSetting struct {
	UserID          string
	InstructionsEnc string
	DefaultModel    string
	// The person's own model for each Janus-run function ("" = automatic).
	ImageModel string
	VideoModel string
	MusicModel string
	UpdatedAt  time.Time
}

// chatModelDefaultsMigration adds the person's own image / video / music
// model (the chat tray's "Models" section; "" = automatic).
var chatModelDefaultsMigration = migration{name: "0066_chat_user_models", stmt: []string{
	`ALTER TABLE chat_user_setting ADD COLUMN image_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_user_setting ADD COLUMN video_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_user_setting ADD COLUMN music_model TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE chat_user_setting DROP COLUMN music_model`,
	`ALTER TABLE chat_user_setting DROP COLUMN video_model`,
	`ALTER TABLE chat_user_setting DROP COLUMN image_model`,
}}

// ChatUserSettings returns the user's chat preferences (zero value when
// never saved).
func (s *Store) ChatUserSettings(ctx context.Context, userID string) (*ChatUserSetting, error) {
	st := &ChatUserSetting{UserID: userID}
	var updated string
	err := s.queryRow(ctx, `SELECT instructions_enc, default_model, image_model, video_model, music_model, updated_at
		FROM chat_user_setting WHERE user_id = ?`, userID).
		Scan(&st.InstructionsEnc, &st.DefaultModel, &st.ImageModel, &st.VideoModel, &st.MusicModel, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.UpdatedAt = ParseTime(updated)
	return st, nil
}

// SaveChatUserSettings upserts the user's chat preferences.
func (s *Store) SaveChatUserSettings(ctx context.Context, st *ChatUserSetting) error {
	st.UpdatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO chat_user_setting (user_id, instructions_enc, default_model, image_model, video_model, music_model, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (user_id) DO UPDATE SET instructions_enc = excluded.instructions_enc,
		default_model = excluded.default_model, image_model = excluded.image_model, video_model = excluded.video_model,
		music_model = excluded.music_model, updated_at = excluded.updated_at`,
		st.UserID, st.InstructionsEnc, st.DefaultModel, st.ImageModel, st.VideoModel, st.MusicModel, FormatTime(st.UpdatedAt))
}

// StaleTemporaryChats lists temporary conversations idle since before
// `before`, for the reaper to purge (with their files).
func (s *Store) StaleTemporaryChats(ctx context.Context, before time.Time, limit int) ([]*ChatConversation, error) {
	rows, err := s.query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE temporary = 1 AND deleted_at = '' AND updated_at < ? ORDER BY updated_at LIMIT ?`, FormatTime(before), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ChatConversation
	for rows.Next() {
		c, err := scanChatConversation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PurgeChatConversation removes a conversation and everything in it for
// good (temporary chats; the normal delete is a soft delete). Files must be
// removed from storage by the caller first.
func (s *Store) PurgeChatConversation(ctx context.Context, userID, id string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		for _, q := range []string{
			`DELETE FROM chat_search_term WHERE conversation_id = ? AND user_id = ?`,
			`DELETE FROM chat_attachment WHERE conversation_id = ? AND user_id = ?`,
			`DELETE FROM chat_run WHERE conversation_id = ? AND user_id = ?`,
			`DELETE FROM chat_share WHERE conversation_id = ? AND user_id = ?`,
		} {
			if err := ts.exec(ctx, q, id, userID); err != nil {
				return err
			}
		}
		if err := ts.exec(ctx, `DELETE FROM chat_message WHERE conversation_id IN
			(SELECT id FROM chat_conversation WHERE id = ? AND user_id = ?)`, id, userID); err != nil {
			return err
		}
		return ts.exec(ctx, `DELETE FROM chat_conversation WHERE id = ? AND user_id = ?`, id, userID)
	})
}

// ChatRequestNote is what the request log says about one chat reply
// beyond tokens and cost: how it was routed and what the Security Gateway
// did. Shown on the message; the request log stays the source of truth.
type ChatRequestNote struct {
	RequestedModel      string
	ServedModel         string
	RouteReason         string
	FallbackReason      string
	PoolReason          string
	ReasoningAdjustment string
	SecgwAction         string
	SecgwViolations     int
}

// ChatRequestNotes returns notes for the given chat request ids (internal
// classifier sub-calls excluded), keyed by request id.
func (s *Store) ChatRequestNotes(ctx context.Context, requestIDs []string) (map[string]ChatRequestNote, error) {
	out := map[string]ChatRequestNote{}
	ids := make([]string, 0, len(requestIDs))
	for _, id := range requestIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	for start := 0; start < len(ids); start += 200 {
		batch := ids[start:min(start+200, len(ids))]
		rows, err := s.query(ctx, `SELECT request_id, requested_model_name, model_name, route_reason, fallback_reason, pool_reason,
			reasoning_adjustment, secgw_action, secgw_violations FROM usage_event
			WHERE request_id IN (`+placeholders(len(batch))+`) AND `+chatOwnUsageClause, toArgs(batch)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n ChatRequestNote
			if err := rows.Scan(&id, &n.RequestedModel, &n.ServedModel, &n.RouteReason, &n.FallbackReason, &n.PoolReason,
				&n.ReasoningAdjustment, &n.SecgwAction, &n.SecgwViolations); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = n
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
