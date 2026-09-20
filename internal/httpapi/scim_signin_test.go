package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
)

func scimSignInCounts(t *testing.T, h *harness) (users, sessions int) {
	t.Helper()
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM app_user`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM web_session`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSCIMSignInGroupSyncFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.server.Sessions = auth.NewDBSessionStore(h.store, time.Hour, time.Hour)
	ctx := context.Background()
	identity := &auth.Identity{Subject: h.user.AuthID, Email: h.user.Email, Name: h.user.Name, Groups: []string{"new-group"}}
	if err := h.store.ReplaceIDPGroupsForUser(ctx, h.user.ID, []string{"stale-access"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`CREATE TRIGGER fail_group_sync BEFORE DELETE ON group_member BEGIN SELECT RAISE(ABORT, 'simulated group failure'); END`); err != nil {
		t.Fatal(err)
	}
	usersBefore, sessionsBefore := scimSignInCounts(t, h)
	rec := httptest.NewRecorder()
	h.server.completeSignIn(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil), identity, "/dashboard")
	usersAfter, sessionsAfter := scimSignInCounts(t, h)
	if usersAfter != usersBefore || sessionsAfter != sessionsBefore || len(rec.Result().Cookies()) != 0 {
		t.Fatal("failed sync created user or session")
	}
	if !strings.Contains(rec.Header().Get("Location"), "/auth/login?error=") {
		t.Fatalf("expected actionable retry error, got %q", rec.Header().Get("Location"))
	}
}

func TestSCIMSignInDeletedAfterLinkRemainsDisabled(t *testing.T) {
	h := newHarness(t)
	h.server.Sessions = auth.NewDBSessionStore(h.store, time.Hour, time.Hour)
	ctx := context.Background()
	identity := &auth.Identity{Subject: "linked-subject", Email: "linked@example.com", Name: "Linked", EmailVerified: true}
	saved, err := h.store.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": identity.Email, "active": true})
	if err != nil {
		t.Fatal(err)
	}
	user := signIn(t, h, identity)
	if user.ID != saved["id"] {
		t.Fatalf("wrong linked identity %s", user.ID)
	}
	if err := h.store.DeleteSCIMResource(ctx, "Users", user.ID); err != nil {
		t.Fatal(err)
	}
	usersBefore, sessionsBefore := scimSignInCounts(t, h)
	if sessionsBefore != 0 {
		t.Fatal("deletion did not revoke sessions")
	}
	rec := httptest.NewRecorder()
	h.server.completeSignIn(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil), identity, "/dashboard")
	usersAfter, sessionsAfter := scimSignInCounts(t, h)
	if usersAfter != usersBefore || sessionsAfter != 0 || len(rec.Result().Cookies()) != 0 {
		t.Fatal("deleted identity was recreated or signed in")
	}
	if !strings.Contains(rec.Header().Get("Location"), "/auth/login?error=") {
		t.Fatalf("missing disabled-account error: %s", rec.Header().Get("Location"))
	}
	user, err = h.store.UserByID(ctx, user.ID)
	if err != nil || user.IsActive {
		t.Fatalf("deleted account reactivated: %+v %v", user, err)
	}
}

func TestSCIMSignInIdentityBridge(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		verified, active, deleted, external, legacy, takeover bool
		denied                                                bool
	}{
		{name: "verified preprovisioned", verified: true, active: true},
		{name: "unverified conflict", active: true, denied: true},
		{name: "external subject without verified email", active: true, external: true},
		{name: "inactive", verified: true, denied: true},
		{name: "deleted before first login", verified: true, active: true, deleted: true, denied: true},
		{name: "legacy subject preserved", active: true, legacy: true},
		{name: "already bound no takeover", verified: true, active: true, takeover: true, denied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.server.Sessions = auth.NewDBSessionStore(h.store, time.Hour, time.Hour)
			ctx := context.Background()
			identity := &auth.Identity{Subject: "oidc-subject", Email: "directory@example.com", Name: "Login Name", EmailVerified: tc.verified}
			resource := map[string]any{"userName": identity.Email, "displayName": "Provisioned Name", "active": tc.active}
			if tc.external {
				resource["externalId"] = identity.Subject
				identity.Email = "different@example.com"
			}
			saved, err := h.store.SaveSCIMResource(ctx, "Users", "", resource)
			if err != nil {
				t.Fatal(err)
			}
			wantID := saved["id"].(string)
			if tc.legacy {
				u, _, err := h.store.UpsertUserFromIdentity(ctx, identity.Subject, identity.Email, "Legacy", false, false)
				if err != nil {
					t.Fatal(err)
				}
				wantID = u.ID
			}
			if tc.takeover {
				if _, err := h.store.ClaimSCIMIdentity(ctx, "different-subject", identity.Email, "Other", true); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deleted {
				if err := h.store.DeleteSCIMResource(ctx, "Users", wantID); err != nil {
					t.Fatal(err)
				}
			}
			usersBefore, sessionsBefore := scimSignInCounts(t, h)
			rec := httptest.NewRecorder()
			h.server.completeSignIn(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil), identity, "/dashboard")
			usersAfter, sessionsAfter := scimSignInCounts(t, h)
			if usersAfter != usersBefore {
				t.Errorf("duplicate users: before=%d after=%d", usersBefore, usersAfter)
			}
			denied := strings.Contains(rec.Header().Get("Location"), "/auth/login?error=")
			if denied != tc.denied {
				t.Errorf("redirect=%q denied=%v want=%v", rec.Header().Get("Location"), denied, tc.denied)
			}
			if tc.denied {
				if sessionsAfter != sessionsBefore || len(rec.Result().Cookies()) != 0 {
					t.Fatal("denied login created session or cookies")
				}
				return
			}
			if sessionsAfter != sessionsBefore+1 {
				t.Errorf("session count=%d want=%d", sessionsAfter, sessionsBefore+1)
			}
			user, err := h.store.UserByAuthID(ctx, identity.Subject)
			if err != nil {
				t.Fatal(err)
			}
			if user.ID != wantID || !user.IsActive || user.Name != identity.Name {
				t.Errorf("linked user=%+v want ID=%s", user, wantID)
			}
		})
	}
}
