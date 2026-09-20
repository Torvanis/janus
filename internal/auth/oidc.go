package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Identity is the normalised result of a successful sign-in.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// ProviderMetadata is the subset of the OIDC discovery document Janus uses.
type ProviderMetadata struct {
	Issuer        string `json:"issuer"`
	AuthURL       string `json:"authorization_endpoint"`
	TokenURL      string `json:"token_endpoint"`
	JWKSURL       string `json:"jwks_uri"`
	UserInfoURL   string `json:"userinfo_endpoint"`
	EndSessionURL string `json:"end_session_endpoint"`
}

// ClaimMapping tells the adapter which claims carry which fields.
type ClaimMapping struct {
	Email  string
	Name   string
	Groups string
}

// OIDCConfig configures the generic OIDC adapter.
type OIDCConfig struct {
	ProviderURL  string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	Claims       ClaimMapping
}

// AuthState is one in-flight sign-in: everything the callback needs to finish
// the exchange, keyed by the single-use state value.
type AuthState struct {
	State       string
	Verifier    string
	Nonce       string
	RedirectTo  string
	RedirectURI string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// ErrStateNotFound indicates an expired, replayed, or unknown state value.
var ErrStateNotFound = fmt.Errorf("the sign-in request has expired or was already used; start again from the sign-in page")

// StateStore persists in-flight sign-ins. The database-backed implementation
// (NewDBOIDCStateStore) is the production one: it lets the OIDC callback land
// on any replica. The in-memory implementation backs tests and single-replica
// evaluation deployments.
type StateStore interface {
	Save(ctx context.Context, st *AuthState) error
	// Consume removes and returns the state atomically; a second call with the
	// same value must return ErrStateNotFound (single-use).
	Consume(ctx context.Context, state string) (*AuthState, error)
}

// MemoryStateStore keeps pending sign-ins in process memory.
type MemoryStateStore struct {
	mu      sync.Mutex
	pending map[string]*AuthState
	now     func() time.Time
}

// NewMemoryStateStore builds an in-process state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{pending: map[string]*AuthState{}, now: func() time.Time { return time.Now().UTC() }}
}

// Save records a pending sign-in and garbage-collects expired ones.
func (m *MemoryStateStore) Save(_ context.Context, st *AuthState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, v := range m.pending {
		if v.ExpiresAt.Before(now) {
			delete(m.pending, k)
		}
	}
	m.pending[st.State] = st
	return nil
}

// Consume removes and returns a pending sign-in.
func (m *MemoryStateStore) Consume(_ context.Context, state string) (*AuthState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.pending[state]
	if !ok {
		return nil, ErrStateNotFound
	}
	delete(m.pending, state) // state is single-use: a replay finds nothing
	if !st.ExpiresAt.After(m.now()) {
		return nil, ErrStateNotFound
	}
	return st, nil
}

// HealthCheckOptions tunes the IdP reachability check behind Healthy.
//
// The defaults (DefaultHealthCheckOptions) are chosen so that:
//
//   - one transient probe failure (a connection refused during an IdP pod
//     restart, a DNS blip, a dropped packet) never flips a replica unready:
//     a failed attempt is retried immediately within the same round, and the
//     cached verdict only turns unhealthy after FailureThreshold consecutive
//     failed rounds;
//   - a genuine, sustained outage is still reported within a bounded window.
//     From the moment the IdP stops answering, the worst case is the sum of
//     HealthyTTL (waiting out a healthy verdict), one failed round, and
//     (FailureThreshold-1) × (UnhealthyTTL plus one failed round), where a
//     round takes at most 2×ProbeTimeout+RetryDelay (and never more than the
//     caller's context allows — /readyz caps it at 3s). With the defaults
//     that is 30s + 3s + 5s + 3s ≈ 41s, and only ≈ 8s from the first probe
//     that observes the outage;
//   - recovery is noticed quickly: an unhealthy (or suspect) verdict is
//     re-probed after UnhealthyTTL, not pinned for the full HealthyTTL.
type HealthCheckOptions struct {
	// HealthyTTL is how long a healthy verdict is served from cache before
	// the provider is probed again.
	HealthyTTL time.Duration
	// UnhealthyTTL is how long an unhealthy verdict — or a healthy verdict
	// that has just seen a failed round — is served before re-probing. It is
	// deliberately much shorter than HealthyTTL so both outage confirmation
	// and recovery happen promptly.
	UnhealthyTTL time.Duration
	// FailureThreshold is the number of consecutive failed probe rounds
	// required before Healthy reports false. Must be at least 1.
	FailureThreshold int
	// ProbeTimeout bounds each individual HTTP attempt so a slow IdP cannot
	// stall the readiness endpoint.
	ProbeTimeout time.Duration
	// RetryDelay is the pause between the first failed attempt of a round and
	// its single in-round retry.
	RetryDelay time.Duration
}

