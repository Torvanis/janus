package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// A vendor that invalidates the access token early (OpenAI did, 2026-10-31,
// two days before the advertised expiry) must not strand the connection:
// the check renews once with the refresh token, proves the new token, and
// keeps the connection active. Only a refused REFRESH means reconnect.
func TestSubscriptionCheckRenewsRefusedAccessToken(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	ctx := context.Background()
	sess := adminSession(t, h)
	h.enableXAISubscriptions(t, sess, true)
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}

	// The vendor kills the live access token; the refresh token still works.
	fake.mu.Lock()
	fake.liveAccess = "revoked-early"
	// A verdict the provider maps to ReauthError (OpenAI: 401/403; xAI: 403).
	fake.modelsDeadStatus = http.StatusForbidden
	fake.mu.Unlock()

	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+connID+"/check", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	conn, err := h.store.SubscriptionByID(ctx, connID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != store.SubscriptionActive {
		t.Fatalf("renewable sign-in marked %q (%s); want active after renewal", conn.Status, conn.LastError)
	}
	fake.mu.Lock()
	seq := fake.accessSeq
	fake.mu.Unlock()
	if seq < 2 {
		t.Fatalf("no refresh happened (accessSeq=%d)", seq)
	}

	// A refused refresh token is the real "reconnect" case.
	fake.mu.Lock()
	fake.liveAccess = "revoked-again"
	fake.refreshFails = true
	fake.mu.Unlock()
	h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+connID+"/check", nil)
	conn, _ = h.store.SubscriptionByID(ctx, connID)
	if conn.Status != store.SubscriptionReauthRequired {
		t.Fatalf("dead refresh token: status %q, want reauth_required", conn.Status)
	}
}
