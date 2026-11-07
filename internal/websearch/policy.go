package websearch

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/torvanis/janus/internal/revocation"
	"github.com/torvanis/janus/internal/truststore"
)

// PagePolicy decides which web pages chat models may make Janus read
// (read_page, deep research). It is set once at startup from JANUS_WEB_*
// variables; the zero value is the default: the public internet only, on
// standard web ports, verified against the system roots, directly.
type PagePolicy struct {
	// AllowPrivate (JANUS_WEB_ALLOW_PRIVATE) also allows private networks
	// (RFC 1918, carrier-grade NAT, IPv6 unique-local) and internal names
	// (single-label, .local, .internal, .lan, .home.arpa, .svc,
	// .cluster.local): for offline networks and intranets.
	AllowPrivate bool
	// AllowNetworks (JANUS_WEB_ALLOW_NETWORKS): when set, only these
	// address ranges can be read (plus private ranges when AllowPrivate);
	// the public internet is no longer implied. 0.0.0.0/0 and ::/0 restore
	// it.
	AllowNetworks []netip.Prefix
	// DenyNetworks (JANUS_WEB_DENY_NETWORKS) are never read; deny wins.
	DenyNetworks []netip.Prefix
	// AllowDomains (JANUS_WEB_ALLOW_DOMAINS): when set, only these sites
	// (each entry matches the name and its subdomains). Links to bare IP
	// addresses then need JANUS_WEB_ALLOW_NETWORKS.
	AllowDomains []string
	// DenyDomains (JANUS_WEB_DENY_DOMAINS) are never read; deny wins.
	DenyDomains []string
	// Ports (JANUS_WEB_ALLOW_PORTS) replaces 80, 443, 8080, 8443.
	Ports map[string]bool
	// HTTPSOnly (JANUS_WEB_HTTPS_ONLY) refuses http:// pages.
	HTTPSOnly bool
	// Proxy (JANUS_WEB_PROXY) sends every page read through this proxy,
	// with no direct fallback. The proxy then connects and is the
	// gatekeeper for where names resolve: Janus checks the link's name or
	// IP address, the domain lists and the port.
	Proxy *url.URL
	// TLSTrust (JANUS_WEB_TLS_TRUST): system (default), custom (system
	// roots + JANUS_CA_BUNDLE), custom-only (JANUS_CA_BUNDLE only: just
	// sites signed by your CAs).
	TLSTrust string
	roots    *x509.CertPool
}

// Always blocked, whatever the policy: this host itself (loopback), the
// link-local range (which carries cloud metadata services such as
// 169.254.169.254), multicast and unspecified addresses. Reading them
// would let a model reach Janus's own port or the host's credentials.
func hardBlocked(a netip.Addr) bool {
	return !a.IsValid() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() || hardPrefix(a)
}

var hardPrefixes = mustPrefixes("0.0.0.0/8", "255.255.255.255/32", "fec0::/10")

