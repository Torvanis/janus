package httpapi

import (
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/fipsmode"
)

// TestDirectorySkipVerifyPerEdition: saving an LDAP directory with
// skip_verify is refused by the FIPS edition with a reason, and accepted by
// the default edition (validation only; no directory needed).
func TestDirectorySkipVerifyPerEdition(t *testing.T) {
	body := directoryBody{URL: "ldaps://ldap.example.com:636", BaseDN: "dc=example,dc=com", SkipVerify: true}
	_, apiErr := body.validate()
	if fipsmode.IsFIPSEdition() {
		if apiErr == nil || !strings.Contains(apiErr.Error(), "FIPS edition") {
			t.Fatalf("FIPS edition accepted skip_verify: %v", apiErr)
		}
		return
	}
	if apiErr != nil {
		t.Fatalf("default edition refused skip_verify: %v", apiErr)
	}
}
