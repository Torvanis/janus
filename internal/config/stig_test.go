package config

import (
	"os"
	"strings"
	"testing"
)

// unsetEnv removes k for the test (t.Setenv restores it afterwards).
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// JANUS_STIG_PROFILE=true sets the profile defaults: offline, license sync
// pinned off, update check off, 913-day audit retention. Contradicting
// settings are kept as given (and flagged) so stigprofile.Check can refuse
// them by name instead of silently overriding them.
func TestSTIGProfileDefaults(t *testing.T) {
	setBaseline(t)
	unsetEnv(t, "JANUS_OFFLINE", "JANUS_LICENSE_SYNC", "JANUS_UPDATE_CHECK", "JANUS_AUDIT_RETENTION_DAYS", "JANUS_TLS_REVOCATION", "JANUS_OCSP_RESPONDER")
	t.Setenv("JANUS_STIG_PROFILE", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.STIGProfile || !c.Offline || c.OfflineDisabled || c.UpdateCheck || c.UpdateCheckRequested {
		t.Fatalf("profile defaults: %+v", c)
	}
	if c.LicenseSync == nil || *c.LicenseSync {
		t.Fatalf("license sync must be pinned off: %v", c.LicenseSync)
	}
	if c.TLSRevocation != "hard" {
		t.Fatalf("revocation default in the profile = %q, want hard", c.TLSRevocation)
	}
	if c.AuditRetentionDays != STIGMinAuditRetentionDays {
		t.Fatalf("audit retention default = %d", c.AuditRetentionDays)
	}
	if c.DevAuth() {
		t.Fatal("dev auth must never be on in the STIG profile")
	}
	if r := c.Redacted(); r["stig_profile"] != true || r["offline"] != true {
		t.Fatalf("redacted: %v", r)
	}

	// Explicit contradictions are recorded, not overridden silently.
	t.Setenv("JANUS_OFFLINE", "false")
	t.Setenv("JANUS_UPDATE_CHECK", "true")
	t.Setenv("JANUS_LICENSE_SYNC", "true")
	t.Setenv("JANUS_AUDIT_RETENTION_DAYS", "365")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.OfflineDisabled || !c.UpdateCheckRequested || c.UpdateCheck || c.LicenseSync == nil || !*c.LicenseSync || c.AuditRetentionDays != 365 {
		t.Fatalf("contradictions not recorded: %+v", c)
	}
}

// Dev auth in the STIG profile reaches stigprofile.Check (a named refusal)
// even when JANUS_DEV_AUTH_ALLOW_UNSAFE is set, rather than being waved
// through by the override.
func TestSTIGProfileLeavesDevAuthToTheProfileCheck(t *testing.T) {
	setBaseline(t)
	t.Setenv("JANUS_STIG_PROFILE", "true")
	t.Setenv("JANUS_DEV_AUTH", "true")
	t.Setenv("JANUS_DEV_AUTH_ALLOW_UNSAFE", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load must leave the refusal to stigprofile.Check: %v", err)
	}
	if !c.DevAuthEnabled || c.DevAuth() {
		t.Fatalf("DevAuthEnabled=%v DevAuth()=%v", c.DevAuthEnabled, c.DevAuth())
	}
}

func TestDefaultsWithoutTheProfileAreUnchanged(t *testing.T) {
	setBaseline(t)
	unsetEnv(t, "JANUS_STIG_PROFILE", "JANUS_OFFLINE", "JANUS_LICENSE_SYNC", "JANUS_AUDIT_RETENTION_DAYS", "JANUS_TLS_REVOCATION", "JANUS_OCSP_RESPONDER")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSRevocation != "off" || c.TLSClientConfig() != nil {
		t.Fatalf("revocation must be off by default: %q", c.TLSRevocation)
	}
	if c.STIGProfile || c.Offline || c.LicenseSync != nil || c.AuditRetentionDays != 730 {
		t.Fatalf("default config changed: offline=%v sync=%v audit=%d", c.Offline, c.LicenseSync, c.AuditRetentionDays)
	}
}

func TestTLSRevocationSetting(t *testing.T) {
	setBaseline(t)
	unsetEnv(t, "JANUS_STIG_PROFILE", "JANUS_OCSP_RESPONDER", "JANUS_CA_BUNDLE")
	for _, mode := range []string{"soft", "hard", "HARD"} {
		t.Setenv("JANUS_TLS_REVOCATION", mode)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		cfg := c.TLSClientConfig()
		if cfg == nil || cfg.VerifyConnection == nil || cfg.MinVersion == 0 {
			t.Fatalf("%s: outbound TLS config does not carry the check: %+v", mode, cfg)
		}
	}
	t.Setenv("JANUS_TLS_REVOCATION", "strict")
	if _, err := Load(); err == nil {
		t.Fatal("unknown mode accepted")
	}
	t.Setenv("JANUS_TLS_REVOCATION", "hard")
	t.Setenv("JANUS_OCSP_RESPONDER", "ftp://x")
	if _, err := Load(); err == nil {
		t.Fatal("non-http OCSP responder accepted")
	}
	t.Setenv("JANUS_OCSP_RESPONDER", "http://ocsp.enclave.mil")
	if c, err := Load(); err != nil || c.OCSPResponder != "http://ocsp.enclave.mil" {
		t.Fatalf("responder: %v %v", c, err)
	}
	// The profile keeps an explicit soft.
	t.Setenv("JANUS_STIG_PROFILE", "true")
	t.Setenv("JANUS_TLS_REVOCATION", "soft")
	if c, err := Load(); err != nil || c.TLSRevocation != "soft" {
		t.Fatalf("explicit soft in profile: %v %v", c, err)
	}
}

func TestListenerTLSSettings(t *testing.T) {
	setBaseline(t)
	unsetEnv(t, "JANUS_TLS_CERT_PATH", "JANUS_TLS_KEY_PATH", "JANUS_TLS_SELF_SIGNED", "JANUS_TLS_REQUIRE", "JANUS_TLS_SELF_SIGNED_NAMES", "JANUS_TLS_SELF_SIGNED_DAYS", "JANUS_STIG_PROFILE")
	c, err := Load()
	if err != nil || c.TLSSelfSigned || c.TLSRequire || c.TLSSelfSignedDays != 365 {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	// Strict mode needs Janus to serve TLS itself.
	t.Setenv("JANUS_TLS_REQUIRE", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JANUS_TLS_REQUIRE") {
		t.Fatalf("require without TLS: %v", err)
	}
	t.Setenv("JANUS_TLS_SELF_SIGNED", "true")
	t.Setenv("JANUS_TLS_SELF_SIGNED_NAMES", "janus.ns.svc, janus.example.mil 10.0.0.5")
	t.Setenv("JANUS_TLS_SELF_SIGNED_DAYS", "90")
	if c, err := Load(); err != nil || !c.TLSRequire || !c.TLSSelfSigned || len(c.TLSSelfSignedNames) != 3 || c.TLSSelfSignedDays != 90 {
		t.Fatalf("self-signed strict: %v %+v", err, c)
	}
	t.Setenv("JANUS_TLS_SELF_SIGNED_DAYS", "2000")
	if _, err := Load(); err == nil {
		t.Fatal("over-long self-signed lifetime accepted")
	}
	t.Setenv("JANUS_TLS_SELF_SIGNED_DAYS", "")
	t.Setenv("JANUS_TLS_CERT_PATH", "/tls/tls.crt")
	t.Setenv("JANUS_TLS_KEY_PATH", "/tls/tls.key")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("mounted + self-signed accepted: %v", err)
	}
	t.Setenv("JANUS_TLS_SELF_SIGNED", "")
	if c, err := Load(); err != nil || !c.TLSRequire || c.TLSCertPath == "" {
		t.Fatalf("mounted strict: %v", err)
	}
}
