package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Janus Chat persistence (Phase 0 spike).
//
// A conversation belongs to one user. Messages form a tree through
// parent_id so edits and regenerations become siblings; the conversation
// remembers which leaf is current. Message bodies are stored already
// encrypted by the caller (content_enc is an opaque Cipher envelope): the
// store never sees plaintext.
//
// A run is one assistant generation in flight. The pod that accepted the
// send owns it, flushes the partial body into the message row as it
// streams, and bumps heartbeat_at; any replica can therefore serve a
// reconnecting browser from the database, and a run whose heartbeat stops
// (the owning pod died) is reaped as interrupted.

// TokenKindChat marks the hidden credential that attributes a user's chat
// traffic. It is never shown on the Tokens page and its digest is random, so
// it can never be presented as a bearer token.
const TokenKindChat = "chat"

// Chat message roles and statuses.
const (
	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"

	ChatStatusComplete    = "complete"
	ChatStatusStreaming   = "streaming"
	ChatStatusStopped     = "stopped"
	ChatStatusError       = "error"
	ChatStatusInterrupted = "interrupted"
	// ChatStatusBlocked is a reply a Security Gateway policy refused or
	// stopped: a clear terminal state, shown calmly, never as an error.
	ChatStatusBlocked = "blocked"
)

var chatMigration = migration{name: "0054_chat", stmt: []string{
	`ALTER TABLE api_token ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_api_token_user_kind ON api_token(user_id, kind, team_id)`,
	`CREATE TABLE IF NOT EXISTS chat_conversation (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		team_id TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		current_leaf_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		deleted_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_conversation_user ON chat_conversation(user_id, deleted_at, updated_at)`,
	`CREATE TABLE IF NOT EXISTS chat_message (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		parent_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL,
		content_enc TEXT NOT NULL DEFAULT '',
		reasoning_enc TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		served_model TEXT NOT NULL DEFAULT '',
		team_id TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'complete',
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		tokens_in BIGINT NOT NULL DEFAULT 0,
		tokens_out BIGINT NOT NULL DEFAULT 0,
		tokens_cached BIGINT NOT NULL DEFAULT 0,
		cost_nano BIGINT NOT NULL DEFAULT 0,
		tokens_out_per_second DOUBLE PRECISION NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		ttfb_ms INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_message_conversation ON chat_message(conversation_id, created_at)`,
	`CREATE TABLE IF NOT EXISTS chat_run (
		id TEXT PRIMARY KEY,
		message_id TEXT NOT NULL,
		conversation_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		owner TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		cancel_requested INTEGER NOT NULL DEFAULT 0,
		seq INTEGER NOT NULL DEFAULT 0,
		heartbeat_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_run_status ON chat_run(status, heartbeat_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_chat_run_status`,
	`DROP TABLE IF EXISTS chat_run`,
	`DROP INDEX IF EXISTS idx_chat_message_conversation`,
	`DROP TABLE IF EXISTS chat_message`,
	`DROP INDEX IF EXISTS idx_chat_conversation_user`,
	`DROP TABLE IF EXISTS chat_conversation`,
	`DROP INDEX IF EXISTS idx_api_token_user_kind`,
	`ALTER TABLE api_token DROP COLUMN kind`,
}}

