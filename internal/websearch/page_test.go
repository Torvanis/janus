package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// The page reader is a model-driven fetch: it must never reach the network
// Janus runs in. Loopback, private, link-local (cloud metadata), CGNAT and
// cluster names are refused by name, by literal address, after DNS (at
// dial), and on every redirect hop.
func TestFetchPageRefusesTheInternalNetwork(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/", "http://localhost:8080/", "http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/", "http://[::1]/", "http://[fd00::1]/", "http://100.64.0.1/",
		"http://janus.janus.svc.cluster.local/", "http://kubernetes.default.svc/", "http://intranet/",
		"http://printer.local/", "http://metadata.google.internal/", "file:///etc/passwd", "ftp://example.com/",
		"http://user:pw@example.com/", "http://example.com:6443/", "http://0.0.0.0/", "http://[::ffff:127.0.0.1]/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if checkPageURL(u) == nil {
			t.Errorf("%s allowed", raw)
		}
		if _, err := FetchPage(context.Background(), raw); err == nil {
			t.Errorf("%s fetched", raw)
		}
	}
	for _, a := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s refused", a)
		}
	}
}

// A public name that resolves (or redirects) to an internal address is
// stopped at dial time: the check runs on the address actually connected.
func TestFetchPageChecksTheDialledAddress(t *testing.T) {
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><title>secret</title><body>internal admin</body></html>"))
	}))
	defer inside.Close()
	// The dialer is the guard: point a public-looking request at the
	// loopback server by dialling its address directly.
	c := pageClient()
	req, _ := http.NewRequest(http.MethodGet, inside.URL, nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), errBlockedThisHost.Error()) {
		t.Fatalf("loopback dial allowed: %v", err)
	}
	// A redirect to an internal address is refused.
	redirect := c.CheckRedirect
	to, _ := url.Parse("http://169.254.169.254/")
	if err := redirect(&http.Request{URL: to}, []*http.Request{{}}); err == nil {
		t.Fatal("redirect to the metadata service allowed")
	}
}

// Readable text: scripts, styles and navigation dropped, headings kept,
// entities decoded, long pages cut with a note.
func TestFetchPageText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Release &amp; notes</title><style>.x{}</style><script>alert(1)</script></head>
<body><nav>Home | Docs</nav><h1>Kubernetes 1.37</h1><p>Released <b>Sept&nbsp;15</b>.</p><script>evil()</script>
<ul><li>Feature A</li><li>Feature B</li></ul><footer>© CNCF</footer></body></html>`))
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(strings.Builder)
	_, _ = bufCopy(buf, resp)
	title, text := htmlText(buf.String())
	text = collapseSpace(text)
	if title != "Release & notes" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{"# Kubernetes 1.37", "Released Sept 15 .", "Feature A", "Feature B"} {
		if !strings.Contains(strings.ReplaceAll(text, "\u00a0", " "), want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"alert", "evil", "Home | Docs", "CNCF", ".x{}"} {
		if strings.Contains(text, bad) {
			t.Fatalf("text kept %q:\n%s", bad, text)
		}
	}
	out := FormatPageForModel(&Page{URL: "https://kubernetes.io/releases/", Title: title, Text: text, Truncated: true})
	if !strings.Contains(out, "only the first part was read") || !strings.Contains(out, "(https://kubernetes.io/releases/)") {
		t.Fatalf("model text = %s", out)
	}
}

func bufCopy(b *strings.Builder, resp *http.Response) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := resp.Body.Read(buf)
		b.Write(buf[:k])
		n += int64(k)
		if err != nil {
			return n, nil
		}
	}
}
