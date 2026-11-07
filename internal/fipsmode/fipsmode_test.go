package fipsmode

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckStartup(t *testing.T) {
	ok := Status{Edition: EditionFIPS, FIPS140Enabled: true, ModuleVersion: "v1.0.0"}
	if err := check(ok); err != nil {
		t.Fatalf("validated module in FIPS mode refused: %v", err)
	}
	if err := check(Status{Edition: EditionFIPS, FIPS140Enabled: true, ModuleVersion: "latest"}); err == nil || !strings.Contains(err.Error(), "GOFIPS140=v1.0.0") {
		t.Fatalf("unvalidated module accepted: %v", err)
	}
	if err := check(Status{Edition: EditionFIPS, FIPS140Enabled: false, ModuleVersion: "v1.0.0"}); err == nil || !strings.Contains(err.Error(), "fips140=off") {
		t.Fatalf("fips140=off accepted: %v", err)
	}
	for _, st := range []Status{{Edition: EditionDefault}, {Edition: EditionDefault, FIPS140Enabled: true, ModuleVersion: "latest"}} {
		if err := check(st); err != nil {
			t.Fatalf("default edition refused: %v", err)
		}
	}
}

// TestRunningBinary checks the binary under test against its own edition.
// CI runs the suite twice: plain (default edition) and with
// GOFIPS140=v1.0.0 -tags fips GODEBUG=fips140=only (FIPS edition).
func TestRunningBinary(t *testing.T) {
	st := Current()
	if err := CheckStartup(); err != nil {
		t.Fatalf("this test binary would refuse to start: %v", err)
	}
	if IsFIPSEdition() {
		if !st.ValidatedModule || st.CMVPCertificate != "#5247" || !st.FIPS140Enabled {
			t.Fatalf("FIPS edition status %+v", st)
		}
		if len(st.Restricted) != len(restrictions) {
			t.Fatalf("restricted = %d", len(st.Restricted))
		}
		err := Unavailable(FeaturePDFExport)
		if !errors.Is(err, ErrUnavailable) || Allowed(FeatureTOTPSHA1) {
			t.Fatalf("restriction not applied: %v", err)
		}
		return
	}
	if len(st.Restricted) != 0 || Unavailable(FeaturePDFExport) != nil || !Allowed(FeatureDevAuth) {
		t.Fatalf("default edition reports restrictions: %+v", st)
	}
}
