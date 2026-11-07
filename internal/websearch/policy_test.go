package websearch

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/torvanis/janus/internal/pki"
	"github.com/torvanis/janus/internal/truststore"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func mustPolicy(t *testing.T, m map[string]string, ca string) *PagePolicy {
	t.Helper()
	p, err := ParsePagePolicy(env(m), ca)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func allowed(p *PagePolicy, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return p.checkURL(u)
}

// The default is unchanged: the public internet only.
func TestDefaultPolicyIsPublicOnly(t *testing.T) {
	p := mustPolicy(t, nil, "")
	for _, raw := range []string{"http://10.0.0.5/", "http://intranet/", "http://wiki.corp.internal/", "http://192.168.1.1/", "http://example.com:6443/"} {
		if allowed(p, raw) == nil {
			t.Errorf("%s allowed by default", raw)
		}
	}
	if err := allowed(p, "http://10.0.0.5/"); err == nil || !strings.Contains(err.Error(), "JANUS_WEB_ALLOW_PRIVATE") {
		t.Fatalf("private refusal does not name the setting: %v", err)
	}
	if allowed(p, "https://example.com/") != nil || !p.addrAllowed(netip.MustParseAddr("8.8.8.8")) || p.addrAllowed(netip.MustParseAddr("10.1.1.1")) {
		t.Fatal("public web refused by default")
	}
	if p.Summary()["default"] != true {
		t.Fatal("default policy not reported as default")
	}
}

// JANUS_WEB_ALLOW_PRIVATE opens private ranges and internal names, never
// this host, link-local or metadata.
func TestAllowPrivate(t *testing.T) {
	p := mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_PRIVATE": "true"}, "")
	for _, raw := range []string{"http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/", "http://[fd00::1]/", "http://100.64.0.1/",
		"http://intranet/", "http://wiki.corp.internal/", "http://docs.lan/", "http://janus.ns.svc.cluster.local/", "https://example.com/"} {
		if err := allowed(p, raw); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://localhost/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/", "http://0.0.0.0/", "http://[::ffff:127.0.0.1]/", "http://app.localhost/"} {
		if allowed(p, raw) == nil {
			t.Errorf("%s allowed with JANUS_WEB_ALLOW_PRIVATE", raw)
		}
	}
	for _, a := range []string{"127.0.0.1", "169.254.169.254", "::1", "fe80::1"} {
		if p.addrAllowed(netip.MustParseAddr(a)) {
			t.Errorf("dial to %s allowed", a)
		}
	}
}

func TestNetworkAndDomainLists(t *testing.T) {
	// Only one intranet range, nothing public.
	p := mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_NETWORKS": "10.20.0.0/16, 192.0.2.7", "JANUS_WEB_DENY_NETWORKS": "10.20.9.0/24"}, "")
	for a, want := range map[string]bool{"10.20.1.1": true, "192.0.2.7": true, "10.20.9.5": false, "10.30.0.1": false, "8.8.8.8": false, "127.0.0.1": false} {
		if got := p.addrAllowed(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: allowed=%v want %v", a, got, want)
		}
	}
	// Only some sites; deny wins.
	p = mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_DOMAINS": "*.mil, docs.corp.example", "JANUS_WEB_DENY_DOMAINS": "secret.army.mil"}, "")
	for raw, want := range map[string]bool{"https://www.disa.mil/": true, "https://disa.mil/": true, "https://docs.corp.example/x": true,
		"https://secret.army.mil/": false, "https://x.secret.army.mil/": false, "https://example.com/": false, "https://evilmil/": false,
		"https://notdisa.mil.evil.com/": false, "http://10.0.0.1/": false} {
		if got := allowed(p, raw) == nil; got != want {
			t.Errorf("%s: allowed=%v want %v", raw, got, want)
		}
	}
	// Ports and https-only.
	p = mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_PORTS": "443,9443", "JANUS_WEB_HTTPS_ONLY": "true"}, "")
	if allowed(p, "https://example.com:9443/") != nil || allowed(p, "https://example.com:8443/") == nil || allowed(p, "http://example.com/") == nil {
		t.Fatal("ports / https-only not applied")
	}
	if err := allowed(p, "https://example.com:8443/"); !strings.Contains(err.Error(), "443, 9443") {
		t.Fatalf("port error does not list the ports: %v", err)
	}
}

func TestParseRefusals(t *testing.T) {
	for _, m := range []map[string]string{
		{"JANUS_WEB_ALLOW_PRIVATE": "maybe"},
		{"JANUS_WEB_ALLOW_NETWORKS": "10.0.0.0/33"},
		{"JANUS_WEB_ALLOW_NETWORKS": "127.0.0.0/8"},
		{"JANUS_WEB_ALLOW_NETWORKS": "169.254.169.254"},
		{"JANUS_WEB_DENY_NETWORKS": "nope"},
		{"JANUS_WEB_ALLOW_DOMAINS": "https://x.mil/"},
		{"JANUS_WEB_ALLOW_PORTS": "70000"},
		{"JANUS_WEB_PROXY": "ftp://proxy:21"},
		{"JANUS_WEB_PROXY": "proxy.corp:3128"},
		{"JANUS_WEB_TLS_TRUST": "custom"},
		{"JANUS_WEB_TLS_TRUST": "anything"},
	} {
		if _, err := ParsePagePolicy(env(m), ""); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
	// Proxy credentials are never shown.
	p := mustPolicy(t, map[string]string{"JANUS_WEB_PROXY": "http://user:pw@proxy.corp:3128"}, "")
	if s := p.Summary()["proxy"].(string); strings.Contains(s, "pw") || !strings.Contains(s, "proxy.corp:3128") {
		t.Fatalf("summary proxy = %q", s)
	}
	if _, err := ParsePagePolicy(env(map[string]string{"JANUS_WEB_PROXY": "http://user:topsecret@:bad"}), ""); err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("proxy error leaks credentials or accepted: %v", err)
	}
}

// The dial check follows the policy: with private allowed, a private
// server is read; a redirect to loopback still is not.
func TestPrivateFetchLive(t *testing.T) {
	ip := firstPrivateIP(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Intranet</title><body><h1>Runbook</h1><p>offline docs</p></body></html>")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	base := "http://" + net.JoinHostPort(ip, port)

	def := mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_PORTS": port}, "")
	if _, err := newPageReader(def).fetch(context.Background(), base+"/"); err == nil {
		t.Fatal("private server read by default")
	}
	p := mustPolicy(t, map[string]string{"JANUS_WEB_ALLOW_PRIVATE": "true", "JANUS_WEB_ALLOW_PORTS": port + ",1"}, "")
	SetPagePolicy(p)
	defer SetPagePolicy(nil)
	page, err := FetchPage(context.Background(), base+"/")
	if err != nil || page.Title != "Intranet" || !strings.Contains(page.Text, "offline docs") {
		t.Fatalf("private page: %v %+v", err, page)
	}
	if _, err := FetchPage(context.Background(), base+"/hop"); err == nil || !strings.Contains(err.Error(), "this server itself") {
		t.Fatalf("redirect to loopback: %v", err)
	}
}

func firstPrivateIP(t *testing.T) string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && isPrivate(ip.Unmap()) && ip.Unmap().Is4() {
				return ip.Unmap().String()
			}
		}
	}
	t.Skip("no private IPv4 address on this host")
	return ""
}

