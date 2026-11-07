// Package truststore is the one trust store for every outbound TLS
// connection Janus makes: AI providers and discovery, identity providers,
// LDAP, object storage, alert webhooks and email, OTLP.
//
// It combines, in addition to the host's system roots:
//
//   - the DoD PKI root CAs (embedded, refreshed per release from DISA's
//     signed bundle by scripts/update-dod-roots.sh): on by default in the
//     Janus FIPS edition, off by default otherwise (JANUS_TRUST_DOD_ROOTS);
//   - a PEM file (JANUS_CA_BUNDLE);
//   - a directory of PEM files (JANUS_CA_DIR), typically Kubernetes
//     ConfigMaps or Secrets mounted by the Helm chart; rescanned while
//     running, so an edited ConfigMap takes effect without a restart;
//   - certificates uploaded by an administrator (stored in the database,
//     shared by every replica, audited).
//
// Changes take effect for new connections: Transport rebuilds its
// connection pool when the trust store's generation changes, and per-dial
// configs (LDAP, SMTP) read Pool() each time.
//
// Per connection, an administrator may allow an upstream whose certificate
// cannot be verified (Transport routes those hosts to a non-verifying
// transport). The Janus FIPS edition and the STIG profile refuse that
// setting; SetUntrustedAllowed(false) makes the router ignore it.
package truststore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	_ "embed" // dod-roots.pem
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/x509"

	"github.com/torvanis/janus/internal/revocation"
)

//go:embed dod/dod-roots.pem
var dodRootsPEM []byte

// Sources a trusted certificate can come from.
const (
	SourceDoD       = "dod"
	SourceFile      = "file"
	SourceDirectory = "directory"
	SourceUploaded  = "uploaded"
)

// CA describes one trusted certificate (system roots are not listed).
type CA struct {
	ID          string    `json:"id,omitempty"` // uploaded only
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	Path        string    `json:"path,omitempty"` // file / directory
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	SHA256      string    `json:"sha256"`
	IsCA        bool      `json:"is_ca"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	ReadOnly    bool      `json:"read_only"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	ExpiresSoon bool      `json:"expires_soon"`
	Expired     bool      `json:"expired"`

	cert *x509.Certificate
}

// Uploaded is one administrator-uploaded certificate as stored.
type Uploaded struct {
	ID, Name, PEM, CreatedBy string
	CreatedAt                time.Time
}

// Options configure the store.
type Options struct {
	// DoD adds the embedded DoD PKI root CAs.
	DoD bool
	// BundleFile is JANUS_CA_BUNDLE; Dir is JANUS_CA_DIR.
	BundleFile string
	Dir        string
	// Uploaded loads administrator uploads (nil: none).
	Uploaded func(ctx context.Context) ([]Uploaded, error)
	Logger   *slog.Logger
	Now      func() time.Time
}

// ExpiryWarning is how far ahead a certificate is flagged as expiring.
const ExpiryWarning = 30 * 24 * time.Hour

type state struct {
	opts Options

	mu   sync.RWMutex
	pool *x509.CertPool
	cas  []CA
	sig  string // fingerprint set, to detect changes on reload
	err  string // last reload problem (directory or database)

	gen atomic.Uint64
}

// baseTransport is net/http's default transport as it was before
// InstallDefault replaced http.DefaultTransport: every transport the store
// builds is a clone of it.
var baseTransport = http.DefaultTransport.(*http.Transport).Clone()

// InstallDefault makes http.DefaultTransport a trust-store Transport, so
// clients built without a transport (vendor sign-in, token exchange, web
// search fallbacks) verify against the same store.
func InstallDefault() { http.DefaultTransport = &Transport{} }

var (
	current          atomic.Pointer[state]
	untrustedAllowed atomic.Bool
	untrustedHosts   atomic.Pointer[map[string]bool]
)

// Configure loads the static sources and installs the store. It fails when
// JANUS_CA_BUNDLE or JANUS_CA_DIR cannot be read or holds no certificate,
// so a mistyped path stops startup instead of surfacing as x509 errors.
// Call Reload afterwards to add the database uploads.
func Configure(o Options) (restore func(), err error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &state{opts: o}
	if err := st.load(context.Background(), false); err != nil {
		return func() {}, err
	}
	prev := current.Swap(st)
	return func() { current.Store(prev) }, nil
}

