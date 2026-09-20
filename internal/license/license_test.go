package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func keys(t *testing.T, id string) (map[string]ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]ed25519.PublicKey{id: pub}, priv
}

func sample(exp time.Time) Claims {
	return Claims{
		KeyID: "k1", LicenseID: "JNS-BUS-TEST", Org: "Acme", IssuedTo: "ops@acme.test",
		Edition: EditionBusiness, Seats: 60, Nodes: 3, Site: "HQ",
		Features: BusinessFeatures, Issued: time.Now().UTC(), Exp: &exp, GraceDays: 30,
		Term: TermSubscription,
	}
}

func TestRoundTrip(t *testing.T) {
	pubs, priv := keys(t, "k1")
	exp := time.Now().Add(365 * 24 * time.Hour)
	s, err := Sign(priv, sample(exp))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, Prefix+".") {
		t.Fatalf("prefix: %s", s[:20])
	}
	c, err := Verify(s, pubs)
	if err != nil {
		t.Fatal(err)
	}
	if c.Seats != 60 || c.Site != "HQ" || c.Edition != EditionBusiness {
		t.Fatalf("claims lost: %+v", c)
	}
}

func TestTamper(t *testing.T) {
	pubs, priv := keys(t, "k1")
	exp := time.Now().Add(24 * time.Hour)
	s, _ := Sign(priv, sample(exp))
	parts := strings.Split(s, ".")
	// flip a payload byte
	b := []byte(parts[1])
	b[5] ^= 1
	parts[1] = string(b)
	if _, err := Verify(strings.Join(parts, "."), pubs); err == nil {
		t.Fatal("tampered license verified")
	}
}

func TestUnknownKey(t *testing.T) {
	_, priv := keys(t, "k1")
	other, _ := keys(t, "k2")
	exp := time.Now().Add(24 * time.Hour)
	s, _ := Sign(priv, sample(exp))
	if _, err := Verify(s, other); err != ErrUnknownKey {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

func TestStatus(t *testing.T) {
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		exp  time.Time
		want Status
	}{
		{now.Add(60 * 24 * time.Hour), StatusValid},
		{now.Add(10 * 24 * time.Hour), StatusExpiring},
		{now.Add(-10 * 24 * time.Hour), StatusGrace},
		{now.Add(-40 * 24 * time.Hour), StatusExpired},
	}
	for _, tc := range cases {
		c := sample(tc.exp)
		if got := c.StatusAt(now); got != tc.want {
			t.Errorf("exp %v: got %s want %s", tc.exp, got, tc.want)
		}
	}
	p := sample(now)
	p.Term, p.Exp, p.MaintenanceUntil = TermPerpetual, nil, "2027-01-01"
	if got := p.StatusAt(now.Add(10 * 365 * 24 * time.Hour)); got != StatusValid {
		t.Errorf("perpetual: got %s", got)
	}
}

func TestValidate(t *testing.T) {
	_, priv := keys(t, "k1")
	c := sample(time.Now())
	c.Term, c.Exp = TermPerpetual, nil
	if _, err := Sign(priv, c); err == nil {
		t.Fatal("perpetual without maintenance_until signed")
	}
	c.Term = TermSubscription
	if _, err := Sign(priv, c); err == nil {
		t.Fatal("subscription without exp signed")
	}
}
