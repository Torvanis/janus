// Package config loads and validates all runtime configuration from environment
// variables. Nothing below this package may call os.Getenv: configuration is read
// once at startup, validated, and passed explicitly.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultDatabaseURL is the embedded-SQLite database used when
// JANUS_DATABASE_URL is unset: a zero-configuration, single-node evaluation
// mode. Production deployments set an explicit postgres:// URL.
const DefaultDatabaseURL = "sqlite:///data/janus.db"

// DefaultBuildVersion is the version used when JANUS_BUILD_VERSION is unset.
// Release builds set it with the Go linker -X flag.
var DefaultBuildVersion = "dev"

// DefaultBuildSHA is the revision used when JANUS_BUILD_SHA is unset.
var DefaultBuildSHA = "unknown"

// DefaultBuildDate is the release date used when JANUS_BUILD_DATE is unset.
// Release builds set YYYY-MM-DD for perpetual-license maintenance checks.
var DefaultBuildDate = ""

// Config is the fully validated runtime configuration for the gateway.
type Config struct {
	// Server
	ListenAddr   string
	PublicURL    string
	TLSCertPath  string
	TLSKeyPath   string
	LogLevel     string
	Environment  string
	BuildVersion string
	BuildSHA     string
	// BuildDate (YYYY-MM-DD) is the release date; perpetual keys are checked
	// against it before migrations run. Empty for dev builds (never refuses).
	BuildDate string
	// LicenseFile is an optional path to a JANUS-LICENSE-1 key file; when set
	// and present it takes precedence over a key installed via Admin → System.
	LicenseFile string
	// UpdateCheck enables the daily, opt-in version check against
	// janusedge.com. Forced off by Offline.
	UpdateCheck bool
	// Offline asserts an air-gapped deployment: no update check, ever.
	Offline bool

	// Storage
	DatabaseURL string
	// DatabaseURLDefaulted is true when JANUS_DATABASE_URL was unset and the
	// embedded-SQLite default was applied. main logs a prominent warning in
	// that case; an explicitly configured URL — even a sqlite one — does not
	// trigger it.
	DatabaseURLDefaulted bool

	// Crypto
	EncryptionKey []byte

	// Identity
	OIDCProviderURL  string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCScopes       []string
	OIDCEmailClaim   string
	OIDCNameClaim    string
	OIDCGroupsClaim  string
	// DevAuthEnabled turns on a local username/password-free login used for
	// evaluation deployments where no IdP exists yet. Never enable in production.
	DevAuthEnabled bool

	BootstrapAdminEmails []string
	// AdminGroups lists IdP group names (normalized to lowercase) whose
	// members are promoted to admin on sign-in, mirroring the promote-only
	// semantics of BootstrapAdminEmails. Optional: empty means group-based
	// admin promotion is inert.
	AdminGroups []string

	// LocalOnly puts the gateway in local-only mode (JANUS_LOCAL_ONLY) for
	// self-hosted deployments with no billing relationship: cost computation
	// is skipped (usage events record zero cost), USD-based quotas become
	// inert and can no longer be created, and every cost/spend/pricing
	// surface is hidden in the UI. Token, request, and latency accounting is
	// unaffected. Default false: behavior is identical to a billed deployment.
	LocalOnly bool

	// Sessions
	SessionTTL         time.Duration
	SessionIdleTimeout time.Duration
	CookieSecure       bool

	// Proxy behaviour
	TrustedProxies []*net.IPNet
	// UpstreamConnTimeout, UpstreamTTFBTimeout and UpstreamTotalTimeout are
	// the per-hop upstream timeouts as loaded from the environment
	// (JANUS_UPSTREAM_*_TIMEOUT_SECONDS). They are the DEFAULTS only: an
	// administrator may override any of them at runtime through
	// PATCH /api/v1/admin/system/upstream-timeouts, and the override is
	// persisted in the database so it survives restarts and applies on every
	// replica. The proxy therefore never reads these fields directly; it goes
	// through UpstreamTimeoutDefaults() merged with the stored overrides
	// (httpapi.Server.effectiveUpstreamTimeouts).
	UpstreamConnTimeout  time.Duration
	UpstreamTTFBTimeout  time.Duration
	UpstreamTotalTimeout time.Duration
	MaxResponseBytes     int64
	CABundlePath         string
	// ConfigCacheTTL bounds how stale the proxy hot path may see slow-changing
	// configuration (models, grants, rules, flags) on replicas other than the
	// one that processed an admin write. Users must see a
	// model enable/disable within 5 seconds, so the default is 5s and raising
	// it above 5 trades that contract for fewer database reads.
	ConfigCacheTTL time.Duration
	// CABundle is the trust store for every outbound TLS call (upstream
	// proxying, model discovery, alert webhooks, OIDC). It is the system pool
	// extended with the PEM certificates from JANUS_CA_BUNDLE, and nil when
	// the variable is unset (default verification applies).
	CABundle *x509.CertPool

	// Jobs
	DiscoveryInterval    time.Duration
	UsageRetentionDays   int
	AuditRetentionDays   int
	PurgeJobTimeUTC      string
	QuotaCheckpointEvery time.Duration

	// Troubleshooting mode (JANUS_TROUBLESHOOT_DIR). When set, an
	// administrator may choose the "disk" storage backend for captured
	// request/response bodies, which are then written as files under this
	// directory instead of into the database. Empty (the default) leaves
	// only the database backend available. Whatever the backend, capture
	// only happens while an administrator has explicitly enabled a
	// troubleshooting session; there is no always-on mode, because every
	// captured body is payload data the gateway otherwise never retains,
	// and a permanently-on session costs a buffered copy of every matching
	// response plus the storage the retention policy allows.
	TroubleshootDir string

	// Notifications
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	// Telemetry. OTELEndpoint empty means OTLP export is off and
	// Prometheus remains the only telemetry surface.
	OTELEndpoint       string
	OTELExportInterval time.Duration
}

