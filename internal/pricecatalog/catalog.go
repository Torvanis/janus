// Package pricecatalog is the Janus price catalog: known prices per
// provider and model, and defaults per provider and model type, each with
// the page it was read from and the date it was read.
//
// A copy is built into every binary (catalog.json, signed; see
// catalog.json.sig) so pricing works offline. An administrator can opt in to
// a daily refresh from a configurable URL (default DefaultURL); a downloaded
// catalog must carry a valid Ed25519 signature unless the operator turns
// that requirement off with JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE=false.
//
// The catalog is the "reference" source of a model's prices
// (store.AutomaticMetadata.Reference). Precedence for any one price, highest
// first:
//
//  1. the provider's quoted cost of the call (usage.cost, cost_in_usd_ticks…)
//  2. a price an administrator set on the model
//  3. the provider's API price, read at discovery
//  4. the catalog entry for the exact provider and model
//  5. the catalog default for the provider and model type
//  6. nothing: the model is unpriced and measured units are recorded as
//     measured_unpriced, never as free
//
// Only 4 and 5 come from this package, and a model takes its lines from one
// of them: an exact entry replaces the default wholesale, so a default never
// adds a unit price to a model the catalog describes exactly.
package pricecatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/usage"
)

// Format names the catalog file format; a file with another value is refused.
const Format = "janus-price-catalog/1"

// MaxBytes bounds a catalog (and its download).
const MaxBytes = 4 << 20

// ModelTypes are the model types a default can be keyed by.
var ModelTypes = []string{"chat", "image", "video", "tts", "stt", "embedding", "rerank", "moderation"}

// Source says where a price was read and when (YYYY-MM-DD).
type Source struct {
	URL    string `json:"url"`
	ReadAt string `json:"read_at"`
}

// Entry is the price of one provider's model. Lines holds the five token
// rates (rate_in_nanousd, …, nano-USD per million tokens) and price lines
// (usage.ParseLine keys), all in nano-USD.
type Entry struct {
	Provider      string           `json:"provider"`
	Model         string           `json:"model"`
	Aliases       []string         `json:"aliases,omitempty"`
	Type          string           `json:"type"`
	ContextWindow int64            `json:"context_window,omitempty"`
	Lines         map[string]int64 `json:"lines"`
	Source        Source           `json:"source"`
	Note          string           `json:"note,omitempty"`
}

// Default is the price of a provider's model of one type that the catalog
// does not list by name.
type Default struct {
	Provider string           `json:"provider"`
	Type     string           `json:"type"`
	Lines    map[string]int64 `json:"lines"`
	Source   Source           `json:"source"`
	Note     string           `json:"note,omitempty"`
}

// Catalog is one parsed, validated catalog.
type Catalog struct {
	Format      string    `json:"format"`
	Version     string    `json:"version"`
	GeneratedAt string    `json:"generated_at"`
	Publisher   string    `json:"publisher"`
	Entries     []Entry   `json:"entries"`
	Defaults    []Default `json:"defaults"`

	byModel   map[string]*Entry
	byDefault map[string]*Default
}

func modelKey(provider, model string) string { return provider + "\x00" + model }