func hardPrefix(a netip.Addr) bool {
	for _, p := range hardPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Private ranges JANUS_WEB_ALLOW_PRIVATE opens.
var privatePrefixes = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7")

func isPrivate(a netip.Addr) bool {
	for _, p := range privatePrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, v := range s {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// addrAllowed reports whether a model may make Janus connect to a.
func (p *PagePolicy) addrAllowed(a netip.Addr) bool {
	a = a.Unmap()
	if hardBlocked(a) || inAny(a, p.DenyNetworks) {
		return false
	}
	if len(p.AllowNetworks) > 0 {
		return inAny(a, p.AllowNetworks) || (p.AllowPrivate && isPrivate(a))
	}
	if isPrivate(a) {
		return p.AllowPrivate
	}
	return publicAddr(a)
}

func domainMatch(host string, list []string) bool {
	for _, d := range list {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func internalName(h string) bool {
	return !strings.Contains(h, ".") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") ||
		strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".home.arpa") || strings.HasSuffix(h, ".svc") ||
		strings.Contains(h, ".svc.") || strings.HasSuffix(h, ".cluster.local")
}

// Errors a model (and the person reading the chat) sees. Each names what
// blocks the page and, for administrators, the setting involved.
var (
	ErrBlockedAddress   = errors.New("that address is not on the public internet")
	errBlockedPrivate   = errors.New("that address is on a private or internal network, which this deployment does not let chat read (an administrator can allow it with JANUS_WEB_ALLOW_PRIVATE)")
	errBlockedPolicy    = errors.New("that address is not allowed by this deployment's web page policy (JANUS_WEB_ALLOW_NETWORKS / JANUS_WEB_DENY_NETWORKS)")
	errBlockedDomain    = errors.New("that site is not allowed by this deployment's web page policy (JANUS_WEB_ALLOW_DOMAINS / JANUS_WEB_DENY_DOMAINS)")
	errBlockedThisHost  = errors.New("that address is this server itself, a link-local or a metadata address, which chat can never read")
	errHTTPSOnly        = errors.New("only https pages can be read on this deployment")
	errBlockedPortNamed = "only these ports can be read: %s"
)

var policyErrors = []error{ErrBlockedAddress, errBlockedPrivate, errBlockedPolicy, errBlockedDomain, errBlockedThisHost, errHTTPSOnly}

func (p *PagePolicy) addrError(a netip.Addr) error {
	a = a.Unmap()
	switch {
	case hardBlocked(a):
		return errBlockedThisHost
	case inAny(a, p.DenyNetworks) || len(p.AllowNetworks) > 0:
		return errBlockedPolicy
	case isPrivate(a) || !publicAddr(a):
		if !p.AllowPrivate && isPrivate(a) {
			return errBlockedPrivate
		}
		return ErrBlockedAddress
	}
	return errBlockedPolicy
}

// checkURL is the check on the link itself (every hop of a redirect).
func (p *PagePolicy) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https pages can be read")
	}
	if p.HTTPSOnly && u.Scheme != "https" {
		return errHTTPSOnly
	}
	if u.User != nil {
		return errors.New("addresses with credentials cannot be read")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("the address has no host")
	}
	if port := u.Port(); port != "" && !p.portAllowed(port) {
		return fmt.Errorf(errBlockedPortNamed, p.portList())
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if len(p.AllowDomains) > 0 && len(p.AllowNetworks) == 0 {
			return errBlockedDomain
		}
		if !p.addrAllowed(a) {
			return p.addrError(a)
		}
		return nil
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return errBlockedThisHost
	}
	if domainMatch(h, p.DenyDomains) {
		return errBlockedDomain
	}
	if len(p.AllowDomains) > 0 {
		if !domainMatch(h, p.AllowDomains) {
			return errBlockedDomain
		}
		return nil // an allowed domain may be an internal name
	}
	if internalName(h) && !p.AllowPrivate {
		return errBlockedPrivate
	}
	return nil
}

var defaultPorts = map[string]bool{"80": true, "443": true, "8080": true, "8443": true}

func (p *PagePolicy) portAllowed(port string) bool {
	if len(p.Ports) > 0 {
		return p.Ports[port]
	}
	return defaultPorts[port]
}

func (p *PagePolicy) portList() string {
	m := p.Ports
	if len(m) == 0 {
		m = defaultPorts
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { a, _ := strconv.Atoi(out[i]); b, _ := strconv.Atoi(out[j]); return a < b })
	return strings.Join(out, ", ")
}

// Summary is the policy as the admin console shows it (read-only).
func (p *PagePolicy) Summary() map[string]any {
	nets := func(ps []netip.Prefix) []string {
		out := []string{}
		for _, x := range ps {
			out = append(out, x.String())
		}
		return out
	}
	proxy := ""
	if p.Proxy != nil {
		u := *p.Proxy
		u.User = nil // never show proxy credentials
		proxy = u.String()
	}
	trust := p.TLSTrust
	if trust == "" {
		trust = "system"
	}
	return map[string]any{
		"allow_private": p.AllowPrivate, "allow_networks": nets(p.AllowNetworks), "deny_networks": nets(p.DenyNetworks),
		"allow_domains": append([]string{}, p.AllowDomains...), "deny_domains": append([]string{}, p.DenyDomains...),
		"ports": strings.Split(p.portList(), ", "), "https_only": p.HTTPSOnly, "proxy": proxy, "tls_trust": trust,
		"default": p.isDefault(),
	}
}

func (p *PagePolicy) isDefault() bool {
	return !p.AllowPrivate && len(p.AllowNetworks) == 0 && len(p.DenyNetworks) == 0 && len(p.AllowDomains) == 0 &&
		len(p.DenyDomains) == 0 && len(p.Ports) == 0 && !p.HTTPSOnly && p.Proxy == nil && (p.TLSTrust == "" || p.TLSTrust == "system")
}

// system: the operating system's roots. custom: the shared trust store
// (system roots plus the DoD roots, JANUS_CA_BUNDLE, JANUS_CA_DIR and
// uploaded certificates). custom-only: the trust store's configured
// authorities without the system roots. Every handshake runs the
// revocation check (JANUS_TLS_REVOCATION). Without a configured trust
// store (tests, embedders) custom trust falls back to JANUS_CA_BUNDLE as
// read at startup.
func (p *PagePolicy) tlsConfig() *tls.Config {
	var pool *x509.CertPool
	switch p.TLSTrust {
	case "custom":
		pool = truststore.Pool()
	case "custom-only":
		pool = truststore.ClientCAPool()
		if pool == nil && truststore.Configured() && p.roots == nil {
			// Configured, but no authority yet (uploads pending): trust
			// nothing rather than fall back to the system roots.
			pool = x509.NewCertPool()
		}
	}
	if pool == nil {
		pool = p.roots
	}
	return revocation.Wrap(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
}

// ParsePagePolicy reads the JANUS_WEB_* variables through getenv.
// caBundlePath is JANUS_CA_BUNDLE (used by JANUS_WEB_TLS_TRUST=custom and
// custom-only). Every mistake is refused by name, so a bad setting stops
// startup instead of silently opening (or closing) the web.
func ParsePagePolicy(getenv func(string) string, caBundlePath string) (*PagePolicy, error) {
	p := &PagePolicy{}
	var err error
	if p.AllowPrivate, err = parseBool(getenv, "JANUS_WEB_ALLOW_PRIVATE"); err != nil {
		return nil, err
	}
	if p.HTTPSOnly, err = parseBool(getenv, "JANUS_WEB_HTTPS_ONLY"); err != nil {
		return nil, err
	}
	if p.AllowNetworks, err = parseNets(getenv, "JANUS_WEB_ALLOW_NETWORKS"); err != nil {
		return nil, err
	}
	for _, n := range p.AllowNetworks {
		if hardBlocked(n.Addr()) && (n.Bits() >= 8 || n.Addr().Is6() && n.Bits() >= 10) {
			return nil, fmt.Errorf("JANUS_WEB_ALLOW_NETWORKS contains %s: loopback, link-local and metadata addresses can never be read by chat", n)
		}
	}
	if p.DenyNetworks, err = parseNets(getenv, "JANUS_WEB_DENY_NETWORKS"); err != nil {
		return nil, err
	}
	if p.AllowDomains, err = parseDomains(getenv, "JANUS_WEB_ALLOW_DOMAINS"); err != nil {
		return nil, err
	}
	if p.DenyDomains, err = parseDomains(getenv, "JANUS_WEB_DENY_DOMAINS"); err != nil {
		return nil, err
	}
	if raw := strings.TrimSpace(getenv("JANUS_WEB_ALLOW_PORTS")); raw != "" {
		p.Ports = map[string]bool{}
		for _, s := range splitList(raw) {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("JANUS_WEB_ALLOW_PORTS contains %q, which is not a port number", s)
			}
			p.Ports[strconv.Itoa(n)] = true
		}
	}
	if raw := strings.TrimSpace(getenv("JANUS_WEB_PROXY")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
			return nil, fmt.Errorf("JANUS_WEB_PROXY must be an http://, https:// or socks5:// proxy URL (got %q)", redactURL(raw))
		}
		p.Proxy = u
	}
	switch p.TLSTrust = strings.ToLower(strings.TrimSpace(getenv("JANUS_WEB_TLS_TRUST"))); p.TLSTrust {
	case "", "system":
		p.TLSTrust = "system"
	case "custom", "custom-only":
		if caBundlePath == "" {
			if truststore.Configured() {
				// The shared trust store supplies the CAs (DoD roots,
				// JANUS_CA_DIR, uploads under Settings → Certificates).
				break
			}
			return nil, fmt.Errorf("JANUS_WEB_TLS_TRUST=%s needs JANUS_CA_BUNDLE (the CAs to trust)", p.TLSTrust)
		}
		pemBytes, err := os.ReadFile(caBundlePath)
		if err != nil {
			return nil, fmt.Errorf("JANUS_WEB_TLS_TRUST=%s: JANUS_CA_BUNDLE cannot be read: %v", p.TLSTrust, err)
		}
		if p.TLSTrust == "custom" {
			if p.roots, err = x509.SystemCertPool(); err != nil || p.roots == nil {
				p.roots = x509.NewCertPool()
			}
		} else {
			p.roots = x509.NewCertPool()
		}
		if !p.roots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("JANUS_WEB_TLS_TRUST=%s: JANUS_CA_BUNDLE holds no PEM certificates", p.TLSTrust)
		}
	default:
		return nil, fmt.Errorf("JANUS_WEB_TLS_TRUST must be system, custom or custom-only (got %q)", p.TLSTrust)
	}
	return p, nil
}