// UpstreamTimeouts is one complete set of per-hop upstream timeouts: how long
// the gateway waits to establish a connection, to receive the first response
// byte (response headers) after sending the request, and for the whole
// exchange. A zero value for any hop means "unbounded" for that hop, which the
// test harness relies on; Load never produces zero because envSeconds rejects
// non-positive values.
type UpstreamTimeouts struct {
	Connect time.Duration
	TTFB    time.Duration
	Total   time.Duration
}

// MaxUpstreamTimeout is the largest value an administrator may set for any
// upstream timeout at runtime (24 hours). It exists to catch a mistyped
// value — 300000 instead of 300 — before it silently disables a safeguard,
// not to express any real operational limit.
const MaxUpstreamTimeout = 24 * time.Hour

// UpstreamTimeoutDefaults returns the environment-derived timeout set. These
// are the values in force until an administrator overrides them at runtime.
func (c *Config) UpstreamTimeoutDefaults() UpstreamTimeouts {
	return UpstreamTimeouts{Connect: c.UpstreamConnTimeout, TTFB: c.UpstreamTTFBTimeout, Total: c.UpstreamTotalTimeout}
}

// WithOverrides returns a copy of t where every positive override replaces
// the corresponding default. Overrides are expressed in whole seconds, the
// unit administrators see; zero (or negative) means "keep the default".
func (t UpstreamTimeouts) WithOverrides(connectSeconds, ttfbSeconds, totalSeconds int) UpstreamTimeouts {
	out := t
	if connectSeconds > 0 {
		out.Connect = time.Duration(connectSeconds) * time.Second
	}
	if ttfbSeconds > 0 {
		out.TTFB = time.Duration(ttfbSeconds) * time.Second
	}
	if totalSeconds > 0 {
		out.Total = time.Duration(totalSeconds) * time.Second
	}
	return out
}

// Validate reports whether the set is internally consistent: a first-byte or
// connect bound longer than the total bound can never fire, so accepting one
// would let an administrator believe they raised a limit that the total still
// cuts. Zero (unbounded) hops are skipped.
func (t UpstreamTimeouts) Validate() error {
	if t.Total > 0 && t.TTFB > t.Total {
		return fmt.Errorf("the time-to-first-byte timeout (%ds) must not exceed the total timeout (%ds)", int(t.TTFB.Seconds()), int(t.Total.Seconds()))
	}
	if t.Total > 0 && t.Connect > t.Total {
		return fmt.Errorf("the connect timeout (%ds) must not exceed the total timeout (%ds)", int(t.Connect.Seconds()), int(t.Total.Seconds()))
	}
	return nil
}

