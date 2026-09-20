package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// Additional OIDC providers configured in Admin → Provisioning (Business:
// multi_oidc). The env-configured provider (JANUS_OIDC_*) keeps /auth/start
// and /auth/callback unchanged; these live under /auth/start/<slug> and
// /auth/callback/<slug>. Sign-in semantics are identical: completeSignIn.

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)

type publicProvider struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// handleAuthProviders lists the sign-in buttons the login page should show.
// Public: it reveals provider names, which the IdP redirect would anyway.
func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	redirectTo := safeRedirect(r.URL.Query().Get("redirect_uri"))
	out := []publicProvider{}
	if s.OIDC != nil && !s.Config.DevAuthEnabled {
		out = append(out, publicProvider{Slug: "", Name: "Corporate SSO", URL: "/auth/start?redirect_uri=" + url.QueryEscape(redirectTo)})
	}
	if s.Registry != nil && s.requireFeature("multi_oidc") == nil {
		rows, err := s.Store.ListIdentityProviders(r.Context())
		if err == nil {
			for _, p := range rows {
				if p.Enabled {
					out = append(out, publicProvider{Slug: p.Slug, Name: p.Name, URL: "/auth/start/" + p.Slug + "?redirect_uri=" + url.QueryEscape(redirectTo)})
				}
			}
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	redirectTo := safeRedirect(r.URL.Query().Get("redirect_uri"))
	p, _, err := s.providerFor(r, chi.URLParam(r, "slug"))
	if err != nil {
		s.renderLoginError(w, r, "That sign-in method is not available.")
		return
	}
	target, err := p.AuthCodeURL(r.Context(), redirectTo)
	if err != nil {
		s.renderLoginError(w, r, "Sign-in could not be started. Try again.")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleProviderCallback(w http.ResponseWriter, r *http.Request) {
	if e := r.URL.Query().Get("error"); e != "" {
		s.renderLoginError(w, r, "The identity provider reported: "+e)
		return
	}
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if code == "" || state == "" {
		s.renderLoginError(w, r, "The sign-in response was incomplete. Start again from the sign-in page.")
		return
	}
	p, row, err := s.providerFor(r, chi.URLParam(r, "slug"))
	if err != nil {
		s.renderLoginError(w, r, "That sign-in method is not available.")
		return
	}
	identity, redirectTo, err := p.Exchange(r.Context(), code, state)
	if err != nil {
		s.Logger.WarnContext(r.Context(), "oidc exchange failed", "provider", row.Slug, "error", err.Error())
		s.renderLoginError(w, r, err.Error())
		return
	}
	// Subjects are only unique within one issuer; namespace them so two
	// providers can never collide on "sub=1".
	identity.Subject = row.Slug + ":" + identity.Subject
	// Per-provider admin groups compose with JANUS_ADMIN_GROUPS (OR) and are
	// re-evaluated on every sign-in like the global ones.
	s.completeSignInWith(w, r, identity, redirectTo, row.AdminGroups)
}

func (s *Server) providerFor(r *http.Request, slug string) (*auth.OIDCProvider, *store.IdentityProvider, error) {
	if s.Registry == nil || !slugRe.MatchString(slug) {
		return nil, nil, store.ErrNotFound
	}
	if err := s.requireFeature("multi_oidc"); err != nil {
		return nil, nil, err
	}
	return s.Registry.Provider(r.Context(), slug)
}

// ---- admin ----

type identityProviderBody struct {
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	IssuerURL    string   `json:"issuer_url"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Scopes       []string `json:"scopes"`
	EmailClaim   string   `json:"email_claim"`
	NameClaim    string   `json:"name_claim"`
	GroupsClaim  string   `json:"groups_claim"`
	AdminGroups  []string `json:"admin_groups"`
	Enabled      *bool    `json:"enabled"`
	SortOrder    int      `json:"sort_order"`
}

func (b identityProviderBody) validate() (store.IdentityProviderInput, *APIError) {
	b.Slug = strings.ToLower(strings.TrimSpace(b.Slug))
	if !slugRe.MatchString(b.Slug) {
		return store.IdentityProviderInput{}, ErrInvalidRequest("slug must be 2–40 lowercase letters, digits or hyphens; it becomes part of the callback URL")
	}
	if strings.TrimSpace(b.Name) == "" {
		return store.IdentityProviderInput{}, ErrInvalidRequest("name is required")
	}
	u, err := url.Parse(strings.TrimSpace(b.IssuerURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return store.IdentityProviderInput{}, ErrInvalidRequest("issuer_url must be an https URL (the OIDC issuer, without /.well-known)")
	}
	if strings.TrimSpace(b.ClientID) == "" {
		return store.IdentityProviderInput{}, ErrInvalidRequest("client_id is required")
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	return store.IdentityProviderInput{
		Slug: b.Slug, Name: strings.TrimSpace(b.Name), IssuerURL: strings.TrimRight(u.String(), "/"), ClientID: strings.TrimSpace(b.ClientID),
		ClientSecret: b.ClientSecret, Scopes: b.Scopes, EmailClaim: b.EmailClaim, NameClaim: b.NameClaim, GroupsClaim: b.GroupsClaim,
		AdminGroups: b.AdminGroups, Enabled: enabled, SortOrder: b.SortOrder,
	}, nil
}

func (s *Server) withCallback(p *store.IdentityProvider) map[string]any {
	return map[string]any{
		"id": p.ID, "slug": p.Slug, "name": p.Name, "issuer_url": p.IssuerURL, "client_id": p.ClientID, "has_secret": p.HasSecret,
		"scopes": p.Scopes, "email_claim": p.EmailClaim, "name_claim": p.NameClaim, "groups_claim": p.GroupsClaim, "admin_groups": p.AdminGroups,
		"enabled": p.Enabled, "sort_order": p.SortOrder, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"callback_url": s.Config.PublicURL + "/auth/callback/" + p.Slug,
	}
}

func (s *Server) handleAdminListIdentityProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListIdentityProviders(r.Context())
	if err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, s.withCallback(p))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"providers": out, "licensed": s.requireFeature("multi_oidc") == nil})
}

func (s *Server) handleAdminCreateIdentityProvider(w http.ResponseWriter, r *http.Request) {
	var body identityProviderBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	in, apiErr := body.validate()
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if in.ClientSecret == "" {
		WriteError(w, r, ErrInvalidRequest("client_secret is required when creating a provider"))
		return
	}
	p, err := s.Store.CreateIdentityProvider(r.Context(), s.Cipher, in)
	if errors.Is(err, store.ErrProviderSlugTaken) {
		WriteError(w, r, ErrConflict(err.Error()))
		return
	}
	if err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	s.audit(r, "identity_provider.create", "identity_provider", p.ID, nil, map[string]any{"slug": p.Slug, "issuer_url": p.IssuerURL})
	WriteJSON(w, http.StatusCreated, s.withCallback(p))
}

func (s *Server) handleAdminUpdateIdentityProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.IdentityProviderByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("identity provider"))
		return
	}
	var body identityProviderBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	in, apiErr := body.validate()
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	p, err := s.Store.UpdateIdentityProvider(r.Context(), s.Cipher, id, in)
	if errors.Is(err, store.ErrProviderSlugTaken) {
		WriteError(w, r, ErrConflict(err.Error()))
		return
	}
	if err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	if s.Registry != nil {
		s.Registry.Forget(before.Slug)
	}
	s.audit(r, "identity_provider.update", "identity_provider", p.ID, map[string]any{"slug": before.Slug, "enabled": before.Enabled}, map[string]any{"slug": p.Slug, "enabled": p.Enabled})
	WriteJSON(w, http.StatusOK, s.withCallback(p))
}

func (s *Server) handleAdminDeleteIdentityProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.IdentityProviderByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("identity provider"))
		return
	}
	if err := s.Store.DeleteIdentityProvider(r.Context(), id); err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	if s.Registry != nil {
		s.Registry.Forget(before.Slug)
	}
	s.audit(r, "identity_provider.delete", "identity_provider", id, map[string]any{"slug": before.Slug}, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminTestIdentityProvider runs discovery against a candidate issuer
// so the admin sees "reachable, issuer X, endpoints Y" before saving.
func (s *Server) handleAdminTestIdentityProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IssuerURL string `json:"issuer_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.IssuerURL) == "" {
		WriteError(w, r, ErrInvalidRequest("issuer_url is required"))
		return
	}
	if s.Registry == nil {
		WriteError(w, r, ErrInternal())
		return
	}
	meta, err := s.Registry.Probe(r.Context(), strings.TrimRight(strings.TrimSpace(body.IssuerURL), "/"))
	if err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "issuer": meta.Issuer, "authorization_endpoint": meta.AuthURL, "token_endpoint": meta.TokenURL, "jwks_uri": meta.JWKSURL, "userinfo_endpoint": meta.UserInfoURL})
}
