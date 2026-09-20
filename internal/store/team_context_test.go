package store

import (
	"context"
	"testing"
)

func TestTeamTokenAndBrowserContextAreIndependent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "context-user", "context@example.com", "Context", false, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTeam(ctx, "Context A", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTeam(ctx, "Context B", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, plain, err := s.CreateTeamToken(ctx, u.ID, "team key", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TeamID != a.ID || plain == "" {
		t.Fatal("key not team-bound")
	}
	if err = s.SetSessionTeam(ctx, "session", u.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.TokenByDigest(ctx, HashToken(plain))
	if err != nil || loaded.TeamID != a.ID {
		t.Fatalf("browser changed token context: %#v %v", loaded, err)
	}
	selected, err := s.SessionTeam(ctx, "session", u.ID)
	if err != nil || selected != b.ID {
		t.Fatalf("session context: %q %v", selected, err)
	}
	if err = s.SetSessionTeam(ctx, "session", u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateTeamToken(ctx, "outsider", "invalid", a.ID); err == nil {
		t.Fatal("nonmember issued key")
	}
	personal, _, err := s.CreateToken(ctx, u.ID, "personal")
	if err != nil || personal.TeamID != "" {
		t.Fatalf("legacy personal key changed: %#v %v", personal, err)
	}
}
