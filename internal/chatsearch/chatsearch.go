// Package chatsearch turns chat text into keyed blind-index terms, so the
// server can search encrypted chat history without storing plaintext words.
//
// A message is split into words, each word is lightly stemmed, and each
// stem is stored only as HMAC-SHA256(searchKey, userID + "\x00" + stem). The
// search key is derived from the instance encryption key, so the index is
// useless without it, and the user ID salts every term so the same word in
// two users' chats produces unrelated hashes (no cross-user correlation).
//
// Matching is whole word (after stemming) only. Substring and prefix search
// are intentionally unsupported: they would need either plaintext n-grams
// or a much larger, leakier index, which the product ruling (content stays
// encrypted at rest) rules out.
package chatsearch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MinTermRunes and MaxTermRunes bound an indexed word: one-letter words
	// carry no signal, and very long "words" are hashes, URLs or base64.
	MinTermRunes = 2
	MaxTermRunes = 40
	// MaxTermsPerMessage caps the distinct terms indexed for one message.
	MaxTermsPerMessage = 2000
	// minStemRunes is the shortest stem a suffix may be stripped down to.
	minStemRunes = 3
)

// suffixes are tried in order; the first one whose removal leaves at least
// minStemRunes runes wins. "ss" endings keep their s (class, glass).
var suffixes = []string{"ing", "ed", "es", "ly", "s"}

// Stem applies the light English stemming rule, in order:
//
//  1. strip the first matching suffix of "ing", "ed", "es", "ly", "s" when
//     at least three runes remain (never the s of a word ending in "ss");
//  2. after "ing"/"ed", undouble a final doubled consonant other than
//     l, s, z ("planning" → "plan", "stopped" → "stop", "falling" → "fall");
//  3. drop a final "e" when at least three runes remain ("paddle" →
//     "paddl", matching "paddling"; "use" stays).
//
// It is deliberately crude — the same rule runs on the index and the query,
// so it only has to be consistent, not linguistically right: "searching",
// "searched" and "searches" all become "search".
func Stem(word string) string {
	for _, suf := range suffixes {
		if !strings.HasSuffix(word, suf) {
			continue
		}
		if suf == "s" && strings.HasSuffix(word, "ss") {
			break
		}
		stem := strings.TrimSuffix(word, suf)
		if utf8.RuneCountInString(stem) < minStemRunes {
			continue
		}
		word = stem
		if suf == "ing" || suf == "ed" {
			word = undouble(word)
		}
		break
	}
	if strings.HasSuffix(word, "e") && utf8.RuneCountInString(word) > minStemRunes {
		word = strings.TrimSuffix(word, "e")
	}
	return word
}

func undouble(w string) string {
	r := []rune(w)
	n := len(r)
	if n > minStemRunes && r[n-1] == r[n-2] && !strings.ContainsRune("aeiouylsz", r[n-1]) && unicode.IsLetter(r[n-1]) {
		return string(r[:n-1])
	}
	return w
}

// Words splits text into lowercase runs of Unicode letters and digits.
func Words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Terms returns the distinct stemmed terms of text in first-seen order,
// dropping words shorter than MinTermRunes or longer than MaxTermRunes and
// stopping at MaxTermsPerMessage. Queries use the same function.
func Terms(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, w := range Words(text) {
		n := utf8.RuneCountInString(w)
		if n < MinTermRunes || n > MaxTermRunes {
			continue
		}
		t := Stem(w)
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= MaxTermsPerMessage {
			break
		}
	}
	return out
}

// Hash is the blind-index value of one term for one user: the first 32 hex
// characters (128 bits) of HMAC-SHA256(key, userID + "\x00" + term).
func Hash(key []byte, userID, term string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(userID))
	m.Write([]byte{0})
	m.Write([]byte(term))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// Hashes hashes every term for userID.
func Hashes(key []byte, userID string, terms []string) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = Hash(key, userID, t)
	}
	return out
}

// Snippet returns about width runes of text around the first word that
// starts with any of terms (case-insensitive; terms are stems, so "search"
// finds "Searching"), with whitespace collapsed and "…" marking cuts. With
// no match it returns the start of the text.
func Snippet(text string, terms []string, width int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if width <= 0 {
		width = 160
	}
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	at := -1
	for i := range lower {
		if i > 0 && isWordRune(lower[i-1]) {
			continue // only match at the start of a word
		}
		for _, t := range terms {
			if t != "" && hasRunePrefix(lower[i:], t) {
				at = i
				break
			}
		}
		if at >= 0 {
			break
		}
	}
	start := 0
	if at > width/3 {
		start = at - width/3
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		if end-width > 0 && at >= 0 {
			start = max(0, min(start, end-width))
		}
	}
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func hasRunePrefix(s []rune, prefix string) bool {
	i := 0
	for _, r := range prefix {
		if i >= len(s) || s[i] != r {
			return false
		}
		i++
	}
	return true
}
