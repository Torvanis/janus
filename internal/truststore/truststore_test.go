package truststore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newCA(t *testing.T, cn string, notAfter time.Time) testCA {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return testCA{cert: c, key: k, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// server starts an HTTPS server for 127.0.0.1 signed by ca.
func (ca testCA) server(t *testing.T) *httptest.Server {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "upstream"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func configure(t *testing.T, o Options) {
	t.Helper()
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	restore, err := Configure(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	t.Cleanup(func() { SetUntrustedAllowed(false); SetUntrustedHosts(nil) })
}

func get(c *http.Client, u string) error {
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func isUnknownAuthority(err error) bool {
	var ua x509.UnknownAuthorityError
	return errors.As(err, &ua) || (err != nil && strings.Contains(err.Error(), "certificate signed by unknown authority"))
}

func TestUploadedCertificateTrustedWithoutRebuild(t *testing.T) {
	ca := newCA(t, "Corp Root", time.Now().Add(365*24*time.Hour))
	srv := ca.server(t)
	var uploads []Uploaded
	configure(t, Options{})
	SetUploadedSource(func(context.Context) ([]Uploaded, error) { return uploads, nil })
	client := &http.Client{Transport: &Transport{}}

	if err := get(client, srv.URL); !isUnknownAuthority(err) {
		t.Fatalf("private CA trusted before upload: %v", err)
	}
	before := Generation()
	uploads = []Uploaded{{ID: "u1", Name: "corp", PEM: ca.pem, CreatedBy: "admin@x"}}
	if err := Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Generation() == before {
		t.Fatal("generation did not change after an upload")
	}
	// The same client (an existing transport) now trusts the CA.
	if err := get(client, srv.URL); err != nil {
		t.Fatalf("uploaded CA not trusted: %v", err)
	}
	st := Current()
	if len(st.CAs) != 1 || st.CAs[0].Source != SourceUploaded || st.CAs[0].ReadOnly || st.CAs[0].Name != "corp" || st.CAs[0].CreatedBy != "admin@x" {
		t.Fatalf("status: %+v", st.CAs)
	}
	// Removal takes effect too, and a no-op reload keeps the generation.
	uploads = nil
	_ = Reload(context.Background())
	g := Generation()
	_ = Reload(context.Background())
	if Generation() != g {
		t.Fatal("generation changed without a certificate change")
	}
	if err := get(client, srv.URL); !isUnknownAuthority(err) {
		t.Fatalf("removed CA still trusted: %v", err)
	}
}

// A Kubernetes ConfigMap mount: files behind ..data symlinks, edited in
// place by kubelet. Bookkeeping entries must not be double-counted.
func TestDirectoryLikeConfigMapMount(t *testing.T) {
	a := newCA(t, "Mounted A", time.Now().Add(365*24*time.Hour))
	b := newCA(t, "Mounted B", time.Now().Add(365*24*time.Hour))
	dir := t.TempDir()
	v1 := filepath.Join(dir, "..2026_11_03_v1")
	if err := os.Mkdir(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1, "a.crt"), []byte(a.pem), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(v1), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/a.crt", filepath.Join(dir, "a.crt")); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("not a cert"), 0o644)
	configure(t, Options{Dir: dir})
	st := Current()
	if len(st.CAs) != 1 || st.CAs[0].Source != SourceDirectory || !st.CAs[0].ReadOnly {
		t.Fatalf("mounted: %+v", st.CAs)
	}
	client := &http.Client{Transport: &Transport{}}
	if err := get(client, a.server(t).URL); err != nil {
		t.Fatalf("mounted CA not trusted: %v", err)
	}
	// kubelet-style update: new versioned dir, ..data flipped, new key.
	v2 := filepath.Join(dir, "..2026_11_03_v2")
	_ = os.Mkdir(v2, 0o755)
	_ = os.WriteFile(filepath.Join(v2, "a.crt"), []byte(a.pem), 0o644)
	_ = os.WriteFile(filepath.Join(v2, "b.crt"), []byte(b.pem), 0o644)
	tmp := filepath.Join(dir, "..data_tmp")
	_ = os.Symlink(filepath.Base(v2), tmp)
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink("..data/b.crt", filepath.Join(dir, "b.crt"))
	_ = os.RemoveAll(v1)
	if err := Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(Current().CAs); n != 2 {
		t.Fatalf("after ConfigMap update: %d certificates, want 2", n)
	}
	if err := get(client, b.server(t).URL); err != nil {
		t.Fatalf("CA added to the ConfigMap not trusted: %v", err)
	}
}

func TestStartupFailsOnBadSources(t *testing.T) {
	if _, err := Configure(Options{Dir: filepath.Join(t.TempDir(), "missing")}); err == nil || !strings.Contains(err.Error(), "JANUS_CA_DIR") {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := Configure(Options{Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("empty dir: %v", err)
	}
	f := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(f, []byte("nope"), 0o644)
	if _, err := Configure(Options{BundleFile: f}); err == nil || !strings.Contains(err.Error(), "JANUS_CA_BUNDLE") {
		t.Fatalf("bad bundle: %v", err)
	}
}

func TestBundleFileAndExtraRoots(t *testing.T) {
	a := newCA(t, "Bundle Root", time.Now().Add(365*24*time.Hour))
	s3ca := newCA(t, "S3 Root", time.Now().Add(365*24*time.Hour))
	f := filepath.Join(t.TempDir(), "bundle.pem")
	_ = os.WriteFile(f, []byte(a.pem), 0o644)
	configure(t, Options{BundleFile: f})
	if err := get(&http.Client{Transport: &Transport{}}, a.server(t).URL); err != nil {
		t.Fatalf("bundle CA not trusted: %v", err)
	}
	s3srv := s3ca.server(t)
	if err := get(&http.Client{Transport: &Transport{}}, s3srv.URL); !isUnknownAuthority(err) {
		t.Fatalf("endpoint CA trusted globally: %v", err)
	}
	extra, _ := ParsePEM([]byte(s3ca.pem))
	if err := get(&http.Client{Transport: &Transport{Extra: extra}}, s3srv.URL); err != nil {
		t.Fatalf("endpoint's own CA not trusted for that client: %v", err)
	}
}

func TestDoDRootsEmbedded(t *testing.T) {
	certs, err := ParsePEM(dodRootsPEM)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DoD Root CA 3": "b107b33f453e5510f68e513110c6f6944bacc263df0137f821c1b3c2f8f863d2",
		"DoD Root CA 4": "559a5189452b13f8233f0022363c06f26e3c517c1d4b77445035959df3244f74",
		"DoD Root CA 5": "1f4ede9dc2a241f6521bf518424acd49ebe84420e69daf5bac57af1f8ee294a9",
		"DoD Root CA 6": "2a5e41aca93df7cc496d2369b7a0a037045d502f1abaaf76975fc07c6660cf93",
	}
	if len(certs) != len(want) {
		t.Fatalf("%d DoD roots embedded, want %d", len(certs), len(want))
	}
	for _, c := range certs {
		d := describe(c, time.Now())
		if want[c.Subject.CommonName] != d.SHA256 || !c.IsCA || c.Subject.String() != c.Issuer.String() {
			t.Fatalf("unexpected DoD root %s %s", c.Subject, d.SHA256)
		}
		if err := c.CheckSignatureFrom(c); err != nil {
			t.Fatalf("%s is not self-signed: %v", c.Subject, err)
		}
	}
	configure(t, Options{DoD: true})
	st := Current()
	if !st.DoD || len(st.CAs) != 4 || st.CAs[0].Source != SourceDoD || !st.CAs[0].ReadOnly {
		t.Fatalf("status: %+v", st)
	}
	configure(t, Options{})
	if Current().DoD || len(Current().CAs) != 0 {
		t.Fatal("DoD roots present when off")
	}
}

func TestUntrustedRouting(t *testing.T) {
	selfSigned := newCA(t, "Lab", time.Now().Add(365*24*time.Hour)).server(t)
	other := newCA(t, "Other", time.Now().Add(365*24*time.Hour)).server(t)
	configure(t, Options{})
	client := &http.Client{Transport: &Transport{}}
	u, _ := url.Parse(selfSigned.URL)
	SetUntrustedHosts([]string{HostPort("https", u.Host)})

	SetUntrustedAllowed(false) // FIPS edition / STIG profile
	if err := get(client, selfSigned.URL); !isUnknownAuthority(err) {
		t.Fatalf("untrusted host skipped verification while refused: %v", err)
	}
	SetUntrustedAllowed(true)
	if err := get(client, selfSigned.URL); err != nil {
		t.Fatalf("allowed untrusted host still verified: %v", err)
	}
	if err := get(client, other.URL); !isUnknownAuthority(err) {
		t.Fatalf("verification skipped for a host that did not opt in: %v", err)
	}
	if HostPort("https", "api.example.com") != "api.example.com:443" || HostPort("http", "x") != "x:80" || HostPort("https", "h:8443") != "h:8443" {
		t.Fatal("HostPort")
	}
}

func TestValidateUpload(t *testing.T) {
	ok := newCA(t, "Good", time.Now().Add(365*24*time.Hour))
	old := newCA(t, "Old", time.Now().Add(-time.Hour))
	if certs, err := ValidateUpload(ok.pem+ok.pem, time.Now()); err != nil || len(certs) != 2 {
		t.Fatalf("chain upload: %v", err)
	}
	if _, err := ValidateUpload(old.pem, time.Now()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired accepted: %v", err)
	}
	if _, err := ValidateUpload("hello", time.Now()); err == nil {
		t.Fatal("non-PEM accepted")
	}
	key := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1}}))
	if _, err := ValidateUpload(ok.pem+key, time.Now()); !errors.Is(err, ErrPrivateKey) {
		t.Fatalf("private key accepted: %v", err)
	}
	if _, err := ValidateUpload(strings.Repeat("x", MaxUploadBytes+1), time.Now()); err == nil {
		t.Fatal("oversized accepted")
	}
}

func TestExpiryFlags(t *testing.T) {
	soon := newCA(t, "Soon", time.Now().Add(10*24*time.Hour))
	later := newCA(t, "Later", time.Now().Add(365*24*time.Hour))
	configure(t, Options{})
	SetUploadedSource(func(context.Context) ([]Uploaded, error) {
		return []Uploaded{{ID: "1", Name: "soon", PEM: soon.pem}, {ID: "2", Name: "later", PEM: later.pem}}, nil
	})
	_ = Reload(context.Background())
	st := Current()
	if st.ExpiringSoon != 1 || st.Expired != 0 {
		t.Fatalf("expiry counts: %+v", st)
	}
	// An expired certificate is flagged (uploads refuse it; a mounted one can age).
	configure(t, Options{Now: func() time.Time { return time.Now().Add(20 * 24 * time.Hour) }})
	SetUploadedSource(func(context.Context) ([]Uploaded, error) {
		return []Uploaded{{ID: "1", Name: "soon", PEM: soon.pem}}, nil
	})
	_ = Reload(context.Background())
	if c := Current().CAs[0]; !c.Expired || c.ExpiresSoon {
		t.Fatalf("expired flag: %+v", c)
	}
}

func TestTransportTuneCannotDropVerification(t *testing.T) {
	srv := newCA(t, "Unknown", time.Now().Add(365*24*time.Hour)).server(t)
	configure(t, Options{})
	tr := &Transport{Tune: func(t *http.Transport) {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test proves this is overridden
	}}
	if err := get(&http.Client{Transport: tr}, srv.URL); !isUnknownAuthority(err) {
		t.Fatalf("Tune disabled verification: %v", err)
	}
}
