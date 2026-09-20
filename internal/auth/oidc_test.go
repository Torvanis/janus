package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that returns whatever ID token the test tells it to.
type fakeIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	// ecKey, when set, makes the JWKS endpoint publish an EC (P-256) signing
	// key instead of an RSA one.
	ecKey   *ecdsa.PrivateKey
	kid     string
	idToken func(nonce string) string
	// lastNonce captures the nonce Janus sent in the authorize redirect.
	lastNonce string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	idp := &fakeIdP{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.server.URL,
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
			"jwks_uri":               idp.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if idp.ecKey != nil {
			pub := &idp.ecKey.PublicKey
			// pub.Bytes() is the SEC 1 uncompressed point 0x04 || X || Y.
			size := (pub.Curve.Params().BitSize + 7) / 8
			raw, err := pub.Bytes()
			if err != nil {
				panic(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{{
					"kty": "EC", "kid": idp.kid, "use": "sig", "alg": "ES256", "crv": "P-256",
					"x": base64.RawURLEncoding.EncodeToString(raw[1 : 1+size]),
					"y": base64.RawURLEncoding.EncodeToString(raw[1+size:]),
				}},
			})
			return
		}
		pub := &idp.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": idp.kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "at-1",
			"id_token":     idp.idToken(idp.lastNonce),
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// sign builds a JWT over the given claims with the IdP's key.
func (f *fakeIdP) sign(t *testing.T, header map[string]any, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// signES builds a JWT over the given claims with the IdP's EC key using the
// JOSE r||s fixed-width signature encoding.
func (f *fakeIdP) signES(t *testing.T, header map[string]any, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, f.ecKey, sum[:])
	if err != nil {
		t.Fatalf("sign token with EC key: %v", err)
	}
	size := (f.ecKey.Curve.Params().BitSize + 7) / 8
	sig := append(r.FillBytes(make([]byte, size)), s.FillBytes(make([]byte, size))...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeIdP) standardClaims(nonce string) map[string]any {
	return map[string]any{
		"iss": f.server.URL, "aud": "janus-client", "sub": "user-42",
		"email": "ada@example.com", "name": "Ada",
		"exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce,
	}
}

// startFlow builds the provider, runs AuthCodeURL, and captures the nonce.
func startFlow(t *testing.T, idp *fakeIdP) (*OIDCProvider, string) {
	t.Helper()
	p, err := NewOIDCProvider(context.Background(), OIDCConfig{
		ProviderURL: idp.server.URL, ClientID: "janus-client", ClientSecret: "s",
		RedirectURL: "https://janus.example.com/auth/callback",
		Scopes:      []string{"openid", "email"},
		Claims:      ClaimMapping{Email: "email", Name: "name", Groups: "groups"},
	}, idp.server.Client())
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	authURL, err := p.AuthCodeURL(context.Background(), "/dashboard")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	idp.lastNonce = parsed.Query().Get("nonce")
	if idp.lastNonce == "" {
		t.Fatal("authorize redirect carries no nonce")
	}
	return p, parsed.Query().Get("state")
}

func TestExchangeVerifiesSignatureAndClaims(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, idp.standardClaims(nonce))
	}
	p, state := startFlow(t, idp)

	identity, redirectTo, err := p.Exchange(context.Background(), "code-1", state)
	if err != nil {
		t.Fatalf("Exchange with a properly signed token must succeed: %v", err)
	}
	if identity.Email != "ada@example.com" || identity.Subject != "user-42" {
		t.Fatalf("identity = %+v", identity)
	}
	if redirectTo != "/dashboard" {
		t.Fatalf("redirectTo = %q", redirectTo)
	}
}

