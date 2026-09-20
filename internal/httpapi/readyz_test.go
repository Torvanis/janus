package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/torvanis/janus/internal/auth"
)

// readyzIdP is a controllable OIDC discovery endpoint for readiness tests:
// while down is set, discovery requests are dropped at the TCP level, which is
// what a restarting IdP pod looks like from the gateway ("connection
// refused" / EOF).
type readyzIdP struct {
	server *httptest.Server
	mu     sync.Mutex
	down   bool
	// failNext drops exactly that many upcoming requests, then recovers.
	failNext int
}

func newReadyzIdP(t *testing.T) *readyzIdP {
	t.Helper()
	idp := &readyzIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		fail := idp.down || idp.failNext > 0
		if idp.failNext > 0 {
			idp.failNext--
		}
		idp.mu.Unlock()
		if fail {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			http.Error(w, "down", http.StatusServiceUnavailable)
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

func (i *readyzIdP) setDown(down bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.down = down
}

func (i *readyzIdP) dropNext(n int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failNext = n
}

// newOIDCReadyzHarness wires a real OIDCProvider into the gateway harness with
// dev-auth off, so /readyz and /auth/health exercise the documented identity
// provider check. Both cache TTLs are zero so every request runs one probe
// round; the failure threshold keeps the production value, which is what the
// tolerance assertions below are about.
func newOIDCReadyzHarness(t *testing.T) (*harness, *readyzIdP) {
	t.Helper()
	h := newHarness(t)
	idp := newReadyzIdP(t)
	client := idp.server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	p, err := auth.NewOIDCProvider(t.Context(), auth.OIDCConfig{
		ProviderURL: idp.server.URL, ClientID: "janus-client", ClientSecret: "s",
		RedirectURL: "http://janus.test/auth/callback",
	}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	p.UseLogger(h.server.Logger)
	opts := auth.DefaultHealthCheckOptions()
	opts.HealthyTTL, opts.UnhealthyTTL, opts.RetryDelay = 0, 0, 0
	p.ConfigureHealthCheck(opts)
	h.server.Config.DevAuthEnabled = false
	h.server.OIDC = p
	return h, idp
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func idpCheck(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeBody(t, rec)
	checks, _ := body["checks"].(map[string]any)
	idp, _ := checks["identity_provider"].(map[string]any)
	if idp == nil {
		t.Fatalf("/readyz body has no identity_provider check: %s", rec.Body.String())
	}
	return idp
}

// TestReadyzToleratesTransientIdPFailure: one dropped discovery
// request must not take the replica out of rotation.
func TestReadyzToleratesTransientIdPFailure(t *testing.T) {
	h, idp := newOIDCReadyzHarness(t)

	if rec := h.get("/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz with a healthy IdP returned %d: %s", rec.Code, rec.Body.String())
	}

	// A single connection failure: the in-round retry succeeds.
	idp.dropNext(1)
	rec := h.get("/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz must stay 200 across a single failed probe, got %d: %s", rec.Code, rec.Body.String())
	}
	if idpCheck(t, rec)["ok"] != true {
		t.Fatalf("identity_provider check must stay ok across a blip: %s", rec.Body.String())
	}

	// A whole failed round (both attempts) is still tolerated once.
	idp.dropNext(2)
	if rec := h.get("/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz must stay 200 after one failed probe round, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := h.get("/auth/health"); rec.Code != http.StatusOK {
		t.Fatalf("/auth/health must share the tolerant verdict, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReadyzFailsOnSustainedIdPOutageAndRecovers: a real
// outage still flips readiness to 503 with detail "unreachable" once the
// tolerance window is exhausted, and readiness clears as soon as a probe
// succeeds again.
func TestReadyzFailsOnSustainedIdPOutageAndRecovers(t *testing.T) {
	h, idp := newOIDCReadyzHarness(t)
	threshold := auth.DefaultHealthCheckOptions().FailureThreshold

	idp.setDown(true)
	// The first threshold-1 rounds are tolerated ...
	for i := 1; i < threshold; i++ {
		if rec := h.get("/readyz"); rec.Code != http.StatusOK {
			t.Fatalf("round %d: /readyz flipped to %d before the tolerance window was exhausted: %s", i, rec.Code, rec.Body.String())
		}
	}
	// ... and the one that reaches the threshold confirms the outage.
	rec := h.get("/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz must be 503 during a sustained IdP outage, got %d: %s", rec.Code, rec.Body.String())
	}
	check := idpCheck(t, rec)
	if check["ok"] != false || check["detail"] != "unreachable" {
		t.Fatalf("identity_provider check = %v, want ok=false detail=unreachable", check)
	}
	body := decodeBody(t, rec)
	if db, _ := body["checks"].(map[string]any)["database"].(map[string]any); db["ok"] != true {
		t.Fatalf("the database check must be unaffected by the IdP outage: %s", rec.Body.String())
	}
	if rec := h.get("/auth/health"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/auth/health must report the outage too, got %d: %s", rec.Code, rec.Body.String())
	}

	// Recovery is immediate on the next probe (the negative verdict is not
	// pinned for the healthy TTL).
	idp.setDown(false)
	rec = h.get("/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz must return to 200 once the IdP answers again, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := h.get("/auth/health"); rec.Code != http.StatusOK {
		t.Fatalf("/auth/health must clear with the recovered verdict, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReadyzDevAuthUnchanged: dev-auth mode never consults an IdP.
func TestReadyzDevAuthUnchanged(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz in dev-auth mode returned %d: %s", rec.Code, rec.Body.String())
	}
	check := idpCheck(t, rec)
	if check["ok"] != true || check["detail"] != "local evaluation sign-in" {
		t.Fatalf("dev-auth identity_provider check = %v", check)
	}
}

// With neither an IdP nor dev auth, local accounts are the sign-in path:
// the replica is ready as long as the database answers.
func TestReadyzLocalAccountsOnly(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DevAuthEnabled = false
	h.server.OIDC = nil
	rec := h.get("/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz with local accounts only returned %d: %s", rec.Code, rec.Body.String())
	}
	if check := idpCheck(t, rec); check["ok"] != true || check["detail"] != "local accounts" {
		t.Fatalf("identity_provider check = %v", check)
	}
}