// SetUploadedSource sets where administrator uploads come from (the
// database); Reload reads it.
func SetUploadedSource(fn func(ctx context.Context) ([]Uploaded, error)) {
	if st := current.Load(); st != nil {
		st.mu.Lock()
		st.opts.Uploaded = fn
		st.mu.Unlock()
	}
}

// Configured reports whether Configure ran.
func Configured() bool { return current.Load() != nil }

// Reload rescans the directory and the uploads. It keeps the previous
// pool when a source fails, and records the problem for Status.
func Reload(ctx context.Context) error {
	st := current.Load()
	if st == nil {
		return nil
	}
	return st.load(ctx, true)
}

func (st *state) load(ctx context.Context, running bool) error {
	var cas []CA
	var problems []string
	add := func(source, name, path string, certs []*x509.Certificate, readOnly bool, up *Uploaded) {
		for _, c := range certs {
			ca := describe(c, st.opts.Now())
			ca.Source, ca.Name, ca.Path, ca.ReadOnly = source, name, path, readOnly
			if ca.Name == "" {
				ca.Name = c.Subject.CommonName
			}
			if up != nil {
				ca.ID, ca.CreatedBy, ca.CreatedAt = up.ID, up.CreatedBy, up.CreatedAt
			}
			cas = append(cas, ca)
		}
	}
	if st.opts.DoD {
		certs, err := ParsePEM(dodRootsPEM)
		if err != nil {
			return fmt.Errorf("embedded DoD roots: %w", err)
		}
		add(SourceDoD, "", "", certs, true, nil)
	}
	if st.opts.BundleFile != "" {
		raw, err := os.ReadFile(st.opts.BundleFile)
		if err != nil {
			return fmt.Errorf("JANUS_CA_BUNDLE points to %q which cannot be read: %v", st.opts.BundleFile, err)
		}
		certs, err := ParsePEM(raw)
		if err != nil || len(certs) == 0 {
			return fmt.Errorf("JANUS_CA_BUNDLE file %q contains no valid PEM certificates", st.opts.BundleFile)
		}
		add(SourceFile, "", st.opts.BundleFile, certs, true, nil)
	}
	if st.opts.Dir != "" {
		found, err := scanDir(st.opts.Dir)
		if err != nil {
			if !running {
				return fmt.Errorf("JANUS_CA_DIR %q cannot be read: %v", st.opts.Dir, err)
			}
			problems = append(problems, "JANUS_CA_DIR: "+err.Error())
			// Keep the directory's previous certificates.
			st.mu.RLock()
			for _, c := range st.cas {
				if c.Source == SourceDirectory {
					cas = append(cas, c)
				}
			}
			st.mu.RUnlock()
		}
		for _, f := range found {
			add(SourceDirectory, "", f.path, f.certs, true, nil)
		}
		if err == nil && len(found) == 0 && !running {
			return fmt.Errorf("JANUS_CA_DIR %q contains no PEM certificates (.pem, .crt or .cer)", st.opts.Dir)
		}
	}
	st.mu.RLock()
	uploaded := st.opts.Uploaded
	st.mu.RUnlock()
	if uploaded != nil && running {
		ups, err := uploaded(ctx)
		if err != nil {
			problems = append(problems, "uploaded certificates: "+err.Error())
			st.mu.RLock()
			for _, c := range st.cas {
				if c.Source == SourceUploaded {
					cas = append(cas, c)
				}
			}
			st.mu.RUnlock()
		}
		for i := range ups {
			certs, err := ParsePEM([]byte(ups[i].PEM))
			if err != nil {
				problems = append(problems, fmt.Sprintf("uploaded %q: %v", ups[i].Name, err))
				continue
			}
			add(SourceUploaded, ups[i].Name, "", certs, false, &ups[i])
		}
	}

	sort.SliceStable(cas, func(i, j int) bool {
		if cas[i].Source != cas[j].Source {
			return sourceOrder(cas[i].Source) < sourceOrder(cas[j].Source)
		}
		return cas[i].Name < cas[j].Name
	})
	fps := make([]string, 0, len(cas))
	for _, c := range cas {
		fps = append(fps, c.SHA256)
	}
	sig := strings.Join(fps, ",")

	st.mu.Lock()
	defer st.mu.Unlock()
	st.err = strings.Join(problems, "; ")
	// The descriptions (expiry flags, names) always refresh; the pool and
	// generation change only when the certificate set does.
	st.cas = cas
	if st.pool != nil && sig == st.sig {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// Distroless images may ship no system store; the configured
		// certificates are then the entire trust store.
		pool = x509.NewCertPool()
	}
	for _, c := range cas {
		pool.AddCert(c.cert)
	}
	if st.pool != nil {
		st.opts.Logger.Info("trust store changed", "certificates", len(cas))
	}
	st.pool, st.sig = pool, sig
	st.gen.Add(1)
	return nil
}

