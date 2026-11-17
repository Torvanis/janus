package pricecatalog

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
)

// DefaultURL is where the Janus price catalog is published. The signature
// is at the same URL with ".sig" appended.
const DefaultURL = "https://janusedge.com/pricing/catalog.json"

// Interval between checks; each replica adds up to MaxJitter so a fleet
// does not fetch at the same moment.
const (
	Interval  = 24 * time.Hour
	MaxJitter = time.Hour
)

// Config is the startup configuration (environment) of the refresh.
type Config struct {
	// EnvRefresh is JANUS_PRICE_CATALOG_REFRESH: nil when unset, else
	// whether the daily refresh is on. When set it wins over the UI.
	EnvRefresh *bool
	// EnvURL is JANUS_PRICE_CATALOG_URL; when set it wins over the UI.
	EnvURL string
	// RequireSignature is on unless JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE=false.
	RequireSignature bool
	// ExtraKeys are trusted besides the built-in keys
	// (JANUS_PRICE_CATALOG_KEYS).
	ExtraKeys []ed25519.PublicKey
	// Offline is JANUS_OFFLINE: the refresh is off unless EnvURL names a
	// catalog the site hosts itself.
	Offline bool
}

// Effective is the refresh configuration in force, with where each part
// comes from, for the admin UI.
type Effective struct {
	RefreshEnabled bool `json:"refresh_enabled"`
	// RefreshLocked: JANUS_PRICE_CATALOG_REFRESH decides (or offline mode
	// does); the UI cannot change it.
	RefreshLocked bool   `json:"refresh_locked"`
	RefreshSource string `json:"refresh_source"` // "env", "settings", "offline"
	URL           string `json:"url"`
	URLLocked     bool   `json:"url_locked"`
	URLSource     string `json:"url_source"` // "env", "settings", "default"
	// RequireSignature is env-only; the UI warns when it is off.
	RequireSignature bool `json:"require_signature"`
	Offline          bool `json:"offline"`
	// OfflineBlocked: offline mode keeps the refresh off (no
	// JANUS_PRICE_CATALOG_URL).
	OfflineBlocked bool `json:"offline_blocked"`
}

// Store is what the manager needs from the database.
type Store interface {
	PriceCatalogSettings(ctx context.Context) (store.PriceCatalogSettings, error)
	PriceCatalogActive(ctx context.Context) (*store.PriceCatalogActive, error)
	SetPriceCatalogActive(ctx context.Context, v store.PriceCatalogActive) error
	PriceCatalogStatus(ctx context.Context) (store.PriceCatalogStatus, error)
	SetPriceCatalogStatus(ctx context.Context, v store.PriceCatalogStatus) error
	ListModels(ctx context.Context, f store.ModelFilter) ([]*store.Model, error)
	ListUpstreams(ctx context.Context) ([]*store.Upstream, error)
	RefreshModelReference(ctx context.Context, id string, reference map[string]int64, sources map[string]store.ReferenceSource) (int, error)
	DecideCatalogAlignment(ctx context.Context, id string, reference map[string]int64, sources map[string]store.ReferenceSource) (decided, aligned bool, err error)
	AppendAudit(ctx context.Context, e *store.AuditEntry) error
}

// Manager holds the catalog in use and runs the refresh.
type Manager struct {
	cfg    Config
	store  Store
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
	jitter func() time.Duration

	current atomic.Pointer[Loaded]
	checkMu sync.Mutex
	// reconciled is the catalog version Reconcile last brought every
	// model up to in this process.
	reconciled atomic.Pointer[string]
	// OnApplied runs after an update was applied to the models (the
	// server drops its config cache there).
	OnApplied func()
}

// NewManager builds a manager using the bundled catalog until Load reads
// a newer downloaded one.
func NewManager(cfg Config, st Store, client *http.Client, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	m := &Manager{cfg: cfg, store: st, client: client, logger: logger, now: time.Now,
		jitter: func() time.Duration { return time.Duration(rand.Int64N(int64(MaxJitter))) }}
	if b, err := Bundled(); err != nil {
		logger.Error("bundled price catalog is unusable; models get no catalog prices until a valid catalog is downloaded", "error", err.Error())
	} else {
		m.current.Store(b)
	}
	return m
}

