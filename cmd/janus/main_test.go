package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var healthz = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
})

// testCert issues a self-signed serving certificate. dns/ips become the
// subjectAltName; notBefore/notAfter set the validity window.
func testCert(t *testing.T, dns []string, ips []net.IP, notBefore, notAfter time.Time) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "janus.example.com"},
		DNSNames:              dns,
		IPAddresses:           ips,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// startTLS serves healthz with cert and writes certPEM to a file the probe
// reads, the way JANUS_TLS_CERT_PATH points at the server's own chain.
func startTLS(t *testing.T, cert tls.Certificate, certPEM []byte) (addr, certPath string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(healthz)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	certPath = filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(srv.URL, "https://"), certPath
}

// TestProbeHealthz locks the --health-probe scheme selection (regression:
// the probe hardcoded http://, so a server configured for direct TLS via
// JANUS_TLS_CERT_PATH/JANUS_TLS_KEY_PATH rejected every probe from its own
// binary and the container crash-looped) and the certificate verification
// that replaced InsecureSkipVerify.
func TestProbeHealthz(t *testing.T) {
	now := time.Now()

	t.Run("plain HTTP listener", func(t *testing.T) {
		srv := httptest.NewServer(healthz)
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		if err := probeHealthz(addr, ""); err != nil {
			t.Fatalf("probe against a plain listener must pass, got: %v", err)
		}
	})

	t.Run("direct TLS listener with a self-signed public-name cert", func(t *testing.T) {
		cert, pemBytes := testCert(t, []string{"janus.example.com"}, nil, now.Add(-time.Hour), now.Add(24*time.Hour))
		addr, certPath := startTLS(t, cert, pemBytes)
		if err := probeHealthz(addr, certPath); err != nil {
			t.Fatalf("probe against a TLS listener must pass when TLS is configured, got: %v", err)
		}
		// The old behavior: an http:// probe against the TLS listener fails.
		if err := probeHealthz(addr, ""); err == nil {
			t.Fatal("an http probe against a TLS-only listener should fail; the scheme selection is not being exercised")
		}
	})

	t.Run("wildcard and IP SAN certs verify", func(t *testing.T) {
		cert, pemBytes := testCert(t, []string{"*.example.com"}, nil, now.Add(-time.Hour), now.Add(time.Hour))
		addr, certPath := startTLS(t, cert, pemBytes)
		if err := probeHealthz(addr, certPath); err != nil {
			t.Fatalf("wildcard cert: %v", err)
		}
		cert, pemBytes = testCert(t, nil, []net.IP{net.ParseIP("10.1.2.3")}, now.Add(-time.Hour), now.Add(time.Hour))
		addr, certPath = startTLS(t, cert, pemBytes)
		if err := probeHealthz(addr, certPath); err != nil {
			t.Fatalf("IP SAN cert: %v", err)
		}
	})

	t.Run("expired serving cert does not fail liveness", func(t *testing.T) {
		cert, pemBytes := testCert(t, []string{"janus.example.com"}, nil, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
		addr, certPath := startTLS(t, cert, pemBytes)
		if err := probeHealthz(addr, certPath); err != nil {
			t.Fatalf("an expired certificate must not crash-loop the pod: %v", err)
		}
	})

	t.Run("a different certificate on the port fails verification", func(t *testing.T) {
		served, servedPEM := testCert(t, []string{"janus.example.com"}, nil, now.Add(-time.Hour), now.Add(time.Hour))
		addr, _ := startTLS(t, served, servedPEM)
		_, configuredPEM := testCert(t, []string{"janus.example.com"}, nil, now.Add(-time.Hour), now.Add(time.Hour))
		other := filepath.Join(t.TempDir(), "other.crt")
		if err := os.WriteFile(other, configuredPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := probeHealthz(addr, other); err == nil {
			t.Fatal("probe accepted a server whose certificate is not the configured one; verification is not happening")
		}
	})

	t.Run("unreadable cert path fails", func(t *testing.T) {
		if err := probeHealthz("127.0.0.1:1", filepath.Join(t.TempDir(), "missing.crt")); err == nil {
			t.Fatal("a missing certificate file must fail the probe")
		}
	})

	t.Run("cert without subjectAltName falls back to a TCP check", func(t *testing.T) {
		cert, pemBytes := testCert(t, nil, nil, now.Add(-time.Hour), now.Add(time.Hour))
		addr, certPath := startTLS(t, cert, pemBytes)
		if err := probeHealthz(addr, certPath); err != nil {
			t.Fatalf("CN-only cert: %v", err)
		}
	})

	t.Run("unhealthy status is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		if err := probeHealthz(addr, ""); err == nil {
			t.Fatal("a non-200 /healthz must fail the probe")
		}
	})
}
