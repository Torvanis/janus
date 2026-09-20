package detect

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestNormalizeLeetspeak(t *testing.T) {
	out, mapping := Normalize("Pr0j3ct H4lb3rd")
	if out != "project halberd" {
		t.Fatalf("got %q", out)
	}
	if len(mapping) != len(out) {
		t.Fatalf("mapping len %d != out len %d", len(mapping), len(out))
	}
}

func TestNormalizeDigitsNotFolded(t *testing.T) {
	out, _ := Normalize("2024-01-01")
	if out != "2024 01 01" {
		t.Fatalf("got %q", out)
	}
	out, _ = Normalize("costs $5 today")
	if out != "costs $5 today" && out != "costs 5 today" {
		// "$" not adjacent to a letter is treated as a symbol separator; both
		// forms are acceptable so long as the digit survives.
		t.Fatalf("got %q", out)
	}
	if !strings.Contains(out, "5") {
		t.Fatalf("digit was folded: %q", out)
	}
}

func TestNormalizeZeroWidth(t *testing.T) {
	in := "Pro\u200Bject\u200D Hal\uFEFFberd\u2060"
	out, _ := Normalize(in)
	if out != "project halberd" {
		t.Fatalf("got %q", out)
	}
}

func TestNormalizeConfusables(t *testing.T) {
	// Cyrillic а, о, е, р, с and fullwidth letters.
	in := "Prоjеct Ｈаlbеrd" // o, e in Cyrillic; fullwidth H; Cyrillic a
	out, _ := Normalize(in)
	if out != "project halberd" {
		t.Fatalf("got %q", out)
	}
}

func TestNormalizeCollapsesPunctuation(t *testing.T) {
	out, _ := Normalize("  Hello,,,   World!!! ")
	if out != "hello world" {
		t.Fatalf("got %q", out)
	}
}

func TestNormalizeMappingRoundTrip(t *testing.T) {
	samples := []string{
		"Pr0j3ct H4lb3rd",
		"Prоjеct\u200B Ｈаlbеrd",
		"  Hello,,,   World!!! ",
		"ÄÖÜ straße",
		"2024-01-01",
	}
	for _, in := range samples {
		out, mapping := Normalize(in)
		if len(mapping) != len(out) {
			t.Fatalf("%q: mapping len %d != out len %d", in, len(mapping), len(out))
		}
		prev := -1
		for i := 0; i < len(out); {
			r, size := utf8.DecodeRuneInString(out[i:])
			orig := mapping[i]
			if orig < 0 || orig >= len(in) {
				t.Fatalf("%q: mapping[%d]=%d out of range", in, i, orig)
			}
			if orig < prev {
				t.Fatalf("%q: mapping not monotonic at %d", in, i)
			}
			prev = orig
			or, _ := utf8.DecodeRuneInString(in[orig:])
			if r == ' ' {
				if !isSeparator(foldRune(or)) {
					t.Fatalf("%q: space at %d maps to non-separator %q", in, i, or)
				}
			} else {
				f := foldRune(or)
				if l, ok := leet(f); ok {
					f = l
				}
				if f != r && unicode.ToLower(or) != r {
					t.Fatalf("%q: out[%d]=%q maps to original %q (folds to %q)", in, i, r, or, f)
				}
			}
			for k := 1; k < size; k++ {
				if mapping[i+k] != orig {
					t.Fatalf("%q: continuation byte mapping mismatch at %d", in, i+k)
				}
			}
			i += size
		}
	}
}

func TestOriginalSpan(t *testing.T) {
	in := "xx Pr0j3ct\u200B H4lb3rd yy"
	out, mapping := Normalize(in)
	idx := strings.Index(out, "project halberd")
	if idx < 0 {
		t.Fatalf("normalized %q", out)
	}
	off, ln := originalSpan(mapping, in, idx, idx+len("project halberd"))
	if in[off:off+ln] != "Pr0j3ct\u200B H4lb3rd" {
		t.Fatalf("got %q", in[off:off+ln])
	}
}
