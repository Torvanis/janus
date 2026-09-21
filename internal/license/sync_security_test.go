package license

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

type syncFixture struct {
	e        *SyncEngine
	db       *store.Store
	now      time.Time
	claims   Claims
	priv     ed25519.PrivateKey
	response map[string]any
	status   int
	calls    atomic.Int32
}

func syncSetup(t *testing.T) *syncFixture {
	t.Helper()
	f := &syncFixture{now: time.Now().UTC().Truncate(time.Second), status: 200}
	var err error
	f.db, err = store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	priv, pubs := testKeys(t)
	f.priv = priv
	f.claims = business(f.now.Add(24*time.Hour), 7)
	f.claims.Issued = f.now.Add(-time.Hour)
	raw, _ := Sign(priv, f.claims)
	if err = f.db.PutLicenseKey(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	m := NewManager("", f.db, pubs, nil)
	m.now = func() time.Time { return f.now }
	_ = m.Refresh(context.Background())
	c, _ := crypto.New(make([]byte, 32))
	f.response = map[string]any{"license": raw, "license_id": f.claims.LicenseID, "status": "active", "subscription": f.metadata()}
	f.e = NewSync(m, f.db, c, SyncOptions{Now: func() time.Time { return f.now }, Transport: syncTransport(func(r *http.Request) (*http.Response, error) {
		f.calls.Add(1)
		b, _ := json.Marshal(f.response)
		return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})})
	return f
}
func (f *syncFixture) metadata() *Subscription {
	return &Subscription{SchemaVersion: 1, LicenseID: f.claims.LicenseID, BillingMode: "live", Status: "active", AutoRenew: true, PaidThrough: f.claims.Exp, VerifiedAt: f.now, FreshUntil: f.now.Add(6 * time.Hour), Revision: 3}
}
func (f *syncFixture) enable(t *testing.T) {
	t.Helper()
	if err := f.e.Configure(context.Background(), true, "secret", false); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDefaultOffEnvironmentAndOffline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pin     *bool
		token   string
		offline bool
		optin   bool
		calls   int32
	}{
		{name: "fresh"}, {name: "token alone", token: "secret"}, {name: "explicit false", pin: boolPtr(false), token: "secret"},
		{name: "explicit true", pin: boolPtr(true), token: "secret", calls: 1}, {name: "offline wins", pin: boolPtr(true), token: "secret", offline: true},
		{name: "UI optin", optin: true, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := syncSetup(t)
			f.e.options.Enabled = tc.pin
			f.e.options.Token = tc.token
			f.e.options.Offline = tc.offline
			if tc.optin {
				f.enable(t)
			}
			_ = f.e.Sync(context.Background(), true)
			if f.calls.Load() != tc.calls {
				t.Fatal("unexpected network", f.calls.Load())
			}
			s, n := f.e.View(context.Background())
			if tc.offline && (s.Mode != "offline" || n.SuppressExpiring) {
				t.Fatal(s, n)
			}
		})
	}
}
func boolPtr(v bool) *bool { return &v }

func TestSyncUntrustedAndInconsistentResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*syncFixture)
		wantError bool
	}{
		{"old server", func(f *syncFixture) { delete(f.response, "subscription") }, false},
		{"unknown schema", func(f *syncFixture) { f.response["subscription"].(*Subscription).SchemaVersion = 2 }, false},
		{"canceled", func(f *syncFixture) { f.response["subscription"].(*Subscription).CancelAtPeriodEnd = true }, false},
		{"past due", func(f *syncFixture) { f.response["subscription"].(*Subscription).Status = "past_due" }, false},
		{"future metadata", func(f *syncFixture) {
			s := f.response["subscription"].(*Subscription)
			s.VerifiedAt = f.now.Add(time.Hour)
			s.FreshUntil = s.VerifiedAt.Add(6 * time.Hour)
		}, true},
		{"paid mismatch", func(f *syncFixture) {
			v := f.now.Add(72 * time.Hour)
			f.response["subscription"].(*Subscription).PaidThrough = &v
		}, true},
		{"wrong identity", func(f *syncFixture) { c := f.claims; c.Org = "Other"; f.response["license"], _ = Sign(f.priv, c) }, true},
		{"old issued", func(f *syncFixture) {
			c := f.claims
			c.Issued = c.Issued.Add(-time.Hour)
			f.response["license"], _ = Sign(f.priv, c)
		}, true},
		{"old exp", func(f *syncFixture) {
			c := f.claims
			exp := c.Exp.Add(-time.Hour)
			c.Exp = &exp
			f.response["license"], _ = Sign(f.priv, c)
		}, true},
		{"sandbox crossing", func(f *syncFixture) {
			c := f.claims
			c.BillingMode = "sandbox"
			f.response["license"], _ = Sign(f.priv, c)
		}, true},
		{"bad signature", func(f *syncFixture) { p, _ := testKeys(t); f.response["license"], _ = Sign(p, f.claims) }, true},
		{"revoked", func(f *syncFixture) { f.response["status"] = "revoked"; delete(f.response, "license") }, true},
		{"http401", func(f *syncFixture) { f.status = 401 }, true},
		{"http500", func(f *syncFixture) { f.status = 500 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := syncSetup(t)
			f.enable(t)
			before, _ := f.db.LicenseKey(context.Background())
			tc.mutate(f)
			err := f.e.Sync(context.Background(), true)
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v", err)
			}
			_, notice := f.e.View(context.Background())
			if notice.SuppressExpiring {
				t.Fatal("unsafe suppression")
			}
			if tc.wantError {
				after, _ := f.db.LicenseKey(context.Background())
				if before != after {
					t.Fatal("failed sync replaced signed key")
				}
			}
		})
	}
}

func TestSyncFailedAttemptReplayAndReconfiguration(t *testing.T) {
	ctx := context.Background()
	f := syncSetup(t)
	f.enable(t)
	if err := f.e.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	f.status = 500
	f.now = f.now.Add(time.Minute)
	if err := f.e.Sync(ctx, true); err == nil {
		t.Fatal("expected failure")
	}
	_, notice := f.e.View(ctx)
	if notice.SuppressExpiring {
		t.Fatal("failed attempt suppressed")
	}
	f.status = 200
	f.response["subscription"].(*Subscription).Revision = 2
	if err := f.e.Sync(ctx, true); err == nil || err.Error() != "revision_regression" {
		t.Fatal(err)
	}
	if err := f.e.Configure(ctx, false, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Configure(ctx, true, "new-token", false); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Sync(ctx, true); err == nil {
		t.Fatal("credential rotation reset highwater")
	}
}

func TestSyncReplicaLeaseAndManualReplacementRace(t *testing.T) {
	ctx := context.Background()
	f := syncSetup(t)
	f.enable(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	f.e.client.Transport = syncTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		b, _ := json.Marshal(f.response)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})
	go func() { done <- f.e.Sync(ctx, true) }()
	<-entered
	other := NewSync(f.e.manager, f.db, f.e.cipher, SyncOptions{Now: func() time.Time { return f.now }, Transport: syncTransport(func(*http.Request) (*http.Response, error) {
		t.Error("replica stole live lease")
		return nil, errors.New("network")
	})})
	if err := other.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Reinstall the identical string: generation change, not string inequality,
	// must invalidate an already in-flight response.
	raw, _ := f.db.LicenseKey(ctx)
	if err := f.db.PutLicenseKey(ctx, raw); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil || err.Error() != "configuration_changed" {
		t.Fatal(err)
	}
	state, notice := other.View(ctx)
	if state.Enabled || notice.SuppressExpiring {
		t.Fatal("manual install retained online observation")
	}
}

func TestSyncSameRevisionCannotReplayHealthyOverCancellation(t *testing.T) {
	f := syncSetup(t)
	f.enable(t)
	ctx := context.Background()
	sub := f.response["subscription"].(*Subscription)
	sub.CancelAtPeriodEnd = true
	if err := f.e.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	sub.CancelAtPeriodEnd = false
	if err := f.e.Sync(ctx, true); err == nil {
		t.Fatal("same revision changed metadata accepted")
	}
	_, notice := f.e.View(ctx)
	if notice.SuppressExpiring {
		t.Fatal("replayed healthy metadata hid cancellation")
	}
}
