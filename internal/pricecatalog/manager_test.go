package pricecatalog_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/pricecatalog"
	"github.com/torvanis/janus/internal/store"
)

// feed is a fake catalog host: /catalog.json and /catalog.json.sig, with
// ETag support and request counting.
type feed struct {
	mu      sync.Mutex
	body    []byte
	sig     []byte
	etag    string
	hits    atomic.Int32
	sigHits atomic.Int32
	headers http.Header
}

func (f *feed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/catalog.json":
		f.hits.Add(1)
		f.headers = r.Header.Clone()
		if f.etag != "" && r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		_, _ = w.Write(f.body)
	case "/catalog.json.sig":
		f.sigHits.Add(1)
		if f.sig == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.sig)
	default:
		http.NotFound(w, r)
	}
}

func (f *feed) set(body, sig []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.sig, f.etag = body, sig, etag
}

type rig struct {
	t     *testing.T
	store *store.Store
	feed  *feed
	srv   *httptest.Server
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newRig(t *testing.T) *rig {
	t.Helper()
	s, err := store.OpenMigratedSQLite(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &feed{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	pub, priv := testKey(t)
	return &rig{t: t, store: s, feed: f, srv: srv, pub: pub, priv: priv}
}

func (r *rig) url() string { return r.srv.URL + "/catalog.json" }

func (r *rig) manager(cfg pricecatalog.Config) *pricecatalog.Manager {
	if cfg.ExtraKeys == nil {
		cfg.ExtraKeys = []ed25519.PublicKey{r.pub}
	}
	m := pricecatalog.NewManager(cfg, r.store, r.srv.Client(), nil)
	return m
}

func on() *bool  { v := true; return &v }
func off() *bool { v := false; return &v }

// A catalog newer than the bundled one, pricing gpt-4o differently and
// adding a default for OpenAI speech.
func (r *rig) catalog(version string, gpt4oIn int64) []byte {
	return catalogJSON(r.t, version, []pricecatalog.Entry{
		{Provider: "openai", Model: "gpt-4o", Type: "chat", Lines: map[string]int64{"rate_in_nanousd": gpt4oIn, "rate_out_nanousd": 10e9}, Source: src},
	}, []pricecatalog.Default{
		{Provider: "openai", Type: "tts", Lines: map[string]int64{"price_mchar_nanousd": 15e9}, Source: src},
	})
}

func TestRefreshOffByDefaultAndEnvOverrides(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.manager(pricecatalog.Config{RequireSignature: true})
	eff := m.Effective(store.PriceCatalogSettings{})
	if eff.RefreshEnabled || eff.RefreshLocked || eff.URL != pricecatalog.DefaultURL || eff.URLSource != "default" || !eff.RequireSignature {
		t.Fatalf("defaults: %+v", eff)
	}
	if _, err := m.Check(ctx, pricecatalog.Actor{}); !errors.Is(err, pricecatalog.ErrRefreshOff) {
		t.Fatalf("check with refresh off: %v", err)
	}
	if r.feed.hits.Load() != 0 {
		t.Fatal("a check ran with the refresh off")
	}
	// The UI turns it on and sets a URL.
	eff = m.Effective(store.PriceCatalogSettings{RefreshEnabled: true, URL: "https://internal.example/c.json"})
	if !eff.RefreshEnabled || eff.RefreshSource != "settings" || eff.URL != "https://internal.example/c.json" || eff.URLSource != "settings" {
		t.Fatalf("settings: %+v", eff)
	}
	// Environment wins over the UI, both ways, and locks it.
	envOff := r.manager(pricecatalog.Config{EnvRefresh: off(), EnvURL: "https://env.example/c.json"})
	eff = envOff.Effective(store.PriceCatalogSettings{RefreshEnabled: true, URL: "https://ui.example/c.json"})
	if eff.RefreshEnabled || !eff.RefreshLocked || eff.RefreshSource != "env" || eff.URL != "https://env.example/c.json" || !eff.URLLocked {
		t.Fatalf("env off: %+v", eff)
	}
	envOn := r.manager(pricecatalog.Config{EnvRefresh: on()})
	if eff = envOn.Effective(store.PriceCatalogSettings{}); !eff.RefreshEnabled || !eff.RefreshLocked {
		t.Fatalf("env on: %+v", eff)
	}
}

// JANUS_OFFLINE keeps the refresh off whatever the UI or
// JANUS_PRICE_CATALOG_REFRESH say, unless JANUS_PRICE_CATALOG_URL names a
// catalog the site hosts itself.
func TestOfflineRefresh(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.store.SetPriceCatalogSettings(ctx, store.PriceCatalogSettings{RefreshEnabled: true}); err != nil {
		t.Fatal(err)
	}
	m := r.manager(pricecatalog.Config{Offline: true, EnvRefresh: on(), RequireSignature: true})
	eff := m.Effective(store.PriceCatalogSettings{RefreshEnabled: true})
	if eff.RefreshEnabled || !eff.OfflineBlocked || !eff.RefreshLocked {
		t.Fatalf("offline: %+v", eff)
	}
	if _, err := m.Check(ctx, pricecatalog.Actor{}); !errors.Is(err, pricecatalog.ErrRefreshOff) {
		t.Fatalf("offline check: %v", err)
	}
	mirror := r.manager(pricecatalog.Config{Offline: true, EnvURL: r.url(), RequireSignature: true})
	if eff := mirror.Effective(store.PriceCatalogSettings{RefreshEnabled: true}); !eff.RefreshEnabled || eff.OfflineBlocked || eff.URL != r.url() {
		t.Fatalf("offline with a site mirror: %+v", eff)
	}
	r.feed.set(r.catalog("9000.1", 3e9), pricecatalog.Sign(r.catalog("9000.1", 3e9), r.priv), "")
	if st, err := mirror.Check(ctx, pricecatalog.Actor{}); err != nil || st.Result != "updated" {
		t.Fatalf("mirror check: %+v %v", st, err)
	}
	if r.feed.hits.Load() != 1 {
		t.Fatalf("hits = %d", r.feed.hits.Load())
	}
}

func seedModels(t *testing.T, s *store.Store) (gpt4o, tts, admin *store.Model) {
	t.Helper()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "openai", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pricecatalog.Bundled()
	for _, name := range []string{"gpt-4o", "tts-new", "gpt-4o-mini"} {
		mods := []string{"chat"}
		if name == "tts-new" {
			mods = []string{"tts"}
		}
		ref, srcs := pricecatalog.Reference(b, up.AdapterType, up.BaseURL, name, mods)
		if _, err := s.UpsertDiscoveredMetadata(ctx, up.ID, name, mods, store.AutomaticMetadata{Reference: ref, ReferenceSources: srcs}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(n string) *store.Model {
		m, err := s.ModelByUpstreamAndName(ctx, up.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// An administrator prices gpt-4o-mini themselves.
	v := int64(123)
	if err := s.PatchModel(ctx, get("gpt-4o-mini").ID, store.ModelPatch{Overrides: map[string]*int64{"rate_in_nanousd": &v}}); err != nil {
		t.Fatal(err)
	}
	return get("gpt-4o"), get("tts-new"), get("gpt-4o-mini")
}

func TestCheckAppliesSignedNewerCatalogOnly(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gpt4o, tts, mini := seedModels(t, r.store)
	if gpt4o.RateInNano != 2.5e9 || tts.Metadata["price_mchar_nanousd"].Value != nil {
		t.Fatalf("bundled prices: %d %+v", gpt4o.RateInNano, tts.Metadata)
	}
	if err := r.store.SetPriceCatalogSettings(ctx, store.PriceCatalogSettings{RefreshEnabled: true, URL: r.url()}); err != nil {
		t.Fatal(err)
	}
	m := r.manager(pricecatalog.Config{RequireSignature: true})
	applied := 0
	m.OnApplied = func() { applied++ }

	// Bad signature: refused, last good kept, nothing applied.
	body := r.catalog("9000.1", 3e9)
	r.feed.set(body, pricecatalog.Sign([]byte("other bytes"), r.priv), `"v1"`)
	st, err := m.Check(ctx, pricecatalog.Actor{})
	if err != nil || st.Result != "error" || !strings.Contains(st.Error, "signature") {
		t.Fatalf("bad signature: %+v %v", st, err)
	}
	if m.Current().Origin != "bundled" || applied != 0 {
		t.Fatal("a badly signed catalog was used")
	}
	// Unsigned (no .sig): refused while signatures are required.
	r.feed.set(body, nil, `"v1"`)
	if st, _ = m.Check(ctx, pricecatalog.Actor{}); st.Result != "error" {
		t.Fatalf("unsigned: %+v", st)
	}
	// Signed by a key nobody trusts: refused.
	_, stranger := testKey(t)
	r.feed.set(body, pricecatalog.Sign(body, stranger), `"v1"`)
	if st, _ = m.Check(ctx, pricecatalog.Actor{}); st.Result != "error" {
		t.Fatalf("untrusted key: %+v", st)
	}

	// Good: applied to the catalog layer of every model.
	r.feed.set(body, pricecatalog.Sign(body, r.priv), `"v1"`)
	st, err = m.Check(ctx, pricecatalog.Actor{Label: "admin@example"})
	if err != nil || st.Result != "updated" || st.AppliedVersion != "9000.1" || applied != 1 {
		t.Fatalf("good: %+v %v", st, err)
	}
	if m.Current().Origin != "downloaded" || m.Current().KeyID != pricecatalog.KeyID(r.pub) {
		t.Fatalf("current: %+v", m.Current())
	}
	got, _ := r.store.ModelByID(ctx, gpt4o.ID)
	if got.RateInNano != 3e9 || got.Metadata["rate_in_nanousd"].SourceDetail.Label != "Test price catalog 9000.1" {
		t.Fatalf("catalog-sourced price not refreshed: %d %+v", got.RateInNano, got.Metadata["rate_in_nanousd"].SourceDetail)
	}
	if got.Metadata["rate_cached_nanousd"].Value != nil {
		t.Fatal("a value the new catalog dropped is still in force")
	}
	got, _ = r.store.ModelByID(ctx, tts.ID)
	if f := got.Metadata["price_mchar_nanousd"]; f.Value == nil || *f.Value != 15e9 || f.SourceDetail.Kind != "default" || !strings.Contains(f.SourceDetail.Label, "default for tts") {
		t.Fatalf("type default not applied: %+v", f)
	}
	got, _ = r.store.ModelByID(ctx, mini.ID)
	if f := got.Metadata["rate_in_nanousd"]; *f.Value != 123 || f.Source != "admin" {
		t.Fatalf("admin line changed: %+v", f)
	}
	if st.ModelsChanged < 3 || st.LinesChanged == 0 {
		t.Fatalf("counts: %+v", st)
	}
	audits, _, err := r.store.ListAudit(ctx, store.AuditFilter{Action: "price_catalog_updated"})
	if err != nil || len(audits) != 1 || !strings.Contains(audits[0].NewValue, `"lines_changed"`) || audits[0].ActorLabel != "admin@example" {
		t.Fatalf("audit: %+v %v", audits, err)
	}

	// Nothing new: the ETag answers 304 and nothing is applied.
	if st, _ = m.Check(ctx, pricecatalog.Actor{}); st.Result != "not_modified" || applied != 1 {
		t.Fatalf("etag: %+v", st)
	}
	if r.feed.headers.Get("If-None-Match") != `"v1"` {
		t.Fatalf("If-None-Match not sent: %v", r.feed.headers)
	}
	// Nothing about the gateway is sent.
	for k := range r.feed.headers {
		if strings.HasPrefix(strings.ToLower(k), "x-janus") || strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") {
			t.Fatalf("sent %s", k)
		}
	}
	if ua := r.feed.headers.Get("User-Agent"); ua != "janus-price-catalog/1" {
		t.Fatalf("user agent %q", ua)
	}
	// Same and older versions are never applied, even when signed.
	for _, v := range []string{"9000.1", "8999"} {
		older := r.catalog(v, 1e9)
		r.feed.set(older, pricecatalog.Sign(older, r.priv), "")
		if st, _ = m.Check(ctx, pricecatalog.Actor{}); st.Result != "up_to_date" || applied != 1 {
			t.Fatalf("version %s: %+v", v, st)
		}
	}
	if got, _ = r.store.ModelByID(ctx, gpt4o.ID); got.RateInNano != 3e9 {
		t.Fatal("an older catalog changed prices")
	}

	// A second replica (fresh manager) loads the stored catalog without fetching.
	hits := r.feed.hits.Load()
	other := r.manager(pricecatalog.Config{RequireSignature: true})
	if err := other.Load(ctx); err != nil || other.Current().Version != "9000.1" || r.feed.hits.Load() != hits {
		t.Fatalf("replica load: %v %+v", err, other.Current())
	}
}

// JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE=false: an unsigned (or edited)
// catalog is accepted and the signature file is not even fetched.
func TestRequireSignatureOff(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	seedModels(t, r.store)
	m := r.manager(pricecatalog.Config{RequireSignature: false, EnvRefresh: on(), EnvURL: r.url(), ExtraKeys: []ed25519.PublicKey{}})
	body := r.catalog("9000.2", 4e9)
	r.feed.set(body, nil, "")
	st, err := m.Check(ctx, pricecatalog.Actor{})
	if err != nil || st.Result != "updated" {
		t.Fatalf("unsigned with the requirement off: %+v %v", st, err)
	}
	if r.feed.sigHits.Load() != 0 || m.Current().KeyID != "" {
		t.Fatal("signature fetched or recorded although not required")
	}
	// Turning the requirement back on refuses the stored unsigned copy.
	strict := r.manager(pricecatalog.Config{RequireSignature: true, ExtraKeys: []ed25519.PublicKey{}})
	if err := strict.Load(ctx); err == nil || strict.Current().Origin != "bundled" {
		t.Fatalf("strict load of unsigned stored catalog: %v %+v", err, strict.Current())
	}
}

func TestCheckLimitsAndErrors(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	m := r.manager(pricecatalog.Config{RequireSignature: true, EnvRefresh: on(), EnvURL: r.url()})
	r.feed.set([]byte(strings.Repeat(" ", pricecatalog.MaxBytes+10)), nil, "")
	if st, _ := m.Check(ctx, pricecatalog.Actor{}); st.Result != "error" || !strings.Contains(st.Error, "larger than") {
		t.Fatalf("size limit: %+v", st)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	bad := pricecatalog.NewManager(pricecatalog.Config{RequireSignature: true, EnvRefresh: on(), EnvURL: srv.URL + "/c.json"}, r.store, srv.Client(), nil)
	st, err := bad.Check(ctx, pricecatalog.Actor{})
	if err != nil || st.Result != "error" || !strings.Contains(st.Error, "502") || st.CheckedAt == nil {
		t.Fatalf("http error: %+v %v", st, err)
	}
	if saved, _ := r.store.PriceCatalogStatus(ctx); saved.Result != "error" {
		t.Fatalf("status not recorded: %+v", saved)
	}
	if u, _ := pricecatalog.SignatureURL("https://h.example/p/catalog.json?x=1"); u != "https://h.example/p/catalog.json.sig?x=1" {
		t.Fatalf("signature url %s", u)
	}
	if pricecatalog.ValidateURL("https://user:pw@h.example/c.json") == nil || pricecatalog.ValidateURL("ftp://h.example/c") == nil {
		t.Fatal("bad URL accepted")
	}
}

// The daily schedule: due when never checked, then only after a day plus
// this replica's jitter.
func TestScheduleIsDailyWithJitter(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	m := r.manager(pricecatalog.Config{RequireSignature: true, EnvRefresh: on(), EnvURL: r.url()})
	m.SetClock(func() time.Time { return now }, func() time.Duration { return 30 * time.Minute })
	body := r.catalog("9000.3", 3e9)
	r.feed.set(body, pricecatalog.Sign(body, r.priv), "")
	pricecatalog.Tick(m, ctx, 30*time.Minute)
	if r.feed.hits.Load() != 1 {
		t.Fatalf("first tick: %d hits", r.feed.hits.Load())
	}
	now = now.Add(24 * time.Hour)
	pricecatalog.Tick(m, ctx, 30*time.Minute)
	if r.feed.hits.Load() != 1 {
		t.Fatal("checked again before the jitter elapsed")
	}
	now = now.Add(31 * time.Minute)
	pricecatalog.Tick(m, ctx, 30*time.Minute)
	if r.feed.hits.Load() != 2 {
		t.Fatal("no check after a day and the jitter")
	}
}
