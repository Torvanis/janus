package store

import "testing"

func TestSecurityBaselines(t *testing.T) {
	def := DefaultSecurityPolicy()
	got := map[string]Baseline{}
	for _, b := range SecurityBaselines(def) {
		got[b.ID] = b
		if len(b.Gaps) == 0 {
			t.Errorf("%s: defaults should fall short of it", b.ID)
		}
	}
	for _, id := range []string{"nist-800-63b", "pci-dss-4", "fedramp-moderate", "dod-stig"} {
		b, ok := got[id]
		if !ok {
			t.Fatalf("missing baseline %s", id)
		}
		// Applying a baseline leaves nothing short of it, and is valid.
		if gaps := baselineGaps(id, b.Values); len(gaps) != 0 {
			t.Errorf("%s: applied values still fall short: %v", id, gaps)
		}
		v := b.Values
		if err := v.Validate(); err != nil {
			t.Errorf("%s: applied values invalid: %v", id, err)
		}
		if b.Summary == "" {
			t.Errorf("%s: no summary", id)
		}
	}
	if got["dod-stig"].Values.Sessions.IdleMinutesAdmin != 10 {
		t.Errorf("STIG baseline must match STIGSecurityPolicy")
	}
	pci := got["pci-dss-4"].Values
	if pci.Lockout.MaxFailures != 10 || pci.Lockout.LockMinutes != 30 || pci.Passwords.MinLength != 12 || pci.Passwords.MaxAgeDays != 90 {
		t.Errorf("PCI values: %+v %+v", pci.Lockout, pci.Passwords)
	}
	nist := got["nist-800-63b"].Values
	if nist.Passwords.RequireSpecial || nist.Passwords.MaxAgeDays != 0 || nist.Passwords.MinLength != 15 || nist.Sessions.IdleMinutesUser != 60 {
		t.Errorf("NIST values: %+v %+v", nist.Sessions, nist.Passwords)
	}
	fed := got["fedramp-moderate"].Values
	if !fed.Banner.Enabled || fed.Banner.Text != FederalBannerText || fed.Lockout.MaxFailures != 3 || fed.Lockout.LockMinutes != 30 {
		t.Errorf("FedRAMP values: %+v %+v", fed.Banner.Enabled, fed.Lockout)
	}
	// A stricter setting survives (lock until an administrator unlocks).
	strict := def
	strict.Lockout.LockMinutes = 0
	for _, b := range SecurityBaselines(strict) {
		if b.Values.Lockout.LockMinutes != 0 {
			t.Errorf("%s weakened an administrator-only unlock", b.ID)
		}
	}
}

func baselineGaps(id string, p SecurityPolicy) []string {
	for _, d := range baselineDefs {
		if d.id == id {
			return d.gaps(p)
		}
	}
	return []string{"unknown"}
}
