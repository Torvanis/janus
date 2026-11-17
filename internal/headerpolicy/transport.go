package headerpolicy

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Calls Janus makes on its own.
//
// A user's request has its headers rewritten by Apply before the adapter
// builds the provider request. Janus also calls upstreams itself: model
// discovery, capability probes, classifiers and decision models, routing
// evaluation, pool health checks. Those requests start with no caller
// headers, so there is nothing to remove, but the headers a policy ADDS
// must still go out: they may be what lets the request through to the
// provider at all (a Cloudflare Access service token, a gateway key).
// UpstreamTransport adds them to every request one upstream's client sends.

// ApplyOwn adds a policy's set and append headers to a request Janus built
// itself. A header Janus already put on the request (credentials, content
// type, provider version headers) is kept: the policy fills in, it never
// replaces. Remove rules and the allow-list are about what a caller sent
// and do nothing here. It reports whether anything was added.
func ApplyOwn(h http.Header, p *Policy) bool {
	if p == nil || h == nil {
		return false
	}
	own := map[string]bool{}
	for name := range h {
		own[strings.ToLower(name)] = true
	}
	added := false
	for _, r := range p.Rules {
		key := strings.ToLower(r.Name)
		if Reserved(r.Name) || own[key] {
			continue
		}
		switch r.Action {
		case ActionSet:
			h.Set(r.Name, r.Value)
			added = true
		case ActionAppend:
			h.Add(r.Name, r.Value)
			added = true
		}
	}
	return added
}

// SameOrigin reports whether u is on the scheme, host and port of base.
func SameOrigin(u *url.URL, base string) bool {
	b, err := url.Parse(base)
	if err != nil || u == nil || b.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(hostPort(u), hostPort(b))
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return u.Hostname() + ":" + port
}

// UpstreamTransport sends one upstream's requests with its header policy
// applied. The policy's headers are added only to requests that go to the
// upstream's own origin: a redirect elsewhere, or a side request an adapter
// makes to another host (an identity provider's token endpoint), never
// carries them.
type UpstreamTransport struct {
	Base http.RoundTripper
	// BaseURL is the upstream's configured base URL.
	BaseURL string
	// Allow, when set, decides which requests are the provider's own
	// instead of BaseURL: a personal plan provider is called at several
	// hosts (sign-in, account, inference).
	Allow func(u *url.URL) bool
	// Policy returns the policy in force for the upstream now, or nil.
	Policy func(ctx context.Context) *Policy
}

// RoundTrip implements http.RoundTripper.
func (t *UpstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.Policy == nil || !t.own(req.URL) {
		return base.RoundTrip(req)
	}
	p := t.Policy(req.Context())
	if p == nil {
		return base.RoundTrip(req)
	}
	// A RoundTripper must not change the request it was given.
	h := req.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	if !ApplyOwn(h, p) {
		return base.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	out.Header = h
	return base.RoundTrip(out)
}

func (t *UpstreamTransport) own(u *url.URL) bool {
	if t.Allow != nil {
		return u != nil && t.Allow(u)
	}
	return SameOrigin(u, t.BaseURL)
}

// ClientFor is Client for a provider called at more than one host: allow
// reports whether a request URL is one of the provider's own.
func ClientFor(c *http.Client, allow func(u *url.URL) bool, policy func(ctx context.Context) *Policy) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	if allow == nil {
		allow = func(*url.URL) bool { return false }
	}
	return &http.Client{
		Transport:     &UpstreamTransport{Base: c.Transport, Allow: allow, Policy: policy},
		CheckRedirect: c.CheckRedirect, Jar: c.Jar, Timeout: c.Timeout,
	}
}

// Client returns a client like c whose requests to the upstream at baseURL
// carry the policy. c's timeout, redirect rule and connection pool are kept.
func Client(c *http.Client, baseURL string, policy func(ctx context.Context) *Policy) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	return &http.Client{
		Transport:     &UpstreamTransport{Base: c.Transport, BaseURL: baseURL, Policy: policy},
		CheckRedirect: c.CheckRedirect, Jar: c.Jar, Timeout: c.Timeout,
	}
}
