package stigprofile

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/license"
)

// goodConfig is a configuration the STIG profile accepts: PostgreSQL,
// https behind a declared TLS-terminating proxy, offline, 913-day audit.
func goodConfig() *config.Config {
	_, proxies, _ := net.ParseCIDR("10.0.0.0/8")
	off := false
	return &config.Config{
		STIGProfile: true, DatabaseURL: "postgres://janus@db/janus", PublicURL: "https://janus.example.mil",
		CookieSecure: true, TrustedProxies: []*net.IPNet{proxies}, Offline: true, LicenseSync: &off,
		AuditRetentionDays: MinAuditRetentionDays, TLSRevocation: "hard",
	}
}

func TestCheckAcceptsTheDocumentedConfiguration(t *testing.T) {
	if err := Check(goodConfig()); err != nil {
		t.Fatalf("documented STIG configuration refused: %v", err)
	}
	direct := goodConfig()
	direct.TrustedProxies = nil
	direct.TLSCertPath, direct.TLSKeyPath = "/tls/tls.crt", "/tls/tls.key"
	if err := Check(direct); err != nil {
		t.Fatalf("direct TLS refused: %v", err)
	}
	self := goodConfig()
	self.TrustedProxies = nil
	self.TLSSelfSigned, self.TLSRequire = true, true
	if err := Check(self); err != nil {
		t.Fatalf("self-signed direct TLS refused: %v", err)
	}
	if p := Effective(self); p.Transport != "direct-tls-self-signed" || !p.ListenerTLSRequired || len(p.Deviations) != 1 || !strings.Contains(p.Deviations[0], "self-signed") {
		t.Fatalf("self-signed profile: %+v", p)
	}
	if p := Effective(goodConfig()); len(p.Deviations) != 0 {
		t.Fatalf("deviations without self-signed: %+v", p.Deviations)
	}
	soft := goodConfig()
	soft.TLSRevocation = "soft"
	if err := Check(soft); err != nil {
		t.Fatalf("soft revocation refused: %v", err)
	}
	if p := Effective(direct); p.Transport != "direct-tls" || !p.AuditFailClosed || p.TOTPAlgorithm != "hmac-sha256" {
		t.Fatalf("effective profile: %+v", p)
	}
}

// One test per refusal: each must fail with its own named error, so an
// operator (and the checklist evidence) can tell exactly what was refused.
func TestCheckRefusals(t *testing.T) {
	on := true
	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   error
		phrase string
	}{
		{"sqlite explicit", func(c *config.Config) { c.DatabaseURL = "sqlite:///data/janus.db" }, ErrSQLite, "requires PostgreSQL"},
		{"sqlite default", func(c *config.Config) { c.DatabaseURL, c.DatabaseURLDefaulted = config.DefaultDatabaseURL, true }, ErrSQLite, "JANUS_DATABASE_URL is unset"},
		{"dev auth", func(c *config.Config) { c.DevAuthEnabled = true }, ErrDevAuth, "JANUS_DEV_AUTH"},
		{"plain http public url", func(c *config.Config) { c.PublicURL = "http://janus.example.mil" }, ErrPlainHTTP, "https://"},
		{"no tls and no proxy", func(c *config.Config) { c.TrustedProxies = nil }, ErrPlainHTTP, "JANUS_TRUSTED_PROXIES"},
		{"insecure cookies", func(c *config.Config) { c.CookieSecure = false }, ErrPlainHTTP, "JANUS_COOKIE_SECURE"},
		{"audit retention 730", func(c *config.Config) { c.AuditRetentionDays = 730 }, ErrAuditRetention, "913"},
		{"audit retention 912", func(c *config.Config) { c.AuditRetentionDays = 912 }, ErrAuditRetention, "913"},
		{"license sync on", func(c *config.Config) { c.LicenseSync = &on }, ErrPhoneHome, "JANUS_LICENSE_SYNC"},
		{"update check on", func(c *config.Config) { c.UpdateCheckRequested = true }, ErrPhoneHome, "JANUS_UPDATE_CHECK"},
		{"offline off", func(c *config.Config) { c.OfflineDisabled = true }, ErrPhoneHome, "JANUS_OFFLINE=false"},
		{"revocation off", func(c *config.Config) { c.TLSRevocation = "off" }, ErrRevocationOff, "JANUS_TLS_REVOCATION=off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodConfig()
			tc.mutate(c)
			err := Check(c)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.phrase) || !strings.Contains(err.Error(), "STIG profile") {
				t.Fatalf("want %v mentioning %q, got %v", tc.want, tc.phrase, err)
			}
		})
	}
}

