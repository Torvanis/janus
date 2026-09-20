package detect

// Automaton is a minimal byte-oriented Aho–Corasick multi-pattern matcher.
// It reports every occurrence of every pattern, overlapping included, in a
// single linear pass over the text. Only the standard library is used.
type Automaton struct {
	// next[state*256+b] is the goto/fail-resolved transition; states are
	// dense so a flat slice keeps lookups cache friendly.
	next []int32
	// out[state] lists pattern indices ending at this state (including
	// those inherited through the failure/dictionary links).
	out    [][]int32
	lens   []int
	nstate int
}

// Hit is one pattern occurrence. Pattern indexes the slice passed to
// NewAutomaton; Offset/Length are byte positions in the searched text.
type Hit struct {
	Pattern int
	Offset  int
	Length  int
}

// NewAutomaton builds the automaton. Empty patterns are ignored (they would
// match at every position and carry no information).
func NewAutomaton(patterns []string) *Automaton {
	a := &Automaton{lens: make([]int, len(patterns))}
	// Trie construction with a sparse child map per state; resolved into a
	// dense table afterwards.
	type node struct {
		child map[byte]int32
		out   []int32
	}
	nodes := []node{{child: map[byte]int32{}}}
	for pi, p := range patterns {
		a.lens[pi] = len(p)
		if p == "" {
			continue
		}
		cur := int32(0)
		for i := 0; i < len(p); i++ {
			b := p[i]
			nx, ok := nodes[cur].child[b]
			if !ok {
				nodes = append(nodes, node{child: map[byte]int32{}})
				nx = int32(len(nodes) - 1)
				nodes[cur].child[b] = nx
			}
			cur = nx
		}
		nodes[cur].out = append(nodes[cur].out, int32(pi))
	}
	n := len(nodes)
	a.nstate = n
	a.next = make([]int32, n*256)
	a.out = make([][]int32, n)
	fail := make([]int32, n)

	// BFS to compute failure links and complete the transition table.
	queue := make([]int32, 0, n)
	for b := 0; b < 256; b++ {
		if nx, ok := nodes[0].child[byte(b)]; ok {
			a.next[b] = nx
			fail[nx] = 0
			queue = append(queue, nx)
		} else {
			a.next[b] = 0
		}
	}
	a.out[0] = nodes[0].out
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		f := fail[s]
		// Output inherits from the failure state.
		o := append([]int32(nil), nodes[s].out...)
		o = append(o, a.out[f]...)
		a.out[s] = o
		base := int(s) * 256
		fbase := int(f) * 256
		for b := 0; b < 256; b++ {
			if nx, ok := nodes[s].child[byte(b)]; ok {
				a.next[base+b] = nx
				fail[nx] = a.next[fbase+b]
				queue = append(queue, nx)
			} else {
				a.next[base+b] = a.next[fbase+b]
			}
		}
	}
	return a
}

// Find returns every occurrence of every pattern in text, ordered by end
// position (and by pattern index within the same end position).
func (a *Automaton) Find(text string) []Hit {
	if a == nil || a.nstate <= 1 {
		return nil
	}
	var hits []Hit
	s := int32(0)
	for i := 0; i < len(text); i++ {
		s = a.next[int(s)*256+int(text[i])]
		if o := a.out[s]; len(o) > 0 {
			for _, pi := range o {
				l := a.lens[pi]
				hits = append(hits, Hit{Pattern: int(pi), Offset: i + 1 - l, Length: l})
			}
		}
	}
	return hits
}
