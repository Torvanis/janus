package httpapi

import (
	"context"
	"github.com/torvanis/janus/internal/auth"
	"testing"
	"time"
)

func TestSCIMSignInGroupOwnershipAndInactivePeer(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "deactivated", true: "deleted"}[deleted], func(t *testing.T) {
			h := newHarness(t)
			h.server.Sessions = auth.NewDBSessionStore(h.store, time.Hour, time.Hour)
			ctx := context.Background()
			identity := &auth.Identity{Subject: "active-directory-subject", Email: "active-directory@example.com", Name: "Active", EmailVerified: true}
			a, err := h.store.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": identity.Email, "active": true})
			if err != nil {
				t.Fatal(err)
			}
			b, err := h.store.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "peer@example.com", "active": true})
			if err != nil {
				t.Fatal(err)
			}
			aid, bid := a["id"].(string), b["id"].(string)
			g, err := h.store.SaveSCIMResource(ctx, "Groups", "", map[string]any{"displayName": "Directory", "members": []any{map[string]any{"value": aid}, map[string]any{"value": bid}}})
			if err != nil {
				t.Fatal(err)
			}
			team, err := h.store.CreateTeam(ctx, "Directory team", "")
			if err != nil {
				t.Fatal(err)
			}
			if err = h.store.SetTeamGroupMappings(ctx, team.ID, []string{g["id"].(string)}); err != nil {
				t.Fatal(err)
			}
			if deleted {
				err = h.store.DeleteSCIMResource(ctx, "Users", bid)
			} else {
				_, err = h.store.SaveSCIMResource(ctx, "Users", bid, map[string]any{"userName": "peer@example.com", "active": false})
			}
			if err != nil {
				t.Fatal(err)
			}
			user := signIn(t, h, identity)
			if user.ID != aid {
				t.Fatal("login did not preserve directory identity")
			}
			if err = h.store.ValidateTeamMember(ctx, team.ID, aid); err != nil {
				t.Fatal(err)
			}
			if err = h.store.ValidateTeamMember(ctx, team.ID, bid); err == nil {
				t.Fatal("disabled peer regained membership")
			}
			outsider := &auth.Identity{Subject: "outsider", Email: "outsider@example.com", Name: "Outsider", EmailVerified: true, Groups: []string{"Directory"}}
			u := signIn(t, h, outsider)
			if err = h.store.ValidateTeamMember(ctx, team.ID, u.ID); err == nil {
				t.Fatal("matching OIDC claim granted SCIM team access")
			}
			outsider.Groups = nil
			signIn(t, h, outsider)
			ids, err := h.store.GroupIDsForUser(ctx, u.ID)
			if err != nil || len(ids) != 0 {
				t.Fatalf("outsider retained group access: %v %v", ids, err)
			}
		})
	}
}