// DefaultHealthCheckOptions returns the production tuning described on
// HealthCheckOptions.
func DefaultHealthCheckOptions() HealthCheckOptions {
	return HealthCheckOptions{
		HealthyTTL:       30 * time.Second,
		UnhealthyTTL:     5 * time.Second,
		FailureThreshold: 2,
		ProbeTimeout:     2 * time.Second,
		RetryDelay:       200 * time.Millisecond,
	}
}

// OIDCProvider implements Authorization Code + PKCE against any compliant IdP.
type OIDCProvider struct {
	cfg      OIDCConfig
	meta     ProviderMetadata
	client   *http.Client
	states   StateStore
	jwks     *jwksCache
	nowFunc  func() time.Time
	stateTTL time.Duration
	logger   *slog.Logger

	// Cached IdP reachability. Readiness probes arrive at kubelet
	// frequency; the cache keeps them from hammering the provider while still
	// noticing an outage within the bounded window documented on
	// HealthCheckOptions. See Healthy for the state machine.
	healthMu        sync.Mutex
	healthOK        bool
	healthNextCheck time.Time // earliest time the cached verdict may be re-probed
	healthFailures  int       // consecutive failed probe rounds
	healthProbing   bool      // a probe round is in flight; callers reuse the cached verdict
	healthOpts      HealthCheckOptions
}

// NewOIDCProvider fetches the discovery document and prepares the adapter. A
// failure here is fatal at startup: the operator gets the exact URL that failed.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig, client *http.Client) (*OIDCProvider, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	discoveryURL := strings.TrimRight(cfg.ProviderURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build OIDC discovery request for %s: %w", discoveryURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the identity provider at %s: %w", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity provider discovery at %s returned HTTP %d", discoveryURL, resp.StatusCode)
	}
	var meta ProviderMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("parse OIDC discovery document from %s: %w", discoveryURL, err)
	}
	if meta.AuthURL == "" || meta.TokenURL == "" {
		return nil, fmt.Errorf("OIDC discovery document from %s is missing authorization_endpoint or token_endpoint", discoveryURL)
	}
	if meta.JWKSURL == "" {
		// OIDC Core requires jwks_uri in the discovery document; without it no
		// ID token signature can ever be verified, so refuse at startup where
		// the operator sees the error immediately.
		return nil, fmt.Errorf("OIDC discovery document from %s is missing jwks_uri; Janus requires it to verify ID token signatures", discoveryURL)
	}
	p := &OIDCProvider{
		cfg:        cfg,
		meta:       meta,
		client:     client,
		states:     NewMemoryStateStore(),
		jwks:       newJWKSCache(meta.JWKSURL, client),
		nowFunc:    func() time.Time { return time.Now().UTC() },
		stateTTL:   10 * time.Minute,
		logger:     slog.Default(),
		healthOpts: DefaultHealthCheckOptions(),
	}
	// The constructor has just fetched the discovery document successfully,
	// so the provider starts healthy; Healthy re-probes once the TTL passes.
	p.healthOK = true
	p.healthNextCheck = p.nowFunc().Add(p.healthOpts.HealthyTTL)
	return p, nil
}

// UseLogger routes health-probe diagnostics to the given logger (the server's
// structured logger in production). A nil logger keeps slog.Default().
func (p *OIDCProvider) UseLogger(l *slog.Logger) {
	if l != nil {
		p.logger = l
	}
}

