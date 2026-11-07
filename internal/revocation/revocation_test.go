package revocation

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"
)

// pki is a throwaway root -> intermediate -> leaf hierarchy with a live
// OCSP responder and CRL distribution point for the intermediate's leaves.
type pki struct {
	t                  *testing.T
	root, inter        *x509.Certificate
	rootKey, interKey  *ecdsa.PrivateKey
	srv                *httptest.Server
	mu                 sync.Mutex
	revoked            map[string]bool // serial hex, as known by OCSP and CRL
	ocspDown, crlDown  atomic.Bool
	ocspHits, crlHits  atomic.Int64
	crlNext            time.Duration
	interRevokedByRoot bool
	serial             int64
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{t: t, revoked: map[string]bool{}, crlNext: time.Hour, serial: 100}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.rootKey, p.interKey = newKey(t), newKey(t)
	now := time.Now()
	rootT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	p.root = p.sign(rootT, rootT, &p.rootKey.PublicKey, p.rootKey)
	interT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test Intermediate CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, CRLDistributionPoints: []string{p.srv.URL + "/root.crl"}}
	p.inter = p.sign(interT, p.root, &p.interKey.PublicKey, p.rootKey)
	return p
}

func (p *pki) sign(tmpl, parent *x509.Certificate, pub crypto.PublicKey, key crypto.Signer) *x509.Certificate {
	p.t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
	if err != nil {
		p.t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		p.t.Fatal(err)
	}
	return c
}

// leaf issues a server certificate for 127.0.0.1 with the chosen sources.
func (p *pki) leaf(withOCSP, withCRL bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	p.t.Helper()
	p.mu.Lock()
	p.serial++
	serial := p.serial
	p.mu.Unlock()
	k := newKey(p.t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "upstream.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if withOCSP {
		tmpl.OCSPServer = []string{p.srv.URL + "/ocsp"}
	}
	if withCRL {
		tmpl.CRLDistributionPoints = []string{p.srv.URL + "/inter.crl"}
	}
	return p.sign(tmpl, p.inter, &k.PublicKey, p.interKey), k
}

func (p *pki) revoke(c *x509.Certificate) {
	p.mu.Lock()
	p.revoked[c.SerialNumber.Text(16)] = true
	p.mu.Unlock()
}

func (p *pki) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ocsp":
		p.ocspHits.Add(1)
		if p.ocspDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		req, err := ocsp.ParseRequest(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		revoked := p.revoked[req.SerialNumber.Text(16)]
		p.mu.Unlock()
		// SHA-256 CertID, like the request: SHA-1 panics under fips140=only.
		tmpl := ocsp.Response{Status: ocsp.Good, IssuerHash: crypto.SHA256, SerialNumber: req.SerialNumber, ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour)}
		if revoked {
			tmpl.Status, tmpl.RevokedAt, tmpl.RevocationReason = ocsp.Revoked, time.Now().Add(-time.Minute), ocsp.KeyCompromise
		}
		der, err := ocsp.CreateResponse(p.inter, p.inter, tmpl, p.interKey)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		_, _ = w.Write(der)
	case "/inter.crl", "/root.crl":
		p.crlHits.Add(1)
		if p.crlDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		issuer, key := p.inter, p.interKey
		var entries []x509.RevocationListEntry
		if r.URL.Path == "/root.crl" {
			issuer, key = p.root, p.rootKey
			if p.interRevokedByRoot {
				entries = append(entries, x509.RevocationListEntry{SerialNumber: p.inter.SerialNumber, RevocationTime: time.Now().Add(-time.Minute)})
			}
		} else {
			p.mu.Lock()
			for s := range p.revoked {
				n, _ := new(big.Int).SetString(s, 16)
				entries = append(entries, x509.RevocationListEntry{SerialNumber: n, RevocationTime: time.Now().Add(-time.Minute)})
			}
			p.mu.Unlock()
		}
		der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(time.Now().UnixNano()),
			ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(p.crlNext), RevokedCertificateEntries: entries}, issuer, key)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(der)
	default:
		http.NotFound(w, r)
	}
}