// TestExchangeVerifiesES256Signature covers EC-signing IdPs: a
// provider publishing only a P-256 key and signing with ES256 must be able to
// sign users in, and tampered ES256 tokens must still be rejected.
func TestExchangeVerifiesES256Signature(t *testing.T) {
	idp := newFakeIdP(t)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	idp.ecKey = ecKey
	idp.idToken = func(nonce string) string {
		return idp.signES(t, map[string]any{"alg": "ES256", "kid": idp.kid}, idp.standardClaims(nonce))
	}
	p, state := startFlow(t, idp)

	identity, _, err := p.Exchange(context.Background(), "code-1", state)
	if err != nil {
		t.Fatalf("Exchange with a properly ES256-signed token must succeed: %v", err)
	}
	if identity.Email != "ada@example.com" || identity.Subject != "user-42" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestExchangeRejectsTamperedES256Token(t *testing.T) {
	idp := newFakeIdP(t)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	idp.ecKey = ecKey
	idp.idToken = func(nonce string) string {
		token := idp.signES(t, map[string]any{"alg": "ES256", "kid": idp.kid}, idp.standardClaims(nonce))
		parts := strings.Split(token, ".")
		claims := idp.standardClaims(nonce)
		claims["sub"] = "attacker"
		forged, _ := json.Marshal(claims)
		parts[1] = base64.RawURLEncoding.EncodeToString(forged)
		return strings.Join(parts, ".")
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered ES256 token must be rejected with a signature error, got: %v", err)
	}
}

func TestExchangeRejectsTamperedToken(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		token := idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, idp.standardClaims(nonce))
		// Swap the payload for one claiming a different subject; the
		// signature no longer matches.
		parts := strings.Split(token, ".")
		claims := idp.standardClaims(nonce)
		claims["sub"] = "attacker"
		forged, _ := json.Marshal(claims)
		parts[1] = base64.RawURLEncoding.EncodeToString(forged)
		return strings.Join(parts, ".")
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered token must be rejected with a signature error, got: %v", err)
	}
}

func TestExchangeRejectsAlgNone(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		h, _ := json.Marshal(map[string]any{"alg": "none"})
		c, _ := json.Marshal(idp.standardClaims(nonce))
		return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c) + "."
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "unsupported signing algorithm") {
		t.Fatalf("alg=none must be rejected, got: %v", err)
	}
}

func TestExchangeRejectsMissingIssuer(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		claims := idp.standardClaims(nonce)
		delete(claims, "iss")
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, claims)
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "no issuer claim") {
		t.Fatalf("a token without iss must be rejected, got: %v", err)
	}
}

func TestExchangeRejectsMissingNonce(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		claims := idp.standardClaims(nonce)
		delete(claims, "nonce")
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, claims)
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "no nonce claim") {
		t.Fatalf("a token without nonce must be rejected, got: %v", err)
	}
}

// TestExchangeRejectsMissingAudience proves an ID token with no aud claim is
// refused (regression: audienceMatches returned true for nil, so a malformed
// or tampered token missing the mandatory OIDC aud claim passed client binding).
func TestExchangeRejectsMissingAudience(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		claims := idp.standardClaims(nonce)
		delete(claims, "aud")
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, claims)
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "different client") {
		t.Fatalf("a token without aud must be rejected, got: %v", err)
	}
}

func TestAudienceMatches(t *testing.T) {
	cases := []struct {
		name string
		aud  any
		want bool
	}{
		{"exact string", "janus-client", true},
		{"wrong string", "someone-else", false},
		{"array containing client", []any{"other", "janus-client"}, true},
		{"array without client", []any{"other"}, false},
		{"missing claim", nil, false},
		{"wrong type", 42.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := audienceMatches(tc.aud, "janus-client"); got != tc.want {
				t.Fatalf("audienceMatches(%v) = %v, want %v", tc.aud, got, tc.want)
			}
		})
	}
}

func TestExchangeRejectsWrongNonce(t *testing.T) {
	idp := newFakeIdP(t)
	idp.idToken = func(nonce string) string {
		claims := idp.standardClaims(nonce)
		claims["nonce"] = "someone-elses-nonce"
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": idp.kid}, claims)
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "nonce does not match") {
		t.Fatalf("a token with a foreign nonce must be rejected, got: %v", err)
	}
}