// ConfigureHealthCheck replaces the reachability-check tuning. Zero or
// negative fields fall back to DefaultHealthCheckOptions, except the TTLs,
// which may legitimately be zero (probe on every call — useful in tests).
func (p *OIDCProvider) ConfigureHealthCheck(opts HealthCheckOptions) {
	def := DefaultHealthCheckOptions()
	if opts.HealthyTTL < 0 {
		opts.HealthyTTL = def.HealthyTTL
	}
	if opts.UnhealthyTTL < 0 {
		opts.UnhealthyTTL = def.UnhealthyTTL
	}
	if opts.FailureThreshold < 1 {
		opts.FailureThreshold = def.FailureThreshold
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = def.ProbeTimeout
	}
	if opts.RetryDelay < 0 {
		opts.RetryDelay = def.RetryDelay
	}
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.healthOpts = opts
	// Re-arm the cache against the new TTLs so a shorter HealthyTTL takes
	// effect immediately rather than after the previously scheduled check.
	ttl := opts.HealthyTTL
	if !p.healthOK || p.healthFailures > 0 {
		ttl = opts.UnhealthyTTL
	}
	p.healthNextCheck = p.nowFunc().Add(ttl)
}

// Healthy reports whether the identity provider currently answers its OIDC
// discovery endpoint (documented: readiness must fail while the IdP is
// unavailable).
//
// The verdict is cached so kubelet-frequency probes cannot hammer the
// provider, and the check is deliberately tolerant of transient faults so a
// single "connection refused" does not rotate a healthy replica out of
// service for the whole cache window:
//
//   - When the cache is fresh, the cached verdict is returned without a probe.
//   - Otherwise one probe *round* runs: an HTTP GET of the discovery document
//     with a short per-attempt timeout, retried once after RetryDelay if the
//     first attempt fails. A round that succeeds resets the failure count,
//     records a healthy verdict, and re-arms the cache for HealthyTTL.
//   - A round that fails increments the consecutive-failure count and re-arms
//     the cache for the much shorter UnhealthyTTL. The verdict only flips to
//     unhealthy once FailureThreshold consecutive rounds have failed, so an
//     outage is confirmed — and a recovery is detected — within the bounded
//     window documented on HealthCheckOptions.
//   - Every failed attempt is logged at warn level with the discovery URL and
//     the underlying error so operators can diagnose the cause from logs.
//     The success path logs nothing; only the unhealthy→healthy transition
//     emits a single info line.
//
// Concurrent callers never run overlapping rounds: while one probe is in
// flight the others receive the current cached verdict.
func (p *OIDCProvider) Healthy(ctx context.Context) bool {
	p.healthMu.Lock()
	if p.healthProbing || p.nowFunc().Before(p.healthNextCheck) {
		ok := p.healthOK
		p.healthMu.Unlock()
		return ok
	}
	p.healthProbing = true
	opts := p.healthOpts
	wasOK := p.healthOK
	p.healthMu.Unlock()

	discoveryURL := strings.TrimRight(p.cfg.ProviderURL, "/") + "/.well-known/openid-configuration"

	// One round: the first attempt plus a single immediate retry. A
	// millisecond-scale socket failure (IdP pod restart, DNS hiccup) is
	// almost always gone by the time the retry fires, so it never even
	// counts as a failed round.
	const attemptsPerRound = 2
	var roundErr error
	for attempt := 1; attempt <= attemptsPerRound; attempt++ {
		if attempt > 1 {
			if opts.RetryDelay > 0 {
				timer := time.NewTimer(opts.RetryDelay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
			}
			if ctx.Err() != nil {
				// The caller's deadline is gone; a retry cannot succeed and
				// its failure would only be misattributed to the IdP. The
				// first attempt's error already describes what happened.
				break
			}
		}
		err := p.probeDiscovery(ctx, discoveryURL, opts.ProbeTimeout)
		if err == nil {
			roundErr = nil
			break
		}
		roundErr = err
		p.logger.WarnContext(ctx, "identity provider health probe failed",
			"url", discoveryURL,
			"error", err.Error(),
			"attempt", attempt,
			"attempts_per_round", attemptsPerRound,
		)
	}

	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.healthProbing = false
	now := p.nowFunc()
	if roundErr == nil {
		p.healthFailures = 0
		p.healthOK = true
		p.healthNextCheck = now.Add(opts.HealthyTTL)
		if !wasOK {
			p.logger.InfoContext(ctx, "identity provider reachable again", "url", discoveryURL)
		}
		return true
	}

	p.healthFailures++
	p.healthNextCheck = now.Add(opts.UnhealthyTTL)
	if p.healthFailures >= opts.FailureThreshold {
		if p.healthOK {
			// Transition only: the per-attempt warnings above already carry
			// the error detail for every failed probe.
			p.logger.WarnContext(ctx, "identity provider marked unreachable; readiness will fail until it answers again",
				"url", discoveryURL,
				"consecutive_failed_rounds", p.healthFailures,
				"error", roundErr.Error(),
			)
		}
		p.healthOK = false
	}
	return p.healthOK
}

// probeDiscovery performs one bounded GET of the discovery document and
// returns nil when the response proves the provider is up. Any response
// counts as up except the two that make sign-in impossible: server errors
// and a vanished discovery document.
func (p *OIDCProvider) probeDiscovery(ctx context.Context, discoveryURL string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("discovery endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Metadata exposes the resolved provider endpoints.
func (p *OIDCProvider) Metadata() ProviderMetadata { return p.meta }

// UseStateStore replaces the in-memory pending-sign-in store. Multi-replica
// deployments must install the database-backed store so the callback can land
// on any replica.
func (p *OIDCProvider) UseStateStore(ss StateStore) {
	if ss != nil {
		p.states = ss
	}
}

// AuthCodeURL starts a login: it mints state, nonce, and a PKCE challenge, and
// returns the provider URL to redirect the browser to.
func (p *OIDCProvider) AuthCodeURL(ctx context.Context, redirectTo string) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	verifier, err := randomToken()
	if err != nil {
		return "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	now := p.nowFunc()
	if err := p.states.Save(ctx, &AuthState{
		State: state, Verifier: verifier, Nonce: nonce,
		RedirectTo: redirectTo, RedirectURI: p.cfg.RedirectURL,
		CreatedAt: now, ExpiresAt: now.Add(p.stateTTL),
	}); err != nil {
		return "", fmt.Errorf("record sign-in state: %w", err)
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return p.meta.AuthURL + "?" + q.Encode(), nil
}

// Exchange completes the callback: it validates single-use state, swaps the code
// for tokens, verifies the ID token, and maps claims onto an Identity.
func (p *OIDCProvider) Exchange(ctx context.Context, code, state string) (*Identity, string, error) {
	req, err := p.states.Consume(ctx, state)
	if err != nil {
		return nil, "", err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", req.RedirectURI)
	form.Set("client_id", p.cfg.ClientID)
	form.Set("client_secret", p.cfg.ClientSecret)
	form.Set("code_verifier", req.Verifier)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.meta.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", fmt.Errorf("build token request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("exchange authorization code with the identity provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("the identity provider rejected the authorization code (HTTP %d)", resp.StatusCode)
	}
	var tokens struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, "", fmt.Errorf("parse token response: %w", err)
	}
	if tokens.IDToken == "" {
		return nil, "", fmt.Errorf("the identity provider did not return an ID token; check that the 'openid' scope is granted to this client")
	}
	// Defence in depth: the token arrived over the client-authenticated
	// back-channel, and its signature must ALSO verify against the provider's
	// published JWKS keys.
	if err := p.jwks.verifyIDTokenSignature(ctx, tokens.IDToken); err != nil {
		return nil, "", err
	}
	claims, err := parseIDTokenClaims(tokens.IDToken)
	if err != nil {
		return nil, "", err
	}
	if err := p.validateClaims(claims, req.Nonce); err != nil {
		return nil, "", err
	}
	identity := p.mapClaims(claims)
	if identity.Subject == "" {
		return nil, "", fmt.Errorf("the ID token has no 'sub' claim, so the user cannot be identified")
	}
	if identity.Email == "" && tokens.AccessToken != "" {
		p.enrichFromUserInfo(ctx, tokens.AccessToken, identity)
	}
	return identity, req.RedirectTo, nil
}

func (p *OIDCProvider) validateClaims(claims map[string]any, nonce string) error {
	// iss, exp, and nonce are required outright: an ID token missing any of
	// them is either malformed or stripped, and either way is not acceptable.
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return fmt.Errorf("the ID token carries no issuer claim")
	}
	if p.meta.Issuer != "" && iss != p.meta.Issuer {
		return fmt.Errorf("the ID token issuer %q does not match the configured provider %q", iss, p.meta.Issuer)
	}
	if !audienceMatches(claims["aud"], p.cfg.ClientID) {
		return fmt.Errorf("the ID token was issued for a different client; check JANUS_OIDC_CLIENT_ID")
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return fmt.Errorf("the ID token carries no expiry claim")
	}
	if p.nowFunc().After(time.Unix(int64(exp), 0).UTC()) {
		return fmt.Errorf("the ID token has expired; sign in again")
	}
	got, _ := claims["nonce"].(string)
	if got == "" {
		return fmt.Errorf("the ID token carries no nonce claim; Janus always sends one with the sign-in request")
	}
	if got != nonce {
		return fmt.Errorf("the ID token nonce does not match the sign-in request")
	}
	return nil
}

func audienceMatches(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == clientID {
				return true
			}
		}
	}
	// OIDC Core requires the aud claim on every ID token; a missing aud means
	// a malformed or tampered token and must never pass client binding.
	return false
}

func (p *OIDCProvider) mapClaims(claims map[string]any) *Identity {
	id := &Identity{}
	id.Subject, _ = claims["sub"].(string)
	id.Email, _ = claims[p.cfg.Claims.Email].(string)
	verified, _ := claims["email_verified"].(bool)
	id.EmailVerified = verified && strings.TrimSpace(id.Email) != ""
	id.Name, _ = claims[p.cfg.Claims.Name].(string)
	if id.Name == "" {
		id.Name, _ = claims["preferred_username"].(string)
	}
	id.Groups = ExtractGroups(claims, p.cfg.Claims.Groups)
	if id.Email == "" {
		id.Email = id.Subject
	}
	if id.Name == "" {
		id.Name = id.Email
	}
	return id
}

func (p *OIDCProvider) enrichFromUserInfo(ctx context.Context, accessToken string, id *Identity) {
	if p.meta.UserInfoURL == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.meta.UserInfoURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&claims); err != nil {
		return
	}
	// OIDC Core requires the UserInfo subject to match the verified ID token.
	// Reject all enrichment, including groups, if that binding is absent.
	if sub, ok := claims["sub"].(string); !ok || sub == "" || sub != id.Subject {
		return
	}
	if email, ok := claims[p.cfg.Claims.Email].(string); ok && email != "" {
		id.Email = email
		verified, _ := claims["email_verified"].(bool)
		id.EmailVerified = verified && strings.TrimSpace(email) != ""
	}
	if len(id.Groups) == 0 {
		id.Groups = ExtractGroups(claims, p.cfg.Claims.Groups)
	}
}

