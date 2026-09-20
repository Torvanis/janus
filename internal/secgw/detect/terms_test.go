package detect

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func mustTerms(t *testing.T, lists ...TermList) *TermsDetector {
	t.Helper()
	d, err := NewTermsDetector(lists)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTermsExactVsSubstring(t *testing.T) {
	text := "I like Apple and pineapple."
	exact := mustTerms(t, TermList{ID: "l1", Name: "fruit", Mode: ModeExact, Severity: SeverityLow, Terms: []string{"apple"}})
	sub := mustTerms(t, TermList{ID: "l2", Name: "fruit", Mode: ModeSubstring, Severity: SeverityLow, Terms: []string{"apple"}})

	em := exact.Detect(text)
	if len(em) != 1 || em[0].Text != "Apple" || em[0].Offset != 7 || em[0].Length != 5 {
		t.Fatalf("exact: %+v", em)
	}
	if em[0].Kind != KindTerms || em[0].RuleID != "l1" || em[0].Replacement != "[REDACTED:fruit]" || em[0].Severity != SeverityLow {
		t.Fatalf("exact fields: %+v", em[0])
	}
	sm := sub.Detect(text)
	if len(sm) != 2 {
		t.Fatalf("substring: %+v", sm)
	}
	if sm[1].Text != "apple" || text[sm[1].Offset:sm[1].Offset+sm[1].Length] != "apple" {
		t.Fatalf("substring second hit: %+v", sm[1])
	}
	if got := Redact(text, sm); got != "I like [REDACTED:fruit] and pine[REDACTED:fruit]." {
		t.Fatalf("redact: %q", got)
	}
}

func TestTermsFuzzyOriginalOffsets(t *testing.T) {
	d := mustTerms(t, TermList{ID: "codenames", Name: "codename", Mode: ModeFuzzy, Severity: SeverityHigh, Terms: []string{"Project Halberd"}})
	text := "Status update on Pr0j3ct\u200B H4lb3rd is due."
	ms := d.Detect(text)
	if len(ms) != 1 {
		t.Fatalf("got %+v", ms)
	}
	want := "Pr0j3ct\u200B H4lb3rd"
	if ms[0].Text != want || text[ms[0].Offset:ms[0].Offset+ms[0].Length] != want {
		t.Fatalf("span %q offset %d length %d", ms[0].Text, ms[0].Offset, ms[0].Length)
	}
	if ms[0].Offset != strings.Index(text, "Pr0j3ct") {
		t.Fatalf("offset %d", ms[0].Offset)
	}
	if got := Redact(text, ms); got != "Status update on [REDACTED:codename] is due." {
		t.Fatalf("redact: %q", got)
	}
	// Confusables and punctuation.
	ms = d.Detect("re: PROJECT-HALBERD launch")
	if len(ms) != 1 || ms[0].Text != "PROJECT-HALBERD" {
		t.Fatalf("got %+v", ms)
	}
	// Not matched as part of a longer word.
	if ms := d.Detect("projecthalberdx"); len(ms) != 0 {
		t.Fatalf("unexpected fuzzy hit: %+v", ms)
	}
}

func TestTermsAllowList(t *testing.T) {
	d := mustTerms(t, TermList{ID: "l", Name: "n", Mode: ModeSubstring, Terms: []string{"apple"}, Allow: []string{"apple inc"}})
	// Every hit's span is "apple" (or "Apple"), which is contained in
	// "apple inc", so all are suppressed.
	if ms := d.Detect("Apple Inc reported earnings"); len(ms) != 0 {
		t.Fatalf("expected suppression, got %+v", ms)
	}
	d2 := mustTerms(t, TermList{ID: "l", Name: "n", Mode: ModeSubstring, Terms: []string{"apple inc", "apple"}, Allow: []string{"apple inc"}})
	ms := d2.Detect("Apple Incorporated and Apple Inc and apple pie")
	// "apple inc" spans and "apple" spans are all within "apple inc" → suppressed.
	if len(ms) != 0 {
		t.Fatalf("expected suppression, got %+v", ms)
	}
	d3 := mustTerms(t, TermList{ID: "l", Name: "n", Mode: ModeSubstring, Terms: []string{"apple pie"}, Allow: []string{"apple inc"}})
	if ms := d3.Detect("apple pie"); len(ms) != 1 {
		t.Fatalf("expected hit, got %+v", ms)
	}
}

func TestTermsRegex(t *testing.T) {
	d := mustTerms(t, TermList{ID: "rx", Name: "ticket", Mode: ModeRegex, Severity: SeverityMedium, Terms: []string{`JIRA-\d{3,5}`, `acct_[a-z0-9]+`}})
	text := "see jira-1234 and ACCT_ab12 for details"
	ms := d.Detect(text)
	if len(ms) != 2 {
		t.Fatalf("got %+v", ms)
	}
	if ms[0].Text != "jira-1234" || ms[1].Text != "ACCT_ab12" {
		t.Fatalf("got %+v", ms)
	}
	if d.MaxSpan() != 512 {
		t.Fatalf("regex MaxSpan %d", d.MaxSpan())
	}

	// Compile error names the list.
	_, err := NewTermsDetector([]TermList{{ID: "bad-list", Mode: ModeRegex, Terms: []string{`(unclosed`}}})
	if err == nil || !strings.Contains(err.Error(), "bad-list") {
		t.Fatalf("expected error naming list, got %v", err)
	}
	// Length cap.
	_, err = NewTermsDetector([]TermList{{ID: "long-list", Mode: ModeRegex, Terms: []string{strings.Repeat("a", 513)}}})
	if err == nil || !strings.Contains(err.Error(), "long-list") {
		t.Fatalf("expected length error, got %v", err)
	}
	// Unknown mode.
	if _, err := NewTermsDetector([]TermList{{ID: "m", Mode: "bogus", Terms: []string{"a"}}}); err == nil {
		t.Fatal("expected unknown mode error")
	}
}

func TestTermsMaxSpan(t *testing.T) {
	d := mustTerms(t,
		TermList{ID: "a", Mode: ModeExact, Terms: []string{"abc", "abcdefghij"}},
		TermList{ID: "b", Mode: ModeFuzzy, Terms: []string{"xy"}},
	)
	if d.MaxSpan() != 10 {
		t.Fatalf("MaxSpan %d", d.MaxSpan())
	}
	var nilD *TermsDetector
	if nilD.MaxSpan() != 0 || nilD.Detect("x") != nil {
		t.Fatal("nil detector should be a no-op")
	}
	var _ SpanBound = d
}

func TestTermsPerformance(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	terms := make([]string, 5000)
	for i := range terms {
		var sb strings.Builder
		n := 6 + rng.Intn(8)
		for k := 0; k < n; k++ {
			sb.WriteByte(alpha[rng.Intn(len(alpha))])
		}
		terms[i] = sb.String()
	}
	var tb strings.Builder
	for tb.Len() < 100*1024 {
		n := 2 + rng.Intn(9)
		for k := 0; k < n; k++ {
			tb.WriteByte(alpha[rng.Intn(len(alpha))])
		}
		tb.WriteByte(' ')
	}
	text := tb.String() + " " + terms[42] + " "
	for _, mode := range []TermMatchMode{ModeExact, ModeSubstring, ModeFuzzy} {
		d := mustTerms(t, TermList{ID: "big", Name: "big", Mode: mode, Terms: terms})
		start := time.Now()
		ms := d.Detect(text)
		el := time.Since(start)
		if el > 200*time.Millisecond {
			t.Fatalf("%s: too slow: %v", mode, el)
		}
		found := false
		for _, m := range ms {
			if m.Text == terms[42] {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: planted term not found", mode)
		}
		t.Logf("%s: 5k terms over 100KB: %v (%d hits)", mode, el, len(ms))
	}
}
