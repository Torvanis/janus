// Package detect holds the deterministic detectors of the Security Gateway:
// secrets, PII, customer term lists and request-shape limits.
//
// Everything in this package is a pure function over text. It has no
// dependency on the store, on net/http, or on any classifier; that boundary
// is what makes the deterministic tier CI-testable against fixed fixtures
// without an evaluation corpus. Every detector satisfies:
//
//	func(text string) []Match
//
// All regular expressions are Go RE2 — linear time, no backtracking — so an
// administrator-authored pattern can never ReDoS the proxy. That guarantee is
// part of the product contract, not an implementation detail.
package detect

// Kind identifies which detector family produced a match.
type Kind string

const (
	KindSecrets Kind = "secrets"
	KindPII     Kind = "pii"
	KindTerms   Kind = "terms"
	KindShape   Kind = "shape"
)

// Severity ranks a match for policy purposes.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Match is one detector hit. Offset/Length are byte positions in the scanned
// text so a caller can redact the span in place. Text is the matched span
// itself; callers decide (per the capture matrix) whether it may be stored —
// for secrets and term lists it must never be persisted.
type Match struct {
	Kind     Kind
	RuleID   string
	Severity Severity
	Offset   int
	Length   int
	Text     string
	// Replacement is the string a redactor substitutes for the span, e.g.
	// "[REDACTED:aws-access-key-id]".
	Replacement string
}

// Redact replaces every match span in text with its Replacement, processing
// from the end so earlier offsets stay valid. Overlapping matches are merged
// by taking the earliest offset and the longest reach.
func Redact(text string, matches []Match) string {
	if len(matches) == 0 {
		return text
	}
	sorted := make([]Match, len(matches))
	copy(sorted, matches)
	sortByOffset(sorted)
	// Merge overlaps.
	merged := sorted[:0]
	for _, m := range sorted {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if m.Offset < last.Offset+last.Length {
				if end := m.Offset + m.Length; end > last.Offset+last.Length {
					last.Length = end - last.Offset
				}
				continue
			}
		}
		merged = append(merged, m)
	}
	out := []byte(text)
	for i := len(merged) - 1; i >= 0; i-- {
		m := merged[i]
		end := m.Offset + m.Length
		if m.Offset < 0 || end > len(out) || m.Offset > end {
			continue
		}
		rep := m.Replacement
		if rep == "" {
			rep = "[REDACTED:" + m.RuleID + "]"
		}
		out = append(out[:m.Offset], append([]byte(rep), out[end:]...)...)
	}
	return string(out)
}

func sortByOffset(ms []Match) {
	// Insertion sort: match lists are short and this avoids importing sort
	// into a package that otherwise has no dependencies.
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].Offset < ms[j-1].Offset; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

// SpanBound is implemented by a detector whose MaxSpan reports the longest
// Length among matches it could produce for the given rule set. The
// streaming hold-back buffer uses it to size its rescan overlap: an overlap
// shorter than the longest possible match lets a secret straddling two scan
// passes escape.
type SpanBound interface {
	MaxSpan() int
}
