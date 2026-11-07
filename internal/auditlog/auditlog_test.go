package auditlog

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

func entry() store.AuditEntry {
	return store.AuditEntry{ID: "a1", Action: "sign_in_failed", ActorLabel: `ev"il]@x`, ActorUserID: "u1", SourceIP: "10.1.2.3",
		ResourceType: "session", Outcome: store.AuditFailure, NewValue: `{"reason":"bad_credentials"}` + "\n", CreatedAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
}

func TestFormatRFC5424AndCEF(t *testing.T) {
	cfg := store.SyslogPolicy{Facility: 10, AppName: "janus-prod", Format: "rfc5424"}
	got := Format(cfg, "pod-1", "2026.11.1", entry())
	want := `<84>1 2026-11-04T12:00:00Z pod-1 janus-prod - audit [janus@32473 id="a1" action="sign_in_failed" outcome="failure" actor="ev\"il\]@x" actorId="u1" ip="10.1.2.3" resourceType="session" resourceId=""] sign_in_failed by ev"il]@x from 10.1.2.3 {"reason":"bad_credentials"} `
	if got != want {
		t.Fatalf("rfc5424:\n got %s\nwant %s", got, want)
	}
	cfg.Format = "cef"
	cef := Format(cfg, "pod-1", "2026.11.1", entry())
	for _, part := range []string{"CEF:0|Janus|janus-prod|2026.11.1|sign_in_failed|sign_in_failed|6|", "outcome=failure", "src=10.1.2.3", `cs3={"reason":"bad_credentials"}\n`} {
		if !strings.Contains(cef, part) {
			t.Errorf("cef lacks %q: %s", part, cef)
		}
	}
	if strings.Contains(cef, "\n") {
		t.Fatal("cef record spans lines")
	}
}

// readFramed reads one octet-counted message ("" on any error; safe in a
// goroutine).
func readFramed(r *bufio.Reader) string {
	n, err := r.ReadString(' ')
	if err != nil {
		return ""
	}
	size, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil {
		return "bad frame length " + n
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return ""
	}
	return string(buf)
}

func TestForwardOverUDPTCPAndTLS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// UDP
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer pc.Close()
	// TCP
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	tcpGot := make(chan string, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(c)
		for {
			m := readFramed(r)
			if m == "" {
				return
			}
			tcpGot <- m
		}
	}()

	udpCfg := store.SyslogPolicy{Enabled: true, Protocol: "udp", Address: pc.LocalAddr().String(), Format: "rfc5424", Facility: 13, AppName: "janus"}
	f := New(nil, "host", "v", func(context.Context) store.SyslogPolicy { return udpCfg })
	go f.Run(ctx)
	f.Enqueue(entry())
	buf := make([]byte, 4096)
	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "<108>1 ") {
		t.Fatalf("udp: %v %q", err, buf[:n])
	}

	tcpCfg := store.SyslogPolicy{Enabled: true, Protocol: "tcp", Address: ln.Addr().String(), Format: "rfc5424", Facility: 10, AppName: "janus"}
	f2 := New(nil, "host", "v", func(context.Context) store.SyslogPolicy { return tcpCfg })
	go f2.Run(ctx)
	f2.Enqueue(entry())
	f2.Enqueue(store.AuditEntry{Action: "sign_in", Outcome: store.AuditSuccess})
	for i, want := range []string{"<84>1 ", "<85>1 "} {
		select {
		case m := <-tcpGot:
			if !strings.HasPrefix(m, want) {
				t.Fatalf("tcp %d: %q", i, m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("tcp: nothing received")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if st := f2.Status(); st.Sent != 2 || st.Destination != "TCP "+ln.Addr().String() {
		t.Fatalf("status = %+v", st)
	}
}

func selfSignedServer(t *testing.T) (tls.Certificate, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "siem.test"}, DNSNames: []string{"siem.test"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	pemBytes := pemCert(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pemBytes
}

func pemCert(der []byte) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + base64Lines(der) + "-----END CERTIFICATE-----\n")
}

func encodeStd(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func base64Lines(b []byte) string {
	s := strings.TrimSpace(encodeStd(b))
	var out strings.Builder
	for len(s) > 64 {
		out.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	out.WriteString(s + "\n")
	return out.String()
}

func TestTLSUsesTheTrustStore(t *testing.T) {
	cert, caPEM := selfSignedServer(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				r := bufio.NewReader(c)
				if err := c.(*tls.Conn).Handshake(); err != nil {
					return
				}
				got <- readFramed(r)
			}(c)
		}
	}()
	cfg := store.SyslogPolicy{Enabled: true, Protocol: "tls", Address: ln.Addr().String(), Format: "rfc5424", Facility: 10, AppName: "janus"}
	f := New(nil, "host", "v", func(context.Context) store.SyslogPolicy { return cfg })

	// Not trusted yet: the test names the fix.
	restore, _ := truststore.Configure(truststore.Options{})
	defer restore()
	if err := f.Test(context.Background(), cfg, entry()); err == nil || !strings.Contains(err.Error(), "Settings → Certificates") {
		t.Fatalf("untrusted SIEM accepted: %v", err)
	}
	// Trusted through a mounted bundle.
	dir := t.TempDir()
	bundle := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(bundle, caPEM, 0o600)
	restore2, err := truststore.Configure(truststore.Options{BundleFile: bundle})
	if err != nil {
		t.Fatal(err)
	}
	defer restore2()
	if err := f.Test(context.Background(), cfg, entry()); err != nil {
		t.Fatalf("trusted SIEM: %v", err)
	}
	select {
	case m := <-got:
		if !strings.Contains(m, `action="sign_in_failed"`) {
			t.Fatalf("tls message %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tls: nothing received")
	}
}

func TestQueueNeverBlocks(t *testing.T) {
	f := New(nil, "h", "v", func(context.Context) store.SyslogPolicy { return store.SyslogPolicy{Enabled: true} })
	start := time.Now()
	for i := 0; i < QueueSize+50; i++ {
		f.Enqueue(entry())
	}
	if time.Since(start) > time.Second || f.Status().Dropped != 50 {
		t.Fatalf("dropped=%d in %s", f.Status().Dropped, time.Since(start))
	}
}
