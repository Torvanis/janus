package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// setBaseline supplies the variables every Load call requires.
func setBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("JANUS_DATABASE_URL", "sqlite:///tmp/janus-test.db")
	t.Setenv("JANUS_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	// Neutralise any ambient configuration from the invoking shell.
	for _, key := range []string{
		"JANUS_DEV_AUTH", "JANUS_DEV_AUTH_ALLOW_UNSAFE", "JANUS_ENV", "JANUS_PUBLIC_URL",
		"JANUS_OIDC_PROVIDER_URL", "JANUS_OIDC_CLIENT_ID", "JANUS_OIDC_CLIENT_SECRET",
	} {
		t.Setenv(key, "")
	}
}

func TestBuildMetadataDefaultsAndOverrides(t *testing.T) {
	setBaseline(t)
	oldVersion, oldSHA, oldDate := DefaultBuildVersion, DefaultBuildSHA, DefaultBuildDate
	t.Cleanup(func() {
		DefaultBuildVersion, DefaultBuildSHA, DefaultBuildDate = oldVersion, oldSHA, oldDate
	})
	DefaultBuildVersion, DefaultBuildSHA, DefaultBuildDate = "2026.9.1", "release-snapshot", "2026-09-20"
	for _, key := range []string{"JANUS_BUILD_VERSION", "JANUS_BUILD_SHA", "JANUS_BUILD_DATE"} {
		t.Setenv(key, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BuildVersion != DefaultBuildVersion || c.BuildSHA != DefaultBuildSHA || c.BuildDate != DefaultBuildDate {
		t.Fatalf("build defaults not applied: %q %q %q", c.BuildVersion, c.BuildSHA, c.BuildDate)
	}
	t.Setenv("JANUS_BUILD_VERSION", "override-version")
	t.Setenv("JANUS_BUILD_SHA", "override-sha")
	t.Setenv("JANUS_BUILD_DATE", "2026-10-01")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BuildVersion != "override-version" || c.BuildSHA != "override-sha" || c.BuildDate != "2026-10-01" {
		t.Fatalf("environment overrides not applied: %q %q %q", c.BuildVersion, c.BuildSHA, c.BuildDate)
	}
}

// TestConfigDatabaseURLDefaulting locks the embedded-SQLite default
// (replaces the old "JANUS_DATABASE_URL is required" hard-fail contract):
// unset means sqlite:///data/janus.db with the defaulted flag set so main can
// warn; any explicit URL — postgres or sqlite — is taken verbatim with no
// flag and therefore no warning.
func TestConfigDatabaseURLDefaulting(t *testing.T) {
	t.Run("unset defaults to embedded sqlite and flags it", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_DATABASE_URL", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load with unset JANUS_DATABASE_URL must boot on the sqlite default, got: %v", err)
		}
		if c.DatabaseURL != DefaultDatabaseURL {
			t.Fatalf("DatabaseURL = %q, want default %q", c.DatabaseURL, DefaultDatabaseURL)
		}
		if !c.DatabaseURLDefaulted {
			t.Fatal("DatabaseURLDefaulted must be true when the default applies (drives the startup warning)")
		}
	})

	t.Run("whitespace-only counts as unset", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_DATABASE_URL", "   ")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DatabaseURL != DefaultDatabaseURL || !c.DatabaseURLDefaulted {
			t.Fatalf("whitespace value must default: url=%q defaulted=%v", c.DatabaseURL, c.DatabaseURLDefaulted)
		}
	})

	t.Run("explicit postgres URL is taken verbatim with no warning flag", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		const dsn = "postgresql://janus:s3cret@db.example.test:5432/janus?sslmode=require"
		t.Setenv("JANUS_DATABASE_URL", dsn)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DatabaseURL != dsn {
			t.Fatalf("DatabaseURL = %q, want the explicit value %q", c.DatabaseURL, dsn)
		}
		if c.DatabaseURLDefaulted {
			t.Fatal("DatabaseURLDefaulted must be false for an explicit postgres URL")
		}
	})

	t.Run("explicit sqlite URL is taken verbatim with no warning flag", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		const dsn = "sqlite:///var/lib/janus/custom.db"
		t.Setenv("JANUS_DATABASE_URL", dsn)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DatabaseURL != dsn {
			t.Fatalf("DatabaseURL = %q, want the explicit value %q", c.DatabaseURL, dsn)
		}
		if c.DatabaseURLDefaulted {
			t.Fatal("DatabaseURLDefaulted must be false for an explicit sqlite URL (even a non-default one)")
		}
	})
}