func TestCheckReportsEveryRefusalAtOnce(t *testing.T) {
	c := goodConfig()
	c.DatabaseURL, c.DevAuthEnabled, c.AuditRetentionDays = "sqlite:///x.db", true, 30
	err := Check(c)
	for _, want := range []error{ErrSQLite, ErrDevAuth, ErrAuditRetention} {
		if !errors.Is(err, want) {
			t.Fatalf("missing %v in %v", want, err)
		}
	}
}

func TestCheckWithoutTheProfile(t *testing.T) {
	c := &config.Config{DatabaseURL: config.DefaultDatabaseURL, DatabaseURLDefaulted: true, PublicURL: "http://localhost:8080", DevAuthEnabled: true, AuditRetentionDays: 1}
	err := Check(c)
	if fipsmode.IsFIPSEdition() {
		// The FIPS edition refuses SQLite on its own, and nothing else.
		if !errors.Is(err, ErrSQLite) || errors.Is(err, ErrDevAuth) || errors.Is(err, ErrAuditRetention) || !strings.Contains(err.Error(), "FIPS edition") {
			t.Fatalf("FIPS edition without the profile: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("default edition without the profile must accept anything: %v", err)
	}
}

func TestCheckUntrustedUpstreams(t *testing.T) {
	prev := Set(true)
	t.Cleanup(func() { Set(prev) })
	if err := CheckUntrustedUpstreams([]string{"lab-vllm"}); !errors.Is(err, ErrUntrustedUpstream) || !strings.Contains(err.Error(), "lab-vllm") {
		t.Fatalf("untrusted upstream accepted: %v", err)
	}
	if err := CheckUntrustedUpstreams(nil); err != nil {
		t.Fatal(err)
	}
	Set(false)
	if err := CheckUntrustedUpstreams([]string{"lab-vllm"}); err != nil {
		t.Fatalf("refused without the profile: %v", err)
	}
}

func TestCheckDirectory(t *testing.T) {
	prev := Set(true)
	t.Cleanup(func() { Set(prev) })
	if err := CheckDirectory(true, true); !errors.Is(err, ErrLDAPSkipVerify) {
		t.Fatalf("skip_verify directory accepted: %v", err)
	}
	if err := CheckDirectory(false, true); err != nil {
		t.Fatalf("disabled directory refused: %v", err)
	}
	if err := CheckDirectory(true, false); err != nil {
		t.Fatalf("verifying directory refused: %v", err)
	}
	Set(false)
	if err := CheckDirectory(true, true); err != nil {
		t.Fatalf("refused without the profile: %v", err)
	}
}

// The profile reuses the FIPS edition's TOTP and LDAP logic, keyed on
// "FIPS edition OR STIG profile".
func TestRestrictedFollowsEditionOrProfile(t *testing.T) {
	prev := Set(false)
	t.Cleanup(func() { Set(prev) })
	for _, f := range []fipsmode.Feature{fipsmode.FeatureTOTPSHA1, fipsmode.FeatureLDAPSkipVerify, fipsmode.FeatureDevAuth} {
		if Restricted(f) != fipsmode.IsFIPSEdition() {
			t.Fatalf("%s without profile: restricted=%v", f, Restricted(f))
		}
	}
	Set(true)
	for _, f := range []fipsmode.Feature{fipsmode.FeatureTOTPSHA1, fipsmode.FeatureLDAPSkipVerify, fipsmode.FeatureDevAuth, fipsmode.FeatureUntrustedTLS} {
		if !Restricted(f) || Allowed(f) {
			t.Fatalf("%s not restricted in the STIG profile", f)
		}
	}
	// PDF export is a FIPS-edition restriction only.
	if Restricted(fipsmode.FeaturePDFExport) != fipsmode.IsFIPSEdition() {
		t.Fatal("STIG profile must not restrict PDF export by itself")
	}
}

// --- license gate ------------------------------------------------------------

type keyStore struct{ key string }

func (m *keyStore) LicenseKey(context.Context) (string, error)      { return m.key, nil }
func (m *keyStore) PutLicenseKey(_ context.Context, k string) error { m.key = k; return nil }
func (m *keyStore) DeleteLicenseKey(context.Context) error          { m.key = ""; return nil }

// stateFor loads raw through a real license.Manager, exactly as main does
// (signature verification against pubs; no network).
func stateFor(t *testing.T, raw string, pubs map[string]ed25519.PublicKey) license.State {
	t.Helper()
	m := license.NewManager("", &keyStore{key: raw}, pubs, nil)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m.State()
}

func signed(t *testing.T, priv ed25519.PrivateKey, c license.Claims) string {
	t.Helper()
	c.KeyID, c.Org, c.IssuedTo = "test", "Acme", "ops@acme.test"
	if c.Issued.IsZero() {
		c.Issued = time.Now().Add(-48 * time.Hour)
	}
	raw, err := license.Sign(priv, c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The license matrix for both the FIPS edition and the STIG profile: no
// key, invalid key, expired Business key, valid trial key, Community key,
// plus a pre-existing Business key without the new feature names.
func TestCheckLicenseMatrix(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubs := map[string]ed25519.PublicKey{"test": pub}
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	past := time.Now().Add(-400 * 24 * time.Hour)
	soon := time.Now().Add(14 * 24 * time.Hour)
	year := time.Now().Add(365 * 24 * time.Hour)
	oldBusiness := []string{"scim", "ldap", "multi_oidc", "ha", "guardrails_enforce", "reports_scheduled", "captures", "audit_export", "model_fallbacks", "email_alerts", "load_balancing"}

	keys := map[string]struct {
		raw string
		ok  bool
		why string
	}{
		"no key":  {"", false, "no license key is installed"},
		"invalid": {signed(t, otherPriv, license.Claims{LicenseID: "JNS-BUS-X", Edition: license.EditionBusiness, Exp: &year, Term: license.TermSubscription}), false, "not valid"},
		"garbage": {"JANUS-LICENSE-1.nope.nope", false, "not valid"},
		"expired business past grace": {signed(t, priv, license.Claims{LicenseID: "JNS-BUS-EXP", Edition: license.EditionBusiness, Features: license.BusinessFeatures,
			Exp: &past, GraceDays: 30, Term: license.TermSubscription}), true, ""},
		"valid trial": {signed(t, priv, license.Claims{LicenseID: "JNS-BUS-TRIAL", Edition: license.EditionBusiness, Features: license.BusinessFeatures,
			Exp: &soon, GraceDays: 0, Term: license.TermTrial}), true, ""},
		"enterprise perpetual": {signed(t, priv, license.Claims{LicenseID: "JNS-ENT-1", Edition: license.EditionEnterprise, Features: license.EnterpriseFeatures,
			Term: license.TermPerpetual, MaintenanceUntil: "2030-01-01"}), true, ""},
		"business without new feature names": {signed(t, priv, license.Claims{LicenseID: "JNS-BUS-OLD", Edition: license.EditionBusiness, Features: oldBusiness,
			Exp: &year, GraceDays: 30, Term: license.TermSubscription}), true, ""},
		"community": {signed(t, priv, license.Claims{LicenseID: "JNS-COM-1", Edition: license.EditionCommunity, Features: []string{"fips_edition", "stig_profile"},
			Exp: &year, Term: license.TermSubscription}), false, "community edition key"},
	}
	for _, profile := range []bool{false, true} {
		prev := Set(profile)
		t.Cleanup(func() { Set(prev) })
		for name, k := range keys {
			st := stateFor(t, k.raw, pubs)
			err := CheckLicense(st)
			if !fipsmode.IsFIPSEdition() && !profile {
				if err != nil {
					t.Fatalf("default edition without the profile needs no license (%s): %v", name, err)
				}
				continue
			}
			if k.ok {
				if err != nil {
					t.Fatalf("profile=%v %s refused: %v", profile, name, err)
				}
				continue
			}
			if !errors.Is(err, ErrLicenseRequired) {
				t.Fatalf("profile=%v %s accepted (%+v)", profile, name, st)
			}
			msg := err.Error()
			for _, want := range []string{k.why, "Business or Enterprise", "https://janusedge.com/pricing", "JANUS_LICENSE_FILE"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("profile=%v %s: message %q lacks %q", profile, name, msg, want)
				}
			}
			if strings.Contains(msg, "\n") {
				t.Fatalf("refusal must be one line: %q", msg)
			}
		}
	}
}

func TestLicenseRequiredNames(t *testing.T) {
	prev := Set(true)
	t.Cleanup(func() { Set(prev) })
	if !strings.Contains(LicenseRequired(), "STIG profile") {
		t.Fatalf("profile on: %q", LicenseRequired())
	}
	Set(false)
	if got := LicenseRequired(); (got != "") != fipsmode.IsFIPSEdition() {
		t.Fatalf("profile off: %q", got)
	}
}
