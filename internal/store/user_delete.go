package store

import (
	"context"
	"database/sql"
	"time"
)

// Deleting a person (administrator delete or a directory DELETE) holds
// their content for UserDeleteHold, then purges it. If the same person is
// provisioned again within the hold (same directory userName or
// externalId), or an administrator restores the account, everything is
// back as it was. Conversations a person deletes themselves are purged
// ChatDeletePurgeAfter after the delete.
//
// deleted_at marks the start of the hold; content_purged_at records that
// the hold ran out and the content is gone (the account row itself stays,
// like today, because usage events and the audit log refer to it).
var userDeleteHoldMigration = migration{name: "0070_user_delete_hold", stmt: []string{
	`ALTER TABLE app_user ADD COLUMN deleted_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE app_user ADD COLUMN content_purged_at TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_app_user_deleted ON app_user(deleted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_notification_created ON notification(created_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_notification_created`,
	`DROP INDEX IF EXISTS idx_app_user_deleted`,
	`ALTER TABLE app_user DROP COLUMN content_purged_at`,
	`ALTER TABLE app_user DROP COLUMN deleted_at`,
}}

// UserDeleteHold is how long a deleted person's content is kept so a
// mistaken delete can be undone.
const UserDeleteHold = 30 * 24 * time.Hour

// ChatDeletePurgeAfter is how long a conversation the person deleted
// themselves is kept before it is removed for good.
const ChatDeletePurgeAfter = 30 * 24 * time.Hour

// NotificationRetention bounds the in-app notification list.
const NotificationRetention = 90 * 24 * time.Hour

// MarkUserDeleted disables the account and starts the content hold. A
// second delete keeps the original start.
func (s *Store) MarkUserDeleted(ctx context.Context, id string) error {
	if err := s.exec(ctx, `UPDATE app_user SET is_active = 0 WHERE id = ?`, id); err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE app_user SET deleted_at = ?, content_purged_at = '' WHERE id = ? AND deleted_at = ''`, FormatTime(nowUTC()), id)
}

// clearUserDeleted ends a hold (the person is back). Called whenever an
// account becomes active again.
func (s *Store) clearUserDeleted(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE app_user SET deleted_at = '', content_purged_at = '' WHERE id = ? AND deleted_at <> ''`, id)
}

// HeldUsersDue lists deleted, still-disabled accounts whose hold ended
// before `before` and whose content has not been purged yet.
func (s *Store) HeldUsersDue(ctx context.Context, before time.Time, limit int) ([]string, error) {
	return s.teamStringIDs(ctx, `SELECT id FROM app_user WHERE is_active = 0 AND deleted_at <> '' AND deleted_at < ?
		AND content_purged_at = '' ORDER BY deleted_at LIMIT ?`, FormatTime(before.Add(-UserDeleteHold)), limit)
}

// ChatAttachmentsForUser lists every attachment row a person owns (for
// removing their files from storage before PurgeUserContent).
func (s *Store) ChatAttachmentsForUser(ctx context.Context, userID string) ([]*ChatAttachment, error) {
	return s.listChatAttachments(ctx, `WHERE user_id = ?`, userID)
}

// PurgeUserContent removes everything a deleted person kept in Janus:
// conversations, messages, attachment rows (files are removed from
// storage by the caller first), search terms, memories, saved prompts,
// chat settings, share links, personal subscriptions, notifications and
// their own Child Safety assignment, then records the purge. Usage
// events, the audit log and violations follow their own retention.
// It refuses (no-op) for an account that is active again.
func (s *Store) PurgeUserContent(ctx context.Context, userID string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ts := &Store{db: s.db, tx: tx, dialect: s.dialect, log: s.log}
		var active int
		if err := ts.queryRow(ctx, `SELECT is_active FROM app_user WHERE id = ?`, userID).Scan(&active); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if active == 1 {
			return nil
		}
		if err := ts.exec(ctx, `DELETE FROM chat_message WHERE conversation_id IN
			(SELECT id FROM chat_conversation WHERE user_id = ?)`, userID); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM chat_search_term WHERE user_id = ?`,
			`DELETE FROM chat_attachment WHERE user_id = ?`,
			`DELETE FROM chat_run WHERE user_id = ?`,
			`DELETE FROM chat_conversation WHERE user_id = ?`,
			`DELETE FROM chat_memory WHERE user_id = ?`,
			`DELETE FROM chat_prompt WHERE user_id = ?`,
			`DELETE FROM chat_user_setting WHERE user_id = ?`,
			`DELETE FROM chat_share WHERE user_id = ?`,
			`DELETE FROM subscription_connection WHERE user_id = ?`,
			`DELETE FROM subscription_pending WHERE user_id = ?`,
			`DELETE FROM notification WHERE user_id = ?`,
		} {
			if err := ts.exec(ctx, q, userID); err != nil {
				return err
			}
		}
		if err := ts.exec(ctx, `DELETE FROM secgw_family_assignment WHERE scope_type = ? AND scope_id = ?`, SecgwFamilyScopeUser, userID); err != nil {
			return err
		}
		return ts.exec(ctx, `UPDATE app_user SET content_purged_at = ? WHERE id = ?`, FormatTime(nowUTC()), userID)
	})
}

// StaleDeletedChats lists conversations the person deleted themselves
// before `before`, for the purge job (files first, then rows).
func (s *Store) StaleDeletedChats(ctx context.Context, before time.Time, limit int) ([]*ChatConversation, error) {
	rows, err := s.query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE deleted_at <> '' AND deleted_at < ? ORDER BY deleted_at LIMIT ?`, FormatTime(before), limit)
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

// PurgeNotifications removes notifications created before `before`.
func (s *Store) PurgeNotifications(ctx context.Context, before time.Time) (int64, error) {
	return s.execCount(ctx, `DELETE FROM notification WHERE created_at < ?`, FormatTime(before))
}

// PruneSecgwViolationsBefore removes violation rows of every kind created
// before `before` (the usage-log retention: a violation never outlives the
// request it belongs to).
func (s *Store) PruneSecgwViolationsBefore(ctx context.Context, before time.Time) (int64, error) {
	return s.execCount(ctx, `DELETE FROM secgw_violation WHERE created_at < ?`, FormatTime(before))
}
