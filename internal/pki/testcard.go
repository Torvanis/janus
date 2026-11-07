package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"time"
)

// TestCard is a throwaway certificate authority and a CAC-shaped client
// certificate it issued, for tests and for proving smart-card sign-in
// without a card (scripts/make-test-cac.sh writes the same shape).
type TestCard struct {
	CA      *x509.Certificate
	CAPEM   []byte
	Cert    *x509.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// NewTestCard issues a client certificate with the subject CN
// LAST.FIRST.MIDDLE.<edipi>, a UPN otherName SAN upn and an rfc822 SAN
// email (empty values are left out).
func NewTestCard(cn, upn, email string) (*TestCard, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Janus Test DoD Root CA", Organization: []string{"U.S. Government"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn, OrganizationalUnit: []string{"DoD", "PKI", "USA"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if upn != "" || email != "" {
		san, err := sanWithUPN(upn, email)
		if err != nil {
			return nil, err
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: oidSAN, Value: san}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return &TestCard{
		CA: ca, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		Cert: cert, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func sanWithUPN(upn, email string) ([]byte, error) {
	var names []asn1.RawValue
	if upn != "" {
		utf8, err := asn1.MarshalWithParams(upn, "utf8")
		if err != nil {
			return nil, err
		}
		explicit, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: utf8})
		if err != nil {
			return nil, err
		}
		oid, _ := asn1.Marshal(oidUPN)
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: append(oid, explicit...)})
	}
	if email != "" {
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, Bytes: []byte(email)})
	}
	return asn1.Marshal(names)
}
