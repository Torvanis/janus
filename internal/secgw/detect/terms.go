package detect

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// TermMatchMode selects how the terms of a TermList are matched.
type TermMatchMode string

const (
	// ModeExact matches case-insensitively with word boundaries on both sides.
	ModeExact TermMatchMode = "exact"
	// ModeSubstring matches case-insensitively anywhere in the text.
	ModeSubstring TermMatchMode = "substring"
	// ModeRegex compiles each term as an RE2 regular expression.
	ModeRegex TermMatchMode = "regex"
	// ModeFuzzy normalizes both terms and text via Normalize (case,
	// confusables, leetspeak, zero-width, punctuation) before matching.
	ModeFuzzy TermMatchMode = "fuzzy"
)

// maxRegexTermLen caps administrator-authored regular expressions. It is
// also the MaxSpan contribution of a regex list, since a bounded pattern
// still may match an unbounded span; callers size hold-back with this.
const maxRegexTermLen = 512

// TermList is one administrator-authored list of terms to detect.
type TermList struct {
	ID       string
	Name     string
	Mode     TermMatchMode
	Severity Severity
	Terms    []string
	// Allow suppresses hits whose matched span (case-insensitively) equals
	// or is contained in one of these strings, e.g. Terms=["apple"],
	// Allow=["apple inc"].
	Allow []string
}

// TermsDetector matches text against a set of TermLists.
type TermsDetector struct {
	lists   []compiledList
	maxSpan int
}

type compiledList struct {
	list  TermList
	mode  TermMatchMode
	auto  *Automaton // exact / substring / fuzzy
	terms []string   // pattern text as fed to the automaton
	res   []*regexp.Regexp
	allow []string // lower-cased
}

// NewTermsDetector compiles lists. It fails when a regex term does not
// compile or exceeds the length cap, naming the offending list.
func NewTermsDetector(lists []TermList) (*TermsDetector, error) {
	d := &TermsDetector{}
	for _, l := range lists {
		cl := compiledList{list: l, mode: l.Mode}
		if cl.mode == "" {
			cl.mode = ModeSubstring
		}
		for _, a := range l.Allow {
			cl.allow = append(cl.allow, strings.ToLower(a))
		}
		switch cl.mode {
		case ModeRegex:
			for _, t := range l.Terms {
				if len(t) > maxRegexTermLen {
					return nil, fmt.Errorf("term list %q: regex term exceeds %d characters", l.ID, maxRegexTermLen)
				}
				re, err := regexp.Compile("(?i)" + t)
				if err != nil {
					return nil, fmt.Errorf("term list %q: invalid regex %q: %w", l.ID, t, err)
				}
				cl.res = append(cl.res, re)
			}
			if d.maxSpan < maxRegexTermLen {
				d.maxSpan = maxRegexTermLen
			}
		case ModeExact, ModeSubstring, ModeFuzzy:
			for _, t := range l.Terms {
				var p string
				if cl.mode == ModeFuzzy {
					p, _ = Normalize(t)
				} else {
					p = strings.ToLower(t)
				}
				if p == "" {
					continue
				}
				cl.terms = append(cl.terms, p)
				if len(t) > d.maxSpan {
					d.maxSpan = len(t)
				}
			}
			cl.auto = NewAutomaton(cl.terms)
		default:
			return nil, fmt.Errorf("term list %q: unknown match mode %q", l.ID, l.Mode)
		}
		d.lists = append(d.lists, cl)
	}
	return d, nil
}

// MaxSpan reports the longest span any list can match: the longest term,
// or maxRegexTermLen when a regex list is present.
func (d *TermsDetector) MaxSpan() int {
	if d == nil {
		return 0
	}
	return d.maxSpan
}

// Detect scans text against every list and returns all matches. Offsets are
// always in the original text, even for fuzzy lists that scan a normalized
// copy.
func (d *TermsDetector) Detect(text string) []Match {
	if d == nil || text == "" {
		return nil
	}
	var out []Match
	var lower string
	var norm string
	var mapping []int
	for i := range d.lists {
		cl := &d.lists[i]
		rep := "[REDACTED:" + cl.list.Name + "]"
		emit := func(off, ln int) {
			span := text[off : off+ln]
			if cl.allowed(span) {
				return
			}
			out = append(out, Match{
				Kind:        KindTerms,
				RuleID:      cl.list.ID,
				Severity:    cl.list.Severity,
				Offset:      off,
				Length:      ln,
				Text:        span,
				Replacement: rep,
			})
		}
		switch cl.mode {
		case ModeRegex:
			for _, re := range cl.res {
				for _, loc := range re.FindAllStringIndex(text, -1) {
					if loc[1] > loc[0] {
						emit(loc[0], loc[1]-loc[0])
					}
				}
			}
		case ModeFuzzy:
			if mapping == nil {
				norm, mapping = Normalize(text)
			}
			for _, h := range cl.auto.Find(norm) {
				if !wordBounded(norm, h.Offset, h.Length) {
					continue
				}
				off, ln := originalSpan(mapping, text, h.Offset, h.Offset+h.Length)
				if ln > 0 {
					emit(off, ln)
				}
			}
		default: // exact, substring
			if lower == "" {
				lower = strings.ToLower(text)
			}
			sameLen := len(lower) == len(text)
			for _, h := range cl.auto.Find(lower) {
				if cl.mode == ModeExact && !wordBounded(lower, h.Offset, h.Length) {
					continue
				}
				off, ln := h.Offset, h.Length
				if !sameLen {
					// ToLower changed byte lengths (rare non-ASCII cases);
					// fall back to a bounds-safe clamp.
					if off+ln > len(text) {
						continue
					}
				}
				emit(off, ln)
			}
		}
	}
	return out
}

// allowed reports whether span is suppressed by the list's allow entries.
func (cl *compiledList) allowed(span string) bool {
	if len(cl.allow) == 0 {
		return false
	}
	ls := strings.ToLower(span)
	for _, a := range cl.allow {
		if strings.Contains(a, ls) {
			return true
		}
	}
	return false
}

// wordBounded reports whether s[off:off+ln] is not immediately preceded or
// followed by a letter or digit.
func wordBounded(s string, off, ln int) bool {
	if off > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:off])
		if isWordRune(r) {
			return false
		}
	}
	if end := off + ln; end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	if r < utf8.RuneSelf {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
	}
	return isLetterOrDigit(r)
}
