package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/pricecatalog"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Precedence for what a call costs, highest first (owner rulings, see
// internal/pricecatalog): the provider's quote of the call > an
// administrator's price > the provider's API price > the catalog entry for
// the exact model > the catalog default for the provider and model type >
// nothing. Each row adds one layer on top of the ones below it and sends a
// real request through the proxy (10 prompt + 2 completion tokens).
func TestPriceCatalogPrecedenceMatrix(t *testing.T) {
	ctx := context.Background()
	src := pricecatalog.Source{URL: "https://prices.example/p", ReadAt: "2026-11-16"}
	raw, err := pricecatalog.Build([]pricecatalog.SourceFile{{Provider: "p",
		Entries:  []pricecatalog.Entry{{Provider: "p", Model: "test-model", Type: "chat", Lines: map[string]int64{"rate_in_nanousd": 2e9, "rate_out_nanousd": 2e9}, Source: src}},
		Defaults: []pricecatalog.Default{{Provider: "p", Type: "chat", Lines: map[string]int64{"rate_in_nanousd": 1e9, "rate_out_nanousd": 1e9}, Source: src}},
	}}, "7", "Janus", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := pricecatalog.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	toStore := func(r pricecatalog.Resolved) (map[string]int64, map[string]store.ReferenceSource) {
		out := map[string]store.ReferenceSource{}
		for k, s := range r.Sources {
			out[k] = store.ReferenceSource(s)
		}
		return r.Values, out
	}
	const noQuote = `{"id":"o-1","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	const quoted = `{"id":"o-1","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.000005}}`
	rate := func(usd int64) map[string]int64 {
		return map[string]int64{"rate_in_nanousd": usd, "rate_out_nanousd": usd}
	}
	for _, tc := range []struct {
		name       string
		lookup     string // model name looked up in the catalog ("" = no catalog value)
		upstream   map[string]int64
		admin      int64
		reply      string
		wantNano   int64
		wantSource string
		wantLabel  string
	}{
		{name: "catalog default for provider and type", lookup: "other-model", reply: noQuote, wantNano: 12_000, wantSource: "reference", wantLabel: "Janus price catalog 7 (default for chat)"},
		{name: "catalog exact model beats the default", lookup: "test-model", reply: noQuote, wantNano: 24_000, wantSource: "reference", wantLabel: "Janus price catalog 7"},
		{name: "provider API price beats the catalog", lookup: "test-model", upstream: rate(3e9), reply: noQuote, wantNano: 36_000, wantSource: "upstream", wantLabel: "Provider API"},
		{name: "administrator beats the provider API", lookup: "test-model", upstream: rate(3e9), admin: 4e9, reply: noQuote, wantNano: 48_000, wantSource: "admin", wantLabel: "Admin override"},
		{name: "the provider's quote of the call beats everything", lookup: "test-model", upstream: rate(3e9), admin: 4e9, reply: quoted, wantNano: 5_000, wantSource: "admin", wantLabel: "Admin override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			clear := map[string]*int64{}
			for _, k := range []string{"rate_in_nanousd", "rate_out_nanousd", "rate_cached_nanousd", "rate_cache_write_5m_nanousd", "rate_cache_write_1h_nanousd"} {
				clear[k] = nil
			}
			if err := h.store.PatchModel(ctx, h.model.ID, store.ModelPatch{Overrides: clear}); err != nil {
				t.Fatal(err)
			}
			ref, refSrc := toStore(cat.Lookup("p", tc.lookup, "chat"))
			if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{Upstream: tc.upstream, Reference: ref, ReferenceSources: refSrc}); err != nil {
				t.Fatal(err)
			}
			if tc.admin != 0 {
				v := tc.admin
				if err := h.store.PatchModel(ctx, h.model.ID, store.ModelPatch{Overrides: map[string]*int64{"rate_in_nanousd": &v, "rate_out_nanousd": &v}}); err != nil {
					t.Fatal(err)
				}
			}
			h.server.InvalidateConfigCache()
			m, err := h.store.ModelByID(ctx, h.model.ID)
			if err != nil {
				t.Fatal(err)
			}
			f := m.Metadata["rate_in_nanousd"]
			if f.Source != tc.wantSource || f.SourceDetail == nil || f.SourceDetail.Label != tc.wantLabel {
				t.Fatalf("source = %s %+v, want %s %q", f.Source, f.SourceDetail, tc.wantSource, tc.wantLabel)
			}
			quotedUpstream(t, h, tc.reply, false, nil)
			if rec := h.do(http.MethodPost, "/v1/chat/completions", quotedChatBody(false)); rec.Code != http.StatusOK {
				t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
			}
			ev := h.waitForUsageEvents(1)[0]
			if ev.CostNano != tc.wantNano {
				t.Fatalf("cost = %d nano (%s), want %d", ev.CostNano, ev.AccountingMode, tc.wantNano)
			}
			if tc.reply == quoted && ev.AccountingMode != usage.AccountingUpstreamCost {
				t.Fatalf("accounting = %s", ev.AccountingMode)
			}
		})
	}
}