// TestConfigCacheTTL locks the documented knob: default 5s, configurable,
// and validated (zero/negative/garbage refused with the variable named).
func TestConfigCacheTTL(t *testing.T) {
	t.Run("defaults to 5 seconds", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ConfigCacheTTL != 5*time.Second {
			t.Fatalf("ConfigCacheTTL default = %v, want 5s (documented cross-replica visibility bound)", c.ConfigCacheTTL)
		}
	})

	t.Run("configurable", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_CONFIG_CACHE_TTL_SECONDS", "2")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ConfigCacheTTL != 2*time.Second {
			t.Fatalf("ConfigCacheTTL = %v, want 2s", c.ConfigCacheTTL)
		}
	})

	t.Run("rejects zero and garbage", func(t *testing.T) {
		for _, bad := range []string{"0", "-1", "five"} {
			setBaseline(t)
			t.Setenv("JANUS_CONFIG_CACHE_TTL_SECONDS", bad)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JANUS_CONFIG_CACHE_TTL_SECONDS") {
				t.Fatalf("JANUS_CONFIG_CACHE_TTL_SECONDS=%q must fail naming the variable, got: %v", bad, err)
			}
		}
	})
}

// TestDevAuthGuard locks the production guardrail on the JANUS_DEV_AUTH
// authentication bypass (regression: Load accepted DevAuthEnabled in any
// environment; the only defense was a log warning).
func TestDevAuthGuard(t *testing.T) {
	t.Run("loopback evaluation deployment boots", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		if _, err := Load(); err != nil {
			t.Fatalf("loopback dev-auth quick start must boot, got: %v", err)
		}
	})

	t.Run("localhost hostname boots", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://localhost:8080")
		if _, err := Load(); err != nil {
			t.Fatalf("localhost dev-auth must boot, got: %v", err)
		}
	})

	t.Run("explicit production environment is refused", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_ENV", "production")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JANUS_DEV_AUTH") {
			t.Fatalf("dev auth with JANUS_ENV=production must refuse to start, got: %v", err)
		}
	})

	t.Run("non-loopback public URL is refused", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "https://janus.corp.example.com")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "not loopback") {
			t.Fatalf("dev auth on a reachable URL must refuse to start, got: %v", err)
		}
	})

	t.Run("explicit unsafe override boots", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "https://demo.example.com")
		t.Setenv("JANUS_ENV", "production")
		t.Setenv("JANUS_DEV_AUTH_ALLOW_UNSAFE", "true")
		if _, err := Load(); err != nil {
			t.Fatalf("the explicit override must boot, got: %v", err)
		}
	})

	t.Run("production with OIDC and no dev auth boots", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_ENV", "production")
		t.Setenv("JANUS_PUBLIC_URL", "https://janus.corp.example.com")
		t.Setenv("JANUS_OIDC_PROVIDER_URL", "https://idp.example.com")
		t.Setenv("JANUS_OIDC_CLIENT_ID", "janus")
		t.Setenv("JANUS_OIDC_CLIENT_SECRET", "secret")
		if _, err := Load(); err != nil {
			t.Fatalf("a normal production configuration must boot, got: %v", err)
		}
	})
}