// SetClock replaces the clock and jitter (tests).
func (m *Manager) SetClock(now func() time.Time, jitter func() time.Duration) {
	m.now, m.jitter = now, jitter
}

// Current is the catalog in use (nil only when the bundled catalog failed
// to verify and nothing was downloaded).
func (m *Manager) Current() *Loaded {
	if m == nil {
		b, _ := Bundled()
		return b
	}
	return m.current.Load()
}

// Keys are the trusted keys: built-in plus configured.
func (m *Manager) Keys() []ed25519.PublicKey {
	return append(BuiltinKeys(), m.cfg.ExtraKeys...)
}

// Config returns the startup configuration.
func (m *Manager) Config() Config { return m.cfg }

// Effective resolves the refresh configuration from env and settings.
func (m *Manager) Effective(settings store.PriceCatalogSettings) Effective {
	e := Effective{RequireSignature: m.cfg.RequireSignature, Offline: m.cfg.Offline}
	switch {
	case m.cfg.EnvURL != "":
		e.URL, e.URLSource, e.URLLocked = m.cfg.EnvURL, "env", true
	case settings.URL != "":
		e.URL, e.URLSource = settings.URL, "settings"
	default:
		e.URL, e.URLSource = DefaultURL, "default"
	}
	switch {
	case m.cfg.Offline && m.cfg.EnvURL == "":
		e.RefreshEnabled, e.RefreshSource, e.RefreshLocked, e.OfflineBlocked = false, "offline", true, true
	case m.cfg.EnvRefresh != nil:
		e.RefreshEnabled, e.RefreshSource, e.RefreshLocked = *m.cfg.EnvRefresh, "env", true
	default:
		e.RefreshEnabled, e.RefreshSource = settings.RefreshEnabled, "settings"
	}
	return e
}

// ValidateURL checks a catalog URL an administrator enters.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("the price catalog URL must be an http:// or https:// URL without credentials")
	}
	return nil
}

// SignatureURL is where a catalog's detached signature lives.
func SignatureURL(catalogURL string) (string, error) {
	u, err := url.Parse(catalogURL)
	if err != nil {
		return "", err
	}
	u.Path += ".sig"
	if u.RawPath != "" {
		u.RawPath += ".sig"
	}
	return u.String(), nil
}

// Load reads the stored downloaded catalog and uses it when it verifies and
// is newer than the one in use (an upgrade can bring a newer bundled one).
func (m *Manager) Load(ctx context.Context) error {
	a, err := m.store.PriceCatalogActive(ctx)
	if err != nil || a == nil {
		return err
	}
	cur := m.Current()
	if cur != nil && cur.Origin == "downloaded" && cur.Version == a.Version {
		return nil
	}
	l, err := m.verify([]byte(a.Body), []byte(a.Signature), a.URL)
	if err != nil {
		return fmt.Errorf("stored price catalog %s: %w", a.Version, err)
	}
	if cur == nil || Newer(l.Version, cur.Version) || cur.Origin == "downloaded" && l.Version != cur.Version {
		m.current.Store(l)
	}
	return nil
}

func (m *Manager) verify(body, sig []byte, from string) (*Loaded, error) {
	keyID := ""
	if m.cfg.RequireSignature {
		id, err := Verify(body, sig, m.Keys())
		if err != nil {
			return nil, err
		}
		keyID = id
	}
	c, err := Parse(body)
	if err != nil {
		return nil, err
	}
	return &Loaded{Catalog: c, Origin: "downloaded", URL: from, KeyID: keyID}, nil
}

// ErrRefreshOff is returned by Check when the refresh is not turned on.
var ErrRefreshOff = errors.New("the price catalog refresh is off")

// Actor names who started a check in the audit log.
type Actor struct {
	UserID string
	Label  string
	IP     string
}