func sourceOrder(s string) int {
	switch s {
	case SourceUploaded:
		return 0
	case SourceDirectory:
		return 1
	case SourceFile:
		return 2
	}
	return 3
}

func describe(c *x509.Certificate, now time.Time) CA {
	sum := sha256.Sum256(c.Raw)
	return CA{
		Subject: c.Subject.String(), Issuer: c.Issuer.String(), SHA256: hex.EncodeToString(sum[:]),
		IsCA: c.IsCA, NotBefore: c.NotBefore, NotAfter: c.NotAfter,
		Expired: now.After(c.NotAfter), ExpiresSoon: !now.After(c.NotAfter) && c.NotAfter.Sub(now) < ExpiryWarning,
		cert: c,
	}
}

type dirFile struct {
	path  string
	certs []*x509.Certificate
}

// scanDir reads every .pem/.crt/.cer file under dir. Kubernetes volume
// bookkeeping entries (..data, ..2026_…) are skipped, so a mounted
// ConfigMap's certificates are read once.
func scanDir(dir string) ([]dirFile, error) {
	var out []dirFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), "..") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".pem", ".crt", ".cer":
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		certs, err := ParsePEM(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(certs) > 0 {
			out = append(out, dirFile{path: path, certs: certs})
		}
		return nil
	})
	return out, err
}

// Upload limits.
const (
	MaxUploadBytes = 1 << 20
	MaxUploadCerts = 100
)

// ErrPrivateKey is returned when a PEM upload contains a private key.
var ErrPrivateKey = errors.New("the PEM contains a private key; upload only certificates")

// ParsePEM returns every certificate in raw. Non-certificate blocks are
// ignored, except private keys, which are refused.
func ParsePEM(raw []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := raw
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if strings.Contains(b.Type, "PRIVATE KEY") {
			return nil, ErrPrivateKey
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a certificate could not be parsed: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// ValidateUpload checks an administrator upload: PEM certificates only, at
// least one, none expired, within the size limits.
func ValidateUpload(raw string, now time.Time) ([]*x509.Certificate, error) {
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("the PEM is larger than %d KiB", MaxUploadBytes>>10)
	}
	certs, err := ParsePEM([]byte(raw))
	if err != nil {
		return nil, err
	}
	switch {
	case len(certs) == 0:
		return nil, errors.New("no PEM certificate found; paste the -----BEGIN CERTIFICATE----- block")
	case len(certs) > MaxUploadCerts:
		return nil, fmt.Errorf("at most %d certificates per upload", MaxUploadCerts)
	}
	for _, c := range certs {
		if now.After(c.NotAfter) {
			return nil, fmt.Errorf("%s expired on %s", c.Subject.CommonName, c.NotAfter.UTC().Format("2006-01-02"))
		}
	}
	return certs, nil
}

// Pool returns the current trust store (system roots plus every source),
// or nil when the store is not configured (default verification).
func Pool() *x509.CertPool {
	st := current.Load()
	if st == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.pool
}

// ClientCAPool returns the configured authorities only (DoD roots, the
// bundle, the directory, uploads) without the operating system's public
// roots: the issuers a smart-card (client) certificate may chain to. A
// public web CA must never vouch for a person. Nil when none is configured.
func ClientCAPool() *x509.CertPool {
	st := current.Load()
	if st == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if len(st.cas) == 0 {
		return nil
	}
	pool := x509.NewCertPool()
	for _, c := range st.cas {
		if c.cert != nil {
			pool.AddCert(c.cert)
		}
	}
	return pool
}

// Generation increases whenever the certificate set changes.
func Generation() uint64 {
	st := current.Load()
	if st == nil {
		return 0
	}
	return st.gen.Load()
}

// Status is the trust store as shown to administrators.
type Status struct {
	DoD          bool   `json:"dod_roots"`
	BundleFile   string `json:"bundle_file,omitempty"`
	Directory    string `json:"directory,omitempty"`
	CAs          []CA   `json:"certificates"`
	LastProblem  string `json:"last_problem,omitempty"`
	UntrustedOK  bool   `json:"untrusted_allowed"`
	Generation   uint64 `json:"generation"`
	ExpiringSoon int    `json:"expiring_soon"`
	Expired      int    `json:"expired"`
}

// Current returns the store's status.
func Current() Status {
	st := current.Load()
	if st == nil {
		return Status{CAs: []CA{}, UntrustedOK: untrustedAllowed.Load()}
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := Status{DoD: st.opts.DoD, BundleFile: st.opts.BundleFile, Directory: st.opts.Dir, CAs: append([]CA{}, st.cas...),
		LastProblem: st.err, UntrustedOK: untrustedAllowed.Load(), Generation: st.gen.Load()}
	for _, c := range st.cas {
		if c.Expired {
			out.Expired++
		} else if c.ExpiresSoon {
			out.ExpiringSoon++
		}
	}
	return out
}

// ClientConfig returns a verifying TLS client config on the current trust
// store, with revocation checking when that is on.
func ClientConfig() *tls.Config {
	return revocation.Wrap(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: Pool()})
}

// SetUntrustedAllowed records whether this edition and profile allow
// per-connection "skip certificate verification" at all.
func SetUntrustedAllowed(ok bool) { untrustedAllowed.Store(ok) }

// SetUntrustedHosts replaces the host:port set whose certificates are not
// verified (upstreams with allow_untrusted_tls). Ignored while
// SetUntrustedAllowed(false).
func SetUntrustedHosts(hosts []string) {
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		m[strings.ToLower(h)] = true
	}
	untrustedHosts.Store(&m)
}

