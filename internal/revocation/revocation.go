// Package revocation checks whether the certificates of servers Janus
// connects to have been revoked, by OCSP (RFC 6960) and CRL (RFC 5280).
//
// It is off by default. JANUS_TLS_REVOCATION=soft or =hard turns it on; the
// STIG profile (JANUS_STIG_PROFILE) defaults it to hard and refuses off.
//
//   - off:  no checks (Go's default behavior: chain and name only).
//   - soft: a certificate reported revoked fails the TLS handshake; when no
//     answer can be obtained (responder down, CRL unreachable or stale) the
//     connection proceeds and a warning is logged.
//   - hard: a certificate reported revoked fails, and so does a certificate
//     whose status cannot be established. Certificates that publish no
//     revocation source at all (no OCSP URL, no CRL distribution point) are
//     accepted in both modes and counted as "no source", because there is
//     nothing to check; the status page shows how many.
//
// Every certificate of the verified chain except the trust anchor is
// checked: the leaf and each intermediate, each against its issuer. OCSP is
// tried first (a stapled response, then the responder), then the CRL.
// Answers are cached in memory per replica until the response's nextUpdate
// (bounded), so a busy upstream costs one lookup per validity period.
//
// The check is installed with Wrap on every outbound tls.Config. It reads
// the mode at handshake time, so Wrap may run before Configure.
package revocation

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ocsp"
)

// Mode is how strictly revocation is enforced.
type Mode string

// Modes.
const (
	ModeOff  Mode = "off"
	ModeSoft Mode = "soft"
	ModeHard Mode = "hard"
)

// ParseMode validates a JANUS_TLS_REVOCATION value ("" is off).
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeSoft:
		return ModeSoft, nil
	case ModeHard:
		return ModeHard, nil
	}
	return "", fmt.Errorf("JANUS_TLS_REVOCATION must be off, soft or hard (got %q)", s)
}

// ErrRevoked is returned (wrapped) when a certificate in the chain is revoked.
var ErrRevoked = errors.New("certificate revoked")

// ErrUnavailable is returned (wrapped) in hard mode when the revocation
// status of a certificate could not be established.
var ErrUnavailable = errors.New("certificate revocation status unavailable")

// Options configure the checker.
type Options struct {
	Mode Mode
	// OCSPResponder, when set, replaces the responder URL in every
	// certificate's AIA extension (an enclave's local OCSP responder).
	OCSPResponder string
	// Timeout bounds one OCSP or CRL fetch (default 5s).
	Timeout time.Duration
	// Client fetches OCSP and CRL; nil builds a plain client honoring
	// HTTP(S)_PROXY. It is never itself revocation-checked (no recursion).
	Client *http.Client
	Logger *slog.Logger
	Now    func() time.Time
}

// Stats counts checks since start, for the status page.
type Stats struct {
	Mode        Mode   `json:"mode"`
	Responder   string `json:"ocsp_responder,omitempty"`
	Good        uint64 `json:"good"`
	Revoked     uint64 `json:"revoked"`
	Unavailable uint64 `json:"unavailable"`
	NoSource    uint64 `json:"no_source"`
	CacheHits   uint64 `json:"cache_hits"`
	LastError   string `json:"last_error,omitempty"`
}

// Checker holds the configuration and the caches.
type Checker struct {
	opts   Options
	client *http.Client

	mu    sync.Mutex
	ocsp  map[string]cached // issuerKey|serial -> status
	crls  map[string]*crlEntry
	fetch map[string]*sync.Mutex // per-URL fetch lock (no thundering herd)

	good, revoked, unavailable, noSource, hits atomic.Uint64
	lastErr                                    atomic.Value // string
}

type status int

const (
	statusUnknown status = iota
	statusGood
	statusRevoked
)

type cached struct {
	st      status
	err     string
	expires time.Time
}

type crlEntry struct {
	revoked map[string]bool // serial (hex) -> revoked
	issuer  []byte          // raw subject of the CRL's verified issuer
	expires time.Time
	err     string // non-empty: a cached failure (short-lived)
}