// Redacted returns a copy of the config safe to log or expose in admin status
// output: every secret is replaced with a presence indicator.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"listen_addr":            c.ListenAddr,
		"public_url":             c.PublicURL,
		"environment":            c.Environment,
		"log_level":              c.LogLevel,
		"database":               scheme(c.DatabaseURL),
		"oidc_provider_url":      c.OIDCProviderURL,
		"oidc_client_id":         c.OIDCClientID,
		"oidc_secret_configured": c.OIDCClientSecret != "",
		"dev_auth_enabled":       c.DevAuthEnabled,
		"local_only":             c.LocalOnly,
		"smtp_configured":        c.SMTPHost != "",
		"tls_configured":         c.TLSCertPath != "",
		"ca_bundle_configured":   c.CABundlePath != "",
		"usage_retention_days":   c.UsageRetentionDays,
		"audit_retention_days":   c.AuditRetentionDays,
		"otlp_endpoint":          c.OTELEndpoint,
		// Presence only, never the values: emails and group names identify
		// people and internal org structure (mirrors the secret handling
		// above).
		"bootstrap_admin_emails_count": len(c.BootstrapAdminEmails),
		"admin_groups_count":           len(c.AdminGroups),
	}
}

func scheme(url string) string {
	if i := strings.Index(url, "://"); i > 0 {
		return url[:i]
	}
	if url == "" {
		return ""
	}
	return "file"
}

