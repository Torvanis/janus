// Package stigprofile is the Janus STIG configuration profile: one switch,
// JANUS_STIG_PROFILE=true, that sets the documented STIG defaults, refuses
// settings that would undo them, and makes a few runtime behaviors stricter
// (approved-only local TOTP, no LDAP skip_verify, fail-closed audit for
// administrative changes).
//
// The profile is designed to support a customer's ATO against the DISA
// Application Security and Development STIG. It is a configuration, not a
// certification. See docs/stig-profile.md for every default and refusal.
//
// The profile is a Business feature: a binary running it (and every Janus
// FIPS edition binary) refuses to start without a signature-verified
// Business or Enterprise key (license.RequireBusinessEdition).
//
// Some refusals also apply to the Janus FIPS edition without the profile,
// because both target the same deployments: SQLite is refused by either.
package stigprofile

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/license"
)

// MinAuditRetentionDays is the STIG floor for audit retention (30 months,
// ASD STIG APSC-DV-002900 via the DoD record retention baseline). It is
// also the profile's default when JANUS_AUDIT_RETENTION_DAYS is unset.
const MinAuditRetentionDays = config.STIGMinAuditRetentionDays

var enabled atomic.Bool

// Enabled reports whether the STIG profile is in force in this process.
// main sets it from the loaded configuration before anything else runs.
func Enabled() bool { return enabled.Load() }

// Set switches the profile on or off for this process and returns the
// previous value. Production code calls it once at startup; tests use it
// to exercise both modes (restore the previous value in t.Cleanup).
func Set(on bool) (previous bool) { return enabled.Swap(on) }

// Restricted reports whether the FIPS edition or the STIG profile switches
// feature f off. The profile adds no restrictions of its own beyond the ones
// listed here; it reuses the FIPS edition's names so code asks one question.
func Restricted(f fipsmode.Feature) bool {
	if !fipsmode.Allowed(f) {
		return true
	}
	if !Enabled() {
		return false
	}
	switch f {
	case fipsmode.FeatureTOTPSHA1, fipsmode.FeatureLDAPSkipVerify, fipsmode.FeatureDevAuth, fipsmode.FeatureUntrustedTLS:
		return true
	}
	return false
}

// Allowed is the inverse of Restricted: whether f is available in this
// edition and profile.
func Allowed(f fipsmode.Feature) bool { return !Restricted(f) }

// Refusal errors. Check wraps one of these per refused setting, so tests
// and operators can tell them apart with errors.Is.
var (
	ErrSQLite         = errors.New("SQLite is not supported")
	ErrDevAuth        = errors.New("JANUS_DEV_AUTH is not allowed")
	ErrPlainHTTP      = errors.New("plain HTTP is not allowed")
	ErrAuditRetention = errors.New("audit retention is below the STIG minimum")
	ErrPhoneHome      = errors.New("contacting janusedge.com is not allowed")
	ErrLDAPSkipVerify = errors.New("LDAP skip_verify is not allowed")
	ErrRevocationOff  = errors.New("certificate revocation checking cannot be turned off")
	// ErrLicenseRequired is returned (wrapped) by CheckLicense.
	ErrLicenseRequired = errors.New("a Business or Enterprise license is required")
)

const errProfileRefusals = "the STIG profile (JANUS_STIG_PROFILE=true)"

// Profile describes the settings in force, for the startup log line and the
// System status page.
type Profile struct {
	Enabled            bool   `json:"enabled"`
	Database           string `json:"database"`
	Transport          string `json:"transport"`
	Offline            bool   `json:"offline"`
	AuditRetentionDays int    `json:"audit_retention_days"`
	AuditFailClosed    bool   `json:"audit_fail_closed"`
	TOTPAlgorithm      string `json:"totp_algorithm"`
	TLSRevocation      string `json:"tls_revocation"`
	// ListenerTLSRequired is JANUS_TLS_REQUIRE: Janus itself must serve TLS.
	ListenerTLSRequired bool `json:"listener_tls_required"`
	// Deviations are documented departures from the STIG baseline that the
	// checklist lists (for example a self-signed listener certificate).
	Deviations []string `json:"deviations"`
	Restricted []string `json:"restricted_features"`
}