const (
	maxCRLBytes   = 64 << 20
	maxOCSPBytes  = 1 << 20
	maxCache      = 24 * time.Hour
	failureCache  = time.Minute
	defaultCache  = time.Hour
	staleCRLGrace = 0
)

var current atomic.Pointer[Checker]

// Configure installs the process-wide checker. Call once at startup (tests
// may call it again and restore the previous checker with the returned
// function).
func Configure(o Options) (restore func()) {
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Mode == "" {
		o.Mode = ModeOff
	}
	c := &Checker{opts: o, client: o.Client, ocsp: map[string]cached{}, crls: map[string]*crlEntry{}, fetch: map[string]*sync.Mutex{}}
	if c.client == nil {
		c.client = &http.Client{Timeout: o.Timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	}
	prev := current.Swap(c)
	return func() { current.Store(prev) }
}

// Current returns the installed checker's statistics (mode off when none).
func Current() Stats {
	c := current.Load()
	if c == nil {
		return Stats{Mode: ModeOff}
	}
	s := Stats{Mode: c.opts.Mode, Responder: c.opts.OCSPResponder, Good: c.good.Load(), Revoked: c.revoked.Load(),
		Unavailable: c.unavailable.Load(), NoSource: c.noSource.Load(), CacheHits: c.hits.Load()}
	if v, ok := c.lastErr.Load().(string); ok {
		s.LastError = v
	}
	return s
}

// Wrap returns a copy of cfg (a new config when nil) whose handshake also
// runs the revocation check. Any VerifyConnection already set runs first.
func Wrap(cfg *tls.Config) *tls.Config {
	var out *tls.Config
	if cfg == nil {
		out = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		out = cfg.Clone()
	}
	prev := out.VerifyConnection
	out.VerifyConnection = func(cs tls.ConnectionState) error {
		if prev != nil {
			if err := prev(cs); err != nil {
				return err
			}
		}
		return VerifyConnection(cs)
	}
	return out
}

// VerifyConnection checks cs against the installed checker. It does nothing
// when revocation is off or the chain was not verified (InsecureSkipVerify).
func VerifyConnection(cs tls.ConnectionState) error {
	c := current.Load()
	if c == nil || c.opts.Mode == ModeOff || len(cs.VerifiedChains) == 0 {
		return nil
	}
	return c.CheckChain(context.Background(), cs.VerifiedChains[0], cs.OCSPResponse)
}

// CheckVerifiedChain checks a chain Janus verified itself (a smart-card
// certificate) with the installed checker; nil when revocation is off.
func CheckVerifiedChain(ctx context.Context, chain []*x509.Certificate) error {
	c := current.Load()
	if c == nil || c.opts.Mode == ModeOff {
		return nil
	}
	return c.CheckChain(ctx, chain, nil)
}

// CheckChain checks every certificate of chain except the last (the trust
// anchor) against its issuer. stapled is the server's stapled OCSP response
// for the leaf, if any.
func (c *Checker) CheckChain(ctx context.Context, chain []*x509.Certificate, stapled []byte) error {
	for i := 0; i+1 < len(chain); i++ {
		var staple []byte
		if i == 0 {
			staple = stapled
		}
		st, err := c.check(ctx, chain[i], chain[i+1], staple)
		subject := chain[i].Subject.String()
		switch st {
		case statusRevoked:
			c.revoked.Add(1)
			c.opts.Logger.Error("tls revocation: certificate revoked", "subject", subject, "serial", chain[i].SerialNumber.Text(16), "issuer", chain[i].Issuer.String())
			return fmt.Errorf("%w: %s (serial %s)", ErrRevoked, subject, chain[i].SerialNumber.Text(16))
		case statusGood:
			c.good.Add(1)
		default:
			if err == errNoSource {
				c.noSource.Add(1)
				continue
			}
			c.unavailable.Add(1)
			msg := "unknown"
			if err != nil {
				msg = err.Error()
			}
			c.lastErr.Store(subject + ": " + msg)
			if c.opts.Mode == ModeHard {
				c.opts.Logger.Error("tls revocation: status unavailable, refusing connection (hard mode)", "subject", subject, "error", msg)
				return fmt.Errorf("%w: %s: %s", ErrUnavailable, subject, msg)
			}
			c.opts.Logger.Warn("tls revocation: status unavailable, connection allowed (soft mode)", "subject", subject, "error", msg)
		}
	}
	return nil
}

var errNoSource = errors.New("certificate publishes no OCSP responder or CRL distribution point")

// check returns the status of cert. It tries the stapled OCSP response,
// the OCSP responder, then the CRL; the first definite answer wins.
func (c *Checker) check(ctx context.Context, cert, issuer *x509.Certificate, stapled []byte) (status, error) {
	var errs []string
	if len(stapled) > 0 {
		if st, _, err := c.parseOCSP(stapled, cert, issuer); err == nil && st != statusUnknown {
			return st, nil
		}
	}
	responder := c.opts.OCSPResponder
	if responder == "" && len(cert.OCSPServer) > 0 {
		responder = cert.OCSPServer[0]
	}
	if responder != "" {
		st, err := c.ocspStatus(ctx, responder, cert, issuer)
		if st != statusUnknown {
			return st, nil
		}
		if err != nil {
			errs = append(errs, "OCSP: "+err.Error())
		}
	}
	var crlErr error
	for _, u := range cert.CRLDistributionPoints {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue // ldap:// distribution points are not fetched
		}
		st, err := c.crlStatus(ctx, u, cert, issuer)
		if st != statusUnknown {
			return st, nil
		}
		crlErr = err
		if err != nil {
			errs = append(errs, "CRL: "+err.Error())
		}
	}
	if responder == "" && len(cert.CRLDistributionPoints) == 0 {
		return statusUnknown, errNoSource
	}
	if len(errs) == 0 && crlErr == nil {
		errs = append(errs, "no usable revocation source")
	}
	return statusUnknown, errors.New(strings.Join(errs, "; "))
}

