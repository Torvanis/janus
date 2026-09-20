package detect

import (
	_ "embed"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Built-in secret rules. The file mirrors the gitleaks rule schema so that
// administrators familiar with gitleaks can author ExtraRulesTOML without
// learning a new format.
//
//go:embed rules/secrets.toml
var builtinSecretsTOML string

// defaultRuleSpan is the span bound assumed for a rule that does not declare
// maxSpan. Every built-in regex is bounded well below this; it exists so the
// streaming hold-back buffer has a number to size against.
const defaultRuleSpan = 1024

// SecretsOptions configures NewSecretsDetector.
type SecretsOptions struct {
	// DisableRules lists rule ids that must not fire even if the rule file
	// marks them enabled.
	DisableRules []string
	// ExtraRulesTOML is an administrator-supplied rule file in the same
	// schema as rules/secrets.toml. Rules with an id equal to a built-in
	// rule replace it; others are appended.
	ExtraRulesTOML string
}

// RuleInfo describes one rule for status/UI purposes.
type RuleInfo struct {
	ID          string
	Description string
	Severity    Severity
	Enabled     bool
}

// secretRule is a compiled rule.
type secretRule struct {
	id          string
	description string
	regex       *regexp.Regexp
	keywords    []string // lowercase
	secretGroup int
	entropy     float64
	severity    Severity
	enabled     bool
	allowRe     []*regexp.Regexp
	stopwords   []string // lowercase
	// endRegex, when set, extends the secret span from the primary match to
	// the end of the first endRegex hit within maxSpan bytes. Used for
	// multi-line blocks (private keys) whose length exceeds what RE2's
	// repeat-count limit (1000) can express.
	endRegex *regexp.Regexp
	maxSpan  int
}

// rawRule is the decoded TOML form before compilation.
type rawRule struct {
	ID          string
	Description string
	Regex       string
	Keywords    []string
	SecretGroup int
	Entropy     float64
	Severity    string
	Enabled     bool
	EnabledSet  bool
	AllowRegex  []string
	Stopwords   []string
	EndRegex    string
	MaxSpan     int
}

// SecretsDetector finds credential material in text.
type SecretsDetector struct {
	rules   []*secretRule
	maxSpan int
}

var _ SpanBound = (*SecretsDetector)(nil)

// NewSecretsDetector compiles the built-in rules plus any extras. Every regex
// is compiled once here; a rule whose regex fails to compile is reported by
// id so an administrator can fix it.
func NewSecretsDetector(opts SecretsOptions) (*SecretsDetector, error) {
	raws, err := parseRulesTOML(builtinSecretsTOML)
	if err != nil {
		return nil, fmt.Errorf("secrets: built-in rules: %w", err)
	}
	if strings.TrimSpace(opts.ExtraRulesTOML) != "" {
		extra, err := parseRulesTOML(opts.ExtraRulesTOML)
		if err != nil {
			return nil, fmt.Errorf("secrets: extra rules: %w", err)
		}
		for _, e := range extra {
			replaced := false
			for i := range raws {
				if raws[i].ID == e.ID {
					raws[i] = e
					replaced = true
					break
				}
			}
			if !replaced {
				raws = append(raws, e)
			}
		}
	}
	disabled := map[string]bool{}
	for _, id := range opts.DisableRules {
		disabled[id] = true
	}
	d := &SecretsDetector{}
	for _, r := range raws {
		cr, err := compileRule(r)
		if err != nil {
			return nil, err
		}
		if disabled[cr.id] {
			cr.enabled = false
		}
		d.rules = append(d.rules, cr)
		if cr.enabled && cr.maxSpan > d.maxSpan {
			d.maxSpan = cr.maxSpan
		}
	}
	return d, nil
}

func compileRule(r rawRule) (*secretRule, error) {
	if r.ID == "" {
		return nil, errors.New("secrets: rule without id")
	}
	if r.Regex == "" {
		return nil, fmt.Errorf("secrets: rule %q: regex is required", r.ID)
	}
	re, err := regexp.Compile(r.Regex)
	if err != nil {
		return nil, fmt.Errorf("secrets: rule %q: invalid regex: %w", r.ID, err)
	}
	if r.SecretGroup < 0 || r.SecretGroup > re.NumSubexp() {
		return nil, fmt.Errorf("secrets: rule %q: secretGroup %d out of range (regex has %d groups)", r.ID, r.SecretGroup, re.NumSubexp())
	}
	cr := &secretRule{
		id:          r.ID,
		description: r.Description,
		regex:       re,
		secretGroup: r.SecretGroup,
		entropy:     r.Entropy,
		enabled:     r.Enabled,
		maxSpan:     r.MaxSpan,
	}
	if !r.EnabledSet {
		cr.enabled = true
	}
	if cr.maxSpan <= 0 {
		cr.maxSpan = defaultRuleSpan
	}
	switch Severity(strings.ToLower(r.Severity)) {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		cr.severity = Severity(strings.ToLower(r.Severity))
	case "":
		cr.severity = SeverityHigh
	default:
		return nil, fmt.Errorf("secrets: rule %q: unknown severity %q", r.ID, r.Severity)
	}
	for _, k := range r.Keywords {
		if k = strings.ToLower(k); k != "" {
			cr.keywords = append(cr.keywords, k)
		}
	}
	for _, s := range r.Stopwords {
		if s = strings.ToLower(s); s != "" {
			cr.stopwords = append(cr.stopwords, s)
		}
	}
	for _, a := range r.AllowRegex {
		are, err := regexp.Compile(a)
		if err != nil {
			return nil, fmt.Errorf("secrets: rule %q: invalid allowlist regex %q: %w", r.ID, a, err)
		}
		cr.allowRe = append(cr.allowRe, are)
	}
	if r.EndRegex != "" {
		ere, err := regexp.Compile(r.EndRegex)
		if err != nil {
			return nil, fmt.Errorf("secrets: rule %q: invalid endRegex: %w", r.ID, err)
		}
		cr.endRegex = ere
	}
	return cr, nil
}

// Rules lists every loaded rule, built-in and extra, in load order.
func (d *SecretsDetector) Rules() []RuleInfo {
	out := make([]RuleInfo, 0, len(d.rules))
	for _, r := range d.rules {
		out = append(out, RuleInfo{ID: r.id, Description: r.description, Severity: r.severity, Enabled: r.enabled})
	}
	return out
}

// MaxSpan reports the longest secret span any enabled rule can produce.
func (d *SecretsDetector) MaxSpan() int { return d.maxSpan }

// Detect returns every secret found in text, ordered by offset. Offset and
// Length cover only the secret capture group, so redaction removes the
// credential and leaves surrounding context (variable names, URL hosts)
// intact.
func (d *SecretsDetector) Detect(text string) []Match {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var out []Match
	for _, r := range d.rules {
		if !r.enabled || !r.keywordHit(lower) {
			continue
		}
		for _, loc := range r.regex.FindAllStringSubmatchIndex(text, -1) {
			start, end := loc[2*r.secretGroup], loc[2*r.secretGroup+1]
			if start < 0 || end < 0 {
				continue
			}
			if r.endRegex != nil {
				start, end = r.extendSpan(text, loc[0])
			}
			secret := text[start:end]
			if r.suppressed(secret) {
				continue
			}
			out = append(out, Match{
				Kind:        KindSecrets,
				RuleID:      r.id,
				Severity:    r.severity,
				Offset:      start,
				Length:      end - start,
				Text:        secret,
				Replacement: "[REDACTED:" + r.id + "]",
			})
		}
	}
	sortByOffset(out)
	return out
}

func (r *secretRule) keywordHit(lower string) bool {
	if len(r.keywords) == 0 {
		return true
	}
	for _, k := range r.keywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// extendSpan grows a block match from its start to the end of the first
// endRegex hit, capped at maxSpan bytes (rounded down to a rune boundary).
func (r *secretRule) extendSpan(text string, start int) (int, int) {
	limit := start + r.maxSpan
	if limit > len(text) {
		limit = len(text)
	}
	if loc := r.endRegex.FindStringIndex(text[start:limit]); loc != nil {
		return start, start + loc[1]
	}
	for limit > start && limit < len(text) && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return start, limit
}

func (r *secretRule) suppressed(secret string) bool {
	if r.entropy > 0 && Entropy(secret) < r.entropy {
		return true
	}
	if len(r.stopwords) > 0 {
		ls := strings.ToLower(secret)
		for _, s := range r.stopwords {
			if strings.Contains(ls, s) {
				return true
			}
		}
	}
	for _, re := range r.allowRe {
		if re.MatchString(secret) {
			return true
		}
	}
	return false
}

// Entropy returns the Shannon entropy of s in bits per byte. Random base64
// material scores around 5–6; English prose and repeated placeholders score
// well under 4, which is what the per-rule entropy floor relies on.
func Entropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// ---------------------------------------------------------------------------
// Minimal TOML reader for the rule schema.
//
// The package is dependency-free by contract, so rather than pulling in a
// TOML library this reads exactly the subset the rule schema uses: [[rules]]
// tables, a [rules.allowlist] sub-table, and key = value pairs whose values
// are strings ("..." with escapes, '...' or '''...''' literal), arrays of
// strings, integers, floats and booleans. Anything else is an error.
// ---------------------------------------------------------------------------

func parseRulesTOML(src string) ([]rawRule, error) {
	var (
		rules   []rawRule
		cur     *rawRule
		section string // "" | "rule" | "allowlist"
	)
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		lineNo := i + 1
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case line == "[[rules]]":
			rules = append(rules, rawRule{})
			cur = &rules[len(rules)-1]
			section = "rule"
			continue
		case line == "[rules.allowlist]":
			if cur == nil {
				return nil, fmt.Errorf("line %d: [rules.allowlist] before any [[rules]]", lineNo)
			}
			section = "allowlist"
			continue
		case strings.HasPrefix(line, "["):
			return nil, fmt.Errorf("line %d: unsupported table %s", lineNo, line)
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: key outside [[rules]]", lineNo)
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		rawVal := strings.TrimSpace(line[eq+1:])
		// Multi-line literal string: gather until closing '''.
		if strings.HasPrefix(rawVal, "'''") && !(len(rawVal) >= 6 && strings.HasSuffix(rawVal, "'''")) {
			for i+1 < len(lines) {
				i++
				rawVal += "\n" + lines[i]
				if strings.Contains(lines[i], "'''") {
					break
				}
			}
			rawVal = strings.TrimSpace(rawVal)
		}
		val, err := parseTOMLValue(rawVal)
		if err != nil {
			return nil, fmt.Errorf("line %d: key %q: %w", lineNo, key, err)
		}
		if err := assignRuleKey(cur, section, key, val); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	return rules, nil
}

type tomlValue struct {
	str  string
	strs []string
	num  float64
	b    bool
	kind byte // 's' string, 'a' array, 'n' number, 'b' bool
}

func assignRuleKey(r *rawRule, section, key string, v tomlValue) error {
	want := func(kind byte) error {
		if v.kind != kind {
			return fmt.Errorf("key %q: wrong value type", key)
		}
		return nil
	}
	if section == "allowlist" {
		switch key {
		case "regexes":
			if err := want('a'); err != nil {
				return err
			}
			r.AllowRegex = v.strs
		case "stopwords":
			if err := want('a'); err != nil {
				return err
			}
			r.Stopwords = v.strs
		default:
			return fmt.Errorf("unknown allowlist key %q", key)
		}
		return nil
	}
	switch key {
	case "id":
		if err := want('s'); err != nil {
			return err
		}
		r.ID = v.str
	case "description":
		if err := want('s'); err != nil {
			return err
		}
		r.Description = v.str
	case "regex":
		if err := want('s'); err != nil {
			return err
		}
		r.Regex = v.str
	case "endRegex":
		if err := want('s'); err != nil {
			return err
		}
		r.EndRegex = v.str
	case "severity":
		if err := want('s'); err != nil {
			return err
		}
		r.Severity = v.str
	case "keywords":
		if err := want('a'); err != nil {
			return err
		}
		r.Keywords = v.strs
	case "secretGroup":
		if err := want('n'); err != nil {
			return err
		}
		r.SecretGroup = int(v.num)
	case "maxSpan":
		if err := want('n'); err != nil {
			return err
		}
		r.MaxSpan = int(v.num)
	case "entropy":
		if err := want('n'); err != nil {
			return err
		}
		r.Entropy = v.num
	case "enabled":
		if err := want('b'); err != nil {
			return err
		}
		r.Enabled = v.b
		r.EnabledSet = true
	default:
		return fmt.Errorf("unknown rule key %q", key)
	}
	return nil
}

func parseTOMLValue(s string) (tomlValue, error) {
	s = stripTrailingComment(s)
	switch {
	case s == "true":
		return tomlValue{kind: 'b', b: true}, nil
	case s == "false":
		return tomlValue{kind: 'b', b: false}, nil
	case strings.HasPrefix(s, "["):
		if !strings.HasSuffix(s, "]") {
			return tomlValue{}, errors.New("unterminated array")
		}
		items, err := splitTOMLArray(s[1 : len(s)-1])
		if err != nil {
			return tomlValue{}, err
		}
		v := tomlValue{kind: 'a', strs: []string{}}
		for _, it := range items {
			sv, err := parseTOMLString(it)
			if err != nil {
				return tomlValue{}, err
			}
			v.strs = append(v.strs, sv)
		}
		return v, nil
	case strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'"):
		sv, err := parseTOMLString(s)
		if err != nil {
			return tomlValue{}, err
		}
		return tomlValue{kind: 's', str: sv}, nil
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil {
		return tomlValue{}, fmt.Errorf("unrecognised value %q", s)
	}
	return tomlValue{kind: 'n', num: f}, nil
}

// stripTrailingComment removes a "# ..." comment that is not inside a string.
func stripTrailingComment(s string) string {
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == '\\' && inStr == '"' {
				i++
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '#':
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// splitTOMLArray splits the interior of an array on commas outside strings.
func splitTOMLArray(s string) ([]string, error) {
	var items []string
	var cur strings.Builder
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			cur.WriteByte(c)
			if c == '\\' && inStr == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
			cur.WriteByte(c)
		case c == ',':
			if t := strings.TrimSpace(cur.String()); t != "" {
				items = append(items, t)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if inStr != 0 {
		return nil, errors.New("unterminated string in array")
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		items = append(items, t)
	}
	return items, nil
}

func parseTOMLString(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, "'''"):
		if len(s) < 6 || !strings.HasSuffix(s, "'''") {
			return "", errors.New("unterminated multi-line literal string")
		}
		body := s[3 : len(s)-3]
		body = strings.TrimPrefix(body, "\n")
		return body, nil
	case strings.HasPrefix(s, "'"):
		if len(s) < 2 || !strings.HasSuffix(s, "'") {
			return "", errors.New("unterminated literal string")
		}
		return s[1 : len(s)-1], nil
	case strings.HasPrefix(s, "\""):
		if len(s) < 2 || !strings.HasSuffix(s, "\"") {
			return "", errors.New("unterminated string")
		}
		return unescapeTOMLBasic(s[1 : len(s)-1])
	}
	return "", fmt.Errorf("expected string, got %q", s)
}

func unescapeTOMLBasic(s string) (string, error) {
	if !strings.Contains(s, "\\") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New("dangling backslash")
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'u', 'U':
			width := 4
			if s[i] == 'U' {
				width = 8
			}
			if i+width > len(s) {
				return "", errors.New("short unicode escape")
			}
			cp, err := strconv.ParseUint(s[i+1:i+1+width], 16, 32)
			if err != nil {
				return "", fmt.Errorf("bad unicode escape: %w", err)
			}
			b.WriteRune(rune(cp))
			i += width
		default:
			return "", fmt.Errorf("unknown escape \\%c", s[i])
		}
	}
	return b.String(), nil
}