// With JANUS_WEB_PROXY every read goes through the proxy, and the proxy
// is the gatekeeper for where names lead.
func TestProxyIsUsed(t *testing.T) {
	var hits atomic.Int32
	var lastURL atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastURL.Store(r.URL.String())
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Via proxy</title><body>proxied</body></html>")
	}))
	defer proxy.Close()
	p := mustPolicy(t, map[string]string{"JANUS_WEB_PROXY": proxy.URL, "JANUS_WEB_ALLOW_DOMAINS": "docs.example.mil"}, "")
	page, err := newPageReader(p).fetch(context.Background(), "http://docs.example.mil/guide")
	if err != nil || page.Title != "Via proxy" || hits.Load() != 1 || lastURL.Load() != "http://docs.example.mil/guide" {
		t.Fatalf("proxied read: %v %+v hits=%d url=%v", err, page, hits.Load(), lastURL.Load())
	}
	// Link checks still run before the proxy sees anything.
	if _, err := newPageReader(p).fetch(context.Background(), "http://other.example.com/"); err == nil || hits.Load() != 1 {
		t.Fatalf("domain outside the allow list reached the proxy: %v", err)
	}
	if _, err := newPageReader(p).fetch(context.Background(), "http://127.0.0.1/"); err == nil || hits.Load() != 1 {
		t.Fatal("loopback link reached the proxy")
	}
	// A dead proxy is named, with no direct fallback.
	dead := mustPolicy(t, map[string]string{"JANUS_WEB_PROXY": "http://127.0.0.1:1"}, "")
	SetPagePolicy(dead)
	defer SetPagePolicy(nil)
	if _, err := FetchPage(context.Background(), "https://example.com/"); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("dead proxy: %v", err)
	}
}

