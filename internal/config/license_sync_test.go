package config

import (
	"os"
	"strings"
	"testing"
)

func TestLicenseSyncEnvironmentPrecedence(t *testing.T) {
	setBaseline(t)
	t.Setenv("JANUS_LICENSE_SYNC", "false")
	t.Setenv("JANUS_LICENSE_SYNC_TOKEN", "secret")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.LicenseSync == nil || *c.LicenseSync || c.LicenseSyncToken != "secret" {
		t.Fatal("explicit false must pin; token must not enable")
	}
	t.Setenv("JANUS_LICENSE_SYNC", "true")
	c, err = Load()
	if err != nil || c.LicenseSync == nil || !*c.LicenseSync {
		t.Fatal("explicit opt in", err)
	}
	_ = os.Unsetenv("JANUS_LICENSE_SYNC")
	c, err = Load()
	if err != nil || c.LicenseSync != nil {
		t.Fatal("unset must leave UI authoritative", err)
	}
	t.Setenv("JANUS_LICENSE_SYNC_TOKEN", "")
	c, err = Load()
	if err != nil || !c.LicenseSyncTokenSet {
		t.Fatal("explicit empty token must be env managed", err)
	}
	t.Setenv("JANUS_LICENSE_SYNC", "garbage")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "JANUS_LICENSE_SYNC") || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid bool must fail without secret")
	}
}
