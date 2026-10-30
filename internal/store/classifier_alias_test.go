package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A guard classifier (any role but decision) can never be an alias target,
// a fallback or a pool member, and a model an alias serves through cannot
// become one.
func TestClassifiersNeverServeThroughManagedModels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	chat := newEnabledModel(t, s, "chat-a")
	chat2 := newEnabledModel(t, s, "chat-b")
	for _, role := range []string{ClassifierRoleGenerativeGuard, ClassifierRoleTextClassification} {
		guard := newEnabledModel(t, s, "guard-"+role)
		if err := s.SetModelClassifierRole(ctx, guard.ID, role); err != nil {
			t.Fatal(err)
		}
		wantClassifierRefusal := func(what string, err error) {
			t.Helper()
			var v *ValidationError
			if !errors.As(err, &v) || !strings.Contains(v.Message, "classifier") {
				t.Fatalf("%s (%s): want a classifier refusal, got %v", what, role, err)
			}
		}
		_, err := s.CreateManagedModel(ctx, "innocent-"+role, "", guard.ID, "")
		wantClassifierRefusal("alias target on create", err)

		mm, err := s.CreateManagedModel(ctx, "alias-"+role, "", chat.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		wantClassifierRefusal("repoint target", s.SetManagedModelTarget(ctx, mm.ID, guard.ID))
		wantClassifierRefusal("fallback", s.SetManagedModelFallback(ctx, mm.ID, guard.ID, nil))
		_, err = s.NormalizePool(ctx, PoolInput{Members: []PoolMemberInput{{ModelID: chat.ID}, {ModelID: guard.ID}}}, "")
		wantClassifierRefusal("pool member", err)
	}

	// A pool member (not the alias's target) cannot be made a classifier.
	mm, err := s.CreateManagedModel(ctx, "pool", "", chat.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := s.NormalizePool(ctx, PoolInput{Members: []PoolMemberInput{{ModelID: chat.ID}, {ModelID: chat2.ID}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetManagedModelPool(ctx, mm.ID, pool); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{ClassifierRoleGenerativeGuard, ClassifierRoleDecision} {
		var v *ValidationError
		if err := s.SetModelClassifierRole(ctx, chat2.ID, role); !errors.As(err, &v) {
			t.Fatalf("pool member became a %s classifier: %v", role, err)
		}
	}
}