// custom-only trusts only JANUS_CA_BUNDLE; system does not trust it.
func TestTLSTrust(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Signed by our CA</title><body>ok</body></html>")
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	// httptest serves 127.0.0.1; the dial guard is the subject of other
	// tests, so fetch through a client without it (dial check off via proxy-less
	// transport built from the policy's TLS config).
	get := func(p *PagePolicy) error {
		c := pageClientFor(p)
		c.Transport.(*http.Transport).DialContext = (&net.Dialer{}).DialContext
		_, err := fetchPageNoCheck(c, srv.URL)
		return err
	}
	if err := get(mustPolicy(t, nil, "")); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("system trust accepted a private CA: %v", err)
	}
	only := mustPolicy(t, map[string]string{"JANUS_WEB_TLS_TRUST": "custom-only"}, ca)
	if err := get(only); err != nil {
		t.Fatalf("custom-only refused its own CA: %v", err)
	}
	custom := mustPolicy(t, map[string]string{"JANUS_WEB_TLS_TRUST": "custom"}, ca)
	if err := get(custom); err != nil {
		t.Fatalf("custom refused its CA: %v", err)
	}
}

// fetchPageNoCheck runs the fetch path past the link check (TLS tests on a
// loopback httptest server).
func fetchPageNoCheck(c *http.Client, raw string) (*Page, error) {
	saved := CurrentPagePolicy()
	SetPagePolicy(&PagePolicy{AllowNetworks: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, Ports: map[string]bool{portOf(raw): true}})
	defer SetPagePolicy(saved)
	u, _ := url.Parse(raw)
	// Bypass the hard loopback block for this test only.
	return fetchPageURL(context.Background(), CurrentPagePolicy(), c, u)
}

func portOf(raw string) string { u, _ := url.Parse(raw); return u.Port() }

// In the release the custom trusts read the shared trust store: a CA added
// to JANUS_CA_DIR (or uploaded) is trusted by chat's page reads without a
// restart, and custom-only needs no JANUS_CA_BUNDLE when the store is
// configured.
func TestTLSTrustFollowsTheSharedTrustStore(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><title>Intranet</title><body>ok</body></html>")
	}))
	defer srv.Close()
	dir := t.TempDir()
	// Start with an unrelated CA (an empty JANUS_CA_DIR is refused).
	// (httptest servers all share one certificate, so use another CA.)
	other, err := pki.NewTestCard("Unrelated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.pem"), other.CAPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := truststore.Configure(truststore.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	p, err := ParsePagePolicy(env(map[string]string{"JANUS_WEB_TLS_TRUST": "custom-only"}), "")
	if err != nil {
		t.Fatalf("custom-only with a configured trust store: %v", err)
	}
	SetPagePolicy(p)
	defer SetPagePolicy(nil)
	get := func() error {
		c := currentReader().client
		c.Transport.(*http.Transport).DialContext = (&net.Dialer{}).DialContext
		_, err := fetchPageNoCheck(c, srv.URL)
		return err
	}
	if err := get(); err == nil {
		t.Fatal("custom-only trusted a site before its CA was added")
	}
	if err := os.WriteFile(filepath.Join(dir, "intranet.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := truststore.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := get(); err != nil {
		t.Fatalf("CA added to the trust store not trusted: %v", err)
	}
	if cfg := p.tlsConfig(); cfg.VerifyConnection == nil {
		t.Fatal("page reads must run the revocation check")
	}
}
