package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// A minimal OIDC issuer: discovery, JWKS, token. Enough to drive the real
// adapter through /auth/start/{slug} → callback with a signed ID token.
type tinyIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	lastNonce string
	claims    func(nonce string) map[string]any
}

func newTinyIdP(t *testing.T) *tinyIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &tinyIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": idp.srv.URL, "authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint": idp.srv.URL + "/token", "jwks_uri": idp.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := &idp.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1"})
		c, _ := json.Marshal(idp.claims(idp.lastNonce))
		in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
		sum := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "id_token": in + "." + base64.RawURLEncoding.EncodeToString(sig)})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func bizLicense(t *testing.T, h *harness) {
	t.Helper()
	exp := time.Now().Add(24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-IDP", Org: "T", IssuedTo: "t@t", Edition: license.EditionBusiness, Features: license.BusinessFeatures,
		Seats: 100, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
}

func idpHarness(t *testing.T) (*harness, *tinyIdP) {
	t.Helper()
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	idp := newTinyIdP(t)
	h.server.Config.PublicURL = "http://janus.test"
	h.server.Registry = auth.NewRegistry(h.store, h.server.Cipher, idp.srv.Client(), "http://janus.test", auth.NewDBOIDCStateStore(h.store), nil)
	return h, idp
}

func TestIdentityProviderAdminCRUDAndGate(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	// Community: list works (shows the upsell), create is 402.
	body := `{"slug":"okta","name":"Okta","issuer_url":"https://okta.example.com","client_id":"c","client_secret":"s"}`
	rec := h.do(http.MethodPost, "/api/v1/admin/identity-providers", json.RawMessage(body))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("community create: want 402, got %d %s", rec.Code, rec.Body)
	}
	if rec = h.do(http.MethodGet, "/api/v1/admin/identity-providers", nil); rec.Code != 200 || decodeBody(t, rec)["licensed"] != false {
		t.Fatalf("community list: %d %s", rec.Code, rec.Body)
	}

	bizLicense(t, h)
	rec = h.do(http.MethodPost, "/api/v1/admin/identity-providers", json.RawMessage(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decodeBody(t, rec)
	if created["has_secret"] != true || created["callback_url"] != h.server.Config.PublicURL+"/auth/callback/okta" {
		t.Fatalf("create body: %v", created)
	}
	if _, ok := created["client_secret"]; ok {
		t.Fatal("secret must never be returned")
	}
	// Duplicate slug → 409. Bad slug → 400.
	if rec = h.do(http.MethodPost, "/api/v1/admin/identity-providers", json.RawMessage(body)); rec.Code != http.StatusConflict {
		t.Fatalf("dup: %d", rec.Code)
	}
	if rec = h.do(http.MethodPost, "/api/v1/admin/identity-providers", json.RawMessage(strings.Replace(body, `"okta"`, `"Bad Slug"`, 1))); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad slug: %d", rec.Code)
	}
	// Update without secret keeps it; disabled providers vanish from /auth/providers.
	id := created["id"].(string)
	rec = h.do(http.MethodPut, "/api/v1/admin/identity-providers/"+id, json.RawMessage(`{"slug":"okta","name":"Okta Prod","issuer_url":"https://okta.example.com","client_id":"c","enabled":false}`))
	if rec.Code != 200 || decodeBody(t, rec)["has_secret"] != true || decodeBody(t, rec)["name"] != "Okta Prod" {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if rec = h.do(http.MethodGet, "/auth/providers", nil); strings.Contains(rec.Body.String(), "okta") {
		t.Fatalf("disabled provider listed publicly: %s", rec.Body)
	}
	if rec = h.do(http.MethodDelete, "/api/v1/admin/identity-providers/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = h.do(http.MethodDelete, "/api/v1/admin/identity-providers/"+id, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}

func TestIdentityProviderEndToEndSignIn(t *testing.T) {
	h, idp := idpHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateIdentityProvider(ctx, h.server.Cipher, store.IdentityProviderInput{
		Slug: "acme", Name: "Acme ID", IssuerURL: idp.srv.URL, ClientID: "janus", ClientSecret: "shh",
		AdminGroups: []string{"Janus-Admins"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Listed publicly with the per-slug start URL.
	rec := h.do(http.MethodGet, "/auth/providers?redirect_uri=/models", nil)
	if !strings.Contains(rec.Body.String(), `"url":"/auth/start/acme?redirect_uri=%2Fmodels"`) {
		t.Fatalf("providers: %s", rec.Body)
	}
	// Start → 302 to the IdP with our per-slug callback.
	rec = h.do(http.MethodGet, "/auth/start/acme?redirect_uri=/models", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), idp.srv.URL+"/authorize") || loc.Query().Get("redirect_uri") != "http://janus.test/auth/callback/acme" {
		t.Fatalf("authorize redirect: %s", loc)
	}
	idp.lastNonce = loc.Query().Get("nonce")
	idp.claims = func(nonce string) map[string]any {
		return map[string]any{"iss": idp.srv.URL, "aud": "janus", "sub": "42", "email": "ada@acme.example", "name": "Ada",
			"groups": []string{"Janus-Admins"}, "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce}
	}
	// Callback → session, redirect to /models.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback/acme?code=abc&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cbRec := httptest.NewRecorder()
	h.handler.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusFound || cbRec.Header().Get("Location") != "/models" {
		t.Fatalf("callback: %d %s loc=%s", cbRec.Code, cbRec.Body, cbRec.Header().Get("Location"))
	}
	// Subject is namespaced by slug; per-provider admin group promoted her.
	u, err := h.store.UserByAuthID(ctx, "acme:42")
	if err != nil {
		t.Fatalf("user by namespaced subject: %v", err)
	}
	if u.Email != "ada@acme.example" || !u.AdminViaGroup {
		t.Fatalf("user: %+v", u)
	}
	// Unknown slug → login error page, not a 500.
	if rec = h.do(http.MethodGet, "/auth/start/nope", nil); rec.Code >= 500 {
		t.Fatalf("unknown slug: %d", rec.Code)
	}
}

func TestIdentityProviderTestEndpoint(t *testing.T) {
	h, idp := idpHarness(t)
	rec := h.do(http.MethodPost, "/api/v1/admin/identity-providers/test", map[string]string{"issuer_url": idp.srv.URL})
	if rec.Code != 200 || decodeBody(t, rec)["ok"] != true || decodeBody(t, rec)["issuer"] != idp.srv.URL {
		t.Fatalf("test ok: %d %s", rec.Code, rec.Body)
	}
	rec = h.do(http.MethodPost, "/api/v1/admin/identity-providers/test", map[string]string{"issuer_url": "http://127.0.0.1:1"})
	if rec.Code != 200 || decodeBody(t, rec)["ok"] != false {
		t.Fatalf("test unreachable: %d %s", rec.Code, rec.Body)
	}
}
