package license

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memStore struct{ key string }

func (m *memStore) LicenseKey(context.Context) (string, error)      { return m.key, nil }
func (m *memStore) PutLicenseKey(_ context.Context, k string) error { m.key = k; return nil }
func (m *memStore) DeleteLicenseKey(context.Context) error          { m.key = ""; return nil }

func testKeys(t *testing.T) (ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{"test": pub}
}

func business(exp time.Time, grace int) Claims {
	return Claims{KeyID: "test", LicenseID: "JNS-BUS-TEST-0001", Org: "Acme", IssuedTo: "ops@acme.test",
		Edition: EditionBusiness, Seats: 100, Nodes: 3, Features: BusinessFeatures,
		Issued: time.Now().Add(-48 * time.Hour), Exp: &exp, GraceDays: grace, Term: TermSubscription}
}

func newMgr(t *testing.T, raw string, pubs map[string]ed25519.PublicKey, now time.Time) *Manager {
	t.Helper()
	m := NewManager("", &memStore{key: raw}, pubs, nil)
	m.now = func() time.Time { return now }
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNoKeyIsCommunity(t *testing.T) {
	_, pubs := testKeys(t)
	st := newMgr(t, "", pubs, time.Now()).State()
	if st.Installed || st.Edition != EditionCommunity || st.Status != StatusValid || st.Seats != CommunitySeats || st.Nodes != CommunityNodes {
		t.Fatalf("unexpected: %+v", st)
	}
	if st.Restricted() {
		t.Fatal("community must never be restricted")
	}
	if st.Has("scim") {
		t.Fatal("community has no business features")
	}
}

func TestStatusMatrix(t *testing.T) {
	priv, pubs := testKeys(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		exp        time.Time
		grace      int
		want       Status
		restricted bool
	}{
		{"valid", now.Add(90 * 24 * time.Hour), 30, StatusValid, false},
		{"expiring", now.Add(10 * 24 * time.Hour), 30, StatusExpiring, false},
		{"grace", now.Add(-5 * 24 * time.Hour), 30, StatusGrace, false},
		{"expired", now.Add(-40 * 24 * time.Hour), 30, StatusExpired, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Sign(priv, business(tc.exp, tc.grace))
			if err != nil {
				t.Fatal(err)
			}
			st := newMgr(t, raw, pubs, now).State()
			if st.Status != tc.want || st.Restricted() != tc.restricted {
				t.Fatalf("got %s restricted=%v want %s restricted=%v", st.Status, st.Restricted(), tc.want, tc.restricted)
			}
			// Features never disappear with state — configured things keep running.
			if !st.Has("scim") || st.Seats != 100 {
				t.Fatalf("claims must stay effective in every state: %+v", st)
			}
		})
	}
}

func TestTamperedAndUnknownKey(t *testing.T) {
	priv, pubs := testKeys(t)
	raw, _ := Sign(priv, business(time.Now().Add(365*24*time.Hour), 30))
	parts := strings.Split(raw, ".")
	// Flip a payload byte.
	b := []byte(parts[1])
	b[10] ^= 1
	tampered := parts[0] + "." + string(b) + "." + parts[2]
	st := newMgr(t, tampered, pubs, time.Now()).State()
	if st.Status != StatusInvalid || !st.Installed || !st.Restricted() || st.Seats != CommunitySeats {
		t.Fatalf("tampered key must be invalid+restricted at community limits: %+v", st)
	}
	// Signed by a key we do not trust.
	otherPriv, _ := testKeys(t)
	oc := business(time.Now().Add(365*24*time.Hour), 30)
	oc.KeyID = "rogue"
	raw2, _ := Sign(otherPriv, oc)
	st = newMgr(t, raw2, pubs, time.Now()).State()
	if st.Status != StatusInvalid || !strings.Contains(st.Error, "unknown key_id") {
		t.Fatalf("unknown key_id must be invalid: %+v", st)
	}
}

