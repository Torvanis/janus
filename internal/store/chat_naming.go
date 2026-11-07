package store

import (
	"context"
	"time"
)

// Chat naming. A chat is named once, in the background, after a few
// replies or once it has gone idle; title_named_at records that it was
// (or that a person named it, or that naming was declined), so no chat
// is named twice and the idle sweep never retries one.
var chatNamingMigration = migration{name: "0074_chat_naming", stmt: []string{
	`ALTER TABLE chat_conversation ADD COLUMN title_named_at TEXT NOT NULL DEFAULT ''`,
	// Chats from before this were named after their first reply already:
	// never send them for naming again.
	`UPDATE chat_conversation SET title_named_at = updated_at`,
}, down: []string{
	`ALTER TABLE chat_conversation DROP COLUMN title_named_at`,
}}

// Chat naming modes (ChatAdminSettings.Naming).
const (
	// ChatNamingSameModel names a chat with the model that wrote its last
	// reply: the content goes nowhere it has not already been.
	ChatNamingSameModel = "same_model"
	// ChatNamingModel names every chat with ChatAdminSettings.NamingModel.
	ChatNamingModel = "model"
	// ChatNamingOff keeps the first message as the title.
	ChatNamingOff = "off"
)

// ClaimChatNaming marks a conversation as named, and reports whether this
// caller was the one to do it (replicas and the idle sweep race for it).
func (s *Store) ClaimChatNaming(ctx context.Context, userID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE chat_conversation SET title_named_at = ?
		WHERE id = ? AND user_id = ? AND title_named_at = '' AND deleted_at = ''`), FormatTime(nowUTC()), id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkChatNamed records that a person named the conversation: automatic
// naming leaves it alone from now on.
func (s *Store) MarkChatNamed(ctx context.Context, userID, id string) error {
	return s.exec(ctx, `UPDATE chat_conversation SET title_named_at = ? WHERE id = ? AND user_id = ? AND title_named_at = ''`,
		FormatTime(nowUTC()), id, userID)
}

// ChatsToName lists saved conversations not yet named, idle since before
// idleBefore but active after notBefore (old chats are never swept up),
// that have at least one finished reply. Oldest first.
func (s *Store) ChatsToName(ctx context.Context, idleBefore, notBefore time.Time, limit int) ([]*ChatConversation, error) {
	rows, err := s.query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation c
		WHERE c.temporary = 0 AND c.deleted_at = '' AND c.title_named_at = ''
		  AND c.updated_at < ? AND c.updated_at > ?
		  AND EXISTS (SELECT 1 FROM chat_message m WHERE m.conversation_id = c.id AND m.role = ? AND m.status = ?)
		ORDER BY c.updated_at LIMIT ?`,
		FormatTime(idleBefore), FormatTime(notBefore), ChatRoleAssistant, ChatStatusComplete, limit)
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

// ChatHadSecurityFindings reports whether anything in the conversation met
// the Security Gateway: a reply it blocked, or any request (the replies,
// their tool rounds and sub-calls) where a policy matched, whether it
// observed, redacted, blocked or cut the stream, or Child Safety stepped
// in. "checked" (a policy ran and found nothing) is not a finding.
func (s *Store) ChatHadSecurityFindings(ctx context.Context, conversationID string) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM chat_message WHERE conversation_id = ? AND status = ?) +
		(SELECT COUNT(*) FROM usage_event u JOIN chat_message m ON m.id = u.chat_message_id
		  WHERE m.conversation_id = ? AND (u.secgw_violations > 0 OR u.secgw_action NOT IN ('', ?)))`,
		conversationID, ChatStatusBlocked, conversationID, SecgwActionChecked).Scan(&n)
	return n > 0, err
}