func ocspKey(cert, issuer *x509.Certificate) string {
	return string(issuer.RawSubjectPublicKeyInfo) + "|" + cert.SerialNumber.Text(16)
}

func (c *Checker) ocspStatus(ctx context.Context, responder string, cert, issuer *x509.Certificate) (status, error) {
	key := ocspKey(cert, issuer)
	now := c.opts.Now()
	c.mu.Lock()
	if e, ok := c.ocsp[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		c.hits.Add(1)
		if e.err != "" {
			return statusUnknown, errors.New(e.err)
		}
		return e.st, nil
	}
	c.mu.Unlock()

	st, next, err := c.fetchOCSP(ctx, responder, cert, issuer)
	e := cached{st: st, expires: now.Add(failureCache)}
	if err != nil {
		e.err = err.Error()
	} else {
		e.expires = boundedExpiry(now, next)
	}
	c.mu.Lock()
	c.ocsp[key] = e
	c.mu.Unlock()
	return st, err
}

func (c *Checker) fetchOCSP(ctx context.Context, responder string, cert, issuer *x509.Certificate) (status, time.Time, error) {
	req, err := ocsp.CreateRequest(cert, issuer, &ocsp.RequestOptions{Hash: crypto.SHA256})
	if err != nil {
		return statusUnknown, time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, responder, bytes.NewReader(req))
	if err != nil {
		return statusUnknown, time.Time{}, err
	}
	hreq.Header.Set("Content-Type", "application/ocsp-request")
	hreq.Header.Set("Accept", "application/ocsp-response")
	resp, err := c.client.Do(hreq)
	if err != nil {
		return statusUnknown, time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusUnknown, time.Time{}, fmt.Errorf("responder answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOCSPBytes))
	if err != nil {
		return statusUnknown, time.Time{}, err
	}
	return c.parseOCSP(body, cert, issuer)
}

// parseOCSP verifies a response (signed by the issuer or a delegated
// responder the issuer certified) and checks it is current.
func (c *Checker) parseOCSP(der []byte, cert, issuer *x509.Certificate) (status, time.Time, error) {
	r, err := ocsp.ParseResponseForCert(der, cert, issuer)
	if err != nil {
		return statusUnknown, time.Time{}, err
	}
	now := c.opts.Now()
	if !r.NextUpdate.IsZero() && now.After(r.NextUpdate) {
		return statusUnknown, time.Time{}, fmt.Errorf("OCSP response expired at %s", r.NextUpdate.UTC().Format(time.RFC3339))
	}
	if r.ThisUpdate.After(now.Add(5 * time.Minute)) {
		return statusUnknown, time.Time{}, errors.New("OCSP response is from the future")
	}
	switch r.Status {
	case ocsp.Good:
		return statusGood, r.NextUpdate, nil
	case ocsp.Revoked:
		return statusRevoked, r.NextUpdate, nil
	}
	return statusUnknown, r.NextUpdate, errors.New("responder does not know the certificate")
}

func (c *Checker) crlStatus(ctx context.Context, url string, cert, issuer *x509.Certificate) (status, error) {
	e, err := c.crl(ctx, url, issuer)
	if err != nil {
		return statusUnknown, err
	}
	if !bytes.Equal(e.issuer, issuer.RawSubject) {
		return statusUnknown, errors.New("CRL was issued by a different CA")
	}
	if e.revoked[cert.SerialNumber.Text(16)] {
		return statusRevoked, nil
	}
	return statusGood, nil
}

func (c *Checker) crl(ctx context.Context, url string, issuer *x509.Certificate) (*crlEntry, error) {
	key := url + "|" + string(issuer.RawSubjectPublicKeyInfo)
	c.mu.Lock()
	lock, ok := c.fetch[key]
	if !ok {
		lock = &sync.Mutex{}
		c.fetch[key] = lock
	}
	c.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	now := c.opts.Now()
	c.mu.Lock()
	if e, ok := c.crls[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		c.hits.Add(1)
		if e.err != "" {
			return nil, errors.New(e.err)
		}
		return e, nil
	}
	c.mu.Unlock()

	e, err := c.fetchCRL(ctx, url, issuer)
	if err != nil {
		e = &crlEntry{err: err.Error(), expires: now.Add(failureCache)}
	}
	c.mu.Lock()
	c.crls[key] = e
	c.mu.Unlock()
	return e, err
}

func (c *Checker) fetchCRL(ctx context.Context, url string, issuer *x509.Certificate) (*crlEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCRLBytes {
		return nil, fmt.Errorf("CRL at %s is larger than %d MiB", url, maxCRLBytes>>20)
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN")) {
		return nil, fmt.Errorf("CRL at %s is PEM; distribution points must serve DER", url)
	}
	rl, err := x509.ParseRevocationList(body)
	if err != nil {
		return nil, fmt.Errorf("parse CRL from %s: %w", url, err)
	}
	if err := rl.CheckSignatureFrom(issuer); err != nil {
		return nil, fmt.Errorf("CRL from %s is not signed by the certificate's issuer: %w", url, err)
	}
	now := c.opts.Now()
	if !rl.NextUpdate.IsZero() && now.After(rl.NextUpdate.Add(staleCRLGrace)) {
		return nil, fmt.Errorf("CRL from %s is stale (nextUpdate %s)", url, rl.NextUpdate.UTC().Format(time.RFC3339))
	}
	e := &crlEntry{revoked: make(map[string]bool, len(rl.RevokedCertificateEntries)), issuer: rl.RawIssuer, expires: boundedExpiry(now, rl.NextUpdate)}
	for _, r := range rl.RevokedCertificateEntries {
		e.revoked[r.SerialNumber.Text(16)] = true
	}
	return e, nil
}

func boundedExpiry(now, next time.Time) time.Time {
	if next.IsZero() {
		return now.Add(defaultCache)
	}
	d := next.Sub(now)
	switch {
	case d <= 0:
		d = failureCache
	case d > maxCache:
		d = maxCache
	}
	return now.Add(d)
}