// UntrustedHost reports whether connections to hostport skip verification.
func UntrustedHost(hostport string) bool {
	if !untrustedAllowed.Load() {
		return false
	}
	m := untrustedHosts.Load()
	return m != nil && (*m)[strings.ToLower(hostport)]
}

// HostPort normalizes a URL host to host:port (443/80 by scheme).
func HostPort(scheme, host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	port := "443"
	if scheme == "http" {
		port = "80"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// Transport is an http.RoundTripper on the trust store. Tune adjusts each
// underlying *http.Transport (timeouts, proxy, dialer). Extra PEM roots
// (an object-storage endpoint's own CA) are added on top of the store.
type Transport struct {
	Tune  func(*http.Transport)
	Extra []*x509.Certificate
	// Fallback supplies the TLS config while the trust store is not
	// configured (tests, embedders): typically config.TLSClientConfig.
	Fallback func() *tls.Config

	mu       sync.Mutex
	gen      uint64
	built    bool
	normal   *http.Transport
	insecure *http.Transport
}

func (t *Transport) build() {
	pool := Pool()
	if pool == nil && t.Fallback != nil {
		if fb := t.Fallback(); fb != nil {
			pool = fb.RootCAs
		}
	}
	if len(t.Extra) > 0 {
		if pool != nil {
			pool = pool.Clone()
		} else if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
			pool = sys
		} else {
			pool = x509.NewCertPool()
		}
		for _, c := range t.Extra {
			pool.AddCert(c)
		}
	}
	mk := func(cfg *tls.Config) *http.Transport {
		tr := baseTransport.Clone()
		tr.TLSClientConfig = cfg
		if t.Tune != nil {
			t.Tune(tr)
		}
		tr.TLSClientConfig = cfg // Tune must not replace verification
		return tr
	}
	old, oldInsecure := t.normal, t.insecure
	t.normal = mk(revocation.Wrap(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}))
	//nolint:gosec // per-connection admin opt-in, refused by the FIPS edition and STIG profile (see SetUntrustedAllowed)
	t.insecure = mk(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
	t.gen, t.built = Generation(), true
	if old != nil {
		old.CloseIdleConnections()
		oldInsecure.CloseIdleConnections()
	}
}

func (t *Transport) transports() (*http.Transport, *http.Transport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.built || t.gen != Generation() {
		t.build()
	}
	return t.normal, t.insecure
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	normal, insecure := t.transports()
	if req.URL.Scheme == "https" && UntrustedHost(HostPort(req.URL.Scheme, req.URL.Host)) {
		return insecure.RoundTrip(req)
	}
	return normal.RoundTrip(req)
}

// CloseIdleConnections closes both pools' idle connections.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.built {
		t.normal.CloseIdleConnections()
		t.insecure.CloseIdleConnections()
	}
}
