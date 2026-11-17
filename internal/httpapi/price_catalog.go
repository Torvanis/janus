package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/pricecatalog"
	"github.com/torvanis/janus/internal/safehttp"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

// Price catalog administration (internal/pricecatalog):
//
//	GET   /api/v1/admin/price-catalog           catalog in use, refresh settings and last check
//	GET   /api/v1/admin/price-catalog/entries   every model entry and type default
//	PATCH /api/v1/admin/price-catalog/settings  {refresh_enabled?, url?}
//	POST  /api/v1/admin/price-catalog/check     check now (refresh must be on)
//
// Environment variables win over the settings: a PATCH of a value the
// environment sets is refused, and the status marks it locked.

func (s *Server) mountPriceCatalogRoutes(r chi.Router) {
	r.Get("/admin/price-catalog", s.handlePriceCatalogStatus)
	r.Get("/admin/price-catalog/entries", s.handlePriceCatalogEntries)
	r.Patch("/admin/price-catalog/settings", s.handlePriceCatalogSettings)
	r.Post("/admin/price-catalog/check", s.handlePriceCatalogCheck)
}

// priceCatalog returns the manager, building one from Config when the
// server was wired without one (tests).
func (s *Server) priceCatalog() *pricecatalog.Manager {
	s.priceCatalogOnce.Do(func() {
		if s.PriceCatalog == nil {
			cfg := pricecatalog.Config{RequireSignature: true}
			if s.Config != nil {
				cfg = PriceCatalogConfig(s.Config)
			}
			s.PriceCatalog = pricecatalog.NewManager(cfg, s.Store, PriceCatalogClient(), s.Logger)
		}
		if s.PriceCatalog.OnApplied == nil {
			s.PriceCatalog.OnApplied = s.InvalidateConfigCache
		}
	})
	return s.PriceCatalog
}

// PriceCatalogConfig turns the environment into the manager's config.
func PriceCatalogConfig(c *config.Config) pricecatalog.Config {
	keys, _ := pricecatalog.ParseKeys(strings.Join(c.PriceCatalogKeys, ","))
	return pricecatalog.Config{
		EnvRefresh: c.PriceCatalogRefresh, EnvURL: c.PriceCatalogURL, RequireSignature: !c.PriceCatalogSignatureOff,
		ExtraKeys: keys, Offline: c.Offline,
	}
}

// PriceCatalogClient is the client the catalog is fetched with: the shared
// trust store (internal CAs, revocation), the environment's proxy, a
// timeout, and no redirect from https to http or to another host with
// credentials. Header policies do not apply: they belong to upstreams and
// personal-plan providers, and the catalog host is neither.
func PriceCatalogClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &truststore.Transport{Tune: func(t *http.Transport) { t.Proxy = http.ProxyFromEnvironment }},
		CheckRedirect: safehttp.CheckRedirect(nil),
	}
}

type priceCatalogKey struct {
	ID     string `json:"id"`
	Source string `json:"source"` // "built-in" or "env"
}

func (s *Server) handlePriceCatalogStatus(w http.ResponseWriter, r *http.Request) {
	m := s.priceCatalog()
	saved, err := s.Store.PriceCatalogSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	status, err := s.Store.PriceCatalogStatus(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := map[string]any{
		"settings":    m.Effective(saved),
		"saved":       saved,
		"status":      status,
		"default_url": pricecatalog.DefaultURL,
	}
	if cur := m.Current(); cur != nil {
		n, d := cur.Counts()
		out["catalog"] = map[string]any{
			"version": cur.Version, "label": cur.Label(), "publisher": cur.Publisher, "generated_at": cur.GeneratedAt,
			"origin": cur.Origin, "url": cur.URL, "key_id": cur.KeyID, "entries": n, "defaults": d,
		}
	}
	if b, err := pricecatalog.Bundled(); err == nil {
		out["bundled"] = map[string]any{"version": b.Version, "generated_at": b.GeneratedAt}
	} else {
		out["bundled_error"] = err.Error()
	}
	keys := []priceCatalogKey{}
	for _, k := range pricecatalog.BuiltinKeys() {
		keys = append(keys, priceCatalogKey{ID: pricecatalog.KeyID(k), Source: "built-in"})
	}
	for _, k := range m.Config().ExtraKeys {
		keys = append(keys, priceCatalogKey{ID: pricecatalog.KeyID(k), Source: "env"})
	}
	out["trusted_keys"] = keys
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handlePriceCatalogEntries(w http.ResponseWriter, r *http.Request) {
	cur := s.priceCatalog().Current()
	if cur == nil {
		WriteError(w, r, ErrNotFoundf("price catalog"))
		return
	}
	provider := r.URL.Query().Get("provider")
	entries := []pricecatalog.Entry{}
	for _, e := range cur.Entries {
		if provider == "" || e.Provider == provider {
			entries = append(entries, e)
		}
	}
	defaults := []pricecatalog.Default{}
	for _, d := range cur.Defaults {
		if provider == "" || d.Provider == provider {
			defaults = append(defaults, d)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"version": cur.Version, "label": cur.Label(), "generated_at": cur.GeneratedAt, "origin": cur.Origin,
		"entries": entries, "defaults": defaults,
	})
}

func (s *Server) handlePriceCatalogSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshEnabled *bool   `json:"refresh_enabled"`
		URL            *string `json:"url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.RefreshEnabled == nil && body.URL == nil {
		WriteError(w, r, ErrInvalidRequest("Send refresh_enabled, url, or both."))
		return
	}
	m := s.priceCatalog()
	before, err := s.Store.PriceCatalogSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	eff := m.Effective(before)
	after := before
	if body.RefreshEnabled != nil {
		if eff.RefreshLocked && *body.RefreshEnabled != eff.RefreshEnabled {
			msg := "The daily refresh is set by JANUS_PRICE_CATALOG_REFRESH and cannot be changed here."
			if eff.OfflineBlocked {
				msg = "Offline mode (JANUS_OFFLINE) keeps the refresh off unless JANUS_PRICE_CATALOG_URL names a catalog this site hosts."
			}
			WriteError(w, r, ErrConflict(msg).WithParam("refresh_enabled"))
			return
		}
		after.RefreshEnabled = *body.RefreshEnabled
	}
	if body.URL != nil {
		u := strings.TrimSpace(*body.URL)
		if eff.URLLocked {
			WriteError(w, r, ErrConflict("The catalog URL is set by JANUS_PRICE_CATALOG_URL and cannot be changed here.").WithParam("url"))
			return
		}
		if u == pricecatalog.DefaultURL {
			u = ""
		}
		if u != "" {
			if err := pricecatalog.ValidateURL(u); err != nil {
				WriteError(w, r, ErrInvalidRequest(err.Error()+".").WithParam("url"))
				return
			}
		}
		after.URL = u
	}
	if err := s.Store.SetPriceCatalogSettings(r.Context(), after); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "price_catalog_settings_changed", "system", store.SettingPriceCatalogSettings, before, after)
	WriteJSON(w, http.StatusOK, map[string]any{"saved": after, "settings": m.Effective(after)})
}

func (s *Server) handlePriceCatalogCheck(w http.ResponseWriter, r *http.Request) {
	actor := pricecatalog.Actor{IP: s.clientIP(r)}
	if u := UserFrom(r.Context()); u != nil {
		actor.UserID, actor.Label = u.ID, u.Email
	}
	st, err := s.priceCatalog().Check(r.Context(), actor)
	if errors.Is(err, pricecatalog.ErrRefreshOff) {
		WriteError(w, r, ErrConflict("Turn on the daily price catalog refresh first."))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": st})
}