// TestCABundleLoading locks the JANUS_CA_BUNDLE contract (regression: the
// variable was loaded into CABundlePath and documented, but nothing parsed it
// or wired it into any TLS client, so private-CA upstreams were unreachable
// and a mistyped path was silently accepted).
func TestCABundleLoading(t *testing.T) {
	t.Run("unset means default verification", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_CA_BUNDLE", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CABundle != nil {
			t.Error("CABundle must be nil when JANUS_CA_BUNDLE is unset")
		}
		if cfg.TLSClientConfig() != nil {
			t.Error("TLSClientConfig must be nil (default verification) without a bundle")
		}
	})

	t.Run("unreadable path fails fast naming the variable", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_CA_BUNDLE", filepath.Join(t.TempDir(), "does-not-exist.pem"))
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JANUS_CA_BUNDLE") {
			t.Fatalf("an unreadable bundle must fail startup with a JANUS_CA_BUNDLE error, got: %v", err)
		}
	})

	t.Run("file without PEM certificates fails fast", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		path := filepath.Join(t.TempDir(), "garbage.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		t.Setenv("JANUS_CA_BUNDLE", path)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JANUS_CA_BUNDLE") {
			t.Fatalf("a bundle with no valid PEM must fail startup with a JANUS_CA_BUNDLE error, got: %v", err)
		}
	})

	t.Run("valid bundle produces a trust store and client TLS config", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, selfSignedCAPEM(t), 0o600); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
		t.Setenv("JANUS_CA_BUNDLE", path)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load with a valid bundle must succeed, got: %v", err)
		}
		if cfg.CABundle == nil {
			t.Fatal("CABundle was not populated from JANUS_CA_BUNDLE")
		}
		tlsCfg := cfg.TLSClientConfig()
		if tlsCfg == nil || tlsCfg.RootCAs == nil {
			t.Fatal("TLSClientConfig must carry the bundle's roots")
		}
	})
}

// selfSignedCAPEM generates a throwaway CA certificate in PEM form.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "janus-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestAdminGroupsParsing locks the JANUS_ADMIN_GROUPS contract: an optional
// comma/space-separated list, normalized to lowercase, deduplicated, and
// sorted for determinism. Unset or empty means the group-admin feature is
// inert — no new required-variable failure mode.
func TestAdminGroupsParsing(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  []string
	}{
		{"comma-separated list", true, "llm-admins,platform-ops", []string{"llm-admins", "platform-ops"}},
		{"space-separated list", true, "llm-admins platform-ops", []string{"llm-admins", "platform-ops"}},
		{"mixed separators with acceptance example", true, "LLM-Admins, platform-ops llm-admins", []string{"llm-admins", "platform-ops"}},
		{"mixed case is lowercased", true, "LLM-Admins Platform-OPS", []string{"llm-admins", "platform-ops"}},
		{"duplicates are deduplicated", true, "llm-admins llm-admins", []string{"llm-admins"}},
		{"case-insensitive duplicates collapse", true, "Admin,admin ADMIN", []string{"admin"}},
		{"surrounding whitespace is trimmed", true, " admin , admin ", []string{"admin"}},
		{"tabs and newlines separate too", true, "one	two\nthree", []string{"one", "three", "two"}},
		{"output is sorted for determinism", true, "zeta alpha midway", []string{"alpha", "midway", "zeta"}},
		{"empty value yields empty slice", true, "", []string{}},
		{"whitespace-only value yields empty slice", true, "   ", []string{}},
		{"unset yields empty slice", false, "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseline(t)
			t.Setenv("JANUS_DEV_AUTH", "true")
			t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
			if tc.set {
				t.Setenv("JANUS_ADMIN_GROUPS", tc.value)
			} else {
				// t.Setenv("", …) registers restoration of any ambient value;
				// then remove it so Load sees the variable truly absent.
				t.Setenv("JANUS_ADMIN_GROUPS", "")
				if err := os.Unsetenv("JANUS_ADMIN_GROUPS"); err != nil {
					t.Fatalf("unsetenv: %v", err)
				}
			}
			c, err := Load()
			if err != nil {
				t.Fatalf("Load must never fail on JANUS_ADMIN_GROUPS (optional variable), got: %v", err)
			}
			if c.AdminGroups == nil {
				t.Fatal("AdminGroups must be an empty slice, never nil")
			}
			if !slices.Equal(c.AdminGroups, tc.want) {
				t.Fatalf("AdminGroups = %q, want %q", c.AdminGroups, tc.want)
			}
		})
	}
}

