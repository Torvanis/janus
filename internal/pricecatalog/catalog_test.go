package pricecatalog_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/pricecatalog"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// catalogJSON builds a small valid catalog with the given version.
func catalogJSON(t *testing.T, version string, entries []pricecatalog.Entry, defaults []pricecatalog.Default) []byte {
	t.Helper()
	files := map[string]*pricecatalog.SourceFile{}
	for _, e := range entries {
		if files[e.Provider] == nil {
			files[e.Provider] = &pricecatalog.SourceFile{Provider: e.Provider}
		}
		files[e.Provider].Entries = append(files[e.Provider].Entries, e)
	}
	for _, d := range defaults {
		if files[d.Provider] == nil {
			files[d.Provider] = &pricecatalog.SourceFile{Provider: d.Provider}
		}
		files[d.Provider].Defaults = append(files[d.Provider].Defaults, d)
	}
	var list []pricecatalog.SourceFile
	for _, f := range files {
		list = append(list, *f)
	}
	raw, err := pricecatalog.Build(list, version, "Test", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

var src = pricecatalog.Source{URL: "https://prices.example/page", ReadAt: "2026-11-16"}

func TestEmbeddedCatalogVerifiesWithBuiltInKey(t *testing.T) {
	b, err := pricecatalog.Bundled()
	if err != nil {
		t.Fatalf("the bundled catalog must verify and parse: %v", err)
	}
	if b.Origin != "bundled" || b.KeyID == "" || !strings.HasPrefix(b.Label(), "Janus price catalog ") {
		t.Fatalf("bundled: %+v", b)
	}
	n, _ := b.Counts()
	if n < 100 {
		t.Fatalf("bundled catalog has %d entries", n)
	}
	// Every entry carries its page and the date it was read (Parse
	// enforces it; this guards the loader against a lax change).
	for _, e := range b.Entries {
		if e.Source.URL == "" || e.Source.ReadAt == "" {
			t.Fatalf("%s/%s has no source", e.Provider, e.Model)
		}
	}
	// Providers in common use are covered.
	for _, pm := range [][2]string{{"openai", "gpt-6-astra"}, {"anthropic", "claude-opus-5-5"}, {"google", "gemini-3.8-flash"}, {"xai", "grok-4.7"},
		{"openai", "gpt-5.5-2026-04-23"}, {"anthropic", "claude-sonnet-4-5"}, {"bedrock", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}} {
		if b.Exact(pm[0], pm[1]) == nil {
			t.Errorf("bundled catalog has no %s/%s", pm[0], pm[1])
		}
	}
}

// The bundled catalog is exactly what the source directory builds: nobody
// hand-edits catalog.json without its sources (and a re-sign).
func TestEmbeddedCatalogMatchesSources(t *testing.T) {
	b, err := pricecatalog.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob("source/*.json")
	sort.Strings(names)
	var files []pricecatalog.SourceFile
	for _, n := range names {
		raw, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		var f pricecatalog.SourceFile
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		files = append(files, f)
	}
	at, _ := time.Parse(time.RFC3339, b.GeneratedAt)
	raw, err := pricecatalog.Build(files, b.Version, b.Publisher, at)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile("catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, embedded) {
		t.Fatal("catalog.json differs from a build of source/: rebuild and re-sign it (cmd/janus-price-catalog build, sign)")
	}
}

func TestSignatureVerify(t *testing.T) {
	pub, priv := testKey(t)
	other, _ := testKey(t)
	body := catalogJSON(t, "1", []pricecatalog.Entry{{Provider: "p", Model: "m", Type: "chat", Lines: map[string]int64{"rate_in_nanousd": 1}, Source: src}}, nil)
	sig := pricecatalog.Sign(body, priv)

	if id, err := pricecatalog.Verify(body, sig, []ed25519.PublicKey{pub}); err != nil || id != pricecatalog.KeyID(pub) {
		t.Fatalf("good signature: %q %v", id, err)
	}
	tampered := bytes.Replace(body, []byte(`"rate_in_nanousd": 1`), []byte(`"rate_in_nanousd": 2`), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("tamper did not change the body")
	}
	if _, err := pricecatalog.Verify(tampered, sig, []ed25519.PublicKey{pub}); !errors.Is(err, pricecatalog.ErrBadSignature) {
		t.Fatalf("bad signature: %v", err)
	}
	if _, err := pricecatalog.Verify(body, sig, []ed25519.PublicKey{other}); !errors.Is(err, pricecatalog.ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := pricecatalog.Verify(body, sig, pricecatalog.BuiltinKeys()); !errors.Is(err, pricecatalog.ErrBadSignature) {
		t.Fatalf("a catalog signed by an unknown key verified against the built-in keys: %v", err)
	}
	// An extra key from JANUS_PRICE_CATALOG_KEYS is trusted next to the built-in ones.
	extra, err := pricecatalog.ParseKeys(base64.StdEncoding.EncodeToString(other) + " , " + base64.StdEncoding.EncodeToString(pub))
	if err != nil || len(extra) != 2 {
		t.Fatalf("parse keys: %v", err)
	}
	if _, err := pricecatalog.Verify(body, sig, append(pricecatalog.BuiltinKeys(), extra...)); err != nil {
		t.Fatalf("extra env key: %v", err)
	}
	if _, err := pricecatalog.Verify(body, nil, []ed25519.PublicKey{pub}); !errors.Is(err, pricecatalog.ErrUnsigned) {
		t.Fatalf("unsigned: %v", err)
	}
	if _, err := pricecatalog.ParseKeys("not-a-key"); err == nil {
		t.Fatal("a malformed key was accepted")
	}
	// A seed file signs the same as the full private key.
	seedKey, err := pricecatalog.ParsePrivateKey([]byte(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"))
	if err != nil || !bytes.Equal(pricecatalog.Sign(body, seedKey), sig) {
		t.Fatalf("seed key: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"format": pricecatalog.Format, "version": "1.2", "generated_at": "2026-11-16T00:00:00Z", "publisher": "X",
			"entries": []any{map[string]any{"provider": "p", "model": "m", "type": "chat", "lines": map[string]any{"rate_in_nanousd": 1},
				"source": map[string]any{"url": "https://x.example", "read_at": "2026-11-16"}}},
			"defaults": []any{},
		}
	}
	enc := func(v map[string]any) []byte { b, _ := json.Marshal(v); return b }
	if _, err := pricecatalog.Parse(enc(good())); err != nil {
		t.Fatalf("good catalog: %v", err)
	}
	entry := func(v map[string]any) map[string]any { return v["entries"].([]any)[0].(map[string]any) }
	for name, mutate := range map[string]func(map[string]any){
		"format":       func(v map[string]any) { v["format"] = "other/1" },
		"version":      func(v map[string]any) { v["version"] = "v1" },
		"generated_at": func(v map[string]any) { v["generated_at"] = "yesterday" },
		"no source":    func(v map[string]any) { delete(entry(v), "source") },
		"no read_at":   func(v map[string]any) { entry(v)["source"] = map[string]any{"url": "https://x.example"} },
		"bad line":     func(v map[string]any) { entry(v)["lines"] = map[string]any{"price_banana_nanousd": 1} },
		"bare rate as line with condition": func(v map[string]any) {
			entry(v)["lines"] = map[string]any{"rate_in_nanousd@quality=high": 1}
		},
		"negative":     func(v map[string]any) { entry(v)["lines"] = map[string]any{"rate_in_nanousd": -1} },
		"type":         func(v map[string]any) { entry(v)["type"] = "hologram" },
		"unknown key":  func(v map[string]any) { v["surprise"] = true },
		"duplicate":    func(v map[string]any) { v["entries"] = append(v["entries"].([]any), entry(v)) },
		"upper vendor": func(v map[string]any) { entry(v)["provider"] = "OpenAI" },
	} {
		v := good()
		mutate(v)
		if _, err := pricecatalog.Parse(enc(v)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := pricecatalog.Parse(bytes.Repeat([]byte(" "), pricecatalog.MaxBytes+1)); err == nil {
		t.Error("an oversized catalog was accepted")
	}
}

// Exact model > default for its provider and type > nothing; an exact
// entry replaces the default wholesale; no cross-provider or fuzzy match.
func TestLookupExactThenDefault(t *testing.T) {
	raw := catalogJSON(t, "3", []pricecatalog.Entry{
		{Provider: "openai", Model: "tts-1", Aliases: []string{"tts-1-1106"}, Type: "tts", Lines: map[string]int64{"price_mchar_nanousd": 15e9}, Source: src},
		{Provider: "openai", Model: "gpt-x", Type: "chat", ContextWindow: 1000, Lines: map[string]int64{"rate_in_nanousd": 2e9}, Source: src},
	}, []pricecatalog.Default{
		{Provider: "openai", Type: "tts", Lines: map[string]int64{"price_mchar_nanousd": 30e9, "price_request_nanousd": 1}, Source: src},
	})
	c, err := pricecatalog.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Lookup("openai", "tts-1", "tts")
	if r.Values["price_mchar_nanousd"] != 15e9 || r.Sources["price_mchar_nanousd"].Kind != "model" || len(r.Values) != 1 {
		t.Fatalf("exact: %+v", r)
	}
	if r := c.Lookup("openai", "tts-1-1106", "tts"); r.Values["price_mchar_nanousd"] != 15e9 {
		t.Fatalf("alias: %+v", r)
	}
	r = c.Lookup("openai", "tts-9", "tts")
	if r.Values["price_mchar_nanousd"] != 30e9 || r.Sources["price_mchar_nanousd"].Kind != "default" || r.Sources["price_mchar_nanousd"].Type != "tts" {
		t.Fatalf("default: %+v", r)
	}
	if r := c.Lookup("openai", "gpt-x", "chat"); r.Values["context_window"] != 1000 || r.Sources["rate_in_nanousd"].Catalog != "Test price catalog 3" {
		t.Fatalf("context window and label: %+v", r)
	}
	for _, miss := range [][3]string{{"openai", "gpt-y", "chat"}, {"azure", "tts-1", "tts"}, {"openai", "TTS-1", "chat"}, {"", "tts-1", "tts"}} {
		if r := c.Lookup(miss[0], miss[1], miss[2]); len(r.Values) != 0 {
			t.Errorf("%v matched: %+v", miss, r)
		}
	}
	if got := pricecatalog.ModelType([]string{"response"}); got != "chat" {
		t.Errorf("response modality = %q", got)
	}
	if got := pricecatalog.ModelType([]string{"audio", "stt"}); got != "stt" {
		t.Errorf("first catalog type = %q", got)
	}
}

func TestVersionOrder(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{
		{"2026.11.16.2", "2026.11.16.1", true},
		{"2026.11.17", "2026.11.16.9", true},
		{"2026.11.16", "2026.11.16.0", false},
		{"2026.11.16.1", "2026.11.16.1", false},
		{"2026.2.1", "2026.11.1", false},
		{"x", "1", false},
		{"1", "x", true},
	} {
		if got := pricecatalog.Newer(c.a, c.b); got != c.newer {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
	if _, err := pricecatalog.ParseVersion("01.2"); err == nil {
		t.Error("leading zero accepted")
	}
}
