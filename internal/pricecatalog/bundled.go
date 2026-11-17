package pricecatalog

import (
	"crypto/ed25519"
	_ "embed"
	"fmt"
	"sync"
)

// The bundled catalog and its signature, built from source/ with
// `go run ./cmd/janus-price-catalog build` and signed with `… sign`.
//
//go:embed catalog.json
var bundledJSON []byte

//go:embed catalog.json.sig
var bundledSig []byte

// Loaded is a catalog in use, with where it came from.
type Loaded struct {
	*Catalog
	// Origin is "bundled" (built into this binary) or "downloaded".
	Origin string `json:"origin"`
	// URL is where a downloaded catalog came from.
	URL string `json:"url,omitempty"`
	// KeyID is the key that verified the signature; empty when the
	// signature was not checked (JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE=false).
	KeyID string `json:"key_id,omitempty"`
}

var (
	bundledOnce sync.Once
	bundled     *Loaded
	bundledErr  error
)

// Bundled returns the catalog built into the binary, verified against the
// built-in keys. A bundled catalog that does not verify is a build error:
// it is reported and not used.
func Bundled() (*Loaded, error) {
	bundledOnce.Do(func() {
		bundled, bundledErr = loadBundled(BuiltinKeys())
	})
	return bundled, bundledErr
}

func loadBundled(keys []ed25519.PublicKey) (*Loaded, error) {
	id, err := Verify(bundledJSON, bundledSig, keys)
	if err != nil {
		return nil, fmt.Errorf("bundled price catalog: %w", err)
	}
	c, err := Parse(bundledJSON)
	if err != nil {
		return nil, fmt.Errorf("bundled price catalog: %w", err)
	}
	return &Loaded{Catalog: c, Origin: "bundled", KeyID: id}, nil
}