// TestRedactedNeverLeaksAdminIdentifiers locks the redaction contract: the
// loggable config view exposes only counts for admin groups and bootstrap
// admin emails, never the values themselves.
func TestRedactedNeverLeaksAdminIdentifiers(t *testing.T) {
	setBaseline(t)
	t.Setenv("JANUS_DEV_AUTH", "true")
	t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("JANUS_ADMIN_GROUPS", "LLM-Admins, platform-ops llm-admins")
	t.Setenv("JANUS_BOOTSTRAP_ADMIN_EMAILS", "root@example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	red := c.Redacted()
	if got, want := red["admin_groups_count"], 2; got != want {
		t.Errorf("admin_groups_count = %v, want %v", got, want)
	}
	if got, want := red["bootstrap_admin_emails_count"], 1; got != want {
		t.Errorf("bootstrap_admin_emails_count = %v, want %v", got, want)
	}
	rendered := fmt.Sprintf("%v", red)
	for _, leaked := range []string{"llm-admins", "platform-ops", "root@example.com"} {
		if strings.Contains(strings.ToLower(rendered), leaked) {
			t.Errorf("Redacted output leaks %q: %s", leaked, rendered)
		}
	}

	t.Run("zero counts when unset", func(t *testing.T) {
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_ADMIN_GROUPS", "")
		t.Setenv("JANUS_BOOTSTRAP_ADMIN_EMAILS", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		red := c.Redacted()
		if got := red["admin_groups_count"]; got != 0 {
			t.Errorf("admin_groups_count = %v, want 0", got)
		}
		if got := red["bootstrap_admin_emails_count"]; got != 0 {
			t.Errorf("bootstrap_admin_emails_count = %v, want 0", got)
		}
	})
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"https://janus.example.com", false},
		{"http://10.0.0.12:8080", false},
		{"http://192.168.1.20", false},
		{"not a url", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackURL(tc.raw); got != tc.want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestLocalOnlyMode locks the JANUS_LOCAL_ONLY contract (local-only mode for
// self-hosted deployments): default false, standard envBool parsing, and the
// mode is visible — as a boolean, never a secret — in Redacted() output so the
// system-status surface can report it.
func TestLocalOnlyMode(t *testing.T) {
	load := func(t *testing.T, value string) *Config {
		t.Helper()
		setBaseline(t)
		t.Setenv("JANUS_DEV_AUTH", "true")
		t.Setenv("JANUS_PUBLIC_URL", "http://127.0.0.1:8080")
		t.Setenv("JANUS_LOCAL_ONLY", value)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load with JANUS_LOCAL_ONLY=%q: %v", value, err)
		}
		return c
	}

	t.Run("defaults to false when unset", func(t *testing.T) {
		if c := load(t, ""); c.LocalOnly {
			t.Fatal("LocalOnly must default to false")
		}
	})

	t.Run("true enables the mode", func(t *testing.T) {
		if c := load(t, "true"); !c.LocalOnly {
			t.Fatal("JANUS_LOCAL_ONLY=true must set LocalOnly")
		}
	})

	t.Run("false keeps the mode off", func(t *testing.T) {
		if c := load(t, "false"); c.LocalOnly {
			t.Fatal("JANUS_LOCAL_ONLY=false must keep LocalOnly off")
		}
	})

	t.Run("invalid values fall back to the default like other envBool vars", func(t *testing.T) {
		if c := load(t, "banana"); c.LocalOnly {
			t.Fatal("an unparseable JANUS_LOCAL_ONLY must fall back to the false default")
		}
	})

	t.Run("redacted output reports the mode", func(t *testing.T) {
		c := load(t, "true")
		got, ok := c.Redacted()["local_only"]
		if !ok {
			t.Fatal("Redacted() must include local_only")
		}
		if got != true {
			t.Fatalf("Redacted()[local_only] = %v, want true", got)
		}
	})
}
