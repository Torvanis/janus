package detect

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

type secretsFixtures struct {
	Rules map[string]struct {
		Positives []string `json:"positives"`
		Negatives []string `json:"negatives"`
		Expect    []string `json:"expect"`
	} `json:"rules"`
	ProseNegatives []string `json:"prose_negatives"`
}

func loadSecretsFixtures(t *testing.T) secretsFixtures {
	t.Helper()
	raw, err := os.ReadFile("testdata/secrets/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx secretsFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fx
}

func newSecrets(t *testing.T, opts SecretsOptions) *SecretsDetector {
	t.Helper()
	d, err := NewSecretsDetector(opts)
	if err != nil {
		t.Fatalf("NewSecretsDetector: %v", err)
	}
	return d
}

func matchesFor(ms []Match, id string) []Match {
	var out []Match
	for _, m := range ms {
		if m.RuleID == id {
			out = append(out, m)
		}
	}
	return out
}

// TestSecretsRulesFixtureCoverage is the merge gate: every enabled rule has
// at least two positive and two negative fixtures, and every rule with a
// fixture actually exists.
func TestSecretsRulesFixtureCoverage(t *testing.T) {
	fx := loadSecretsFixtures(t)
	d := newSecrets(t, SecretsOptions{})
	known := map[string]bool{}
	for _, r := range d.Rules() {
		known[r.ID] = true
		if !r.Enabled {
			continue
		}
		f, ok := fx.Rules[r.ID]
		if !ok {
			t.Errorf("rule %q has no fixtures", r.ID)
			continue
		}
		if len(f.Positives) < 2 || len(f.Negatives) < 2 {
			t.Errorf("rule %q needs >=2 positives and >=2 negatives, got %d/%d", r.ID, len(f.Positives), len(f.Negatives))
		}
	}
	for id := range fx.Rules {
		if !known[id] {
			t.Errorf("fixture for unknown rule %q", id)
		}
	}
	if n := len(d.Rules()); n < 25 {
		t.Errorf("expected ~25+ rules, got %d", n)
	}
}

func TestSecretsPositiveFixtures(t *testing.T) {
	fx := loadSecretsFixtures(t)
	d := newSecrets(t, SecretsOptions{})
	for id, f := range fx.Rules {
		for i, text := range f.Positives {
			ms := matchesFor(d.Detect(text), id)
			if len(ms) == 0 {
				t.Errorf("%s positive[%d] did not fire: %q", id, i, text)
				continue
			}
			m := ms[0]
			if m.Kind != KindSecrets {
				t.Errorf("%s: kind = %q", id, m.Kind)
			}
			if m.Replacement != "[REDACTED:"+id+"]" {
				t.Errorf("%s: replacement = %q", id, m.Replacement)
			}
			if m.Offset < 0 || m.Offset+m.Length > len(text) || text[m.Offset:m.Offset+m.Length] != m.Text {
				t.Errorf("%s: span %d+%d does not equal Text %q", id, m.Offset, m.Length, m.Text)
			}
			if i < len(f.Expect) && m.Text != f.Expect[i] {
				t.Errorf("%s positive[%d]: Text = %q, want %q", id, i, m.Text, f.Expect[i])
			}
			if m.Length > d.MaxSpan() {
				t.Errorf("%s: match length %d exceeds MaxSpan %d", id, m.Length, d.MaxSpan())
			}
		}
	}
}

func TestSecretsNegativeFixtures(t *testing.T) {
	fx := loadSecretsFixtures(t)
	d := newSecrets(t, SecretsOptions{})
	for id, f := range fx.Rules {
		for i, text := range f.Negatives {
			if ms := matchesFor(d.Detect(text), id); len(ms) != 0 {
				t.Errorf("%s negative[%d] fired on %q: %+v", id, i, text, ms)
			}
		}
	}
}

func TestSecretsProseNegatives(t *testing.T) {
	fx := loadSecretsFixtures(t)
	d := newSecrets(t, SecretsOptions{})
	if len(fx.ProseNegatives) < 6 {
		t.Fatalf("expected a prose FP set, got %d entries", len(fx.ProseNegatives))
	}
	for _, text := range fx.ProseNegatives {
		if ms := d.Detect(text); len(ms) != 0 {
			t.Errorf("prose negative fired: %q -> %+v", text, ms)
		}
	}
	// The whole set concatenated as one document must also be clean.
	if ms := d.Detect(strings.Join(fx.ProseNegatives, "\n")); len(ms) != 0 {
		t.Errorf("joined prose fired: %+v", ms)
	}
}

// TestSecretsSecretGroupSpan checks that Offset/Length cover only the secret
// capture group, so redaction keeps the surrounding context.
func TestSecretsSecretGroupSpan(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	cases := []struct {
		id, text, want string
	}{
		{"connection-string-uri", "DATABASE_URL=postgres://app:Qz8vL2mXp4Rt7Ky1@db.example.test:5432/app", "Qz8vL2mXp4Rt7Ky1"},
		{"generic-password-assignment", `password = "Xk9#mP2vQ7zL"`, "Xk9#mP2vQ7zL"},
		{"azure-storage-key", "AccountKey=" + strings.Repeat("Ab1", 28) + "Ab==;EndpointSuffix=x", strings.Repeat("Ab1", 28) + "Ab=="},
	}
	for _, c := range cases {
		ms := matchesFor(d.Detect(c.text), c.id)
		if len(ms) != 1 {
			t.Errorf("%s: got %d matches: %+v", c.id, len(ms), ms)
			continue
		}
		if ms[0].Text != c.want {
			t.Errorf("%s: Text = %q, want %q", c.id, ms[0].Text, c.want)
		}
		red := Redact(c.text, ms)
		if strings.Contains(red, c.want) || !strings.Contains(red, "[REDACTED:"+c.id+"]") {
			t.Errorf("%s: bad redaction %q", c.id, red)
		}
	}
	// URI redaction keeps host and scheme.
	red := Redact(cases[0].text, d.Detect(cases[0].text))
	if red != "DATABASE_URL=postgres://app:[REDACTED:connection-string-uri]@db.example.test:5432/app" {
		t.Errorf("uri redaction = %q", red)
	}
}

func TestSecretsRedactAWSAndJWT(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	text := "Use key AKIAIOSFODNN7EXAMPLE and header Authorization: Bearer " + jwt + " for the call."
	ms := d.Detect(text)
	if len(ms) != 2 {
		t.Fatalf("expected 2 matches, got %d: %+v", len(ms), ms)
	}
	if ms[0].RuleID != "aws-access-key-id" || ms[1].RuleID != "jwt" {
		t.Fatalf("unexpected rule order: %s, %s", ms[0].RuleID, ms[1].RuleID)
	}
	if ms[0].Severity != SeverityCritical || ms[1].Severity != SeverityHigh {
		t.Errorf("severities: %s, %s", ms[0].Severity, ms[1].Severity)
	}
	got := Redact(text, ms)
	want := "Use key [REDACTED:aws-access-key-id] and header Authorization: Bearer [REDACTED:jwt] for the call."
	if got != want {
		t.Errorf("Redact =\n %q\nwant\n %q", got, want)
	}
}

func TestSecretsPrivateKeyBlock(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	body := strings.Repeat("MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AXbXW2hR3Uf5ZG7l6X4Y\n", 20)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "-----END RSA PRIVATE KEY-----"
	text := "here is my key:\n" + pem + "\nthanks"
	ms := matchesFor(d.Detect(text), "private-key")
	if len(ms) != 1 {
		t.Fatalf("expected 1 private-key match, got %d", len(ms))
	}
	if ms[0].Text != pem {
		t.Errorf("span did not cover the whole block; got %d bytes, want %d", len(ms[0].Text), len(pem))
	}
	if ms[0].Severity != SeverityCritical {
		t.Errorf("severity = %s", ms[0].Severity)
	}
	if got := Redact(text, ms); got != "here is my key:\n[REDACTED:private-key]\nthanks" {
		t.Errorf("redaction = %q", got)
	}

	// A block with no END marker is bounded at maxSpan, not the end of text.
	huge := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 10000)
	ms = matchesFor(d.Detect(huge), "private-key")
	if len(ms) != 1 {
		t.Fatalf("unterminated block: got %d matches", len(ms))
	}
	if ms[0].Length > d.MaxSpan() || ms[0].Length < 4096 {
		t.Errorf("unterminated block length %d, MaxSpan %d", ms[0].Length, d.MaxSpan())
	}
}