// Effective reports the profile as applied to cfg.
func Effective(cfg *config.Config) Profile {
	p := Profile{Enabled: cfg.STIGProfile, Database: databaseScheme(cfg.DatabaseURL), Offline: cfg.Offline,
		AuditRetentionDays: cfg.AuditRetentionDays, AuditFailClosed: cfg.STIGProfile, TOTPAlgorithm: "hmac-sha1", TLSRevocation: cfg.TLSRevocation, ListenerTLSRequired: cfg.TLSRequire, Deviations: []string{},
		Restricted: []string{}}
	switch {
	case cfg.TLSCertPath != "":
		p.Transport = "direct-tls"
	case cfg.TLSSelfSigned:
		p.Transport = "direct-tls-self-signed"
	case len(cfg.TrustedProxies) > 0 && strings.HasPrefix(cfg.PublicURL, "https://"):
		p.Transport = "tls-terminating-proxy"
	default:
		p.Transport = "plain-http"
	}
	if cfg.TLSSelfSigned {
		// APSC-DV-002300 expects DoD-approved PKI certificates; a
		// self-signed listener certificate is a documented, temporary
		// deviation the customer replaces with an issued one.
		p.Deviations = append(p.Deviations, "self-signed listener certificate (replace with a DoD-issued certificate)")
	}
	if fipsmode.IsFIPSEdition() || cfg.STIGProfile {
		p.TOTPAlgorithm = "hmac-sha256"
	}
	if cfg.STIGProfile {
		p.Restricted = []string{string(fipsmode.FeatureTOTPSHA1), string(fipsmode.FeatureLDAPSkipVerify), string(fipsmode.FeatureDevAuth), string(fipsmode.FeatureUntrustedTLS)}
	}
	return p
}

// Check refuses settings the STIG profile (or, for SQLite, the FIPS
// edition) does not allow. It returns nil when nothing is refused, and
// otherwise every refusal joined into one error, each wrapping its named
// sentinel. It needs no database and no network.
func Check(cfg *config.Config) error {
	var errs []error
	refuse := func(sentinel error, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...)))
	}
	who := errProfileRefusals
	if !cfg.STIGProfile {
		who = "the Janus FIPS edition"
	}
	if (cfg.STIGProfile || fipsmode.IsFIPSEdition()) && databaseScheme(cfg.DatabaseURL) != "postgres" {
		what := "JANUS_DATABASE_URL is a SQLite database"
		if cfg.DatabaseURLDefaulted {
			what = "JANUS_DATABASE_URL is unset, so Janus would use its embedded SQLite file"
		}
		refuse(ErrSQLite, "%s requires PostgreSQL, but %s; set JANUS_DATABASE_URL=postgres://…", who, what)
	}
	if !cfg.STIGProfile {
		return errors.Join(errs...)
	}
	if cfg.DevAuthEnabled {
		refuse(ErrDevAuth, "%s refuses the JANUS_DEV_AUTH sign-in bypass; use local accounts or an identity provider", who)
	}
	if err := transportProblem(cfg); err != "" {
		refuse(ErrPlainHTTP, "%s requires TLS: %s", who, err)
	}
	if cfg.AuditRetentionDays < MinAuditRetentionDays {
		refuse(ErrAuditRetention, "%s requires JANUS_AUDIT_RETENTION_DAYS of at least %d (30 months); it is %d", who, MinAuditRetentionDays, cfg.AuditRetentionDays)
	}
	if cfg.TLSRevocation == "off" {
		refuse(ErrRevocationOff, "%s checks every outbound server certificate for revocation; remove JANUS_TLS_REVOCATION=off, or use soft while an OCSP responder or CRL is being made reachable", who)
	}
	if cfg.LicenseSync != nil && *cfg.LicenseSync {
		refuse(ErrPhoneHome, "%s runs offline; remove JANUS_LICENSE_SYNC=true and install renewed keys by file or under Administration → Settings → License & updates", who)
	}
	if cfg.UpdateCheckRequested {
		refuse(ErrPhoneHome, "%s runs offline; remove JANUS_UPDATE_CHECK=true", who)
	}
	if cfg.OfflineDisabled || !cfg.Offline {
		// Load turns Offline on in the profile; an explicit
		// JANUS_OFFLINE=false contradicts it.
		refuse(ErrPhoneHome, "%s runs offline; remove JANUS_OFFLINE=false", who)
	}
	return errors.Join(errs...)
}

