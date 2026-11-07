// Package pki reads a smart card (CAC/PIV) client certificate: from the TLS
// connection when Janus terminates TLS, or from a header a trusted proxy
// set, and extracts the identities a person can be matched by.
package pki

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Identity is what a card certificate says about its holder.
type Identity struct {
	// UPN is the Microsoft User Principal Name (SAN otherName
	// 1.3.6.1.4.1.311.20.2.3), e.g. 1234567890@mil on a CAC.
	UPN string `json:"upn,omitempty"`
	// Emails are the rfc822Name SANs.
	Emails []string `json:"emails,omitempty"`
	// EDIPI is the DoD ID number from a CAC subject CN
	// LAST.FIRST.MIDDLE.1234567890.
	EDIPI string `json:"edipi,omitempty"`
	// Name is a display name from the CN (First Last) when it has the CAC shape.
	Name    string `json:"name,omitempty"`
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	Serial  string `json:"serial"`
	// NotAfter is the certificate's expiry.
	NotAfter time.Time `json:"not_after"`
}

var oidUPN = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 20, 2, 3}
var oidSAN = asn1.ObjectIdentifier{2, 5, 29, 17}

// ErrNoCertificate means no client certificate was presented.
var ErrNoCertificate = errors.New("no client certificate was presented")

var cacCN = regexp.MustCompile(`^([A-Za-z'\-]+)\.([A-Za-z'\-]+)(?:\.([A-Za-z'\-]+))?\.(\d{10})$`)

// Extract reads the identities in cert.
func Extract(cert *x509.Certificate) Identity {
	id := Identity{Subject: cert.Subject.String(), Issuer: cert.Issuer.String(), Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter}
	for _, e := range cert.EmailAddresses {
		id.Emails = append(id.Emails, strings.ToLower(e))
	}
	id.UPN = upn(cert)
	if m := cacCN.FindStringSubmatch(cert.Subject.CommonName); m != nil {
		id.EDIPI = m[4]
		id.Name = titleWord(m[2]) + " " + titleWord(m[1])
	} else if cert.Subject.CommonName != "" {
		id.Name = cert.Subject.CommonName
	}
	return id
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// upn finds the otherName UPN in the SAN extension.
func upn(cert *x509.Certificate) string {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidSAN) {
			continue
		}
		var seq asn1.RawValue
		if rest, err := asn1.Unmarshal(ext.Value, &seq); err != nil || len(rest) != 0 {
			return ""
		}
		data := seq.Bytes
		for len(data) > 0 {
			var gn asn1.RawValue
			rest, err := asn1.Unmarshal(data, &gn)
			if err != nil {
				return ""
			}
			data = rest
			if gn.Class != asn1.ClassContextSpecific || gn.Tag != 0 {
				continue // otherName is [0]
			}
			var oid asn1.ObjectIdentifier
			inner, err := asn1.Unmarshal(gn.Bytes, &oid)
			if err != nil || !oid.Equal(oidUPN) {
				continue
			}
			var wrapped asn1.RawValue // [0] EXPLICIT
			if _, err := asn1.Unmarshal(inner, &wrapped); err != nil {
				continue
			}
			var s string
			if _, err := asn1.UnmarshalWithParams(wrapped.Bytes, &s, "utf8"); err == nil {
				return strings.ToLower(s)
			}
		}
	}
	return ""
}

// ParseHeader reads a certificate a proxy forwarded. format is xfcc (Envoy
// / Istio X-Forwarded-Client-Cert: Cert="<url-escaped PEM>"), pem
// (URL-escaped or plain PEM, e.g. nginx $ssl_client_escaped_cert) or base64
// (base64 of DER or of PEM). The leaf comes first; any further certificates
// are returned as intermediates.
func ParseHeader(value, format string) (*x509.Certificate, []*x509.Certificate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil, ErrNoCertificate
	}
	var raw []byte
	switch format {
	case "xfcc":
		// The first element (the client's own certificate) is enough;
		// Chain= carries the full chain when the proxy forwards it.
		elem := value
		if i := strings.Index(value, ","); i >= 0 && !strings.Contains(value[:i], "BEGIN") {
			elem = value[:i]
		}
		cert, chain := "", ""
		for _, kv := range splitXFCC(elem) {
			k, v, _ := strings.Cut(kv, "=")
			v = strings.Trim(v, `"`)
			switch strings.ToLower(k) {
			case "cert":
				cert = v
			case "chain":
				chain = v
			}
		}
		if cert == "" && chain == "" {
			return nil, nil, errors.New("the X-Forwarded-Client-Cert header has no Cert= or Chain= element: configure the proxy to forward the client certificate (Envoy: forward_client_cert_details SANITIZE_SET with set_current_client_cert_details cert: true)")
		}
		s, err := url.QueryUnescape(firstNonEmpty(chain, cert))
		if err != nil {
			return nil, nil, fmt.Errorf("decode the forwarded certificate: %w", err)
		}
		raw = []byte(s)
	case "pem":
		s := value
		if !strings.Contains(s, "-----BEGIN") || strings.Contains(s, "%") {
			if u, err := url.QueryUnescape(s); err == nil {
				s = u
			}
		}
		// Some proxies fold the PEM onto one line with spaces.
		if !strings.Contains(s, "\n") {
			s = unfoldPEM(s)
		}
		raw = []byte(s)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, nil, fmt.Errorf("decode the forwarded certificate: %w", err)
		}
		raw = b
	default:
		return nil, nil, fmt.Errorf("unknown certificate header format %q", format)
	}
	certs, err := parseCerts(raw)
	if err != nil {
		return nil, nil, err
	}
	return certs[0], certs[1:], nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func splitXFCC(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ';' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

func unfoldPEM(s string) string {
	re := regexp.MustCompile(`-----BEGIN CERTIFICATE-----(.*?)-----END CERTIFICATE-----`)
	return re.ReplaceAllStringFunc(s, func(block string) string {
		body := strings.TrimSuffix(strings.TrimPrefix(block, "-----BEGIN CERTIFICATE-----"), "-----END CERTIFICATE-----")
		return "-----BEGIN CERTIFICATE-----\n" + strings.Join(strings.Fields(body), "\n") + "\n-----END CERTIFICATE-----\n"
	})
}

func parseCerts(raw []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	if strings.Contains(string(raw), "-----BEGIN") {
		rest := raw
		for {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			if b.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse the forwarded certificate: %w", err)
			}
			out = append(out, c)
		}
	} else {
		cs, err := x509.ParseCertificates(raw)
		if err != nil {
			return nil, fmt.Errorf("parse the forwarded certificate: %w", err)
		}
		out = cs
	}
	if len(out) == 0 {
		return nil, errors.New("the forwarded value holds no certificate")
	}
	return out, nil
}

// Verify builds a chain from leaf to a root in roots (intermediates may
// help) and checks it is valid now for client authentication.
func Verify(leaf *x509.Certificate, intermediates []*x509.Certificate, roots *x509.CertPool, now time.Time) ([][]*x509.Certificate, error) {
	if roots == nil {
		return nil, errors.New("no trusted certificate authorities are configured for smart cards (add the DoD roots or your CA under Settings → Certificates)")
	}
	inter := x509.NewCertPool()
	for _, c := range intermediates {
		inter.AddCert(c)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return nil, fmt.Errorf("the card certificate is not trusted: %w", err)
	}
	return chains, nil
}
