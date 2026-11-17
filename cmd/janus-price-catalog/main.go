// Command janus-price-catalog builds, signs and verifies Janus price
// catalogs. The daily validation job that publishes the catalog (to
// janusedge.com, or an internal mirror) runs:
//
//	janus-price-catalog build  -src internal/pricecatalog/source -version 2026.11.17.1 -out catalog.json
//	janus-price-catalog sign   -key /path/to/signing.key -in catalog.json          # writes catalog.json.sig
//	janus-price-catalog verify -in catalog.json [-keys <base64>,…]                 # built-in keys + -keys
//
// keygen writes a new signing key (mode 0600) and prints its public key,
// which gateways trust through JANUS_PRICE_CATALOG_KEYS.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/pricecatalog"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "janus-price-catalog:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: janus-price-catalog build|sign|verify|keygen [flags]")
	}
	switch args[0] {
	case "build":
		return build(args[1:])
	case "sign":
		return sign(args[1:])
	case "verify":
		return verify(args[1:])
	case "keygen":
		return keygen(args[1:])
	}
	return fmt.Errorf("unknown command %q (build, sign, verify, keygen)", args[0])
}

func build(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	src := fs.String("src", "internal/pricecatalog/source", "directory of per-provider source files (*.json)")
	version := fs.String("version", "", "catalog version, dot-separated numbers (e.g. 2026.11.17.1)")
	publisher := fs.String("publisher", "Janus", "publisher named in source labels")
	at := fs.String("generated-at", "", "RFC 3339 time (default now)")
	out := fs.String("out", "catalog.json", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	when := time.Now().UTC().Truncate(time.Second)
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("-generated-at: %w", err)
		}
		when = t
	}
	files, err := ReadSources(*src)
	if err != nil {
		return err
	}
	raw, err := pricecatalog.Build(files, *version, *publisher, when)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	c, _ := pricecatalog.Parse(raw)
	n, d := c.Counts()
	fmt.Printf("built %s: version %s, %d models, %d defaults\n", *out, c.Version, n, d)
	return nil
}

// ReadSources reads every *.json file of a source directory, in name order.
func ReadSources(dir string) ([]pricecatalog.SourceFile, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no *.json source files in %s", dir)
	}
	sort.Strings(names)
	var out []pricecatalog.SourceFile
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var f pricecatalog.SourceFile
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, f)
	}
	return out, nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "signing key file (base64 Ed25519 private key or seed)")
	in := fs.String("in", "catalog.json", "catalog to sign")
	out := fs.String("out", "", "signature file (default <in>.sig)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" {
		return errors.New("-key is required")
	}
	keyText, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	key, err := pricecatalog.ParsePrivateKey(keyText)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	if _, err := pricecatalog.Parse(body); err != nil {
		return fmt.Errorf("refusing to sign an invalid catalog: %w", err)
	}
	if *out == "" {
		*out = *in + ".sig"
	}
	if err := os.WriteFile(*out, pricecatalog.Sign(body, key), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s with key %s -> %s\n", *in, pricecatalog.KeyID(key.Public().(ed25519.PublicKey)), *out)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := fs.String("in", "catalog.json", "catalog to verify")
	sigPath := fs.String("sig", "", "signature file (default <in>.sig)")
	extra := fs.String("keys", "", "extra trusted public keys, base64, comma-separated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sigPath == "" {
		*sigPath = *in + ".sig"
	}
	body, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(*sigPath)
	if err != nil {
		return err
	}
	keys, err := pricecatalog.ParseKeys(*extra)
	if err != nil {
		return err
	}
	id, err := pricecatalog.Verify(body, sig, append(pricecatalog.BuiltinKeys(), keys...))
	if err != nil {
		return err
	}
	c, err := pricecatalog.Parse(body)
	if err != nil {
		return err
	}
	n, d := c.Counts()
	fmt.Printf("ok: %s, signed by key %s, %d models, %d defaults\n", c.Label(), id, n, d)
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to write the private key to (mode 0600; must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv) + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("public key: %s (id %s)\n", base64.StdEncoding.EncodeToString(pub), pricecatalog.KeyID(pub))
	return nil
}
