package store

import (
	"context"
	"testing"
)

// Behavior-only form of TestSCIMDeleteThenReprovisionRestores (compiles
// against the code before the hold existed): a directory DELETE followed
// by re-provisioning the same userName returns the same account, active,
// with its content.
func TestSCIMReprovisionSameUserNameGetsAccountBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "oops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	id := r["id"].(string)
	if err := s.InsertChatMemory(ctx, &ChatMemory{UserID: id, ContentEnc: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSCIMResource(ctx, "Users", id); err != nil {
		t.Fatal(err)
	}
	back, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "OOPS@example.com"})
	if err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if back["id"] != id {
		t.Fatalf("re-provisioned as %v, want the held account %s", back["id"], id)
	}
	if u, _ := s.UserByID(ctx, id); u == nil || !u.IsActive {
		t.Fatalf("held account not active again: %+v", u)
	}
}
