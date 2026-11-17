package pricecatalog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Signatures.
//
// A catalog is signed with Ed25519 over its exact bytes. The signature is a
// detached file next to the catalog: catalog.json.sig holds the base64
// (standard encoding) of the 64-byte signature, and may end in a newline.
// A mirror copies both files unchanged; Janus fetches <url> and <url>.sig.
//
// Trusted keys are the built-in Janus key(s) below plus any the operator
// adds with JANUS_PRICE_CATALOG_KEYS (base64 Ed25519 public keys, comma
// separated), so an internal provider can publish a catalog signed with its
// own key.

// builtinKeys are the Janus price-catalog public keys (base64, 32 bytes).
//
// DEV KEY: generated 2026-11-16 for development; its private half is not in
// the repository. Replace it with the owner's release key before a public
// release, and re-sign catalog.json with that key.
var builtinKeys = []string{
	"FU/XAvq0WqnoJRmYUtn63XQaEBsbeXGaZ2HZAFCJQUI=",
}

// ErrUnsigned means a catalog came with no signature.
var ErrUnsigned = errors.New("the price catalog is not signed")

// ErrBadSignature means no trusted key verifies the signature.
var ErrBadSignature = errors.New("the price catalog signature does not verify with any trusted key")

// ParseKey reads one base64 Ed25519 public key.
func ParseKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("a price catalog key must be a base64 Ed25519 public key (%d bytes)", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParseKeys reads a comma-separated list (JANUS_PRICE_CATALOG_KEYS).
func ParseKeys(list string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, err := ParseKey(part)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// BuiltinKeys returns the keys built into this binary.
func BuiltinKeys() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, 0, len(builtinKeys))
	for _, s := range builtinKeys {
		k, err := ParseKey(s)
		if err != nil {
			panic("pricecatalog: bad built-in key: " + err.Error())
		}
		out = append(out, k)
	}
	return out
}

// KeyID is a short fingerprint of a key: the first 8 bytes of its SHA-256,
// in hex.
func KeyID(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:8])
}

// Sign returns the detached signature text for body.
func Sign(body []byte, key ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, body)) + "\n")
}

// Verify checks a detached signature over body against the trusted keys and
// returns the id of the key that verified it.
func Verify(body, sig []byte, keys []ed25519.PublicKey) (string, error) {
	text := strings.TrimSpace(string(sig))
	if text == "" {
		return "", ErrUnsigned
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", ErrBadSignature
	}
	for _, k := range keys {
		if ed25519.Verify(k, body, raw) {
			return KeyID(k), nil
		}
	}
	return "", ErrBadSignature
}

// ParsePrivateKey reads a signing key file: the base64 of the 64-byte
// Ed25519 private key or of its 32-byte seed.
func ParsePrivateKey(text []byte) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		return nil, errors.New("the signing key must be base64")
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, fmt.Errorf("the signing key must be %d or %d bytes", ed25519.SeedSize, ed25519.PrivateKeySize)
}
