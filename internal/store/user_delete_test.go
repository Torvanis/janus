package store

import (
	"context"
	"testing"
	"time"
)

// A directory DELETE holds the person's content; provisioning the same
// person again within the hold (same userName, any case, or the same
// externalId) gives the same account back with its content.
func TestSCIMDeleteThenReprovisionRestores(t *testing.T) {
	for name, again := range map[string]map[string]any{
		"same userName, other case": {"userName": "Mistake@Example.com"},
		"same externalId":           {"userName": "renamed@example.com", "externalId": "oidc-subject-1"},
		"same sign-in subject":      {"userName": "renamed@example.com", "externalId": "oidc:bound"},
	} {
		t.Run(name, func(t *testing.T) {
			s := scimTestStore(t)
			ctx := context.Background()
			r, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "mistake@example.com", "externalId": "oidc-subject-1"})
			if err != nil {
				t.Fatal(err)
			}
			id := r["id"].(string)
			if err := s.InsertChatMemory(ctx, &ChatMemory{UserID: id, ContentEnc: "x"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, s.rebind(`UPDATE app_user SET auth_provider_id = 'oidc:bound' WHERE id = ?`), id); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteSCIMResource(ctx, "Users", id); err != nil {
				t.Fatal(err)
			}
			u, _ := s.UserByID(ctx, id)
			if u.IsActive || u.DeletedAt == nil || u.HeldUntil == nil || u.HeldUntil.Sub(*u.DeletedAt) != UserDeleteHold {
				t.Fatalf("deleted user not held: %+v", u)
			}
			back, err := s.SaveSCIMResource(ctx, "Users", "", again)
			if err != nil {
				t.Fatal(err)
			}
			if back["id"] != id {
				t.Fatalf("re-provisioned as a new account %v, want %s", back["id"], id)
			}
			u, _ = s.UserByID(ctx, id)
			if !u.IsActive || u.DeletedAt != nil {
				t.Fatalf("restored user: %+v", u)
			}
			mems, _ := s.ChatMemories(ctx, id)
			if len(mems) != 1 {
				t.Fatalf("memories after reconnect = %d", len(mems))
			}
			// Restored: the purge never touches it.
			due, _ := s.HeldUsersDue(ctx, time.Now().Add(365*24*time.Hour), 10)
			if len(due) != 0 {
				t.Fatalf("restored user still due for purge: %v", due)
			}
		})
	}
}

// PurgeUserContent keeps the Child Safety assignment out of the hold but
// removes it at purge, and never touches an account that is active again.
func TestPurgeUserContentScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "sub-x", "x@example.com", "X", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChatMemory(ctx, &ChatMemory{UserID: u.ID, ContentEnc: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeUserContent(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if mems, _ := s.ChatMemories(ctx, u.ID); len(mems) != 1 {
		t.Fatal("an active account's content was purged")
	}
	if err := s.MarkUserDeleted(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO secgw_family_assignment (scope_type, scope_id, profile_id, created_by, created_at) VALUES (?,?,?,?,?)`),
		SecgwFamilyScopeUser, u.ID, "cs_kids", "", FormatTime(nowUTC())); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeUserContent(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if mems, _ := s.ChatMemories(ctx, u.ID); len(mems) != 0 {
		t.Fatal("memories kept after purge")
	}
	var n int
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM secgw_family_assignment WHERE scope_id = ?`), u.ID).Scan(&n)
	if n != 0 {
		t.Fatal("child safety assignment kept after purge")
	}
}