// redactURL hides credentials in a proxy URL, even one that does not
// parse.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		start := 0
		if i := strings.Index(raw, "://"); i >= 0 && i < at {
			start = i + 3
		}
		return raw[:start] + "***" + raw[at:]
	}
	return raw
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

func parseBool(getenv func(string) string, key string) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false (got %q)", key, v)
	}
	return b, nil
}

func parseNets(getenv func(string) string, key string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range splitList(getenv(key)) {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%s contains %q, which is not an IP address or CIDR block", key, s)
		}
		out = append(out, pfx.Masked())
	}
	return out, nil
}

func parseDomains(getenv func(string) string, key string) ([]string, error) {
	var out []string
	for _, s := range splitList(getenv(key)) {
		d := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "*."), "."), "."))
		if d == "" || strings.ContainsAny(d, "/:@*") {
			return nil, fmt.Errorf("%s contains %q, which is not a domain name (use example.mil to match it and its subdomains)", key, s)
		}
		out = append(out, d)
	}
	return out, nil
}

var currentPolicy atomic.Pointer[PagePolicy]

// SetPagePolicy installs the policy (startup) and rebuilds the page
// client.
func SetPagePolicy(p *PagePolicy) {
	if p == nil {
		p = &PagePolicy{}
	}
	r := newPageReader(p)
	sharedPage.Store(r)
	currentPolicy.Store(p)
}

// CurrentPagePolicy is the installed policy.
func CurrentPagePolicy() *PagePolicy {
	if p := currentPolicy.Load(); p != nil {
		return p
	}
	return &PagePolicy{}
}
