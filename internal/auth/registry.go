package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

// Registry serves the OIDC providers configured in the admin UI (Business:
// multi_oidc). Each gets its own callback URL /auth/callback/<slug>, so the
// IdP's redirect-URI allowlist does the routing and no provider id has to
// travel in the state. Adapters are built lazily on first use (discovery is
// a network call) and cached until the row changes.
type Registry struct {
	db        *store.Store
	cipher    *crypto.Cipher
	client    *http.Client
	publicURL string
	states    StateStore
	logger    *slog.Logger

	mu      sync.Mutex
	entries map[string]*registryEntry // by slug
}

type registryEntry struct {
	updatedAt time.Time
	provider  *OIDCProvider
}

// NewRegistry wires the registry; publicURL is the gateway's external base.
func NewRegistry(db *store.Store, cipher *crypto.Cipher, client *http.Client, publicURL string, states StateStore, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{db: db, cipher: cipher, client: client, publicURL: publicURL, states: states, logger: logger, entries: map[string]*registryEntry{}}
}

// CallbackURL is the redirect URI an operator registers at the IdP.
func (r *Registry) CallbackURL(slug string) string {
	return r.publicURL + "/auth/callback/" + slug
}

// Provider returns a ready adapter for an enabled provider, building it on
// first use. Disabled or unknown slugs return store.ErrNotFound.
func (r *Registry) Provider(ctx context.Context, slug string) (*OIDCProvider, *store.IdentityProvider, error) {
	row, err := r.db.IdentityProviderBySlug(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	if !row.Enabled {
		return nil, nil, store.ErrNotFound
	}
	r.mu.Lock()
	if e, ok := r.entries[slug]; ok && e.updatedAt.Equal(row.UpdatedAt) {
		r.mu.Unlock()
		return e.provider, row, nil
	}
	r.mu.Unlock()

	secret, err := row.ClientSecret(r.cipher)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt client secret for provider %q: %w", slug, err)
	}
	p, err := NewOIDCProvider(ctx, OIDCConfig{
		ProviderURL:  row.IssuerURL,
		ClientID:     row.ClientID,
		ClientSecret: secret,
		RedirectURL:  r.CallbackURL(slug),
		Scopes:       row.Scopes,
		Claims:       ClaimMapping{Email: row.EmailClaim, Name: row.NameClaim, Groups: row.GroupsClaim},
	}, r.client)
	if err != nil {
		return nil, nil, err
	}
	p.UseLogger(r.logger.With("provider", slug))
	if r.states != nil {
		p.UseStateStore(r.states)
	}
	r.mu.Lock()
	r.entries[slug] = &registryEntry{updatedAt: row.UpdatedAt, provider: p}
	r.mu.Unlock()
	return p, row, nil
}

// Forget drops a cached adapter (after delete/disable); the next use rebuilds.
func (r *Registry) Forget(slug string) {
	r.mu.Lock()
	delete(r.entries, slug)
	r.mu.Unlock()
}

// Probe runs discovery for a candidate configuration without storing
// anything — the admin UI's "Test" button.
func (r *Registry) Probe(ctx context.Context, issuerURL string) (ProviderMetadata, error) {
	p, err := NewOIDCProvider(ctx, OIDCConfig{ProviderURL: issuerURL, ClientID: "probe", RedirectURL: r.publicURL}, r.client)
	if err != nil {
		return ProviderMetadata{}, err
	}
	return p.Metadata(), nil
}
