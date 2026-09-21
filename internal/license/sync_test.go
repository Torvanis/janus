package license

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

type syncTransport func(*http.Request) (*http.Response, error)

func (f syncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSyncRenewalDurabilityAndFreshness(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	priv, pubs := testKeys(t)
	old := business(now.Add(time.Hour), 7)
	old.Issued = now.Add(-time.Hour)
	raw, _ := Sign(priv, old)
	if err = db.PutLicenseKey(ctx, raw); err != nil {
		t.Fatal(err)
	}
	m := NewManager("", db, pubs, nil)
	m.now = func() time.Time { return now }
	_ = m.Refresh(ctx)
	c, _ := crypto.New(make([]byte, 32))
	next := old
	exp := now.Add(48 * time.Hour)
	next.Exp = &exp
	next.Issued = now
	renewed, _ := Sign(priv, next)
	calls := 0
	transport := syncTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != SyncEndpoint || r.Header.Get("Authorization") != "Bearer "+old.LicenseID+".secret" {
			t.Fatal("unsafe request")
		}
		body, _ := json.Marshal(map[string]any{"license": renewed, "status": "active", "license_id": old.LicenseID, "subscription": Subscription{SchemaVersion: 1, LicenseID: old.LicenseID, BillingMode: "live", Status: "active", AutoRenew: true, PaidThrough: &exp, VerifiedAt: now, FreshUntil: now.Add(6 * time.Hour), Revision: 1}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	engine := NewSync(m, db, c, SyncOptions{Now: func() time.Time { return now }, Transport: transport})
	if err = engine.Configure(ctx, true, "secret", false); err != nil {
		t.Fatal(err)
	}
	if err = engine.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	state, notice := engine.View(ctx)
	if state.Health != "healthy" || !notice.SuppressExpiring {
		t.Fatalf("%+v %+v", state, notice)
	}
	stored, key, _ := db.LicenseSyncSnapshot(ctx)
	if key != renewed || strings.Contains(stored, "secret") {
		t.Fatal("renewal or encryption")
	}
	restarted := NewSync(m, db, c, SyncOptions{Now: func() time.Time { return now }, Transport: transport})
	_, notice = restarted.View(ctx)
	if !notice.SuppressExpiring {
		t.Fatal("lost persisted observation")
	}
	now = now.Add(6 * time.Hour)
	state, notice = restarted.View(ctx)
	if notice.SuppressExpiring || state.Health != "stale" {
		t.Fatalf("stale %+v %+v", state, notice)
	}
	if calls != 1 {
		t.Fatal("GET must not call network")
	}
	now = exp.Add(7 * 24 * time.Hour)
	if m.State().Status != StatusExpired {
		t.Fatal("cached status outlived signed grace")
	}
}