// Check fetches the catalog once, if the refresh is on, and applies it when
// it is newer. It always records the outcome.
func (m *Manager) Check(ctx context.Context, actor Actor) (store.PriceCatalogStatus, error) {
	settings, err := m.store.PriceCatalogSettings(ctx)
	if err != nil {
		return store.PriceCatalogStatus{}, err
	}
	eff := m.Effective(settings)
	if !eff.RefreshEnabled {
		return store.PriceCatalogStatus{}, ErrRefreshOff
	}
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	prev, err := m.store.PriceCatalogStatus(ctx)
	if err != nil {
		return store.PriceCatalogStatus{}, err
	}
	now := m.now().UTC()
	st := prev
	st.CheckedAt, st.URL, st.Error = &now, eff.URL, ""
	result, l, etag, ferr := m.fetch(ctx, eff)
	switch {
	case ferr != nil:
		st.Result, st.Error = "error", ferr.Error()
		m.logger.WarnContext(ctx, "price catalog check failed", "url", eff.URL, "error", ferr.Error())
	case result != "":
		st.Result = result
		if l != nil {
			st.Version = l.Version
		}
	default:
		st.Result, st.Version = "updated", l.Version
		if err := m.store.SetPriceCatalogActive(ctx, store.PriceCatalogActive{
			Version: l.Version, Body: string(l.raw), Signature: string(l.sig), URL: eff.URL, ETag: etag, FetchedAt: now,
		}); err != nil {
			return st, err
		}
		before := m.Current()
		m.current.Store(l.Loaded)
		res, err := m.Apply(ctx)
		models, lines := res.Models, res.Lines
		if err != nil {
			st.Result, st.Error = "error", "applying the catalog to models: "+err.Error()
		} else {
			st.AppliedAt, st.AppliedVersion, st.ModelsChanged, st.LinesChanged = &now, l.Version, models, lines
			prevVersion := ""
			if before != nil {
				prevVersion = before.Version
			}
			m.auditUpdate(ctx, actor, prevVersion, l, models, lines)
			if m.OnApplied != nil {
				m.OnApplied()
			}
		}
	}
	if err := m.store.SetPriceCatalogStatus(ctx, st); err != nil {
		return st, err
	}
	return st, nil
}

type fetched struct {
	*Loaded
	raw, sig []byte
}

const userAgent = "janus-price-catalog/1"

// fetch downloads and verifies the catalog. result is non-empty when there
// is nothing to apply ("not_modified", "up_to_date").
func (m *Manager) fetch(ctx context.Context, eff Effective) (result string, l *fetched, etag string, err error) {
	stored, err := m.store.PriceCatalogActive(ctx)
	if err != nil {
		return "", nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eff.URL, nil)
	if err != nil {
		return "", nil, "", err
	}
	// Nothing about this gateway is sent: no version, instance or host.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if stored != nil && stored.URL == eff.URL && stored.ETag != "" {
		req.Header.Set("If-None-Match", stored.ETag)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", nil, "", fmt.Errorf("fetch %s: %w", eff.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		return "not_modified", nil, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("fetch %s: HTTP %d", eff.URL, resp.StatusCode)
	}
	body, err := readLimited(resp.Body)
	if err != nil {
		return "", nil, "", fmt.Errorf("fetch %s: %w", eff.URL, err)
	}
	var sig []byte
	if eff.RequireSignature {
		sigURL, err := SignatureURL(eff.URL)
		if err != nil {
			return "", nil, "", err
		}
		if sig, err = m.get(ctx, sigURL); err != nil {
			return "", nil, "", fmt.Errorf("fetch signature %s: %w", sigURL, err)
		}
	}
	loaded, err := m.verify(body, sig, eff.URL)
	if err != nil {
		return "", nil, "", err
	}
	// Only a newer version replaces the catalog in use (or one stored by
	// another replica); the same or an older one is never applied.
	newest := ""
	if cur := m.Current(); cur != nil {
		newest = cur.Version
	}
	if stored != nil && Newer(stored.Version, newest) {
		newest = stored.Version
	}
	if newest != "" && !Newer(loaded.Version, newest) {
		return "up_to_date", &fetched{Loaded: loaded}, "", nil
	}
	return "", &fetched{Loaded: loaded, raw: body, sig: sig}, resp.Header.Get("ETag"), nil
}

func (m *Manager) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUnsigned
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return readLimited(resp.Body)
}

func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBytes {
		return nil, fmt.Errorf("larger than %d bytes", MaxBytes)
	}
	return body, nil
}