// Parse reads and validates a catalog. It does not check a signature.
func Parse(raw []byte) (*Catalog, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("price catalog is larger than %d bytes", MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("read price catalog: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Catalog) validate() error {
	if c.Format != Format {
		return fmt.Errorf("price catalog format is %q, want %q", c.Format, Format)
	}
	if _, err := ParseVersion(c.Version); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, c.GeneratedAt); err != nil {
		return fmt.Errorf("price catalog generated_at must be an RFC 3339 time: %w", err)
	}
	if strings.TrimSpace(c.Publisher) == "" {
		return errors.New("price catalog has no publisher")
	}
	c.byModel = map[string]*Entry{}
	c.byDefault = map[string]*Default{}
	for i := range c.Entries {
		e := &c.Entries[i]
		where := fmt.Sprintf("entry %s/%s", e.Provider, e.Model)
		if err := checkCommon(where, e.Provider, e.Type, e.Lines, e.Source); err != nil {
			return err
		}
		if e.Model == "" {
			return fmt.Errorf("entry %d has no model", i)
		}
		if e.ContextWindow < 0 {
			return fmt.Errorf("%s: context_window must not be negative", where)
		}
		for _, name := range append([]string{e.Model}, e.Aliases...) {
			k := modelKey(e.Provider, name)
			if _, dup := c.byModel[k]; dup {
				return fmt.Errorf("%s: %s/%s is listed twice", where, e.Provider, name)
			}
			c.byModel[k] = e
		}
	}
	for i := range c.Defaults {
		d := &c.Defaults[i]
		where := fmt.Sprintf("default %s/%s", d.Provider, d.Type)
		if err := checkCommon(where, d.Provider, d.Type, d.Lines, d.Source); err != nil {
			return err
		}
		k := modelKey(d.Provider, d.Type)
		if _, dup := c.byDefault[k]; dup {
			return fmt.Errorf("%s is listed twice", where)
		}
		c.byDefault[k] = d
	}
	return nil
}

func checkCommon(where, provider, typ string, lines map[string]int64, src Source) error {
	if provider == "" || provider != strings.ToLower(strings.TrimSpace(provider)) {
		return fmt.Errorf("%s: provider must be a lower-case name", where)
	}
	if !validType(typ) {
		return fmt.Errorf("%s: type %q is not one of %s", where, typ, strings.Join(ModelTypes, ", "))
	}
	if len(lines) == 0 {
		return fmt.Errorf("%s has no prices", where)
	}
	for k, v := range lines {
		if !usage.IsTokenRate(k) {
			if _, err := usage.ParseLine(k); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		if v < 0 {
			return fmt.Errorf("%s: %s must not be negative", where, k)
		}
	}
	u, err := url.Parse(src.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s: source url must be an http(s) URL", where)
	}
	if _, err := time.Parse("2006-01-02", src.ReadAt); err != nil {
		return fmt.Errorf("%s: source read_at must be a date (YYYY-MM-DD)", where)
	}
	return nil
}

func validType(t string) bool {
	for _, m := range ModelTypes {
		if m == t {
			return true
		}
	}
	return false
}

// Label names the catalog in source labels: "Janus price catalog 2026.11.16.1".
func (c *Catalog) Label() string { return c.Publisher + " price catalog " + c.Version }

// Counts is how many entries and defaults the catalog holds.
func (c *Catalog) Counts() (entries, defaults int) { return len(c.Entries), len(c.Defaults) }

// Exact returns the entry for this provider's model (by name or alias).
func (c *Catalog) Exact(provider, model string) *Entry { return c.byModel[modelKey(provider, model)] }

// DefaultFor returns the default for a provider's model type.
func (c *Catalog) DefaultFor(provider, typ string) *Default {
	return c.byDefault[modelKey(provider, typ)]
}

// LineSource says where one resolved value came from.
type LineSource struct {
	Catalog string `json:"catalog"`        // the catalog's label
	Kind    string `json:"kind"`           // "model" or "default"
	Type    string `json:"type,omitempty"` // the model type a default is for
	URL     string `json:"url,omitempty"`  // the page the price was read from
	ReadAt  string `json:"read_at,omitempty"`
}

// Resolved is what the catalog says about one model.
type Resolved struct {
	Values  map[string]int64
	Sources map[string]LineSource
}

// Lookup resolves a model's prices: the exact entry when the catalog has
// one, else the default for its provider and type, else nothing.
func (c *Catalog) Lookup(provider, model, typ string) Resolved {
	out := Resolved{Values: map[string]int64{}, Sources: map[string]LineSource{}}
	if c == nil || provider == "" {
		return out
	}
	if e := c.Exact(provider, model); e != nil {
		src := LineSource{Catalog: c.Label(), Kind: "model", URL: e.Source.URL, ReadAt: e.Source.ReadAt}
		for k, v := range e.Lines {
			out.Values[k], out.Sources[k] = v, src
		}
		if e.ContextWindow > 0 {
			out.Values["context_window"], out.Sources["context_window"] = e.ContextWindow, src
		}
		return out
	}
	if d := c.DefaultFor(provider, typ); d != nil {
		src := LineSource{Catalog: c.Label(), Kind: "default", Type: typ, URL: d.Source.URL, ReadAt: d.Source.ReadAt}
		for k, v := range d.Lines {
			out.Values[k], out.Sources[k] = v, src
		}
	}
	return out
}

// ModelType reads a model's type from its discovered modalities: the first
// that names a catalog type ("response" counts as chat). Empty when none do.
func ModelType(modalities []string) string {
	for _, m := range modalities {
		if m == "response" {
			return "chat"
		}
		if validType(m) {
			return m
		}
	}
	return ""
}

// Version is a parsed catalog version: dot-separated numbers such as
// 2026.11.16.1, compared number by number.
type Version []int64

// ParseVersion reads a version.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 6 {
		return nil, fmt.Errorf("price catalog version %q must be up to six dot-separated numbers", s)
	}
	v := make(Version, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != p {
			return nil, fmt.Errorf("price catalog version %q must be up to six dot-separated numbers", s)
		}
		v[i] = n
	}
	return v, nil
}

// Compare returns -1, 0 or 1 (missing trailing parts count as zero).
func (v Version) Compare(o Version) int {
	for i := 0; i < len(v) || i < len(o); i++ {
		var a, b int64
		if i < len(v) {
			a = v[i]
		}
		if i < len(o) {
			b = o[i]
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Newer reports whether version a is newer than b. Unparseable versions are
// never newer.
func Newer(a, b string) bool {
	va, err := ParseVersion(a)
	if err != nil {
		return false
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return true
	}
	return va.Compare(vb) > 0
}

// SourceFile is one file of a catalog source directory: a provider's
// entries and defaults, without the catalog header.
type SourceFile struct {
	Provider string    `json:"provider"`
	Entries  []Entry   `json:"entries"`
	Defaults []Default `json:"defaults,omitempty"`
}

// Build assembles a catalog from source files: entries and defaults sorted,
// the header stamped, the result validated. The output is deterministic for
// the same inputs.
func Build(files []SourceFile, version, publisher string, generatedAt time.Time) ([]byte, error) {
	c := Catalog{Format: Format, Version: version, Publisher: publisher, GeneratedAt: generatedAt.UTC().Format(time.RFC3339)}
	for _, f := range files {
		for _, e := range f.Entries {
			if e.Provider != f.Provider {
				return nil, fmt.Errorf("source %s lists a model of provider %q", f.Provider, e.Provider)
			}
			c.Entries = append(c.Entries, e)
		}
		for _, d := range f.Defaults {
			if d.Provider != f.Provider {
				return nil, fmt.Errorf("source %s lists a default of provider %q", f.Provider, d.Provider)
			}
			c.Defaults = append(c.Defaults, d)
		}
	}
	sort.SliceStable(c.Entries, func(i, j int) bool {
		a, b := c.Entries[i], c.Entries[j]
		return a.Provider < b.Provider || a.Provider == b.Provider && a.Model < b.Model
	})
	sort.SliceStable(c.Defaults, func(i, j int) bool {
		a, b := c.Defaults[i], c.Defaults[j]
		return a.Provider < b.Provider || a.Provider == b.Provider && a.Type < b.Type
	})
	if c.Defaults == nil {
		c.Defaults = []Default{}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
