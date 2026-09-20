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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testServiceAccount builds a syntactically real service-account JSON key with
// a freshly generated RSA key, pointing token_uri at the given URL.
func testServiceAccount(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"client_email": "janus-test@example.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    tokenURI,
		"project_id":   "proj-1",
	}
	encoded, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("encode service account: %v", err)
	}
	return string(encoded), key
}

func TestVertexPrepareUsesCachedToken(t *testing.T) {
	a := &Vertex{tokens: map[string]*cachedToken{
		"up-v": {value: "cached-token", expiresAt: time.Now().UTC().Add(time.Hour)},
	}}
	up := Upstream{
		ID:      "up-v",
		BaseURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1",
		APIKey:  `{"unused":"the cache makes credential parsing unnecessary"}`,
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	req := &Request{
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Header:    hdr,
		Model:     "google/gemini-1.5-pro",
		Streaming: true,
		Body:      []byte(`{"model":"google/gemini-1.5-pro","stream":true,"messages":[]}`),
	}

	prep, err := a.Prepare(up, req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi/v1/chat/completions"
	if prep.URL != want {
		t.Fatalf("URL = %q, want %q", prep.URL, want)
	}
	if got := prep.Header.Get("Authorization"); got != "Bearer cached-token" {
		t.Fatalf("Authorization = %q, want the cached bearer token", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(prep.Body, &payload); err != nil {
		t.Fatalf("prepared body is not valid JSON: %v", err)
	}
	opts, ok := payload["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("streaming requests must opt into usage reporting: %s", prep.Body)
	}
}

func TestVertexAccessTokenMintsAndCaches(t *testing.T) {
	var (
		calls   int32
		key     *rsa.PrivateKey
		wantAud string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, "wrong grant_type: "+got, http.StatusBadRequest)
			return
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		if len(parts) != 3 {
			http.Error(w, "malformed assertion", http.StatusBadRequest)
			return
		}
		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil || string(headerJSON) != `{"alg":"RS256","typ":"JWT"}` {
			http.Error(w, "unexpected JWT header", http.StatusBadRequest)
			return
		}
		claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			http.Error(w, "bad claims encoding", http.StatusBadRequest)
			return
		}
		var claims map[string]any
		if err := json.Unmarshal(claimsJSON, &claims); err != nil {
			http.Error(w, "bad claims JSON", http.StatusBadRequest)
			return
		}
		// aud must name the endpoint that receives the exchange — this
		// server's own URL, taken from the key's token_uri — not the
		// hardcoded public Google default (regression).
		if claims["iss"] != "janus-test@example.iam.gserviceaccount.com" ||
			claims["aud"] != wantAud ||
			claims["scope"] != "https://www.googleapis.com/auth/cloud-platform" {
			http.Error(w, "wrong claims", http.StatusBadRequest)
			return
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			http.Error(w, "bad signature encoding", http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			http.Error(w, "assertion signature does not verify", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-token","expires_in":3600}`))
	}))
	defer srv.Close()

	saJSON, generated := testServiceAccount(t, srv.URL)
	key = generated
	wantAud = srv.URL

	a := &Vertex{tokens: map[string]*cachedToken{}}
	up := Upstream{ID: "up-mint", APIKey: saJSON}

	tok, err := a.accessToken(context.Background(), up, srv.Client())
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if tok != "minted-token" {
		t.Fatalf("token = %q, want minted-token", tok)
	}

	tok2, err := a.accessToken(context.Background(), up, srv.Client())
	if err != nil {
		t.Fatalf("accessToken (cached): %v", err)
	}
	if tok2 != "minted-token" {
		t.Fatalf("cached token = %q", tok2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (second call must use the cache)", got)
	}
}

// TestVertexPrepareThreadsContextAndClient is the regression for token minting
// on the proxy path: Prepare used to call accessToken(context.Background(),
// up, nil), so caller cancellation never propagated and the mint bypassed the
// gateway's CA-aware upstream client.
func TestVertexPrepareThreadsContextAndClient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-token","expires_in":3600}`))
	}))
	defer srv.Close()
	saJSON, _ := testServiceAccount(t, srv.URL)

	newReq := func(ctx context.Context, client *http.Client) *Request {
		hdr := http.Header{}
		hdr.Set("Content-Type", "application/json")
		return &Request{
			Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr,
			Model: "google/gemini-1.5-pro", Body: []byte(`{"messages":[]}`),
			Context: ctx, Client: client,
		}
	}

	// A cancelled request context must abort the mint before it happens.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Vertex{tokens: map[string]*cachedToken{}}
	if _, err := a.Prepare(Upstream{ID: "up-ctx", APIKey: saJSON, BaseURL: srv.URL}, newReq(cancelled, srv.Client())); err == nil {
		t.Fatal("Prepare with a cancelled request context must fail instead of minting")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("token endpoint hit %d times under a cancelled context, want 0", got)
	}

	// With a live context the mint runs through the supplied client.
	a = &Vertex{tokens: map[string]*cachedToken{}}
	prep, err := a.Prepare(Upstream{ID: "up-live", APIKey: saJSON, BaseURL: srv.URL}, newReq(context.Background(), srv.Client()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := prep.Header.Get("Authorization"); got != "Bearer minted-token" {
		t.Fatalf("Authorization = %q, want the freshly minted token", got)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1", got)
	}
}

func TestVertexExtractUsage(t *testing.T) {
	a := &Vertex{tokens: map[string]*cachedToken{}}

	// OpenAI-compatible surface.
	u, ok := a.ExtractUsage([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3},"choices":[{"finish_reason":"stop"}]}`))
	if !ok || u.TokensIn != 7 || u.TokensOut != 3 || u.FinishReason != "stop" {
		t.Fatalf("openai-shape usage = %+v ok=%v", u, ok)
	}

	// Native usageMetadata block.
	u, ok = a.ExtractUsage([]byte(`{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4,"cachedContentTokenCount":2}}`))
	if !ok || u.TokensIn != 11 || u.TokensOut != 4 || u.TokensCached != 2 {
		t.Fatalf("native usage = %+v ok=%v", u, ok)
	}

	if _, ok := a.ExtractUsage([]byte(`{}`)); ok {
		t.Fatal("zero usage must not be reported")
	}
}

func TestVertexStreamCollector(t *testing.T) {
	c := (&Vertex{tokens: map[string]*cachedToken{}}).NewStreamCollector()
	c.Feed([]byte(`data: {"choices":[{"delta":{"content":"hel"}}]}`))
	c.Feed([]byte(`data: {"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":9}}`))
	c.Feed([]byte("data: [DONE]"))

	u := c.Usage()
	if !u.Reported || u.TokensIn != 6 || u.TokensOut != 9 {
		t.Fatalf("streamed usage = %+v", u)
	}
}

func TestParseServiceAccountErrors(t *testing.T) {
	if _, err := parseServiceAccount("not json"); err == nil {
		t.Fatal("non-JSON credentials must be rejected")
	}
	if _, err := parseServiceAccount(`{"client_email":"x@example.com"}`); err == nil {
		t.Fatal("credentials without private_key must be rejected")
	}
}

func TestSignServiceAccountJWT(t *testing.T) {
	saJSON, key := testServiceAccount(t, "")
	sa, err := parseServiceAccount(saJSON)
	if err != nil {
		t.Fatalf("parseServiceAccount: %v", err)
	}
	assertion, err := signServiceAccountJWT(sa, "https://oauth2.googleapis.com/token")
	if err != nil {
		t.Fatalf("signServiceAccountJWT: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion is not a three-part JWT: %q", assertion)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature encoding: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("assertion signature does not verify against the key: %v", err)
	}
}
