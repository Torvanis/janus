package fipsmode

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// Edition comparison benchmarks: the primitives Janus runs per request and
// per sign-in. Run them in both editions and compare:
//
//	go test -run '^$' -bench . ./internal/fipsmode/
//	GOFIPS140=v1.0.0 go test -tags fips -run '^$' -bench . ./internal/fipsmode/

func BenchmarkSHA256_4KiB(b *testing.B) {
	buf := make([]byte, 4096)
	b.SetBytes(int64(len(buf)))
	for i := 0; i < b.N; i++ {
		sha256.Sum256(buf)
	}
}

func BenchmarkHMACSHA256_256B(b *testing.B) {
	key, msg := make([]byte, 32), make([]byte, 256)
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		m := hmac.New(sha256.New, key)
		m.Write(msg)
		m.Sum(nil)
	}
}

func BenchmarkAESGCMRandomNonce_Seal4KiB(b *testing.B) {
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		b.Fatal(err)
	}
	msg := make([]byte, 4096)
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		aead.Seal(nil, nil, msg, nil)
	}
}

func BenchmarkPBKDF2SHA512_400k(b *testing.B) {
	salt := make([]byte, 16)
	for i := 0; i < b.N; i++ {
		if _, err := pbkdf2.Key(sha512.New, "correct-horse-battery", salt, 400_000, 64); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkECDSAP256Sign(b *testing.B) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	digest := sha256.Sum256([]byte("x"))
	for i := 0; i < b.N; i++ {
		if _, err := ecdsa.SignASN1(rand.Reader, key, digest[:]); err != nil {
			b.Fatal(err)
		}
	}
}

func benchTLSConfig(b testing.TB) (*tls.Config, *tls.Config) {
	b.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bench"}, DNSNames: []string{"bench"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		b.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	srv := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	cli := &tls.Config{RootCAs: pool, ServerName: "bench", MinVersion: tls.VersionTLS12}
	return srv, cli
}

// BenchmarkTLSHandshakeAndTransfer: one full TLS handshake over an in-memory
// pipe (no session resumption) plus a 64 KiB transfer, the cost of a new
// client connection to Janus with direct TLS.
func BenchmarkTLSHandshakeAndTransfer(b *testing.B) {
	srvCfg, cliCfg := benchTLSConfig(b)
	payload := bytes.Repeat([]byte("j"), 64<<10)
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		c1, c2 := net.Pipe()
		done := make(chan error, 1)
		go func() {
			s := tls.Server(c1, srvCfg)
			_, err := io.Copy(io.Discard, s)
			done <- err
			_ = s.Close()
		}()
		c := tls.Client(c2, cliCfg)
		if _, err := c.Write(payload); err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
		<-done
	}
}

// TestTLSNegotiatesApprovedSuite records what the edition actually
// negotiates (TLS 1.3 with an AES-GCM suite and an approved key exchange in
// the FIPS edition).
func TestTLSNegotiatesApprovedSuite(t *testing.T) {
	srvCfg, cliCfg := benchTLSConfig(t)
	c1, c2 := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		s := tls.Server(c1, srvCfg)
		errc <- s.Handshake()
		_ = s.Close()
	}()
	c := tls.Client(c2, cliCfg)
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	st := c.ConnectionState()
	_ = c.Close()
	<-errc
	t.Logf("edition=%s TLS version=%s suite=%s curve=%s", Edition(), tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite), st.CurveID)
	if Active() {
		switch st.CipherSuite {
		case tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384:
		default:
			t.Fatalf("FIPS mode negotiated %s", tls.CipherSuiteName(st.CipherSuite))
		}
	}
}