// upstream serves HTTPS with leaf (chain leaf+intermediate).
func (p *pki) upstream(leaf *x509.Certificate, key *ecdsa.PrivateKey) *httptest.Server {
	p.t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw, p.inter.Raw}, PrivateKey: key, Leaf: leaf}}}
	s.StartTLS()
	p.t.Cleanup(s.Close)
	return s
}

func (p *pki) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(p.root)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: Wrap(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})}}
}

func configure(t *testing.T, mode Mode, responder string) {
	t.Helper()
	restore := Configure(Options{Mode: mode, OCSPResponder: responder, Timeout: 2 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(restore)
}

func get(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func TestOffByDefault(t *testing.T) {
	p := newPKI(t)
	leaf, key := p.leaf(true, true)
	p.revoke(leaf)
	srv := p.upstream(leaf, key)
	configure(t, ModeOff, "")
	if err := get(t, p.client(), srv.URL); err != nil {
		t.Fatalf("off must not check: %v", err)
	}
	if p.ocspHits.Load()+p.crlHits.Load() != 0 {
		t.Fatal("off mode contacted a revocation source")
	}
	if m, _ := ParseMode(""); m != ModeOff {
		t.Fatal("empty mode is not off")
	}
}

func TestOCSPGoodAndRevoked(t *testing.T) {
	for _, mode := range []Mode{ModeSoft, ModeHard} {
		t.Run(string(mode), func(t *testing.T) {
			p := newPKI(t)
			configure(t, mode, "")
			good, gk := p.leaf(true, false)
			if err := get(t, p.client(), p.upstream(good, gk).URL); err != nil {
				t.Fatalf("good certificate refused: %v", err)
			}
			bad, bk := p.leaf(true, false)
			p.revoke(bad)
			err := get(t, p.client(), p.upstream(bad, bk).URL)
			if !errors.Is(err, ErrRevoked) {
				t.Fatalf("revoked certificate accepted: %v", err)
			}
			if st := Current(); st.Revoked == 0 || st.Good == 0 {
				t.Fatalf("stats: %+v", st)
			}
		})
	}
}

func TestCRLRevokedLeafAndIntermediate(t *testing.T) {
	p := newPKI(t)
	configure(t, ModeHard, "")
	leaf, key := p.leaf(false, true)
	srv := p.upstream(leaf, key)
	if err := get(t, p.client(), srv.URL); err != nil {
		t.Fatalf("good via CRL refused: %v", err)
	}
	// Revoked leaf; a fresh checker (no cached CRL) sees it.
	p.revoke(leaf)
	configure(t, ModeHard, "")
	if err := get(t, p.client(), srv.URL); !errors.Is(err, ErrRevoked) {
		t.Fatalf("CRL-revoked leaf accepted: %v", err)
	}
	// Revoked intermediate (on the root's CRL) fails every leaf under it.
	p2 := newPKI(t)
	p2.interRevokedByRoot = true
	configure(t, ModeHard, "")
	l2, k2 := p2.leaf(true, false)
	if err := get(t, p2.client(), p2.upstream(l2, k2).URL); !errors.Is(err, ErrRevoked) || !strings.Contains(err.Error(), "Intermediate") {
		t.Fatalf("revoked intermediate accepted: %v", err)
	}
}

func TestUnavailableSoftVersusHard(t *testing.T) {
	p := newPKI(t)
	p.ocspDown.Store(true)
	p.crlDown.Store(true)
	leaf, key := p.leaf(true, true)
	srv := p.upstream(leaf, key)

	configure(t, ModeSoft, "")
	if err := get(t, p.client(), srv.URL); err != nil {
		t.Fatalf("soft mode refused when sources are down: %v", err)
	}
	if st := Current(); st.Unavailable == 0 || st.LastError == "" {
		t.Fatalf("soft stats: %+v", st)
	}
	configure(t, ModeHard, "")
	if err := get(t, p.client(), srv.URL); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("hard mode accepted unknown status: %v", err)
	}
	// OCSP down, CRL up: falls back to the CRL and passes in hard mode.
	p.crlDown.Store(false)
	configure(t, ModeHard, "")
	if err := get(t, p.client(), srv.URL); err != nil {
		t.Fatalf("CRL fallback failed: %v", err)
	}
}

func TestStaleCRLIsUnavailable(t *testing.T) {
	p := newPKI(t)
	p.crlNext = -time.Minute // nextUpdate in the past
	leaf, key := p.leaf(false, true)
	configure(t, ModeHard, "")
	if err := get(t, p.client(), p.upstream(leaf, key).URL); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale CRL accepted: %v", err)
	}
}

func TestNoSourceIsAcceptedAndCounted(t *testing.T) {
	p := newPKI(t)
	// The intermediate points at the root CRL; the leaf publishes nothing.
	leaf, key := p.leaf(false, false)
	configure(t, ModeHard, "")
	if err := get(t, p.client(), p.upstream(leaf, key).URL); err != nil {
		t.Fatalf("certificate without revocation sources refused: %v", err)
	}
	if st := Current(); st.NoSource == 0 {
		t.Fatalf("no-source not counted: %+v", st)
	}
}

func TestResponderOverrideAndCache(t *testing.T) {
	p := newPKI(t)
	// The certificate names an unreachable responder; the override wins.
	l2, k2 := p.leafWithOCSP("http://127.0.0.1:1/unreachable")
	configure(t, ModeHard, p.srv.URL+"/ocsp")
	srv := p.upstream(l2, k2)
	if err := get(t, p.client(), srv.URL); err != nil {
		t.Fatalf("override responder not used: %v", err)
	}
	// The override applies to every certificate in the chain, so the first
	// handshake asks once for the leaf and once for the intermediate.
	first := p.ocspHits.Load() + p.crlHits.Load()
	for i := 0; i < 3; i++ {
		if err := get(t, p.client(), srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if again := p.ocspHits.Load() + p.crlHits.Load(); again != first {
		t.Fatalf("repeat handshakes fetched again (%d -> %d), want cached", first, again)
	}
	if st := Current(); st.CacheHits == 0 {
		t.Fatalf("cache hits not counted: %+v", st)
	}
}

func (p *pki) leafWithOCSP(url string) (*x509.Certificate, *ecdsa.PrivateKey) {
	p.t.Helper()
	p.mu.Lock()
	p.serial++
	serial := p.serial
	p.mu.Unlock()
	k := newKey(p.t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "upstream.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, OCSPServer: []string{url}}
	return p.sign(tmpl, p.inter, &k.PublicKey, p.interKey), k
}

// A response signed by a key the issuer never certified must not count.
func TestForgedOCSPResponseRejected(t *testing.T) {
	p := newPKI(t)
	leaf, _ := p.leaf(true, false)
	forger := newKey(t)
	der, err := ocsp.CreateResponse(p.inter, p.inter, ocsp.Response{Status: ocsp.Good, IssuerHash: crypto.SHA256, SerialNumber: leaf.SerialNumber,
		ThisUpdate: time.Now(), NextUpdate: time.Now().Add(time.Hour)}, forger)
	if err != nil {
		t.Fatal(err)
	}
	configure(t, ModeHard, "")
	c := current.Load()
	if st, _, err := c.parseOCSP(der, leaf, p.inter); err == nil || st != statusUnknown {
		t.Fatalf("forged response accepted: %v %v", st, err)
	}
	// And a stapled forgery falls through to the real responder.
	p.revoke(leaf)
	if err := c.CheckChain(context.Background(), []*x509.Certificate{leaf, p.inter, p.root}, der); !errors.Is(err, ErrRevoked) {
		t.Fatalf("stapled forgery masked a revocation: %v", err)
	}
}

func TestInsecureSkipVerifyIsNotChecked(t *testing.T) {
	configure(t, ModeHard, "")
	if err := VerifyConnection(tls.ConnectionState{}); err != nil {
		t.Fatalf("unverified connection checked: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"soft": ModeSoft, " HARD ": ModeHard, "off": ModeOff} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseMode("strict"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
