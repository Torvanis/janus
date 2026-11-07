package pki

import (
	"crypto/x509"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExtractCACIdentity(t *testing.T) {
	c, err := NewTestCard("DOE.JANE.Q.1234567890", "1234567890@mil", "jane.q.doe.civ@mail.mil")
	if err != nil {
		t.Fatal(err)
	}
	id := Extract(c.Cert)
	if id.UPN != "1234567890@mil" || id.EDIPI != "1234567890" || id.Name != "Jane Doe" || len(id.Emails) != 1 || id.Emails[0] != "jane.q.doe.civ@mail.mil" {
		t.Fatalf("identity = %+v", id)
	}
	plain, _ := NewTestCard("Server Thing", "", "")
	if id := Extract(plain.Cert); id.EDIPI != "" || id.UPN != "" || id.Name != "Server Thing" {
		t.Fatalf("non-CAC = %+v", id)
	}
}

func TestParseForwardedCertificate(t *testing.T) {
	c, _ := NewTestCard("DOE.JOHN.1234567891", "1234567891@mil", "")
	esc := url.QueryEscape(string(c.CertPEM))
	for name, tc := range map[string]struct{ value, format string }{
		"xfcc":          {`By=spiffe://cluster/ns/janus;Hash=abc;Cert="` + esc + `";Subject="CN=x";URI=`, "xfcc"},
		"nginx escaped": {esc, "pem"},
		"plain pem":     {string(c.CertPEM), "pem"},
		"folded pem":    {strings.ReplaceAll(strings.TrimSpace(string(c.CertPEM)), "\n", " "), "pem"},
		"base64 der":    {base64.StdEncoding.EncodeToString(c.Cert.Raw), "base64"},
		"base64 of pem": {base64.StdEncoding.EncodeToString(c.CertPEM), "base64"},
	} {
		leaf, _, err := ParseHeader(tc.value, tc.format)
		if err != nil || leaf.SerialNumber.Cmp(c.Cert.SerialNumber) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := ParseHeader("", "xfcc"); err != ErrNoCertificate {
		t.Fatalf("empty: %v", err)
	}
	if _, _, err := ParseHeader(`By=x;Hash=y`, "xfcc"); err == nil || !strings.Contains(err.Error(), "forward_client_cert_details") {
		t.Fatalf("xfcc without Cert: %v", err)
	}
}

func TestVerifyAgainstConfiguredAuthoritiesOnly(t *testing.T) {
	c, _ := NewTestCard("DOE.JANE.1234567890", "1234567890@mil", "")
	other, _ := NewTestCard("EVIL.MALLORY.1111111111", "1234567890@mil", "")
	roots := x509.NewCertPool()
	roots.AddCert(c.CA)
	if _, err := Verify(c.Cert, nil, roots, time.Now()); err != nil {
		t.Fatalf("own CA: %v", err)
	}
	if _, err := Verify(other.Cert, nil, roots, time.Now()); err == nil {
		t.Fatal("a card from another CA was accepted")
	}
	if _, err := Verify(c.Cert, nil, roots, time.Now().Add(48*time.Hour)); err == nil {
		t.Fatal("expired card accepted")
	}
	if _, err := Verify(c.Cert, nil, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "Settings → Certificates") {
		t.Fatalf("no CAs: %v", err)
	}
}
