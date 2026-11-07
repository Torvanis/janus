package listenertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memDB is a shared "database" so two providers act like two replicas.
type memDB struct {
	mu  sync.Mutex
	raw string
}

func (m *memDB) persist() Persistence {
	enc := func(s string) (string, error) { return "enc:" + base64.StdEncoding.EncodeToString([]byte(s)), nil }
	dec := func(s string) (string, error) {
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "enc:"))
		return string(b), err
	}
	return Persistence{
		Load: func(context.Context) (*Stored, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.raw == "" {
				return nil, nil
			}
			return UnmarshalStored(m.raw)
		},
		Save: func(_ context.Context, s *Stored) error {
			raw, err := MarshalStored(s)
			m.mu.Lock()
			m.raw = raw
			m.mu.Unlock()
			return err
		},
		Encrypt: enc, Decrypt: dec,
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve starts a TLS listener with p and returns its address.
func serve(t *testing.T, p *Provider) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", p.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// trusting returns a client that trusts exactly pemText (what a proxy
// does with the downloaded certificate).
func trusting(t *testing.T, pemText string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pemText)) {
		t.Fatal("downloaded certificate is not PEM")
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func get(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestSelfSignedGeneratedStoredAndTrustedByDownload(t *testing.T) {
	db := &memDB{}
	p, err := New(context.Background(), Options{SelfSigned: true, Names: []string{"janus.janus.svc", "127.0.0.1"}, Days: 365, Persist: db.persist(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	info := p.Info()
	if info.Mode != ModeSelfSigned || info.GeneratedBy != "startup" || len(info.Names) != 2 {
		t.Fatalf("info: %+v", info)
	}
	if d := time.Until(info.NotAfter); d < 364*24*time.Hour || d > 366*24*time.Hour {
		t.Fatalf("lifetime %v, want about 365 days", d)
	}
	// The stored key is encrypted, never plain PEM.
	if strings.Contains(db.raw, "PRIVATE KEY") {
		t.Fatal("private key stored unencrypted")
	}
	// A proxy that trusts the downloaded certificate verifies the listener.
	addr := serve(t, p)
	dl := p.CertificatePEM()
	if strings.Contains(dl, "PRIVATE") {
		t.Fatal("download contains a private key")
	}
	if err := get(trusting(t, dl), "https://"+addr); err != nil {
		t.Fatalf("proxy trusting the download cannot verify: %v", err)
	}
	// A client without it does not.
	plain := &http.Client{Timeout: 5 * time.Second}
	if err := get(plain, "https://"+addr); err == nil {
		t.Fatal("self-signed certificate verified without being trusted")
	}
}

func TestSelfSignedSharedAcrossReplicasAndRegenerated(t *testing.T) {
	db := &memDB{}
	a, err := New(context.Background(), Options{SelfSigned: true, Names: []string{"127.0.0.1"}, Persist: db.persist(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(context.Background(), Options{SelfSigned: true, Names: []string{"127.0.0.1"}, Persist: db.persist(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Info().SHA256 != b.Info().SHA256 {
		t.Fatal("replicas serve different self-signed certificates")
	}
	addrB := serve(t, b)
	old := a.CertificatePEM()
	info, err := a.Regenerate(context.Background(), []string{"127.0.0.1", "janus.example.mil"}, 30, "admin@example.mil")
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 == "" || info.GeneratedBy != "admin@example.mil" || info.ExpiresSoon != true {
		t.Fatalf("regenerated: %+v", info) // 30 days is inside the 30-day warning window boundary
	}
	// Replica b follows after Refresh, and serves the new certificate.
	if err := b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.Info().SHA256 != info.SHA256 {
		t.Fatal("replica b did not pick up the regenerated certificate")
	}
	if err := get(trusting(t, old), "https://"+addrB); err == nil {
		t.Fatal("old certificate still trusted after regeneration")
	}
	if err := get(trusting(t, a.CertificatePEM()), "https://"+addrB); err != nil {
		t.Fatalf("new certificate not served: %v", err)
	}
	if _, err := a.Regenerate(context.Background(), []string{"bad name!"}, 30, "x"); err == nil {
		t.Fatal("invalid name accepted")
	}
	if _, err := a.Regenerate(context.Background(), nil, 2000, "x"); err == nil {
		t.Fatal("over-long lifetime accepted")
	}
}

func writePair(t *testing.T, dir string, names []string, life time.Duration) (string, string) {
	t.Helper()
	certPEM, keyPEM, err := Generate(names, life, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(k+".tmp", []byte(keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c+".tmp", []byte(certPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Rename(k+".tmp", k)
	_ = os.Rename(c+".tmp", c)
	return c, k
}

// A mounted certificate (cert-manager) is re-read when it changes: the
// renewed certificate is served without a restart.
func TestMountedCertificateReloads(t *testing.T) {
	dir := t.TempDir()
	c, k := writePair(t, dir, []string{"127.0.0.1"}, 48*time.Hour)
	p, err := New(context.Background(), Options{CertPath: c, KeyPath: k, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Info().Mode != ModeMounted || p.Info().CertPath != c {
		t.Fatalf("info: %+v", p.Info())
	}
	addr := serve(t, p)
	first := p.CertificatePEM()
	if err := get(trusting(t, first), "https://"+addr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	writePair(t, dir, []string{"127.0.0.1"}, 90*24*time.Hour)
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(c, future, future)
	p.mu.Lock()
	p.checked = time.Time{} // skip the 10-second throttle
	p.mu.Unlock()
	renewed, _ := os.ReadFile(c)
	if err := get(trusting(t, string(renewed)), "https://"+addr); err != nil {
		t.Fatalf("renewed certificate not served: %v", err)
	}
	if p.CertificatePEM() == first {
		t.Fatal("provider still reports the old certificate")
	}
	// A broken renewal keeps serving the last good certificate.
	_ = os.WriteFile(c, []byte("garbage"), 0o644)
	future = future.Add(time.Second)
	_ = os.Chtimes(c, future, future)
	p.mu.Lock()
	p.checked = time.Time{}
	p.mu.Unlock()
	if err := get(trusting(t, string(renewed)), "https://"+addr); err != nil {
		t.Fatalf("broken renewal took the listener down: %v", err)
	}
	if p.Info().LastError == "" {
		t.Fatal("reload failure not reported")
	}
	if _, err := p.Regenerate(context.Background(), nil, 0, "x"); err != ErrNotSelfSigned {
		t.Fatalf("regenerate on a mounted certificate: %v", err)
	}
}

func TestGenerate(t *testing.T) {
	certPEM, keyPEM, err := Generate([]string{"janus", "janus.ns.svc.cluster.local", "10.0.0.5", "*.janus.example.mil"}, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if c.IsCA || len(c.DNSNames) != 3 || len(c.IPAddresses) != 1 || !c.IPAddresses[0].Equal(net.ParseIP("10.0.0.5")) ||
		c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || c.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("certificate: %+v", c)
	}
	// Self-signed leaf (not a CA): the signature verifies under its own key.
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil || c.Subject.String() != c.Issuer.String() {
		t.Fatalf("not self-signed: %v", err)
	}
	if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Generate(nil, time.Hour, time.Now()); err == nil {
		t.Fatal("no names accepted")
	}
}

func TestNoneMode(t *testing.T) {
	p, err := New(context.Background(), Options{Logger: quiet()})
	if err != nil || p.Enabled() || p.Info().Mode != ModeNone {
		t.Fatalf("none: %v %+v", err, p.Info())
	}
	if _, err := New(context.Background(), Options{CertPath: "/nope", KeyPath: "/nope", Logger: quiet()}); err == nil {
		t.Fatal("missing mounted pair accepted")
	}
}