// PriceProvider names the catalog provider of an upstream: the provider's
// official endpoint (CapabilityProvider) or Amazon Bedrock. Self-hosted and
// proxy upstreams have none.
func PriceProvider(adapterType, baseURL string) string {
	if p := adapter.CapabilityProvider(adapterType, baseURL); p != "" {
		return p
	}
	if adapterType == "bedrock" {
		return "bedrock"
	}
	return ""
}

// Reference is what the catalog gives one model, ready for the store's
// reference layer.
func Reference(l *Loaded, adapterType, baseURL, model string, modalities []string) (map[string]int64, map[string]store.ReferenceSource) {
	if l == nil {
		return map[string]int64{}, nil
	}
	r := l.Lookup(PriceProvider(adapterType, baseURL), model, ModelType(modalities))
	var src map[string]store.ReferenceSource
	if len(r.Sources) > 0 {
		src = make(map[string]store.ReferenceSource, len(r.Sources))
		for k, s := range r.Sources {
			src[k] = store.ReferenceSource(s)
		}
	}
	return r.Values, src
}

// ApplyResult counts what an apply changed.
type ApplyResult struct {
	Models, Lines int
	// Decided is how many models had "keep aligned" decided, and Aligned
	// how many of those it turned on.
	Decided, Aligned int
}

// Apply rewrites the catalog layer of every model from the catalog in use,
// including models discovered before the catalog existed (stale models
// excepted: discovery prices them if they are listed again). Administrator
// overrides and the provider's API prices are another layer and are never
// touched, so a model kept aligned to the catalog (no administrator
// prices) bills the new catalog prices, and a model with prices set by an
// administrator keeps them. A model from before "keep aligned" existed has
// it decided first, against this catalog, without changing a price
// (store.DecideCatalogAlignment). It returns how many models and catalog
// values changed.
func (m *Manager) Apply(ctx context.Context) (ApplyResult, error) {
	var res ApplyResult
	l := m.Current()
	ups, err := m.store.ListUpstreams(ctx)
	if err != nil {
		return res, err
	}
	byID := make(map[string]*store.Upstream, len(ups))
	for _, u := range ups {
		byID[u.ID] = u
	}
	list, err := m.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		return res, err
	}
	for _, md := range list {
		up := byID[md.UpstreamID]
		if up == nil {
			continue
		}
		ref, src := Reference(l, up.AdapterType, up.BaseURL, md.Name, md.Modalities)
		if !md.CatalogAlignmentDecided() {
			decided, aligned, err := m.store.DecideCatalogAlignment(ctx, md.ID, ref, src)
			if err != nil {
				return res, err
			}
			if decided {
				res.Decided++
				if aligned {
					res.Aligned++
				}
			}
		}
		// A stale model is not listed by its upstream and cannot be
		// called; discovery applies the catalog in use if it comes back.
		if md.Status == store.ModelStale {
			continue
		}
		n, err := m.store.RefreshModelReference(ctx, md.ID, ref, src)
		if err != nil {
			return res, err
		}
		if n > 0 {
			res.Models++
			res.Lines += n
		}
	}
	return res, nil
}