// ChatConversation is one chat thread.
type ChatConversation struct {
	ID            string    `json:"id"`
	UserID        string    `json:"-"`
	TeamID        string    `json:"team_id"`
	Title         string    `json:"title"`
	ModelName     string    `json:"model"`
	CurrentLeafID string    `json:"current_leaf_id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Pinned conversations sort first in the list.
	Pinned bool `json:"pinned"`
	// Temporary conversations are never listed or searchable and are
	// purged once idle (ChatTemporaryTTL).
	Temporary bool `json:"temporary"`
	// InstructionsEnc is this conversation's own instructions (Cipher
	// envelope); the API returns the plaintext as "instructions".
	InstructionsEnc string `json:"-"`
	// ReasoningEffort is sent as reasoning_effort ("" = model default).
	ReasoningEffort string `json:"reasoning_effort"`
	// ToolsOff lists the Chat tools (ChatTools) turned off for this
	// conversation; tools are on by default.
	ToolsOff []string `json:"tools_off"`
	// ImageModel is the model this chat makes images with ("" = the
	// default: the plan's own image model, else the admin's choice).
	ImageModel string `json:"image_model"`
}

// ChatMessage is one message. ContentEnc/ReasoningEnc are Cipher envelopes.
type ChatMessage struct {
	ID             string
	ConversationID string
	ParentID       string
	Role           string
	ContentEnc     string
	ReasoningEnc   string
	ModelName      string
	ServedModel    string
	TeamID         string
	RequestID      string
	Status         string
	ErrorCode      string
	ErrorMessage   string
	// ContextDropped is how many of the oldest history messages were left
	// out of the request because the model's context window was too small.
	ContextDropped int
	// ExtrasEnc is the encrypted JSON of what the reply used besides text:
	// the provider's search activity and the sources it cited.
	ExtrasEnc          string
	TokensIn           int64
	TokensOut          int64
	TokensCached       int64
	CostNano           int64
	TokensOutPerSecond float64
	LatencyMs          int
	TTFBMs             int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ChatRun is one generation in flight (or finished).
type ChatRun struct {
	ID              string
	MessageID       string
	ConversationID  string
	UserID          string
	Owner           string
	Status          string
	CancelRequested bool
	Seq             int64
	HeartbeatAt     time.Time
	CreatedAt       time.Time
}

// ChatToken returns the user's hidden chat credential for one billing
// context (teamID "" = personal), creating it on first use. A revoked one
// (the user was disabled and re-enabled) is replaced, never revived.
func (s *Store) ChatToken(ctx context.Context, userID, teamID string) (*Token, error) {
	t, err := scanToken(s.queryRow(ctx, `SELECT `+tokenColumns+` FROM api_token
		WHERE user_id = ? AND kind = ? AND team_id = ? AND revoked_at = '' ORDER BY created_at DESC LIMIT 1`,
		userID, TokenKindChat, teamID).Scan)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load chat token: %w", err)
	}
	if teamID != "" {
		if err := s.ValidateTeamMember(ctx, teamID, userID); err != nil {
			return nil, err
		}
	}
	_, digest, prefix, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	t = &Token{ID: NewID(), UserID: userID, Prefix: prefix, Description: "Janus Chat", TeamID: teamID, Kind: TokenKindChat, CreatedAt: nowUTC()}
	if err := s.exec(ctx,
		`INSERT INTO api_token (id, user_id, token_digest, prefix, description, created_at, last_used_at, revoked_at, team_id, kind)
		 VALUES (?,?,?,?,?,?,'','',?,?)`,
		t.ID, t.UserID, digest, t.Prefix, t.Description, FormatTime(t.CreatedAt), t.TeamID, TokenKindChat); err != nil {
		return nil, err
	}
	return t, nil
}

const chatConversationColumns = `id, user_id, team_id, title, model_name, current_leaf_id, created_at, updated_at,
	pinned, temporary, instructions_enc, reasoning_effort, tools_off, image_model`

func scanChatConversation(scan func(...any) error) (*ChatConversation, error) {
	var c ChatConversation
	var created, updated string
	var pinned, temporary int
	var toolsOff string
	if err := scan(&c.ID, &c.UserID, &c.TeamID, &c.Title, &c.ModelName, &c.CurrentLeafID, &created, &updated,
		&pinned, &temporary, &c.InstructionsEnc, &c.ReasoningEffort, &toolsOff, &c.ImageModel); err != nil {
		return nil, err
	}
	c.ToolsOff = splitToolList(toolsOff)
	c.CreatedAt, c.UpdatedAt = ParseTime(created), ParseTime(updated)
	c.Pinned, c.Temporary = pinned != 0, temporary != 0
	return &c, nil
}

// CreateChatConversation starts a thread.
func (s *Store) CreateChatConversation(ctx context.Context, c *ChatConversation) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	now := nowUTC()
	c.CreatedAt, c.UpdatedAt = now, now
	return s.exec(ctx, `INSERT INTO chat_conversation (id, user_id, team_id, title, model_name, current_leaf_id, created_at, updated_at, deleted_at,
		pinned, temporary, instructions_enc, reasoning_effort, tools_off, image_model)
		VALUES (?,?,?,?,?,?,?,?,'',?,?,?,?,?,?)`, c.ID, c.UserID, c.TeamID, c.Title, c.ModelName, c.CurrentLeafID, FormatTime(now), FormatTime(now),
		boolInt(c.Pinned), boolInt(c.Temporary), c.InstructionsEnc, c.ReasoningEffort, strings.Join(c.ToolsOff, ","), c.ImageModel)
}

// ChatConversation loads one live thread owned by userID.
func (s *Store) ChatConversation(ctx context.Context, userID, id string) (*ChatConversation, error) {
	c, err := scanChatConversation(s.queryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE id = ? AND user_id = ? AND deleted_at = ''`, id, userID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListChatConversations returns the user's live threads, most recent first.
func (s *Store) ListChatConversations(ctx context.Context, userID string, limit int) ([]*ChatConversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE user_id = ? AND deleted_at = '' AND temporary = 0 ORDER BY pinned DESC, updated_at DESC LIMIT ?`, userID, limit)
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

// UpdateChatConversation saves title, model, billing team and current leaf.
func (s *Store) UpdateChatConversation(ctx context.Context, c *ChatConversation) error {
	c.UpdatedAt = nowUTC()
	return s.exec(ctx, `UPDATE chat_conversation SET title = ?, model_name = ?, team_id = ?, current_leaf_id = ?, updated_at = ?,
		pinned = ?, instructions_enc = ?, reasoning_effort = ?, tools_off = ?, image_model = ?
		WHERE id = ? AND user_id = ?`, c.Title, c.ModelName, c.TeamID, c.CurrentLeafID, FormatTime(c.UpdatedAt),
		boolInt(c.Pinned), c.InstructionsEnc, c.ReasoningEffort, strings.Join(c.ToolsOff, ","), c.ImageModel, c.ID, c.UserID)
}

// DeleteChatConversation soft-deletes a thread and drops its search terms,
// so a deleted conversation can no longer be found.
func (s *Store) DeleteChatConversation(ctx context.Context, userID, id string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		if err := ts.exec(ctx, `UPDATE chat_conversation SET deleted_at = ? WHERE id = ? AND user_id = ? AND deleted_at = ''`,
			FormatTime(nowUTC()), id, userID); err != nil {
			return err
		}
		return ts.exec(ctx, `DELETE FROM chat_search_term WHERE conversation_id = ? AND user_id = ?`, id, userID)
	})
}

const chatMessageColumns = `id, conversation_id, parent_id, role, content_enc, reasoning_enc, model_name, served_model, team_id,
	request_id, status, error_code, error_message, tokens_in, tokens_out, tokens_cached, cost_nano, tokens_out_per_second,
	latency_ms, ttfb_ms, created_at, updated_at, context_dropped, extras_enc`

func scanChatMessage(scan func(...any) error) (*ChatMessage, error) {
	var m ChatMessage
	var created, updated string
	if err := scan(&m.ID, &m.ConversationID, &m.ParentID, &m.Role, &m.ContentEnc, &m.ReasoningEnc, &m.ModelName, &m.ServedModel,
		&m.TeamID, &m.RequestID, &m.Status, &m.ErrorCode, &m.ErrorMessage, &m.TokensIn, &m.TokensOut, &m.TokensCached,
		&m.CostNano, &m.TokensOutPerSecond, &m.LatencyMs, &m.TTFBMs, &created, &updated, &m.ContextDropped, &m.ExtrasEnc); err != nil {
		return nil, err
	}
	m.CreatedAt, m.UpdatedAt = ParseTime(created), ParseTime(updated)
	return &m, nil
}

// InsertChatMessage appends a message.
func (s *Store) InsertChatMessage(ctx context.Context, m *ChatMessage) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = nowUTC()
	}
	m.UpdatedAt = m.CreatedAt
	return s.exec(ctx, `INSERT INTO chat_message (`+chatMessageColumns+`) VALUES (`+placeholders(24)+`)`,
		m.ID, m.ConversationID, m.ParentID, m.Role, m.ContentEnc, m.ReasoningEnc, m.ModelName, m.ServedModel, m.TeamID,
		m.RequestID, m.Status, m.ErrorCode, m.ErrorMessage, m.TokensIn, m.TokensOut, m.TokensCached, m.CostNano,
		m.TokensOutPerSecond, m.LatencyMs, m.TTFBMs, FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt), m.ContextDropped, m.ExtrasEnc)
}

// UpdateChatMessage rewrites a message's mutable fields (body, status,
// metering) — the streaming flush and the final write both use it.
func (s *Store) UpdateChatMessage(ctx context.Context, m *ChatMessage) error {
	m.UpdatedAt = nowUTC()
	return s.exec(ctx, `UPDATE chat_message SET content_enc = ?, reasoning_enc = ?, served_model = ?, request_id = ?, status = ?,
		error_code = ?, error_message = ?, tokens_in = ?, tokens_out = ?, tokens_cached = ?, cost_nano = ?,
		tokens_out_per_second = ?, latency_ms = ?, ttfb_ms = ?, context_dropped = ?, extras_enc = ?, updated_at = ? WHERE id = ?`,
		m.ContentEnc, m.ReasoningEnc, m.ServedModel, m.RequestID, m.Status, m.ErrorCode, m.ErrorMessage,
		m.TokensIn, m.TokensOut, m.TokensCached, m.CostNano, m.TokensOutPerSecond, m.LatencyMs, m.TTFBMs,
		m.ContextDropped, m.ExtrasEnc, FormatTime(m.UpdatedAt), m.ID)
}

// ChatMessage loads one message.
func (s *Store) ChatMessage(ctx context.Context, id string) (*ChatMessage, error) {
	m, err := scanChatMessage(s.queryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_message WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ListChatMessages returns every message of a thread (all branches), oldest first.
func (s *Store) ListChatMessages(ctx context.Context, conversationID string) ([]*ChatMessage, error) {
	rows, err := s.query(ctx, `SELECT `+chatMessageColumns+` FROM chat_message WHERE conversation_id = ? ORDER BY created_at, id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ChatMessage{}
	for rows.Next() {
		m, err := scanChatMessage(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const chatRunColumns = `id, message_id, conversation_id, user_id, owner, status, cancel_requested, seq, heartbeat_at, created_at`

func scanChatRun(scan func(...any) error) (*ChatRun, error) {
	var r ChatRun
	var cancel int
	var hb, created string
	if err := scan(&r.ID, &r.MessageID, &r.ConversationID, &r.UserID, &r.Owner, &r.Status, &cancel, &r.Seq, &hb, &created); err != nil {
		return nil, err
	}
	r.CancelRequested = cancel == 1
	r.HeartbeatAt, r.CreatedAt = ParseTime(hb), ParseTime(created)
	return &r, nil
}

// InsertChatRun records a generation starting on this pod.
func (s *Store) InsertChatRun(ctx context.Context, r *ChatRun) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	now := nowUTC()
	r.HeartbeatAt, r.CreatedAt = now, now
	return s.exec(ctx, `INSERT INTO chat_run (`+chatRunColumns+`) VALUES (?,?,?,?,?,?,0,0,?,?)`,
		r.ID, r.MessageID, r.ConversationID, r.UserID, r.Owner, r.Status, FormatTime(now), FormatTime(now))
}

// ChatRun loads one run owned by userID.
func (s *Store) ChatRun(ctx context.Context, userID, id string) (*ChatRun, error) {
	r, err := scanChatRun(s.queryRow(ctx, `SELECT `+chatRunColumns+` FROM chat_run WHERE id = ? AND user_id = ?`, id, userID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// StreamingChatRuns maps message id -> run id for a conversation's runs still
// generating, so a reloaded page can reattach to them.
func (s *Store) StreamingChatRuns(ctx context.Context, conversationID string) (map[string]string, error) {
	rows, err := s.query(ctx, `SELECT message_id, id FROM chat_run WHERE conversation_id = ? AND status = ?`,
		conversationID, ChatStatusStreaming)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var msg, run string
		if err := rows.Scan(&msg, &run); err != nil {
			return nil, err
		}
		out[msg] = run
	}
	return out, rows.Err()
}

// HeartbeatChatRun bumps the liveness clock and sequence, and reports
// whether someone asked for the run to stop.
func (s *Store) HeartbeatChatRun(ctx context.Context, id string, seq int64) (cancelRequested bool, err error) {
	if err := s.exec(ctx, `UPDATE chat_run SET heartbeat_at = ?, seq = ? WHERE id = ?`, FormatTime(nowUTC()), seq, id); err != nil {
		return false, err
	}
	var cancel int
	if err := s.queryRow(ctx, `SELECT cancel_requested FROM chat_run WHERE id = ?`, id).Scan(&cancel); err != nil {
		return false, err
	}
	return cancel == 1, nil
}

// FinishChatRun records a run's terminal status.
func (s *Store) FinishChatRun(ctx context.Context, id, status string) error {
	return s.exec(ctx, `UPDATE chat_run SET status = ?, heartbeat_at = ? WHERE id = ?`, status, FormatTime(nowUTC()), id)
}

// RequestChatRunCancel asks the owning pod to stop a run.
func (s *Store) RequestChatRunCancel(ctx context.Context, userID, id string) error {
	return s.exec(ctx, `UPDATE chat_run SET cancel_requested = 1 WHERE id = ? AND user_id = ?`, id, userID)
}

// ReapChatRuns marks runs whose owner stopped heart-beating as interrupted,
// along with their messages, and returns how many it reaped. Safe on every
// replica: the UPDATE is conditional on the stale heartbeat.
func (s *Store) ReapChatRuns(ctx context.Context, staleBefore time.Time) (int, error) {
	rows, err := s.query(ctx, `SELECT id, message_id FROM chat_run WHERE status = ? AND heartbeat_at < ?`,
		ChatStatusStreaming, FormatTime(staleBefore))
	if err != nil {
		return 0, err
	}
	type pair struct{ run, msg string }
	var stale []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.run, &p.msg); err != nil {
			_ = rows.Close()
			return 0, err
		}
		stale = append(stale, p)
	}
	_ = rows.Close()
	n := 0
	for _, p := range stale {
		if err := s.InTx(ctx, func(tx *sql.Tx) error {
			ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
			if err := ts.exec(ctx, `UPDATE chat_run SET status = ? WHERE id = ? AND status = ? AND heartbeat_at < ?`,
				ChatStatusInterrupted, p.run, ChatStatusStreaming, FormatTime(staleBefore)); err != nil {
				return err
			}
			return ts.exec(ctx, `UPDATE chat_message SET status = ?, error_code = 'chat.interrupted',
				error_message = 'The server handling this reply stopped before it finished.', updated_at = ?
				WHERE id = ? AND status = ?`, ChatStatusInterrupted, FormatTime(nowUTC()), p.msg, ChatStatusStreaming)
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// UsageEventByRequestID returns the user-facing metering row for a request
// (internal classifier sub-calls share the request_id and are skipped).
func (s *Store) UsageEventByRequestID(ctx context.Context, requestID string) (*UsageEvent, error) {
	e, err := scanUsage(s.queryRow(ctx, `SELECT `+usageColumns+` FROM usage_event
		WHERE request_id = ? AND `+chatOwnUsageClause+` ORDER BY created_at DESC LIMIT 1`, requestID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}
