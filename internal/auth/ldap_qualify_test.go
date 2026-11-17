package auth

import (
	"errors"
	"testing"
)

func TestQualifyLogin(t *testing.T) {
	cases := []struct {
		login, domain, windows, want string
		err                          error
	}{
		// Bare name gets the default domain.
		{"ada", "corp.example.com", "", "ada@corp.example.com", nil},
		{"  ada ", "corp.example.com", "", "ada@corp.example.com", nil},
		{"ada", "@corp.example.com", "", "ada@corp.example.com", nil},
		// Full names are left alone, whatever their domain.
		{"ada@corp.example.com", "corp.example.com", "", "ada@corp.example.com", nil},
		{"ada@other.example.com", "corp.example.com", "", "ada@other.example.com", nil},
		// DOMAIN\name: derived NetBIOS name, any case.
		{`CORP\ada`, "corp.example.com", "", "ada@corp.example.com", nil},
		{`corp\ada`, "corp.example.com", "", "ada@corp.example.com", nil},
		{` Corp\ada `, "corp.example.com", "", "ada@corp.example.com", nil},
		// DOMAIN\name: a configured NetBIOS name replaces the derived one.
		{`ACME\ada`, "corp.example.com", "ACME", "ada@corp.example.com", nil},
		{`acme\ada`, "corp.example.com", "Acme", "ada@corp.example.com", nil},
		{`CORP\ada`, "corp.example.com", "ACME", `CORP\ada`, ErrLoginDomain},
		// Wrong domain, or no default domain: refused.
		{`OTHER\ada`, "corp.example.com", "", `OTHER\ada`, ErrLoginDomain},
		{`CORP\ada`, "", "", `CORP\ada`, ErrLoginDomain},
		{`CORP\ada`, "", "CORP", `CORP\ada`, ErrLoginDomain},
		// Malformed backslash names.
		{`CORP\`, "corp.example.com", "", `CORP\`, ErrLoginDomain},
		{`\ada`, "corp.example.com", "", `\ada`, ErrLoginDomain},
		{`CORP\ada@corp.example.com`, "corp.example.com", "", `CORP\ada@corp.example.com`, ErrLoginDomain},
		{`CORP\x\ada`, "corp.example.com", "", `CORP\x\ada`, ErrLoginDomain},
		// No default domain: as typed (trimmed).
		{"ada", "", "", "ada", nil},
		{" ada ", "  ", "", "ada", nil},
		{"", "corp.example.com", "", "", nil},
	}
	for _, c := range cases {
		got, err := QualifyLogin(c.login, c.domain, c.windows)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("QualifyLogin(%q, %q, %q) = %q, %v; want %q, %v", c.login, c.domain, c.windows, got, err, c.want, c.err)
		}
	}
}

func TestWindowsDomain(t *testing.T) {
	cases := []struct{ domain, windows, want string }{
		{"corp.example.com", "", "CORP"},
		{"example.com", "", "EXAMPLE"},
		{"lab", "", "LAB"},
		{"averyveryverylongname.example.com", "", "AVERYVERYVERYLO"},
		{"corp.example.com", "acme", "ACME"},
		{"", "", ""},
		{"", "ACME", "ACME"},
	}
	for _, c := range cases {
		if got := WindowsDomain(c.domain, c.windows); got != c.want {
			t.Errorf("WindowsDomain(%q, %q) = %q, want %q", c.domain, c.windows, got, c.want)
		}
	}
}