func TestExchangeRejectsUnknownKeyAfterRefresh(t *testing.T) {
	idp := newFakeIdP(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}
	idp.idToken = func(nonce string) string {
		// Signed by a key the JWKS endpoint never publishes.
		saved := idp.key
		idp.key = other
		defer func() { idp.key = saved }()
		return idp.sign(t, map[string]any{"alg": "RS256", "kid": "rogue-key"}, idp.standardClaims(nonce))
	}
	p, state := startFlow(t, idp)

	if _, _, err := p.Exchange(context.Background(), "code-1", state); err == nil || !strings.Contains(err.Error(), "no key matching") {
		t.Fatalf("a token signed by an unpublished key must be rejected, got: %v", err)
	}
}

func TestDiscoveryWithoutJWKSIsFatal(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	_, err := NewOIDCProvider(context.Background(), OIDCConfig{ProviderURL: srv.URL, ClientID: "c"}, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "jwks_uri") {
		t.Fatalf("a provider without jwks_uri must be refused at startup, got: %v", err)
	}
}

// healthClock is an injectable clock for the health-check tests: the cache
// TTLs are stepped through by advancing it, never by sleeping.
type healthClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *healthClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *healthClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// flakyIdP is a discovery endpoint whose failure mode the test controls:
// failNext > 0 makes that many upcoming requests fail (either by dropping
// the TCP connection, which the client sees as a transport error like a
// connection refused / reset, or with an HTTP 503).
type flakyIdP struct {
	server *httptest.Server
	mu     sync.Mutex
	// failNext is the number of upcoming discovery requests that must fail.
	failNext int
	// dropConnection selects the transport-level failure mode; when false a
	// 503 is returned instead.
	dropConnection bool
	requests       int
}

