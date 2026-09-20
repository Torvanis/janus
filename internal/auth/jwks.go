package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// supportedAlgsMessage names every ID-token signing algorithm Janus accepts.
// Keep it in sync with verifyIDTokenSignature and the README.
const supportedAlgsMessage = "RS256, RS384, RS512, ES256, ES384, or ES512"

// jwksCache fetches and caches the identity provider's signing keys (RSA and
// ECDSA) so ID token signatures can be verified without a network round trip
// per sign-in. EdDSA (Ed25519) keys are not supported; tokens signed with any
// other algorithm are rejected (fail closed).
type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey // *rsa.PublicKey or *ecdsa.PublicKey, keyed by kid; "" holds a single un-labelled key
	fetchedAt time.Time
	ttl       time.Duration
}

func newJWKSCache(url string, client *http.Client) *jwksCache {
	return &jwksCache{
		url:    url,
		client: client,
		now:    func() time.Time { return time.Now().UTC() },
		ttl:    15 * time.Minute,
	}
}

// key returns the public key for kid, refreshing the key set when the kid is
// unknown or the cache is stale (providers rotate keys without notice).
func (c *jwksCache) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.lookupLocked(kid); k != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return k, nil
	}
	if err := c.refreshLocked(ctx); err != nil {
		// A stale key beats no key when the provider is briefly unreachable.
		if k := c.lookupLocked(kid); k != nil {
			return k, nil
		}
		return nil, err
	}
	if k := c.lookupLocked(kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("the identity provider's key set has no key matching the ID token (kid %q)", kid)
}

func (c *jwksCache) lookupLocked(kid string) crypto.PublicKey {
	if k, ok := c.keys[kid]; ok {
		return k
	}
	// A token without a kid is acceptable only when the provider publishes
	// exactly one signing key.
	if kid == "" && len(c.keys) == 1 {
		for _, k := range c.keys {
			return k
		}
	}
	return nil
}

func (c *jwksCache) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("build JWKS request for %s: %w", c.url, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch the identity provider's signing keys from %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the identity provider's JWKS endpoint %s returned HTTP %d", c.url, resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			// RSA
			N string `json:"n"`
			E string `json:"e"`
			// EC
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("parse JWKS document from %s: %w", c.url, err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
			if err != nil {
				continue
			}
			eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
			if err != nil {
				continue
			}
			e := 0
			for _, b := range eBytes {
				e = e<<8 | int(b)
			}
			if e <= 1 {
				continue
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				continue
			}
			xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil {
				continue
			}
			yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
			if err != nil {
				continue
			}
			// A JWK carries X and Y as fixed-width big-endian coordinates; the
			// SEC 1 uncompressed encoding is 0x04 || X || Y at the curve's
			// byte size. Parsing that way (rather than building the struct
			// from raw big.Ints) performs the on-curve check and rejects
			// malformed points — the same outcome as before, on the API the
			// standard library keeps supported.
			size := (curve.Params().BitSize + 7) / 8
			if len(xBytes) != size || len(yBytes) != size {
				continue
			}
			point := make([]byte, 0, 1+2*size)
			point = append(point, 0x04)
			point = append(point, xBytes...)
			point = append(point, yBytes...)
			pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
			if err != nil {
				continue
			}
			keys[k.Kid] = pub
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("the identity provider's JWKS document at %s contains no usable signing keys (Janus supports RSA and EC keys; token algorithms %s)", c.url, supportedAlgsMessage)
	}
	c.keys = keys
	c.fetchedAt = c.now()
	return nil
}

// verifyIDTokenSignature checks the JWT's signature against the provider's
// published keys. Supported algorithms: RS256/RS384/RS512 (RSA PKCS#1 v1.5)
// and ES256/ES384/ES512 (ECDSA). Any other algorithm — including "none" and
// EdDSA — is rejected outright.
func (c *jwksCache) verifyIDTokenSignature(ctx context.Context, idToken string) error {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return fmt.Errorf("the ID token is not a well-formed JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode ID token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("parse ID token header: %w", err)
	}
	var hash crypto.Hash
	switch header.Alg {
	case "RS256", "ES256":
		hash = crypto.SHA256
	case "RS384", "ES384":
		hash = crypto.SHA384
	case "RS512", "ES512":
		hash = crypto.SHA512
	default:
		return fmt.Errorf("the ID token uses unsupported signing algorithm %q; Janus accepts %s", header.Alg, supportedAlgsMessage)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode ID token signature: %w", err)
	}
	key, err := c.key(ctx, header.Kid)
	if err != nil {
		return err
	}
	signed := []byte(parts[0] + "." + parts[1])
	var digest []byte
	switch hash {
	case crypto.SHA256:
		sum := sha256.Sum256(signed)
		digest = sum[:]
	case crypto.SHA384:
		sum := sha512.Sum384(signed)
		digest = sum[:]
	case crypto.SHA512:
		sum := sha512.Sum512(signed)
		digest = sum[:]
	}
	invalid := fmt.Errorf("the ID token signature is invalid: it was not signed by the configured identity provider")
	switch strings.HasPrefix(header.Alg, "RS") {
	case true:
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("the ID token is signed with %s but the matching provider key is not an RSA key", header.Alg)
		}
		if err := rsa.VerifyPKCS1v15(rsaKey, hash, digest, sig); err != nil {
			return invalid
		}
	default: // ES*
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("the ID token is signed with %s but the matching provider key is not an EC key", header.Alg)
		}
		// RFC 7518 §3.4 pins each ES* algorithm to one curve.
		wantBits := map[string]int{"ES256": 256, "ES384": 384, "ES512": 521}[header.Alg]
		if ecKey.Curve.Params().BitSize != wantBits {
			return fmt.Errorf("the ID token is signed with %s but the matching provider key uses curve %s", header.Alg, ecKey.Curve.Params().Name)
		}
		// JOSE encodes ECDSA signatures as r||s with fixed per-curve widths.
		size := (ecKey.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return invalid
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(ecKey, digest, r, s) {
			return invalid
		}
	}
	return nil
}