// CheckDirectory refuses a stored LDAP directory that skips certificate
// verification. The directory lives in the database, so main calls this
// after the database opens; saving such a directory is refused by the admin
// API and the setting is ignored when connecting.
func CheckDirectory(enabled, skipVerify bool) error {
	if !Enabled() || !enabled || !skipVerify {
		return nil
	}
	return fmt.Errorf("%w: %s refuses the saved LDAP directory because it has skip_verify on; trust the directory's CA through JANUS_CA_BUNDLE, then turn skip_verify off (or start once without the profile to change it)", ErrLDAPSkipVerify, errProfileRefusals)
}

// ErrUntrustedUpstream is returned (wrapped) by CheckUntrustedUpstreams.
var ErrUntrustedUpstream = errors.New("upstreams with unverified TLS are not allowed")

// CheckUntrustedUpstreams refuses saved upstreams that skip TLS
// verification (allow_untrusted_tls). The setting lives in the database, so
// main calls this after the database opens; it is also refused when saved
// and ignored when connecting.
func CheckUntrustedUpstreams(names []string) error {
	if !Enabled() || len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s refuses %d upstream(s) with Allow untrusted certificate on (%s); upload the upstream's CA under Administration → Settings → Certificates (or mount it with JANUS_CA_DIR), then turn the setting off (or start once without the profile to change it)",
		ErrUntrustedUpstream, errProfileRefusals, len(names), strings.Join(names, ", "))
}

// transportProblem returns why cfg does not satisfy the transport rule, or
// "". The rule: the public URL is https://, cookies are Secure, and TLS is
// either terminated by Janus (JANUS_TLS_CERT_PATH) or by a declared proxy
// (JANUS_TRUSTED_PROXIES lists the proxy that terminates it).
func transportProblem(cfg *config.Config) string {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme != "https" {
		return fmt.Sprintf("JANUS_PUBLIC_URL must start with https:// (it is %q)", cfg.PublicURL)
	}
	if !cfg.CookieSecure {
		return "JANUS_COOKIE_SECURE=false would send session cookies over plain HTTP; remove it"
	}
	if cfg.TLSCertPath == "" && !cfg.TLSSelfSigned && len(cfg.TrustedProxies) == 0 {
		return "serve TLS directly (JANUS_TLS_CERT_PATH and JANUS_TLS_KEY_PATH, or JANUS_TLS_SELF_SIGNED=true), or declare the TLS-terminating proxy in JANUS_TRUSTED_PROXIES"
	}
	return ""
}

func databaseScheme(raw string) string {
	switch {
	case strings.HasPrefix(raw, "postgres://"), strings.HasPrefix(raw, "postgresql://"):
		return "postgres"
	default:
		return "sqlite"
	}
}

// LicenseRequired names what needs a Business license in this process (the
// FIPS edition, the STIG profile, or both), or "" when nothing does.
func LicenseRequired() string {
	switch {
	case fipsmode.IsFIPSEdition() && Enabled():
		return "The Janus FIPS edition with the STIG profile"
	case fipsmode.IsFIPSEdition():
		return "The Janus FIPS edition"
	case Enabled():
		return "The Janus STIG profile (JANUS_STIG_PROFILE=true)"
	}
	return ""
}

// CheckLicense refuses to run the FIPS edition or the STIG profile without
// a signature-verified Business or Enterprise key (trial keys are business
// or enterprise edition too). Expired keys pass: expiry never disrupts work.
// The key is read from JANUS_LICENSE_FILE or the database; the check makes
// no network call, so an offline key works in an air-gapped enclave.
func CheckLicense(st license.State) error {
	what := LicenseRequired()
	if what == "" {
		return nil
	}
	if err := license.RequireBusinessEdition(st); err != nil {
		return fmt.Errorf("%w: %s requires a Janus Business or Enterprise license key (trial keys qualify), but %v. Put the key in the file named by JANUS_LICENSE_FILE (no network needed). Start a free trial at %s",
			ErrLicenseRequired, what, err, license.TrialURL)
	}
	return nil
}
