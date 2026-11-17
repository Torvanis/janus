package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// Regression tests for the 2026-11 security review, batch 7: the console's
// resource policy (the rest of WEB-02).

const testIndex = `<!doctype html><html><head><script>
      (function () { document.documentElement.setAttribute('data-theme', 'dark'); })();
    </script></head><body><div id="root"></div><script type="module" src="/assets/index-abc.js"></script></body></html>`

func cspFor(t *testing.T, mode string) http.Header {
	t.Helper()
	assets := fstest.MapFS{"index.html": {Data: []byte(testIndex)}}
	rec := httptest.NewRecorder()
	SecurityHeaders(false, mode, assets)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))
	return rec.Header()
}

func TestConsoleResourcePolicy(t *testing.T) {
	enforced := cspFor(t, CSPEnforce)
	policy := enforced.Get("Content-Security-Policy")
	if enforced.Get("Content-Security-Policy-Report-Only") != "" {
		t.Error("enforce mode also sent a report-only policy")
	}
	// Script only from this server, plus the one inline script by its hash.
	inline := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(testIndex)[1]
	sum := sha256.Sum256([]byte(inline))
	wantScript := "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	directives := map[string]string{}
	for _, d := range strings.Split(policy, "; ") {
		name, _, _ := strings.Cut(d, " ")
		directives[name] = d
	}
	for name, want := range map[string]string{
		"default-src":     "default-src 'self'",
		"script-src":      wantScript,
		"connect-src":     "connect-src 'self'",
		"frame-src":       "frame-src 'self'",
		"frame-ancestors": "frame-ancestors 'none'",
		"object-src":      "object-src 'none'",
		"base-uri":        "base-uri 'none'",
		"form-action":     "form-action 'self'",
		"report-uri":      "report-uri /csp-report",
	} {
		if directives[name] != want {
			t.Errorf("%s = %q, want %q", name, directives[name], want)
		}
	}
	// Nothing lets script in from elsewhere or run from a string.
	if s := directives["script-src"]; strings.Contains(s, "unsafe") || strings.Contains(s, "http") || strings.Contains(s, "*") || strings.Contains(s, "data:") || strings.Contains(s, "blob:") {
		t.Errorf("script-src is too wide: %s", s)
	}

	// Report-only: the framing rules stay enforced, the rest only reports.
	ro := cspFor(t, CSPReportOnly)
	if ro.Get("Content-Security-Policy") != framingPolicy || ro.Get("Content-Security-Policy-Report-Only") != policy {
		t.Errorf("report-only headers: %q / %q", ro.Get("Content-Security-Policy"), ro.Get("Content-Security-Policy-Report-Only"))
	}
	off := cspFor(t, CSPOff)
	if off.Get("Content-Security-Policy") != framingPolicy || off.Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("off headers: %v", off)
	}
	if !ValidCSPMode("enforce") || !ValidCSPMode("report-only") || !ValidCSPMode("off") || ValidCSPMode("") || ValidCSPMode("on") {
		t.Error("mode validation")
	}
}

// The policy must allow the inline script the built index.html really has.
func TestConsolePolicyAllowsTheRealInlineScript(t *testing.T) {
	index, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Skip("web/index.html not found")
	}
	hashes := inlineScriptHashes(fstest.MapFS{"index.html": {Data: index}})
	if len(hashes) != 1 || !strings.HasPrefix(hashes[0], "sha256-") {
		t.Fatalf("inline scripts in index.html: %v (the module script has a src and is not inline)", hashes)
	}
}

// A chat preview is a page of its own with a policy of its own: under the
// console's policy a srcdoc frame could run no script at all.
func TestPreviewFramePolicy(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/chat/preview-frame", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("preview frame: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src data: blob:", "form-action 'none'", "sandbox allow-scripts allow-pointer-lock allow-modals", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("preview policy lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "allow-same-origin") || regexp.MustCompile(`https?:|\*`).MatchString(csp) {
		t.Errorf("preview policy reaches a host or keeps an origin: %s", csp)
	}
	if rec.Header().Get("Content-Security-Policy-Report-Only") != "" || rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("preview frame headers: %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "janusPreviewReady") || !strings.Contains(rec.Body.String(), "janusPreviewDocument") {
		t.Error("preview frame does not ask for its document")
	}
	// The same policy is written into the previewed page by the web app.
	ts, err := os.ReadFile("../../web/src/routes/chat/chat-files.ts")
	if err != nil {
		t.Skip("web source not found")
	}
	m := regexp.MustCompile(`(?s)export const PREVIEW_POLICY =\n(.*?);\n`).FindSubmatch(ts)
	if m == nil {
		t.Fatal("PREVIEW_POLICY not found in chat-files.ts")
	}
	// One quoted piece per line, joined with +; the formatter picks the quote.
	joined := ""
	for _, line := range strings.Split(string(m[1]), "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), " +")
		if len(line) < 2 {
			t.Fatalf("unexpected line in PREVIEW_POLICY: %q", line)
		}
		joined += line[1 : len(line)-1]
	}
	if joined != PreviewPolicy {
		t.Errorf("the web app's preview policy differs from the server's:\n web: %s\n  go: %s", joined, PreviewPolicy)
	}
}

func TestCSPReportIsLoggedAndBounded(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	h.server.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/csp-report", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/csp-report")
		req.RemoteAddr = "198.51.100.7:4000"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	// No session and no CSRF header: a browser sends these on its own.
	if code := post(`{"csp-report":{"effective-directive":"script-src-elem","blocked-uri":"https://evil.example/x.js","document-uri":"https://janus.example/chat/abc?q=secret+search#frag","line-number":3,"disposition":"report"}}`); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	out := logs.String()
	if !strings.Contains(out, "content security policy violation") || !strings.Contains(out, "script-src-elem") || !strings.Contains(out, "https://evil.example/x.js") {
		t.Fatalf("report not logged: %s", out)
	}
	if strings.Contains(out, "secret+search") || strings.Contains(out, "frag") {
		t.Errorf("the page's query string was logged: %s", out)
	}
	// Junk is accepted and ignored; a flood from one address stops being logged.
	logs.Reset()
	if post(`not json`) != http.StatusNoContent || post(`{"csp-report":{"blocked-uri":"`+strings.Repeat("a", 40000)+`"}}`) != http.StatusNoContent || logs.Len() != 0 {
		t.Errorf("junk report: logged %d bytes", logs.Len())
	}
	for i := 0; i < 200; i++ {
		post(`{"csp-report":{"effective-directive":"img-src","blocked-uri":"https://a.example"}}`)
	}
	if n := strings.Count(logs.String(), "content security policy violation"); n == 0 || n > cspReportsPerAddressPerMin {
		t.Errorf("%d of 200 reports from one address were logged, want at most %d", n, cspReportsPerAddressPerMin)
	}
}
