package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 4: findings that
// needed no decision. (The PDF page-tree loop is in internal/chatfile, the
// redirect rule in internal/safehttp, the capture-download audit record in
// troubleshoot_test.go.)

// AUTH-07: a backslash is a slash to a browser, so "/\host" left the site.
func TestSafeRedirectStaysOnTheSite(t *testing.T) {
	for target, want := range map[string]string{
		"":                      "/dashboard",
		"/chat":                 "/chat",
		"/admin/models?tab=a#x": "/admin/models?tab=a#x",
		"https://evil.example":  "/dashboard",
		"//evil.example":        "/dashboard",
		`/\evil.example`:        "/dashboard",
		`/\/evil.example`:       "/dashboard",
		`/chat\..\x`:            "/dashboard",
		"/\tevil.example":       "/dashboard",
		"/\n/evil.example":      "/dashboard",
		"/%5Cevil.example":      "/%5Cevil.example", // an encoded backslash is a path character, not a separator
	} {
		if got := safeRedirect(target); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", target, got, want)
		}
	}
}

// INFRA-06 through the proxy: a provider that answers with a redirect to
// another host does not get the request, the key or the prompt sent there.
func TestProxyDoesNotFollowAProviderRedirectToAnotherHost(t *testing.T) {
	h := newHarness(t)
	var got []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from the collector"}}]}`))
	}))
	defer collector.Close()
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer away.Close()
	if err := h.store.UpdateUpstream(t.Context(), h.model.UpstreamID, away.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("private text", false))
	if rec.Code < 500 || strings.Contains(rec.Body.String(), "from the collector") {
		t.Fatalf("got %d %s, want an upstream error", rec.Code, rec.Body.String())
	}
	if len(got) != 0 {
		t.Fatalf("the other host received: %v", got)
	}
}

// What the checks find in a request is recorded even when the request then
// fails at the provider. It was recorded only when the request completed.
func TestSecgwViolationIsRecordedWhenTheProviderFails(t *testing.T) {
	h := newHarness(t)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "observe", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeObserve},
	}})
	h.upstream.Close() // the provider is unreachable
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("key "+testAWSKey, false))
	if rec.Code < 500 {
		t.Fatalf("got %d, want an upstream error", rec.Code)
	}
	if vs := h.waitForViolations(t, 1); len(vs) != 1 || vs[0].Kind != string(store.SecgwCheckSecrets) {
		t.Fatalf("violations recorded: %+v", vs)
	}
}

// WEB-02 (the part that changes nothing a page relies on): no framing, no
// content sniffing, and HSTS when the deployment is served over HTTPS.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/api/v1/config", "/v1/models", "/no/such/page"} {
		rec := h.do(http.MethodGet, path, nil)
		hd := rec.Header()
		if hd.Get("X-Frame-Options") != "DENY" || hd.Get("Content-Security-Policy") != "frame-ancestors 'none'; frame-src 'self'" || hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", path, hd)
		}
		if hd.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS sent by a deployment that is not on HTTPS", path)
		}
	}
	rec := httptest.NewRecorder()
	SecurityHeaders(true, CSPOff, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("HSTS on HTTPS = %q", got)
	}
}
