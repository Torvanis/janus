package detect

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

func sortHits(h []Hit) {
	sort.Slice(h, func(i, j int) bool {
		if h[i].Offset != h[j].Offset {
			return h[i].Offset < h[j].Offset
		}
		return h[i].Pattern < h[j].Pattern
	})
}

func TestAutomatonOverlapping(t *testing.T) {
	pats := []string{"he", "she", "his", "hers"}
	a := NewAutomaton(pats)
	hits := a.Find("ushers")
	sortHits(hits)
	want := []Hit{
		{Pattern: 1, Offset: 1, Length: 3}, // she
		{Pattern: 0, Offset: 2, Length: 2}, // he
		{Pattern: 3, Offset: 2, Length: 4}, // hers
	}
	if len(hits) != len(want) {
		t.Fatalf("got %+v want %+v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("hit %d: got %+v want %+v", i, hits[i], want[i])
		}
	}
	for _, h := range hits {
		if got := "ushers"[h.Offset : h.Offset+h.Length]; got != pats[h.Pattern] {
			t.Fatalf("span %q != pattern %q", got, pats[h.Pattern])
		}
	}
}

func TestAutomatonEmpty(t *testing.T) {
	if hits := NewAutomaton(nil).Find("anything"); len(hits) != 0 {
		t.Fatalf("expected no hits, got %+v", hits)
	}
	if hits := NewAutomaton([]string{"", ""}).Find("anything"); len(hits) != 0 {
		t.Fatalf("expected no hits for empty patterns, got %+v", hits)
	}
	if hits := NewAutomaton([]string{"a"}).Find(""); len(hits) != 0 {
		t.Fatalf("expected no hits on empty text, got %+v", hits)
	}
}

func TestAutomatonStartEnd(t *testing.T) {
	a := NewAutomaton([]string{"start", "end", "x"})
	hits := a.Find("start middle end")
	sortHits(hits)
	if len(hits) != 2 {
		t.Fatalf("got %+v", hits)
	}
	if hits[0] != (Hit{Pattern: 0, Offset: 0, Length: 5}) {
		t.Fatalf("start: %+v", hits[0])
	}
	if hits[1] != (Hit{Pattern: 1, Offset: 13, Length: 3}) {
		t.Fatalf("end: %+v", hits[1])
	}
}

func TestAutomatonRepeated(t *testing.T) {
	a := NewAutomaton([]string{"aa"})
	hits := a.Find("aaaa")
	if len(hits) != 3 {
		t.Fatalf("expected 3 overlapping hits, got %+v", hits)
	}
}

func TestAutomatonPerformance(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	pats := make([]string, 10000)
	for i := range pats {
		var sb strings.Builder
		for k := 0; k < 8; k++ {
			sb.WriteByte(alpha[rng.Intn(len(alpha))])
		}
		pats[i] = sb.String()
	}
	text := make([]byte, 1<<20)
	for i := range text {
		text[i] = alpha[rng.Intn(len(alpha))]
	}
	// Plant a few known patterns.
	copy(text[100:], pats[0])
	copy(text[len(text)-8:], pats[1])

	start := time.Now()
	a := NewAutomaton(pats)
	hits := a.Find(string(text))
	el := time.Since(start)
	// Correctness at scale is what this test guards. Wall-clock is logged,
	// not asserted: under -race on a loaded CI runner the same work takes
	// 2-3s and a hard bound only measures the machine. The linear-time
	// guarantee is exercised by BenchmarkAutomatonFind.
	t.Logf("10k patterns over 1 MiB: %v (%d hits)", el, len(hits))
	found0, found1 := false, false
	for _, h := range hits {
		if h.Pattern == 0 && h.Offset == 100 {
			found0 = true
		}
		if h.Pattern == 1 && h.Offset == len(text)-8 {
			found1 = true
		}
	}
	if !found0 || !found1 {
		t.Fatalf("planted patterns not found (%v %v)", found0, found1)
	}
	t.Logf("10k patterns over 1MB: %v, %d hits", el, len(hits))
}

func BenchmarkAutomatonFind(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	pats := make([]string, 10000)
	for i := range pats {
		var sb strings.Builder
		for k := 0; k < 8; k++ {
			sb.WriteByte(alpha[rng.Intn(len(alpha))])
		}
		pats[i] = sb.String()
	}
	text := make([]byte, 1<<20)
	for i := range text {
		text[i] = alpha[rng.Intn(len(alpha))]
	}
	a := NewAutomaton(pats)
	s := string(text)
	b.SetBytes(int64(len(s)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Find(s)
	}
}