// Load reads configuration from the process environment and validates it.
// It returns an error naming the offending variable so operators can fix the
// deployment without reading source (requirements/non-functional: startup
// validation fails fast with a human-readable message).
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:           envOr("JANUS_LISTEN_ADDR", ":8080"),
		PublicURL:            strings.TrimRight(envOr("JANUS_PUBLIC_URL", "http://localhost:8080"), "/"),
		TLSCertPath:          os.Getenv("JANUS_TLS_CERT_PATH"),
		TLSKeyPath:           os.Getenv("JANUS_TLS_KEY_PATH"),
		LogLevel:             envOr("JANUS_LOG_LEVEL", "info"),
		Environment:          envOr("JANUS_ENV", "production"),
		BuildVersion:         envOr("JANUS_BUILD_VERSION", DefaultBuildVersion),
		BuildSHA:             envOr("JANUS_BUILD_SHA", DefaultBuildSHA),
		BuildDate:            envOr("JANUS_BUILD_DATE", DefaultBuildDate),
		LicenseFile:          envOr("JANUS_LICENSE_FILE", ""),
		UpdateCheck:          envBool("JANUS_UPDATE_CHECK", false),
		Offline:              envBool("JANUS_OFFLINE", false),
		DatabaseURL:          envOr("JANUS_DATABASE_URL", DefaultDatabaseURL),
		OIDCProviderURL:      strings.TrimRight(os.Getenv("JANUS_OIDC_PROVIDER_URL"), "/"),
		OIDCClientID:         os.Getenv("JANUS_OIDC_CLIENT_ID"),
		OIDCClientSecret:     os.Getenv("JANUS_OIDC_CLIENT_SECRET"),
		OIDCEmailClaim:       envOr("JANUS_OIDC_EMAIL_CLAIM", "email"),
		OIDCNameClaim:        envOr("JANUS_OIDC_NAME_CLAIM", "name"),
		OIDCGroupsClaim:      envOr("JANUS_OIDC_GROUPS_CLAIM", "groups"),
		DevAuthEnabled:       envBool("JANUS_DEV_AUTH", false),
		LocalOnly:            envBool("JANUS_LOCAL_ONLY", false),
		SMTPHost:             os.Getenv("JANUS_SMTP_HOST"),
		SMTPUser:             os.Getenv("JANUS_SMTP_USER"),
		SMTPPass:             os.Getenv("JANUS_SMTP_PASSWORD"),
		SMTPFrom:             envOr("JANUS_SMTP_FROM", "janus@localhost"),
		CABundlePath:         os.Getenv("JANUS_CA_BUNDLE"),
		PurgeJobTimeUTC:      envOr("JANUS_PURGE_JOB_TIME_UTC", "02:00"),
		TroubleshootDir:      strings.TrimSpace(os.Getenv("JANUS_TROUBLESHOOT_DIR")),
		OIDCScopes:           splitList(envOr("JANUS_OIDC_SCOPES", "openid,profile,email,groups")),
		BootstrapAdminEmails: lowerAll(splitList(os.Getenv("JANUS_BOOTSTRAP_ADMIN_EMAILS"))),
		AdminGroups:          normalizeSet(splitList(os.Getenv("JANUS_ADMIN_GROUPS"))),
	}
	// the standard OpenTelemetry variable wins; the JANUS_-prefixed
	// alias exists for deployments that namespace every variable.
	c.OTELEndpoint = envOr("OTEL_EXPORTER_OTLP_ENDPOINT", strings.TrimSpace(os.Getenv("JANUS_OTEL_EXPORTER_OTLP_ENDPOINT")))

	// Malformed database URLs are diagnosed by store.Open, which owns dialect
	// detection; config only records whether the embedded default applied.
	c.DatabaseURLDefaulted = strings.TrimSpace(os.Getenv("JANUS_DATABASE_URL")) == ""

	var err error
	if c.SMTPPort, err = envInt("JANUS_SMTP_PORT", 587); err != nil {
		return nil, err
	}
	if c.UsageRetentionDays, err = envInt("JANUS_USAGE_RETENTION_DAYS", 365); err != nil {
		return nil, err
	}
	if c.AuditRetentionDays, err = envInt("JANUS_AUDIT_RETENTION_DAYS", 730); err != nil {
		return nil, err
	}
	if c.SessionTTL, err = envHours("JANUS_SESSION_TTL_HOURS", 24); err != nil {
		return nil, err
	}
	if c.SessionIdleTimeout, err = envHours("JANUS_SESSION_IDLE_TIMEOUT_HOURS", 4); err != nil {
		return nil, err
	}
	if c.ConfigCacheTTL, err = envSeconds("JANUS_CONFIG_CACHE_TTL_SECONDS", 5); err != nil {
		return nil, err
	}
	if c.QuotaCheckpointEvery, err = envHours("JANUS_QUOTA_CHECKPOINT_INTERVAL_HOURS", 24); err != nil {
		return nil, err
	}
	// Default 2 minutes: discovery doubles as the upstream reachability
	// probe (it records each upstream's last check and error), so a short
	// cadence is what makes the admin Upstreams view and managed-model
	// fallback react to an outage in minutes rather than an hour. It is a
	// single GET /models per upstream per cycle. Admins can change it at
	// runtime from System → Model discovery without a restart.
	if c.DiscoveryInterval, err = envMinutes("JANUS_DISCOVERY_INTERVAL_MINUTES", 2); err != nil {
		return nil, err
	}
	if c.UpstreamConnTimeout, err = envSeconds("JANUS_UPSTREAM_CONNECT_TIMEOUT_SECONDS", 10); err != nil {
		return nil, err
	}
	if c.UpstreamTTFBTimeout, err = envSeconds("JANUS_UPSTREAM_TTFB_TIMEOUT_SECONDS", 30); err != nil {
		return nil, err
	}
	if c.UpstreamTotalTimeout, err = envSeconds("JANUS_UPSTREAM_TOTAL_TIMEOUT_SECONDS", 600); err != nil {
		return nil, err
	}
	if c.OTELExportInterval, err = envSeconds("JANUS_OTEL_EXPORT_INTERVAL_SECONDS", 30); err != nil {
		return nil, err
	}
	maxBytes, err := envInt("JANUS_MAX_RESPONSE_BYTES", 100*1024*1024)
	if err != nil {
		return nil, err
	}
	c.MaxResponseBytes = int64(maxBytes)

	if c.EncryptionKey, err = loadEncryptionKey(); err != nil {
		return nil, err
	}
	if c.TrustedProxies, err = parseCIDRs(os.Getenv("JANUS_TRUSTED_PROXIES")); err != nil {
		return nil, err
	}
	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return nil, fmt.Errorf("JANUS_TLS_CERT_PATH and JANUS_TLS_KEY_PATH must be set together")
	}
	if c.CABundle, err = loadCABundle(c.CABundlePath); err != nil {
		return nil, err
	}
	c.CookieSecure = envBool("JANUS_COOKIE_SECURE", strings.HasPrefix(c.PublicURL, "https://"))

	// An identity provider is optional: with none configured Janus offers
	// local accounts (first run creates the administrator). But a HALF
	// configured one is a mistake, so all-or-nothing is enforced.
	if !c.DevAuthEnabled && c.OIDCEnabled() {
		missing := []string{}
		if c.OIDCClientID == "" {
			missing = append(missing, "JANUS_OIDC_CLIENT_ID")
		}
		if c.OIDCClientSecret == "" {
			missing = append(missing, "JANUS_OIDC_CLIENT_SECRET")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("JANUS_OIDC_PROVIDER_URL is set, so %s must be set too", strings.Join(missing, ", "))
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("JANUS_LOG_LEVEL must be one of debug, info, warn, error (got %q)", c.LogLevel)
	}
	if err := devAuthGuard(c); err != nil {
		return nil, err
	}
	return c, nil
}

