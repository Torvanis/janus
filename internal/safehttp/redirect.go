// Package safehttp holds the rules for HTTP calls Janus makes to servers it
// was configured with (model providers, identity providers, webhooks).
package safehttp

import (
	"errors"
	"net/http"
	"strings"
)

// maxRedirects is how many redirects one call follows.
const maxRedirects = 10

// keptAcrossHosts are the request headers that say nothing about the caller
// or the configured credential; a redirect to another host carries only
// these.
var keptAcrossHosts = map[string]bool{
	"Accept": true, "Accept-Encoding": true, "Accept-Language": true, "User-Agent": true, "Range": true,
}

// ErrRedirectRefused is returned when a redirect is not followed.
var ErrRedirectRefused = errors.New("the server redirected the request somewhere it may not be sent")

// CheckRedirect is the redirect rule for calls that carry a configured
// credential or someone's data. Go's default follows any redirect to any
// host; it removes Authorization on the way but not the key headers some
// providers use (x-api-key, x-goog-api-key), and on a 307 or 308 it sends
// the request body again. So a provider (or an open redirect on its host)
// could send the key and the prompt to a server of its choosing.
//
//   - A redirect from https to http is refused.
//   - The same host: followed as sent; fix, when given, adjusts the headers.
//   - Another host: followed only as a GET or HEAD (no body), with every
//     header removed except the few that carry nothing. This is what a
//     download link to a storage host needs. Anything else is refused.
func CheckRedirect(fix func(http.Header)) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0]
		if first.URL.Scheme == "https" && req.URL.Scheme != "https" {
			return ErrRedirectRefused
		}
		if strings.EqualFold(req.URL.Host, first.URL.Host) {
			if fix != nil {
				fix(req.Header)
			}
			return nil
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return ErrRedirectRefused
		}
		for name := range req.Header {
			if !keptAcrossHosts[http.CanonicalHeaderKey(name)] {
				req.Header.Del(name)
			}
		}
		return nil
	}
}
