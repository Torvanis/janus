package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Content security policy modes (JANUS_CONTENT_SECURITY_POLICY).
const (
	// CSPEnforce: the browser refuses what the policy does not allow.
	CSPEnforce = "enforce"
	// CSPReportOnly: nothing is refused; the browser reports what would be.
	CSPReportOnly = "report-only"
	// CSPOff: only the framing rules are sent.
	CSPOff = "off"
)

// ValidCSPMode reports whether m is one of the modes.
func ValidCSPMode(m string) bool { return m == CSPEnforce || m == CSPReportOnly || m == CSPOff }

// cspReportPath is where browsers post what the policy stopped (or, in
// report-only mode, would have stopped).
const cspReportPath = "/csp-report"

// previewFramePath is the page the chat's HTML preview is loaded from.
const previewFramePath = "/chat/preview-frame"

// framingPolicy is always enforced: the console is never shown inside
// another site's page, and the only thing it loads into a frame is its own
// preview page. The second rule is what stops a previewed page from sending
// its frame to another site (a document cannot forbid its own navigation;
// only the page that holds the frame can).
const framingPolicy = "frame-ancestors 'none'; frame-src 'self'"

// consolePolicy is the resource policy for the console. Model output is
// rendered into these pages (Markdown, diagrams, math); the renderer is
// built not to let any of it run as script, and this is the backstop if it
// ever does: a script that did not come from this server does not run, and
// nothing can be sent to another server.
//
//   - script-src: this server's files, plus the one inline script in
//     index.html (it sets the theme before first paint), allowed by hash.
//   - style-src allows inline styles: React, the diagram renderer and the
//     math renderer all write them. Styles cannot run code.
//   - img-src allows any https/http image because Chat shows a remote image
//     after the person clicks "Load image from <host>".
//   - connect-src 'self': fetch, EventSource and WebSocket only to Janus.
func consolePolicy(scriptHashes []string) string {
	script := "script-src 'self'"
	for _, h := range scriptHashes {
		script += " '" + h + "'"
	}
	return strings.Join([]string{
		"default-src 'self'",
		script,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https: http:",
		"media-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"worker-src 'self' blob:",
		"frame-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"report-uri " + cspReportPath,
	}, "; ")
}

var inlineScript = regexp.MustCompile(`(?is)<script>(.*?)</script>`)

// inlineScriptHashes returns the CSP hash sources for the inline scripts in
// the built index.html, so the policy follows the file instead of a
// constant someone must remember to update.
func inlineScriptHashes(assets fs.FS) []string {
	if assets == nil {
		return nil
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		sum := sha256.Sum256(m[1])
		out = append(out, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	}
	return out
}

// SecurityHeaders sets, on every response:
//
//   - nosniff: a response is what its Content-Type says.
//   - The framing rules (framingPolicy), always enforced.
//   - The console's resource policy (consolePolicy), enforced or report-only
//     by mode, or left out when mode is off.
//   - Strict-Transport-Security when the deployment is served over HTTPS
//     (secure cookies on), so a browser that has been here does not try
//     plain HTTP again. The gateway's own host only: sibling hosts are not
//     this server's to decide for.
//
// A handler that sets its own Content-Security-Policy (attachment downloads,
// the preview page) replaces the one set here.
func SecurityHeaders(https bool, mode string, assets fs.FS) func(http.Handler) http.Handler {
	full := consolePolicy(inlineScriptHashes(assets))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			switch mode {
			case CSPEnforce:
				h.Set("Content-Security-Policy", full)
			case CSPReportOnly:
				h.Set("Content-Security-Policy", framingPolicy)
				h.Set("Content-Security-Policy-Report-Only", full)
			default:
				h.Set("Content-Security-Policy", framingPolicy)
			}
			if https {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PreviewPolicy is the policy a chat preview runs under: what is written
// into the page itself (inline, data: and blob:) and no host at all. The
// same text is in web/src/routes/chat/chat-files.ts (PREVIEW_POLICY), which
// writes it into the previewed page as well; a test keeps the two equal.
const PreviewPolicy = "default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob: data:; style-src 'unsafe-inline'; " +
	"img-src data: blob:; media-src data: blob:; font-src data:; worker-src blob:; connect-src data: blob:; " +
	"form-action 'none'; base-uri 'none'; webrtc 'block'"

// previewFrame is the whole preview page: it asks the chat page that holds
// it for the document to show, and writes it. Nothing else is in it.
const previewFrame = `<!DOCTYPE html><meta charset="utf-8"><title>Preview</title><script>
addEventListener('message', function show(e) {
  if (e.source !== parent || !e.data || typeof e.data.janusPreviewDocument !== 'string') return;
  removeEventListener('message', show);
  document.open();
  document.write(e.data.janusPreviewDocument);
  document.close();
});
parent.postMessage({ janusPreviewReady: true }, '*');
</script>`

// handlePreviewFrame serves the page a chat preview runs in.
//
// The preview used to be a srcdoc frame. A srcdoc frame takes on the policy
// of the page that holds it, and the console's policy does not allow inline
// script, which a previewed page is made of. So the preview is a page of its
// own with a policy of its own, sent here as a header: no network, and a
// sandbox with no origin, whoever opens it and however.
func (s *Server) handlePreviewFrame(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Security-Policy", PreviewPolicy+"; sandbox allow-scripts allow-pointer-lock allow-modals; frame-ancestors 'self'")
	h.Del("Content-Security-Policy-Report-Only")
	_, _ = io.WriteString(w, previewFrame)
}

// cspReportsPerAddressPerMin and cspReportsTotalPerMin bound what an
// unauthenticated caller can make the gateway log.
const (
	cspReportsPerAddressPerMin = 30
	cspReportsTotalPerMin      = 300
)

// handleCSPReport logs what a browser reports the policy stopped. Browsers
// post these on their own, without a session's CSRF header, so the endpoint
// takes no credentials and does nothing but log a bounded line.
func (s *Server) handleCSPReport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	w.WriteHeader(http.StatusNoContent)
	if ok, _ := s.RateLimits.Allow("csp-report:"+s.clientIP(r), cspReportsPerAddressPerMin, now); !ok {
		return
	}
	if ok, _ := s.RateLimits.Allow("csp-report:all", cspReportsTotalPerMin, now); !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		return
	}
	var body struct {
		Report map[string]any `json:"csp-report"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Report == nil {
		return
	}
	field := func(k string) string {
		v, _ := body.Report[k].(string)
		return truncate(strings.ToValidUTF8(v, ""), 200)
	}
	line := 0
	if n, ok := body.Report["line-number"].(float64); ok {
		line = int(n)
	}
	s.Logger.Warn("content security policy violation",
		"directive", field("effective-directive"),
		"violated", field("violated-directive"),
		"blocked", field("blocked-uri"),
		"page", pathOnly(field("document-uri")),
		"source", pathOnly(field("source-file")),
		"line", line,
		"disposition", field("disposition"),
	)
}

// pathOnly drops the query and fragment of a URL: a report names the page
// it happened on, and a page's address can carry what a person searched for.
func pathOnly(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}