func TestSecretsMaxSpan(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	if d.MaxSpan() < 4096 {
		t.Errorf("MaxSpan = %d, want >= 4096 (private-key bound)", d.MaxSpan())
	}
	var _ SpanBound = d
}

func TestSecretsDisabledRules(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	var generic *RuleInfo
	for _, r := range d.Rules() {
		if r.ID == "generic-api-key" {
			rr := r
			generic = &rr
		}
	}
	if generic == nil {
		t.Fatal("generic-api-key rule missing")
	}
	if generic.Enabled {
		t.Error("generic-api-key should be disabled by default")
	}
	text := "api_key = Qz8vL2mXp4Rt7Ky1Wn3Bd6Hs"
	if ms := matchesFor(d.Detect(text), "generic-api-key"); len(ms) != 0 {
		t.Errorf("disabled rule fired: %+v", ms)
	}

	// DisableRules turns off a rule that is enabled by default.
	aws := "AKIAIOSFODNN7EXAMPLE"
	if len(matchesFor(d.Detect(aws), "aws-access-key-id")) != 1 {
		t.Fatal("sanity: aws rule should fire when enabled")
	}
	d2 := newSecrets(t, SecretsOptions{DisableRules: []string{"aws-access-key-id"}})
	if ms := matchesFor(d2.Detect(aws), "aws-access-key-id"); len(ms) != 0 {
		t.Errorf("DisableRules did not disable aws rule: %+v", ms)
	}
	for _, r := range d2.Rules() {
		if r.ID == "aws-access-key-id" && r.Enabled {
			t.Error("Rules() still reports aws-access-key-id enabled")
		}
	}
}

