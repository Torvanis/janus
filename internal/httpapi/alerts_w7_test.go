package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// A pool ejecting a member tells admins once, even when several replicas
// eject it.
func TestPoolMemberEjectionAlertsAdminsOnce(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	mm, engines := seedPool(t, h, "round_robin", "off", "a", "b")
	engines[0].down.Store(true)
	for i := 0; i < 6; i++ {
		if rec := h.chatPool(t, fmt.Sprint("r", i), nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	h.server.pending.Wait()
	// Another replica ejecting the same member must not alert again.
	other := h.replica()
	m, _ := h.store.ModelByID(context.Background(), mm.Pool.Members[0].ModelID)
	other.alertPoolMemberDown(context.Background(), mm.ID, m, time.Now().Add(time.Minute), "upstream 503")
	if got := countTitles(t, h, h.user.ID, "taken out of rotation"); got != 1 {
		t.Fatalf("pool ejection alerts = %d, want 1", got)
	}
}

// A personal subscription that needs reconnecting tells its owner, once.
func TestSubscriptionReauthNotifiesOwnerOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	enc, _ := h.server.Cipher.Encrypt("tok")
	created, err := h.store.UpsertSubscription(ctx, store.SubscriptionUpsert{
		UserID: h.user.ID, Provider: "xai", AccountSubject: "1", AccountEmail: "a@x", Name: "a", EncryptedAccess: enc,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.server.markSubscriptionReauth(ctx, created, "xAI", "The sign-in expired.")
	h.replica().markSubscriptionReauth(ctx, created, "xAI", "The sign-in expired.")
	if got := countTitles(t, h, h.user.ID, "Reconnect your xAI subscription"); got != 1 {
		t.Fatalf("reauth notices = %d, want 1", got)
	}
	fresh, _ := h.store.SubscriptionByID(ctx, created.ID)
	if fresh.Status != store.SubscriptionReauthRequired {
		t.Fatalf("status = %s", fresh.Status)
	}
}

// Installing a license updates the expiry gauge at once, and a license in
// its last 30 days raises one admin alert across replicas.
func TestLicenseInstallUpdatesGaugeAndAlertsExpiringOnce(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	raw, err := license.Sign(priv, license.Claims{KeyID: "test", LicenseID: "JNS-BUS-SOON", Org: "T", IssuedTo: "t@t",
		Edition: license.EditionBusiness, Features: license.BusinessFeatures, Exp: &exp, GraceDays: 30,
		Term: license.TermSubscription, Issued: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	mgr := license.NewManager("", h.store, map[string]ed25519.PublicKey{"test": pub}, nil)
	mgr.OnRefresh(h.server.OnLicenseState)
	h.server.License = mgr
	if _, err := mgr.Install(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(h.server.Metrics.LicenseExpiry); got != float64(exp.Unix()) {
		t.Fatalf("license expiry gauge = %v after install, want %d", got, exp.Unix())
	}
	// A second replica refreshing the same license must not alert again.
	h.replica().OnLicenseState(mgr.State())
	_ = mgr.Refresh(context.Background())
	if got := countTitles(t, h, h.user.ID, "Janus license expires on"); got != 1 {
		t.Fatalf("license expiring alerts = %d, want 1", got)
	}
}

// Chat failures alert admins, rate-limited per model across replicas.
func TestChatFailureAlertIsRateLimited(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	h.server.alertChatFailure("gpt-x", "upstream.unavailable", "The model did not answer.")
	h.server.alertChatFailure("gpt-x", "upstream.unavailable", "The model did not answer.")
	h.replica().alertChatFailure("gpt-x", "upstream.unavailable", "The model did not answer.")
	h.server.alertChatFailure("gpt-y", "upstream.unavailable", "The model did not answer.")
	if got := countTitles(t, h, h.user.ID, "Chat replies are failing on gpt-x"); got != 1 {
		t.Fatalf("gpt-x alerts = %d, want 1", got)
	}
	if got := countTitles(t, h, h.user.ID, "Chat replies are failing on gpt-y"); got != 1 {
		t.Fatalf("gpt-y alerts = %d, want 1", got)
	}
}
