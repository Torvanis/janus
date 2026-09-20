package detect

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Normalize folds text into a canonical form suited to fuzzy term matching
// and returns, for every byte of the output, the byte offset in the ORIGINAL
// input that produced it. A match found in the folded text can therefore be
// reported at its original Offset/Length.
//
// Folding steps, applied per rune:
//   - zero-width characters (U+200B..U+200D, U+FEFF, U+2060) are dropped;
//   - fullwidth ASCII (U+FF01..U+FF5E) is mapped to plain ASCII;
//   - common Cyrillic confusables (а е о р с х and capitals) become latin;
//   - letters are lower-cased via unicode.SimpleFold / unicode.ToLower;
//   - leetspeak digits/symbols (0 1 3 4 5 7 @ $) become letters, but only
//     when adjacent to a letter, so "2024" or "$5" stay untouched;
//   - runs of whitespace and punctuation collapse to a single ASCII space;
//     leading/trailing runs are removed.
//
// Only the standard library is used; this is deliberately not full NFKC.
func Normalize(s string) (string, []int) {
	type tok struct {
		r   rune
		off int
	}
	// Pass 1: decode, drop zero-width, fold confusables/case, keep offsets.
	toks := make([]tok, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		off := i
		i += size
		if isZeroWidth(r) {
			continue
		}
		r = foldRune(r)
		toks = append(toks, tok{r: r, off: off})
	}

	// Pass 2: leetspeak, only when adjacent to a letter (looking at the
	// already-folded neighbours).
	isLetterAt := func(i int) bool {
		return i >= 0 && i < len(toks) && unicode.IsLetter(toks[i].r)
	}
	for i := range toks {
		if l, ok := leet(toks[i].r); ok && (isLetterAt(i-1) || isLetterAt(i+1)) {
			toks[i].r = l
		}
	}

	// Pass 3: collapse whitespace/punctuation runs into one space, emit.
	var b strings.Builder
	b.Grow(len(s))
	mapping := make([]int, 0, len(s))
	pendingSpace := -1 // original offset of a pending separator, -1 if none
	for _, t := range toks {
		if isSeparator(t.r) {
			if pendingSpace < 0 {
				pendingSpace = t.off
			}
			continue
		}
		if pendingSpace >= 0 {
			if b.Len() > 0 {
				b.WriteByte(' ')
				mapping = append(mapping, pendingSpace)
			}
			pendingSpace = -1
		}
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], t.r)
		b.Write(buf[:n])
		for k := 0; k < n; k++ {
			mapping = append(mapping, t.off)
		}
	}
	return b.String(), mapping
}

func isZeroWidth(r rune) bool {
	switch r {
	case 0x200B, 0x200C, 0x200D, 0xFEFF, 0x2060:
		return true
	}
	return false
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isSeparator(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsControl(r)
}

// foldRune maps fullwidth ASCII and Cyrillic confusables to latin, then
// lower-cases.
func foldRune(r rune) rune {
	// Fullwidth ASCII block U+FF01..U+FF5E maps to U+0021..U+007E.
	if r >= 0xFF01 && r <= 0xFF5E {
		r = r - 0xFF01 + 0x21
	} else if r == 0x3000 { // ideographic space
		r = ' '
	}
	switch r {
	case 'а', 'А':
		return 'a'
	case 'е', 'Е':
		return 'e'
	case 'о', 'О':
		return 'o'
	case 'р', 'Р':
		return 'p'
	case 'с', 'С':
		return 'c'
	case 'х', 'Х':
		return 'x'
	case 'у', 'У':
		return 'y'
	case 'і', 'І':
		return 'i'
	case 'ј', 'Ј':
		return 'j'
	}
	if r < utf8.RuneSelf {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	lr := unicode.ToLower(r)
	if lr != r {
		return lr
	}
	// SimpleFold walks the case orbit; pick the smallest lower-case member.
	best := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if unicode.IsLower(f) && f < best {
			best = f
		}
	}
	return best
}

func leet(r rune) (rune, bool) {
	switch r {
	case '0':
		return 'o', true
	case '1':
		return 'l', true
	case '3':
		return 'e', true
	case '4':
		return 'a', true
	case '5':
		return 's', true
	case '7':
		return 't', true
	case '@':
		return 'a', true
	case '$':
		return 's', true
	}
	return r, false
}

// normalizeMapping is used by the terms detector to translate a span in the
// folded text back to the original text. It returns (offset, length) in the
// original string covering all original bytes that produced out[start:end].
func originalSpan(mapping []int, orig string, start, end int) (int, int) {
	if start >= end || start < 0 || end > len(mapping) {
		return 0, 0
	}
	first := mapping[start]
	lastStart := mapping[end-1]
	// Extend to the end of the rune that starts at lastStart.
	_, size := utf8.DecodeRuneInString(orig[lastStart:])
	if size == 0 {
		size = 1
	}
	return first, lastStart + size - first
}