// Reconcile brings every model up to the catalog in use when it has not
// been applied yet: after an upgrade that bundles a newer catalog, after
// another replica stored a newer download, and once for models from before
// "keep aligned" existed. It runs at startup and on every scheduled tick;
// with nothing to do it reads the status and the model list only.
func (m *Manager) Reconcile(ctx context.Context) (res ApplyResult, err error) {
	l := m.Current()
	if l == nil {
		return ApplyResult{}, nil
	}
	if v := m.reconciled.Load(); v != nil && *v == l.Version {
		return ApplyResult{}, nil
	}
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	st, err := m.store.PriceCatalogStatus(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	version := l.Version
	defer func() {
		if err == nil {
			m.reconciled.Store(&version)
		}
	}()
	pending := st.AppliedVersion != l.Version
	if !pending {
		list, err := m.store.ListModels(ctx, store.ModelFilter{})
		if err != nil {
			return ApplyResult{}, err
		}
		for _, md := range list {
			if !md.CatalogAlignmentDecided() {
				pending = true
				break
			}
		}
	}
	if !pending {
		return ApplyResult{}, nil
	}
	res, err = m.Apply(ctx)
	if err != nil {
		return res, err
	}
	now := m.now().UTC()
	prev := st.AppliedVersion
	st.AppliedAt, st.AppliedVersion, st.ModelsChanged, st.LinesChanged = &now, l.Version, res.Models, res.Lines
	if err := m.store.SetPriceCatalogStatus(ctx, st); err != nil {
		return res, err
	}
	if prev != l.Version || res.Models > 0 || res.Decided > 0 {
		e := &store.AuditEntry{
			Action: "price_catalog_applied", ResourceType: "price_catalog", ResourceID: l.Version,
			ActorLabel: "price catalog (" + l.Origin + ")",
			OldValue:   fmt.Sprintf(`{"version":%q}`, prev),
			NewValue: fmt.Sprintf(`{"version":%q,"origin":%q,"models_changed":%d,"lines_changed":%d,"alignment_decided":%d,"aligned":%d}`,
				l.Version, l.Origin, res.Models, res.Lines, res.Decided, res.Aligned),
		}
		if err := m.store.AppendAudit(ctx, e); err != nil {
			m.logger.ErrorContext(ctx, "audit price catalog apply", "error", err.Error())
		}
	}
	if res.Models > 0 && m.OnApplied != nil {
		m.OnApplied()
	}
	m.logger.InfoContext(ctx, "price catalog applied to models", "version", l.Version, "models_changed", res.Models,
		"lines_changed", res.Lines, "alignment_decided", res.Decided, "aligned", res.Aligned)
	return res, nil
}

func (m *Manager) auditUpdate(ctx context.Context, actor Actor, prev string, l *fetched, models, lines int) {
	label := actor.Label
	if label == "" {
		label = "price catalog refresh"
	}
	n, d := l.Counts()
	e := &store.AuditEntry{
		Action: "price_catalog_updated", ResourceType: "price_catalog", ResourceID: l.Version,
		ActorUserID: actor.UserID, ActorLabel: label, SourceIP: actor.IP,
		OldValue: fmt.Sprintf(`{"version":%q}`, prev),
		NewValue: fmt.Sprintf(`{"version":%q,"url":%q,"key_id":%q,"entries":%d,"defaults":%d,"models_changed":%d,"lines_changed":%d}`,
			l.Version, l.URL, l.KeyID, n, d, models, lines),
	}
	if err := m.store.AppendAudit(ctx, e); err != nil {
		m.logger.ErrorContext(ctx, "audit price catalog update", "error", err.Error())
	}
}

// Due reports whether a scheduled check should run now: never checked, or
// the last check is older than Interval plus this replica's jitter.
func (m *Manager) due(st store.PriceCatalogStatus, jitter time.Duration) bool {
	if st.CheckedAt == nil {
		return true
	}
	return !m.now().Before(st.CheckedAt.Add(Interval + jitter))
}

// Run loads the stored catalog, then every few minutes picks up a catalog
// another replica downloaded and, when the refresh is on and a check is
// due, checks. It returns when ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if err := m.Load(ctx); err != nil {
		m.logger.WarnContext(ctx, "load stored price catalog", "error", err.Error())
	}
	jitter := m.jitter()
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		m.tick(ctx, jitter)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *Manager) tick(ctx context.Context, jitter time.Duration) {
	if err := m.Load(ctx); err != nil {
		m.logger.WarnContext(ctx, "load stored price catalog", "error", err.Error())
	}
	if _, err := m.Reconcile(ctx); err != nil {
		m.logger.WarnContext(ctx, "apply price catalog to models", "error", err.Error())
	}
	settings, err := m.store.PriceCatalogSettings(ctx)
	if err != nil || !m.Effective(settings).RefreshEnabled {
		return
	}
	st, err := m.store.PriceCatalogStatus(ctx)
	if err != nil || !m.due(st, jitter) {
		return
	}
	if _, err := m.Check(ctx, Actor{}); err != nil && !errors.Is(err, ErrRefreshOff) {
		m.logger.WarnContext(ctx, "price catalog check", "error", err.Error())
	}
}