func TestSecretsExtraRulesTOML(t *testing.T) {
	extra := `
[[rules]]
id = "acme-token"
description = "ACME internal token"
regex = 'acme_[a-z0-9]{12}'
keywords = ["acme_"]
severity = "high"
`
	d := newSecrets(t, SecretsOptions{ExtraRulesTOML: extra})
	found := false
	for _, r := range d.Rules() {
		if r.ID == "acme-token" {
			found = true
			if !r.Enabled || r.Severity != SeverityHigh || r.Description != "ACME internal token" {
				t.Errorf("acme-token info = %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("extra rule not listed")
	}
	ms := matchesFor(d.Detect("token acme_abc123def456 ok"), "acme-token")
	if len(ms) != 1 || ms[0].Text != "acme_abc123def456" {
		t.Errorf("extra rule matches = %+v", ms)
	}

	// Extra rule with the same id replaces the built-in.
	override := `
[[rules]]
id = "aws-access-key-id"
regex = 'NEVERMATCH[0-9]{40}'
`
	d2 := newSecrets(t, SecretsOptions{ExtraRulesTOML: override})
	if ms := matchesFor(d2.Detect("AKIAIOSFODNN7EXAMPLE"), "aws-access-key-id"); len(ms) != 0 {
		t.Errorf("override did not replace built-in: %+v", ms)
	}

	// A bad regex is rejected with the rule id in the error.
	bad := `
[[rules]]
id = "broken-rule"
regex = '(?P<x>unclosed'
`
	if _, err := NewSecretsDetector(SecretsOptions{ExtraRulesTOML: bad}); err == nil {
		t.Error("expected error for bad regex")
	} else if !strings.Contains(err.Error(), "broken-rule") {
		t.Errorf("error should name the rule id: %v", err)
	}

	// Lookaround is not RE2 and must be rejected too.
	lookahead := "\n[[rules]]\nid = \"lookahead\"\nregex = 'foo(?=bar)'\n"
	if _, err := NewSecretsDetector(SecretsOptions{ExtraRulesTOML: lookahead}); err == nil {
		t.Error("expected error for lookahead regex")
	}

	// secretGroup beyond the regex's group count is rejected.
	badGroup := "\n[[rules]]\nid = \"badgroup\"\nregex = 'foo(bar)'\nsecretGroup = 2\n"
	if _, err := NewSecretsDetector(SecretsOptions{ExtraRulesTOML: badGroup}); err == nil || !strings.Contains(err.Error(), "badgroup") {
		t.Errorf("expected secretGroup error naming rule, got %v", err)
	}
}

func TestSecretsKeywordPrefilter(t *testing.T) {
	// A rule whose keyword is absent must not fire even if the regex would.
	extra := "\n[[rules]]\nid = \"kw\"\nregex = '[0-9]{6}'\nkeywords = [\"zipcode\"]\n"
	d := newSecrets(t, SecretsOptions{ExtraRulesTOML: extra})
	if ms := matchesFor(d.Detect("call 123456 now"), "kw"); len(ms) != 0 {
		t.Errorf("fired without keyword: %+v", ms)
	}
	if ms := matchesFor(d.Detect("ZIPCODE 123456"), "kw"); len(ms) != 1 {
		t.Errorf("did not fire with (case-insensitive) keyword: %+v", ms)
	}
	// No keywords: always runs.
	nokw := "\n[[rules]]\nid = \"nokw\"\nregex = 'zz[0-9]{4}'\n"
	d2 := newSecrets(t, SecretsOptions{ExtraRulesTOML: nokw})
	if ms := matchesFor(d2.Detect("zz1234"), "nokw"); len(ms) != 1 {
		t.Errorf("keyword-less rule did not fire: %+v", ms)
	}
}

func TestSecretsAllowlist(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	// openai-api-key allowlists sk-ant- so anthropic keys are reported once.
	key := "sk-ant-api03-" + strings.Repeat("Ab3", 20)
	ms := d.Detect(key)
	if len(ms) != 1 || ms[0].RuleID != "anthropic-api-key" {
		t.Errorf("anthropic key matches = %+v", ms)
	}
	// Stopwords are case-insensitive.
	for _, text := range []string{"password = ChangeMe12345", "password = MyPassword2024!", "password = PLACEHOLDER_VALUE"} {
		if ms := matchesFor(d.Detect(text), "generic-password-assignment"); len(ms) != 0 {
			t.Errorf("stopword not applied for %q: %+v", text, ms)
		}
	}
	// Entropy floor: repetitive strings are dropped, random ones kept.
	if ms := matchesFor(d.Detect("password = abababababab"), "generic-password-assignment"); len(ms) != 0 {
		t.Errorf("low-entropy secret should be dropped: %+v", ms)
	}
	if ms := matchesFor(d.Detect("password = k7Qp2#Lx9vRz"), "generic-password-assignment"); len(ms) != 1 {
		t.Errorf("high-entropy secret should be kept: %+v", ms)
	}
}

func TestSecretsMultipleAndOrdering(t *testing.T) {
	d := newSecrets(t, SecretsOptions{})
	text := "b: glpat-" + strings.Repeat("x9", 12) + " a: AKIAIOSFODNN7EXAMPLE c: ghp_" + strings.Repeat("Q1", 18)
	ms := d.Detect(text)
	if len(ms) != 3 {
		t.Fatalf("got %d matches: %+v", len(ms), ms)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Offset < ms[i-1].Offset {
			t.Errorf("matches not ordered by offset: %+v", ms)
		}
	}
	if ms[0].RuleID != "gitlab-pat" || ms[1].RuleID != "aws-access-key-id" || ms[2].RuleID != "github-pat" {
		t.Errorf("order = %s %s %s", ms[0].RuleID, ms[1].RuleID, ms[2].RuleID)
	}
	if d.Detect("") != nil {
		t.Error("empty text should yield nil")
	}
}

func TestEntropy(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"aaaa", 0},
		{"ab", 1},
		{"abcd", 2},
		{"abcdefgh", 3},
		{"0123456789abcdef", 4},
	}
	for _, c := range cases {
		if got := Entropy(c.in); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Entropy(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if Entropy("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") < 4 {
		t.Error("random-looking key should have entropy >= 4")
	}
	if Entropy("password") > 3 {
		t.Error("english word should have entropy <= 3")
	}
}

func TestSecretsTOMLParser(t *testing.T) {
	// Comments, blank lines, escapes and multi-line literal strings.
	src := `
# leading comment
[[rules]]
id = "t1" # trailing comment
description = "has \"quotes\" and a hash # inside"
regex = '''x['"]y'''
keywords = ["A", 'b']
secretGroup = 0
entropy = 1.5
enabled = true
[rules.allowlist]
regexes = ['^skip']
stopwords = ["nope"]
`
	rs, err := parseRulesTOML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d rules", len(rs))
	}
	r := rs[0]
	if r.ID != "t1" || r.Description != `has "quotes" and a hash # inside` || r.Regex != `x['"]y` {
		t.Errorf("rule = %+v", r)
	}
	if len(r.Keywords) != 2 || r.Keywords[1] != "b" || r.Entropy != 1.5 || !r.EnabledSet || !r.Enabled {
		t.Errorf("rule = %+v", r)
	}
	if len(r.AllowRegex) != 1 || r.AllowRegex[0] != "^skip" || len(r.Stopwords) != 1 {
		t.Errorf("allowlist = %+v", r)
	}
	for _, bad := range []string{
		"id = \"x\"\n",                       // key outside table
		"[[rules]]\nid = \"x\"\nbogus = 1\n", // unknown key
		"[[rules]]\nid = \"x\"\nregex = 'unterminated\n",
		"[other]\n",
		"[[rules]]\nid = \"x\"\nenabled = \"yes\"\n", // wrong type
	} {
		if _, err := parseRulesTOML(bad); err == nil {
			t.Errorf("expected parse error for %q", bad)
		}
	}
}