// devAuthGuard refuses to boot with the JANUS_DEV_AUTH authentication bypass on
// anything that looks production-facing. Two independent tripwires:
//
//   - JANUS_ENV is explicitly "production" (the raw variable is consulted, not
//     the defaulted field, so the loopback quick start — which sets neither —
//     keeps working);
//   - the public URL is not loopback/localhost, i.e. the deployment is
//     reachable by other people.
//
// JANUS_DEV_AUTH_ALLOW_UNSAFE=true overrides both for intentional, isolated
// demos; the startup warning still fires.
func devAuthGuard(c *Config) error {
	if !c.DevAuthEnabled || envBool("JANUS_DEV_AUTH_ALLOW_UNSAFE", false) {
		return nil
	}
	const remedy = "configure OIDC (JANUS_OIDC_PROVIDER_URL, JANUS_OIDC_CLIENT_ID, JANUS_OIDC_CLIENT_SECRET) instead, or set JANUS_DEV_AUTH_ALLOW_UNSAFE=true only for an intentional, isolated demo"
	if strings.EqualFold(strings.TrimSpace(os.Getenv("JANUS_ENV")), "production") {
		return fmt.Errorf("JANUS_DEV_AUTH=true signs in any email address with no identity provider, and JANUS_ENV=production: refusing to start; %s", remedy)
	}
	if !isLoopbackURL(c.PublicURL) {
		return fmt.Errorf("JANUS_DEV_AUTH=true signs in any email address with no identity provider, but JANUS_PUBLIC_URL (%s) is not loopback, so other people can reach this deployment: refusing to start; %s", c.PublicURL, remedy)
	}
	return nil
}

// isLoopbackURL reports whether the URL's host is localhost or a loopback IP.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadEncryptionKey resolves the AES-256-GCM key used for upstream credential
// storage. A 32-byte key is required; hex and base64-ish raw forms are accepted.
func loadEncryptionKey() ([]byte, error) {
	raw := os.Getenv("JANUS_ENCRYPTION_KEY")
	if raw == "" {
		return nil, fmt.Errorf("JANUS_ENCRYPTION_KEY is required: supply 32 bytes (64 hex characters), e.g. `openssl rand -hex 32`")
	}
	key, err := decodeKey(raw)
	if err != nil {
		return nil, fmt.Errorf("JANUS_ENCRYPTION_KEY is invalid: %w", err)
	}
	return key, nil
}

// loadCABundle builds the outbound trust store: the system roots extended with
// every PEM certificate in the JANUS_CA_BUNDLE file. It returns nil (default
// verification) when the variable is unset, and fails fast with a
// named-variable error when the file is unreadable or contains no usable PEM
// certificate, so a mistyped path is caught at startup rather than as x509
// errors on the first upstream call.
func loadCABundle(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("JANUS_CA_BUNDLE points to %q which cannot be read: %v", path, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		// Distroless images may ship no system store at all; the operator's
		// bundle is then the entire trust store.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("JANUS_CA_BUNDLE file %q contains no valid PEM certificates", path)
	}
	return pool, nil
}

// TLSClientConfig returns the tls.Config every outbound HTTP client must use,
// or nil when no private CA bundle is configured (default verification).
func (c *Config) TLSClientConfig() *tls.Config {
	if c.CABundle == nil {
		return nil
	}
	return &tls.Config{RootCAs: c.CABundle}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (got %q)", key, v)
	}
	return n, nil
}

func envHours(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return time.Duration(n) * time.Hour, nil
}

func envMinutes(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return time.Duration(n) * time.Minute, nil
}

func envSeconds(key string, def int) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return time.Duration(n) * time.Second, nil
}

func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '	' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(v)
	}
	return out
}

// normalizeSet lowercases every entry, drops duplicates, and sorts the result
// so downstream comparisons and logs are deterministic regardless of the
// order the operator wrote the list in.
func normalizeSet(in []string) []string {
	out := lowerAll(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func parseCIDRs(v string) ([]*net.IPNet, error) {
	out := []*net.IPNet{}
	for _, item := range splitList(v) {
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("JANUS_TRUSTED_PROXIES contains %q which is not an IP address or CIDR block", item)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("JANUS_TRUSTED_PROXIES contains %q which is not a valid CIDR block", item)
		}
		out = append(out, network)
	}
	return out, nil
}

// OIDCEnabled reports whether an identity provider is configured. Without
// one, sign-in is by local account (see internal/httpapi/local_auth.go).
func (c *Config) OIDCEnabled() bool { return c.OIDCProviderURL != "" }
