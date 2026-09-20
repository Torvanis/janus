package adapter

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func init() { Register(&Vertex{tokens: map[string]*cachedToken{}}) }

// Vertex talks to Google Vertex AI. The upstream's encrypted credential holds a
// service-account JSON key; Janus signs a JWT assertion with it and exchanges
// that for a short-lived OAuth2 access token, which it caches until just before
// expiry.
//
// NOTE: Google Vertex AI is a paid cloud service; the customer supplies their
// own GCP project and service account.
type Vertex struct {
	mu     sync.Mutex
	tokens map[string]*cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	ProjectID   string `json:"project_id"`
}

// Type returns the stored adapter enum value.
func (a *Vertex) Type() string { return "vertex" }

// Discover lists publisher models available to the project.
func (a *Vertex) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	token, err := a.accessToken(ctx, up, client)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(up.BaseURL, "/") + "/publishers/google/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build Vertex discovery request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	body, err := doDiscovery(client, req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
		PublisherModels []struct {
			Name string `json:"name"`
		} `json:"publisherModels"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse Vertex model list: %w", err)
	}
	out := []DiscoveredModel{}
	for _, m := range payload.Models {
		if name := lastPathSegment(m.Name); name != "" {
			out = append(out, DiscoveredModel{Name: name, Modalities: guessModalities(name)})
		}
	}
	for _, m := range payload.PublisherModels {
		if name := lastPathSegment(m.Name); name != "" {
			out = append(out, DiscoveredModel{Name: name, Modalities: guessModalities(name)})
		}
	}
	return out, nil
}

func lastPathSegment(name string) string {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// Prepare targets the Vertex OpenAI-compatible endpoint and attaches a bearer
// access token minted from the service account.
func (a *Vertex) Prepare(up Upstream, req *Request) (*Prepared, error) {
	// Derive from the proxied request's context so a client disconnect
	// cancels the token mint, while still bounding the mint itself to 15s
	// even when the caller allows a much longer total exchange.
	baseCtx := req.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	defer cancel()
	token, err := a.accessToken(ctx, up, req.Client)
	if err != nil {
		return nil, err
	}
	header := cloneHeader(req.Header)
	header.Set("Authorization", "Bearer "+token)
	body := req.Body
	if req.Streaming && isJSON(header) {
		body = ensureStreamUsage(body)
	}
	target := strings.TrimRight(up.BaseURL, "/") + "/endpoints/openapi" + req.Path
	return &Prepared{URL: target, Header: header, Body: body}, nil
}

// ExtractUsage understands both the OpenAI-compatible surface and the native
// usageMetadata block.
func (a *Vertex) ExtractUsage(body []byte) (Usage, bool) {
	if u, ok := parseOpenAIUsage(body); ok {
		return u, true
	}
	var env struct {
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			CachedContentTokens  int64 `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, false
	}
	u := Usage{
		TokensIn:     env.UsageMetadata.PromptTokenCount,
		TokensOut:    env.UsageMetadata.CandidatesTokenCount,
		TokensCached: env.UsageMetadata.CachedContentTokens,
	}
	u.Reported = u.TokensIn > 0 || u.TokensOut > 0
	return u, u.Reported
}

// NewStreamCollector accumulates counts across streamed frames.
func (a *Vertex) NewStreamCollector() StreamCollector { return &vertexStreamCollector{adapter: a} }

// TransformResponse is a pass-through. Prepare targets Vertex's
// /endpoints/openapi surface, which answers in the OpenAI chat.completion
// shape (JSON and SSE alike), so no response-side translation is needed —
// unlike Anthropic and Bedrock, whose native responses must be rewritten.
// Only usage extraction tolerates the native usageMetadata block, for the
// rare deployments that surface it.
func (a *Vertex) TransformResponse(req *Request, body []byte) ([]byte, error) {
	return PassthroughResponse(req, body)
}

// NewStreamTransformer is a pass-through for the same reason.
func (a *Vertex) NewStreamTransformer(*Request, http.Header) StreamTransformer {
	return PassthroughStream{}
}

type vertexStreamCollector struct {
	adapter *Vertex
	usage   Usage
}

func (c *vertexStreamCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	if u, found := c.adapter.ExtractUsage(payload); found {
		c.usage.MergeStream(u)
	}
}

func (c *vertexStreamCollector) Usage() Usage { return c.usage }

// accessToken returns a cached OAuth2 token, minting a new one when needed.
func (a *Vertex) accessToken(ctx context.Context, up Upstream, client *http.Client) (string, error) {
	a.mu.Lock()
	if tok, ok := a.tokens[up.ID]; ok && time.Now().UTC().Before(tok.expiresAt) {
		a.mu.Unlock()
		return tok.value, nil
	}
	a.mu.Unlock()

	sa, err := parseServiceAccount(up.APIKey)
	if err != nil {
		return "", err
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	// The assertion's aud must name the endpoint the exchange is sent to:
	// a key with a custom token_uri (private Google endpoints, sovereign
	// clouds) is rejected when aud stays pinned to the public default.
	assertion, err := signServiceAccountJWT(sa, tokenURI)
	if err != nil {
		return "", err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build Vertex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange Vertex service-account assertion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read Vertex token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google rejected the service-account assertion (HTTP %d); check the stored credential", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("parse Vertex token response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("google returned no access token for this service account")
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	a.mu.Lock()
	a.tokens[up.ID] = &cachedToken{value: payload.AccessToken, expiresAt: time.Now().UTC().Add(ttl - 60*time.Second)}
	a.mu.Unlock()
	return payload.AccessToken, nil
}

func parseServiceAccount(raw string) (*serviceAccount, error) {
	var sa serviceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, fmt.Errorf("vertex credentials must be the service-account JSON key: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("vertex service-account key is missing client_email or private_key")
	}
	return &sa, nil
}

func signServiceAccountJWT(sa *serviceAccount, tokenURI string) (string, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("vertex service-account private_key is not valid PEM")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("vertex service-account key is not an RSA key")
		}
		key = rsaKey
	} else if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else {
		return "", fmt.Errorf("parse vertex service-account private key: %w", err)
	}

	now := time.Now().UTC()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode vertex assertion claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign vertex assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