func newFlakyIdP(t *testing.T) *flakyIdP {
	t.Helper()
	idp := &flakyIdP{dropConnection: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		idp.requests++
		fail := idp.failNext > 0
		if fail {
			idp.failNext--
		}
		drop := idp.dropConnection
		idp.mu.Unlock()
		if fail {
			if drop {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
			}
			http.Error(w, "identity provider restarting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.server.URL,
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
			"jwks_uri":               idp.server.URL + "/jwks",
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (f *flakyIdP) failNextRequests(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

func (f *flakyIdP) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// newHealthProbeProvider builds a provider against the flaky IdP with an
// injected clock, a captured logger, and the production tuning minus the
// real-time retry pause (so tests never sleep).
func newHealthProbeProvider(t *testing.T, idp *flakyIdP) (*OIDCProvider, *healthClock, *bytes.Buffer) {
	t.Helper()
	// Disable keep-alives: a dropped connection must not leave a poisoned
	// pooled socket for the very next attempt to trip over, which is also
	// how the production client behaves after a reset.
	client := idp.server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	client = &http.Client{Transport: transport}
	p, err := NewOIDCProvider(context.Background(), OIDCConfig{
		ProviderURL: idp.server.URL, ClientID: "janus-client", ClientSecret: "s",
		RedirectURL: "https://janus.example.com/auth/callback",
	}, client)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	clock := &healthClock{now: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
	p.nowFunc = clock.Now
	var logs bytes.Buffer
	p.UseLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	opts := DefaultHealthCheckOptions()
	opts.RetryDelay = 0
	p.ConfigureHealthCheck(opts)
	return p, clock, &logs
}

// TestHealthyTracksIdPReachability is the documented baseline: the verdict is
// cached while fresh, and a sustained outage is detected — under the
// fault-tolerant semantics, after FailureThreshold consecutive failed rounds
// rather than on the very first failed probe.
func TestHealthyTracksIdPReachability(t *testing.T) {
	idp := newFlakyIdP(t)
	p, clock, _ := newHealthProbeProvider(t, idp)
	opts := DefaultHealthCheckOptions()

	// Fresh from a successful discovery fetch, the provider starts healthy.
	if !p.Healthy(context.Background()) {
		t.Fatal("provider must be healthy right after successful discovery")
	}

	// The verdict is cached: with the IdP gone but the healthy TTL not yet
	// expired, the last answer is returned without a probe.
	idp.server.Close()
	before := idp.requestCount()
	clock.Advance(opts.HealthyTTL - time.Second)
	if !p.Healthy(context.Background()) {
		t.Fatal("within the TTL the cached healthy verdict must be returned")
	}
	if idp.requestCount() != before {
		t.Fatal("a fresh cached verdict must not trigger a probe")
	}

	// Once the TTL lapses, the probe runs and fails. The first failed round
	// alone is tolerated (it could be a blip), but the cache is now re-armed
	// on the short unhealthy interval rather than the 30s healthy one.
	clock.Advance(time.Second)
	if !p.Healthy(context.Background()) {
		t.Fatal("a single failed probe round must not flip readiness")
	}
	clock.Advance(opts.UnhealthyTTL - time.Millisecond)
	if !p.Healthy(context.Background()) {
		t.Fatal("the suspect verdict must be served from cache until the unhealthy TTL lapses")
	}

	// The second consecutive failed round confirms the outage (documented:
	// /readyz turns 503 on this signal).
	clock.Advance(time.Millisecond)
	if p.Healthy(context.Background()) {
		t.Fatal("an unreachable IdP must fail the health probe after the tolerance window")
	}
	// ... and it stays unhealthy while the outage persists.
	clock.Advance(opts.UnhealthyTTL)
	if p.Healthy(context.Background()) {
		t.Fatal("the IdP is still down; the verdict must stay unhealthy")
	}
}

// TestHealthyToleratesSingleProbeFailure covers the incident that motivated
// the tolerance logic: one discovery request fails (connection dropped, as a
// restarting IdP pod produces) and the next succeeds. The replica must stay
// ready throughout, and the failure must be logged with URL and error.
func TestHealthyToleratesSingleProbeFailure(t *testing.T) {
	for _, mode := range []struct {
		name string
		drop bool
	}{{"connection dropped", true}, {"http 503", false}} {
		t.Run(mode.name, func(t *testing.T) {
			idp := newFlakyIdP(t)
			idp.dropConnection = mode.drop
			p, clock, logs := newHealthProbeProvider(t, idp)
			opts := DefaultHealthCheckOptions()

			// Exactly one request fails; the in-round retry sees a healthy IdP.
			idp.failNextRequests(1)
			clock.Advance(opts.HealthyTTL)
			if !p.Healthy(context.Background()) {
				t.Fatal("one failed attempt followed by a successful retry must keep the provider healthy")
			}
			if got := idp.requestCount(); got != 3 { // constructor fetch + failed attempt + retry
				t.Fatalf("expected the failed attempt to be retried within the round, saw %d requests", got)
			}
			// A successful round resets the failure count: the cache is
			// re-armed on the healthy TTL, not the short suspect interval.
			before := idp.requestCount()
			clock.Advance(opts.UnhealthyTTL)
			if !p.Healthy(context.Background()) || idp.requestCount() != before {
				t.Fatal("after a successful round the healthy verdict must be cached for the full healthy TTL")
			}

			// the failed attempt is diagnosable from the logs.
			line := logs.String()
			if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, "identity provider health probe failed") {
				t.Fatalf("a failed probe must be logged at warn level, got: %s", line)
			}
			if !strings.Contains(line, idp.server.URL+"/.well-known/openid-configuration") {
				t.Fatalf("the warning must name the discovery URL, got: %s", line)
			}
			if mode.drop {
				if !strings.Contains(line, "EOF") && !strings.Contains(line, "connection reset") && !strings.Contains(line, "connection refused") {
					t.Fatalf("the warning must carry the transport error detail, got: %s", line)
				}
			} else if !strings.Contains(line, "HTTP 503") {
				t.Fatalf("the warning must carry the HTTP status detail, got: %s", line)
			}
			if strings.Count(line, "identity provider health probe failed") != 1 {
				t.Fatalf("exactly one failed attempt must be logged (none on the success path), got: %s", line)
			}
		})
	}
}

// TestHealthyToleratesOneFailedRound: both attempts of a round fail but the
// IdP is back by the next (short-interval) re-check. Readiness never flipped.
func TestHealthyToleratesOneFailedRound(t *testing.T) {
	idp := newFlakyIdP(t)
	p, clock, logs := newHealthProbeProvider(t, idp)
	opts := DefaultHealthCheckOptions()

	idp.failNextRequests(2)
	clock.Advance(opts.HealthyTTL)
	if !p.Healthy(context.Background()) {
		t.Fatal("a single failed round must not flip readiness")
	}
	if got := strings.Count(logs.String(), "identity provider health probe failed"); got != 2 {
		t.Fatalf("both failed attempts must be logged, got %d warnings: %s", got, logs.String())
	}
	// The next re-check comes after the short unhealthy interval and succeeds.
	clock.Advance(opts.UnhealthyTTL)
	if !p.Healthy(context.Background()) {
		t.Fatal("a recovered IdP must keep the provider healthy")
	}
	if strings.Contains(logs.String(), "marked unreachable") {
		t.Fatalf("readiness must never have flipped, logs: %s", logs.String())
	}
}

// TestHealthyOutageAndRecovery drives the full matrix: a
// sustained outage flips the verdict within the documented bound, and after
// the IdP returns the negative verdict is re-checked on the short interval
// and clears well inside the 30s healthy TTL.
func TestHealthyOutageAndRecovery(t *testing.T) {
	idp := newFlakyIdP(t)
	p, clock, logs := newHealthProbeProvider(t, idp)
	opts := DefaultHealthCheckOptions()

	// Sustained outage: every request fails.
	idp.failNextRequests(1 << 20)
	clock.Advance(opts.HealthyTTL)
	if !p.Healthy(context.Background()) {
		t.Fatal("first failed round must be tolerated")
	}
	clock.Advance(opts.UnhealthyTTL)
	if p.Healthy(context.Background()) {
		t.Fatal("second consecutive failed round must confirm the outage")
	}
	if !strings.Contains(logs.String(), "marked unreachable") {
		t.Fatalf("the unhealthy transition must be logged, got: %s", logs.String())
	}

	// Recovery: the IdP answers again. The unhealthy verdict is not pinned
	// for the 30s healthy TTL — it is re-probed after the unhealthy interval.
	idp.failNextRequests(0)
	clock.Advance(opts.UnhealthyTTL - time.Millisecond)
	if p.Healthy(context.Background()) {
		t.Fatal("the unhealthy verdict must be cached until the unhealthy TTL lapses")
	}
	clock.Advance(time.Millisecond)
	if !p.Healthy(context.Background()) {
		t.Fatal("once the IdP answers again readiness must return to healthy on the short interval")
	}
	if opts.UnhealthyTTL >= opts.HealthyTTL {
		t.Fatalf("the unhealthy re-check interval (%s) must be shorter than the healthy TTL (%s)", opts.UnhealthyTTL, opts.HealthyTTL)
	}
	if !strings.Contains(logs.String(), "identity provider reachable again") {
		t.Fatalf("the recovery transition must be logged, got: %s", logs.String())
	}

	// Recovery also resets the failure count: a fresh single blip is again
	// tolerated rather than counted on top of the old outage.
	idp.failNextRequests(2)
	clock.Advance(opts.HealthyTTL)
	if !p.Healthy(context.Background()) {
		t.Fatal("after recovery the failure count must start from zero")
	}
}

// TestHealthyUsesCallerDeadline: when the readiness handler's context is
// already spent, the probe fails fast instead of retrying into a dead
// deadline, and the attempt is still logged.
func TestHealthyUsesCallerDeadline(t *testing.T) {
	idp := newFlakyIdP(t)
	p, clock, logs := newHealthProbeProvider(t, idp)
	opts := DefaultHealthCheckOptions()
	opts.RetryDelay = time.Hour // would hang forever if the deadline were ignored
	opts.HealthyTTL = 0
	opts.UnhealthyTTL = 0
	p.ConfigureHealthCheck(opts)

	idp.failNextRequests(1 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	clock.Advance(time.Second)
	done := make(chan bool, 1)
	go func() { done <- p.Healthy(ctx) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("a single failed round must still be tolerated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Healthy must honour the caller's context instead of sleeping through the retry delay")
	}
	if !strings.Contains(logs.String(), "identity provider health probe failed") {
		t.Fatalf("the failed attempt must be logged, got: %s", logs.String())
	}
}