// ExtractGroups reads group memberships from a claim set, accepting both the
// configured claim and the common `memberOf` fallback, and normalising LDAP
// distinguished names down to their common name.
func ExtractGroups(claims map[string]any, claimName string) []string {
	raw, ok := claims[claimName]
	if !ok {
		raw, ok = claims["groups"]
	}
	if !ok {
		raw = claims["memberOf"]
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(v string) {
		v = normaliseGroup(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	switch v := raw.(type) {
	case string:
		for _, part := range strings.Split(v, ",") {
			if !strings.Contains(strings.ToUpper(part), "=") {
				add(part)
			}
		}
		if len(out) == 0 {
			add(v)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	}
	return out
}

func normaliseGroup(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// "CN=engineers,OU=groups,DC=corp" -> "engineers"
	if strings.Contains(strings.ToUpper(v), "CN=") {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToUpper(part), "CN=") {
				return strings.TrimSpace(part[3:])
			}
		}
	}
	return v
}

// parseIDTokenClaims decodes the JWT payload. The signature has already been
// verified against the provider's JWKS keys by the time this runs (see
// jwksCache.verifyIDTokenSignature in Exchange); the transport-level trust of
// the client-authenticated, PKCE-bound back-channel is treated as a second
// layer, not the only one.
func parseIDTokenClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("the ID token is not a well-formed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode ID token payload: %w", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse ID token claims: %w", err)
	}
	return claims, nil
}

// DevIdentity mints a deterministic local identity for evaluation deployments
// that have no identity provider yet (JANUS_DEV_AUTH=true).
func DevIdentity(email, name string, groups []string) *Identity {
	sum := sha256.Sum256([]byte("janus-dev:" + strings.ToLower(email)))
	return &Identity{
		Subject:       "dev|" + base64.RawURLEncoding.EncodeToString(sum[:12]),
		Email:         strings.ToLower(email),
		EmailVerified: true, // Explicit development-only authentication bypass.
		Name:          name,
		Groups:        groups,
	}
}

func init() {
	// Fail loudly during development if the crypto source is unusable rather
	// than silently issuing predictable session identifiers.
	buf := make([]byte, 1)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
}