// catalogFeed serves a signed catalog for the admin API tests.
func catalogFeed(t *testing.T, priv ed25519.PrivateKey, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			hits.Add(1)
			_, _ = w.Write(body)
		case "/catalog.json.sig":
			_, _ = w.Write(pricecatalog.Sign(body, priv))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// reloadPriceCatalog rebuilds the server's catalog manager from its Config
// (the harness built one before the test changed the environment).
func reloadPriceCatalog(h *harness) {
	m := pricecatalog.NewManager(PriceCatalogConfig(h.server.Config), h.store, PriceCatalogClient(), h.server.Logger)
	m.OnApplied = h.server.InvalidateConfigCache
	h.server.PriceCatalog = m
}

type catalogStatus struct {
	Settings pricecatalog.Effective `json:"settings"`
	Catalog  struct {
		Version string `json:"version"`
		Origin  string `json:"origin"`
		Label   string `json:"label"`
		Entries int    `json:"entries"`
	} `json:"catalog"`
	Status      store.PriceCatalogStatus `json:"status"`
	DefaultURL  string                   `json:"default_url"`
	TrustedKeys []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	} `json:"trusted_keys"`
}

func readCatalogStatus(t *testing.T, h *harness, admin *auth.Session) catalogStatus {
	t.Helper()
	rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/price-catalog", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var st catalogStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPriceCatalogAdminAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := adminSession(t, h)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h.server.Config.PriceCatalogKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	reloadPriceCatalog(h)

	// Off by default, bundled catalog in use, signature required.
	st := readCatalogStatus(t, h, admin)
	if st.Settings.RefreshEnabled || st.Settings.RefreshLocked || !st.Settings.RequireSignature || st.Catalog.Origin != "bundled" ||
		st.Catalog.Entries < 100 || st.DefaultURL != pricecatalog.DefaultURL || len(st.TrustedKeys) != 2 || st.TrustedKeys[1].Source != "env" {
		t.Fatalf("defaults: %+v", st)
	}
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/price-catalog/check", nil); rec.Code != http.StatusConflict {
		t.Fatalf("check with refresh off: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/price-catalog/settings", map[string]any{"url": "ftp://nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url: %d", rec.Code)
	}

	// A model the new catalog prices differently.
	if err := h.store.RefreshModelMetadata(ctx, h.model.ID, store.AutomaticMetadata{}); err != nil {
		t.Fatal(err)
	}
	body, err := pricecatalog.Build([]pricecatalog.SourceFile{{Provider: "openai", Entries: []pricecatalog.Entry{{
		Provider: "openai", Model: "gpt-4o", Type: "chat", Lines: map[string]int64{"rate_in_nanousd": 7e9},
		Source: pricecatalog.Source{URL: "https://prices.example", ReadAt: "2026-11-16"},
	}}}}, "9999.1", "Internal", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	feed, hits := catalogFeed(t, priv, body)
	up, err := h.store.CreateUpstream(ctx, "oa", "openai_compatible", "https://api.openai.com/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredMetadata(ctx, up.ID, "gpt-4o", []string{"chat"}, store.AutomaticMetadata{}); err != nil {
		t.Fatal(err)
	}

	rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/price-catalog/settings", map[string]any{"refresh_enabled": true, "url": feed.URL + "/catalog.json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(admin, http.MethodPost, "/api/v1/admin/price-catalog/check", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"result":"updated"`) || hits.Load() != 1 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, "gpt-4o")
	if err != nil || m.RateInNano != 7e9 || m.Metadata["rate_in_nanousd"].SourceDetail.Label != "Internal price catalog 9999.1" {
		t.Fatalf("applied: %+v %v", m.Metadata["rate_in_nanousd"], err)
	}
	st = readCatalogStatus(t, h, admin)
	if st.Catalog.Origin != "downloaded" || st.Catalog.Version != "9999.1" || st.Status.Result != "updated" || st.Status.CheckedAt == nil {
		t.Fatalf("after check: %+v", st)
	}
	rec = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/price-catalog/entries?provider=openai", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"gpt-4o"`) || !strings.Contains(rec.Body.String(), `"read_at":"2026-11-16"`) {
		t.Fatalf("entries: %d %s", rec.Code, rec.Body.String())
	}
	// The model list carries the label the drawer shows.
	rec = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models", nil)
	if !strings.Contains(rec.Body.String(), `"label":"Internal price catalog 9999.1"`) {
		t.Fatalf("model list has no catalog label: %s", rec.Body.String()[:min(400, len(rec.Body.String()))])
	}
	audits, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "price_catalog_updated"})
	if err != nil || len(audits) != 1 {
		t.Fatalf("audit: %v %v", audits, err)
	}
	if audits, _, _ := h.store.ListAudit(ctx, store.AuditFilter{Action: "price_catalog_settings_changed"}); len(audits) != 1 {
		t.Fatalf("settings audit: %v", audits)
	}

	// A member can do none of it.
	_, member := h.otherUser(t, "catalog-member", "catalog-member@example.com")
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/api/v1/admin/price-catalog"}, {http.MethodPost, "/api/v1/admin/price-catalog/check"},
		{http.MethodPatch, "/api/v1/admin/price-catalog/settings"}} {
		if rec := h.doWithToken(member, c.method, c.path, map[string]any{"refresh_enabled": false}); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("member %s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

// Environment variables win over the UI and lock it; offline mode keeps the
// refresh off unless JANUS_PRICE_CATALOG_URL is set.
func TestPriceCatalogEnvLocksTheUI(t *testing.T) {
	h := newHarness(t)
	admin := adminSession(t, h)
	off := false
	h.server.Config.PriceCatalogRefresh = &off
	h.server.Config.PriceCatalogURL = "https://mirror.internal.example/catalog.json"
	h.server.Config.PriceCatalogSignatureOff = true
	reloadPriceCatalog(h)
	st := readCatalogStatus(t, h, admin)
	if st.Settings.RefreshEnabled || !st.Settings.RefreshLocked || st.Settings.RefreshSource != "env" || !st.Settings.URLLocked ||
		st.Settings.URL != "https://mirror.internal.example/catalog.json" || st.Settings.RequireSignature {
		t.Fatalf("env: %+v", st.Settings)
	}
	if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/price-catalog/settings", map[string]any{"refresh_enabled": true}); rec.Code != http.StatusConflict {
		t.Fatalf("UI changed an env-set refresh: %d", rec.Code)
	}
	if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/price-catalog/settings", map[string]any{"url": "https://other.example/c.json"}); rec.Code != http.StatusConflict {
		t.Fatalf("UI changed an env-set URL: %d", rec.Code)
	}

	o := newHarness(t)
	oadmin := adminSession(t, o)
	o.server.Config.Offline = true
	reloadPriceCatalog(o)
	st = readCatalogStatus(t, o, oadmin)
	if st.Settings.RefreshEnabled || !st.Settings.OfflineBlocked {
		t.Fatalf("offline: %+v", st.Settings)
	}
	rec := o.doAsSession(oadmin, http.MethodPatch, "/api/v1/admin/price-catalog/settings", map[string]any{"refresh_enabled": true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "JANUS_OFFLINE") {
		t.Fatalf("offline toggle: %d %s", rec.Code, rec.Body.String())
	}
}