func TestCommunityKeyNeverLiftsLimits(t *testing.T) {
	priv, pubs := testKeys(t)
	c := business(time.Now().Add(365*24*time.Hour), 30)
	c.Edition = EditionCommunity
	c.Seats = 500
	c.Nodes = 9
	raw, _ := Sign(priv, c)
	st := newMgr(t, raw, pubs, time.Now()).State()
	if st.Seats != CommunitySeats || st.Nodes != CommunityNodes || len(st.Features) != 0 {
		t.Fatalf("community key must keep community limits: %+v", st)
	}
}

func TestClockSkewIsInformational(t *testing.T) {
	priv, pubs := testKeys(t)
	c := business(time.Now().Add(365*24*time.Hour), 30)
	c.Issued = time.Now().Add(3 * 24 * time.Hour) // signer clock 3 days ahead
	raw, _ := Sign(priv, c)
	st := newMgr(t, raw, pubs, time.Now()).State()
	if !st.ClockSkew || st.Status != StatusValid || st.Restricted() {
		t.Fatalf("skew must warn, never block: %+v", st)
	}
}

func TestPerpetualMaintenance(t *testing.T) {
	c := &Claims{Term: TermPerpetual, LicenseID: "JNS-ENT-X", MaintenanceUntil: "2027-06-30"}
	if err := CheckMaintenance(c, "2027-06-30"); err != nil {
		t.Fatalf("same-day build must run: %v", err)
	}
	if err := CheckMaintenance(c, "2027-07-01"); err == nil {
		t.Fatal("newer build must refuse")
	}
	if err := CheckMaintenance(c, ""); err != nil {
		t.Fatal("dev build never refuses")
	}
	sub := &Claims{Term: TermSubscription, MaintenanceUntil: "2020-01-01"}
	if err := CheckMaintenance(sub, "2027-01-01"); err != nil {
		t.Fatal("subscription keys ignore maintenance_until")
	}
}

func TestFileWinsOverDatabaseAndInstall(t *testing.T) {
	priv, pubs := testKeys(t)
	dbKey, _ := Sign(priv, business(time.Now().Add(365*24*time.Hour), 30))
	fc := business(time.Now().Add(365*24*time.Hour), 30)
	fc.LicenseID = "JNS-BUS-FILE-0001"
	fileKey, _ := Sign(priv, fc)
	dir := t.TempDir()
	path := filepath.Join(dir, "license.key")
	if err := os.WriteFile(path, []byte(fileKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, &memStore{key: dbKey}, pubs, nil)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := m.State(); st.Source != "file" || st.Claims.LicenseID != "JNS-BUS-FILE-0001" {
		t.Fatalf("file must win: %+v", st)
	}
	// Missing file falls through to the database.
	m2 := NewManager(filepath.Join(dir, "nope.key"), &memStore{key: dbKey}, pubs, nil)
	_ = m2.Refresh(context.Background())
	if st := m2.State(); st.Source != "database" {
		t.Fatalf("db fallback: %+v", st)
	}
	// Install rejects garbage without touching the store.
	ms := &memStore{}
	m3 := NewManager("", ms, pubs, nil)
	_ = m3.Refresh(context.Background())
	if _, err := m3.Install(context.Background(), "JANUS-LICENSE-1.garbage.garbage"); err == nil || ms.key != "" {
		t.Fatal("bad key must not be stored")
	}
	if st, err := m3.Install(context.Background(), dbKey); err != nil || st.Edition != EditionBusiness || ms.key != dbKey {
		t.Fatalf("install: %v %+v", err, st)
	}
	if st, _ := m3.Remove(context.Background()); st.Installed {
		t.Fatal("remove must return to community")
	}
}

func TestEmbeddedKeysDecode(t *testing.T) {
	if _, ok := TrustedKeys()["2026-09"]; !ok {
		t.Fatal("production key 2026-09 missing")
	}
}
