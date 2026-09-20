// Package stream implements the hold-back buffer for streaming egress.
//
// The scan unit is NOT the SSE frame: a delta carries a handful of
// characters and "AKIA" arrives as "AK" + "IA". Holdback keeps a separate
// reassembled plaintext buffer per choice index (and per tool-call index)
// and scans that, while the frames themselves queue for release. Frames are
// the transport; the text is the subject.
//
// HoldBytes is the entire protection: anything already released is
// unrecoverable. Overlap must be at least the longest match any active rule
// can produce, or a match straddling two scan passes escapes; the engine
// supplies it from the compiled rule set rather than a constant.
package stream

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Match is a detector hit on a channel's accumulated text.
type Match struct {
	Offset, Length int
	Replacement    string
	Block          bool // true → cut the stream; false → redact in place
	Observe        bool // true → report only; the text is neither cut nor rewritten
	// Tag carries whatever the scanner needs to recognise this match when
	// it comes back in FeedResult.Matches (the engine keeps the violation).
	Tag any
}

// Scanner is what Holdback calls on newly accumulated text. It receives the
// channel's full accumulated text and the offset from which new content
// begins (minus overlap), and returns matches with offsets into the full
// text. The scanner decides block vs redact per match.
type Scanner func(channel string, full string, from int) []Match

// frame is one SSE frame with the text deltas it contributed.
type frame struct {
	raw    []byte
	deltas []delta
}

// delta is one text contribution of a frame to a channel.
type delta struct {
	channel  string // "c<choice>" | "c<choice>:r" (reasoning) | "c<choice>:t<tool>"
	start    int    // offset into the channel's accumulated text
	length   int
	jsonPath []string // path within the frame JSON to the string to rewrite
}

// channel is the accumulated text of one stream channel.
type channel struct {
	buf         []byte
	scannedTo   int
	released    int // bytes of this channel already released to the client
	dirtyRanges ranges
}

// Holdback buffers frames and releases them once their text is older than
// HoldBytes and has been scanned clean.
type Holdback struct {
	HoldBytes int
	Overlap   int
	Scan      Scanner

	pending  []frame
	channels map[string]*channel
	cut      bool
	// Released is the count of bytes handed back by Feed/Flush.
	Released int64
}

// New builds a Holdback.
func New(holdBytes, overlap int, scan Scanner) *Holdback {
	if holdBytes < 0 {
		holdBytes = 0
	}
	if overlap < 0 {
		overlap = 0
	}
	return &Holdback{HoldBytes: holdBytes, Overlap: overlap, Scan: scan, channels: map[string]*channel{}}
}

// Result is what Feed returns.
type Result struct {
	// Release are frames safe to write to the client, in order.
	Release [][]byte
	// Cut reports that a blocking match was found. The pending queue has
	// been dropped (never returned) and no further frames will be released.
	Cut bool
	// Blocked is the first blocking match, for the audit row.
	Blocked *Match
	// Redactions counts spans rewritten in this call.
	Redactions int
	// Matches lists every match found in this call (blocking or not).
	Matches []Match
}

// Feed accepts one transformed SSE frame (a full "data: {...}\n\n" line).
func (h *Holdback) Feed(raw []byte) Result {
	if h.cut {
		return Result{Cut: true}
	}
	f := frame{raw: raw}
	h.extractDeltas(&f)
	h.pending = append(h.pending, f)

	res := Result{}
	// A frame with no text delta (keepalive, [DONE], the blank line that
	// terminates an SSE event, a usage-only chunk) has nothing to scan,
	// but it must not overtake frames queued ahead of it: releasing the
	// "\n" terminator before its "data:" line would corrupt the stream,
	// and releasing [DONE] early would end it. releaseReady handles it in
	// order: a delta-less frame is ready whenever everything before it is.
	if len(f.deltas) == 0 {
		res.Release = h.releaseReady(false)
		return res
	}
	// Scan every channel that grew.
	touched := map[string]bool{}
	for _, d := range f.deltas {
		touched[d.channel] = true
	}
	for name := range touched {
		ch := h.channels[name]
		from := ch.scannedTo - h.Overlap
		if from < 0 {
			from = 0
		}
		matches := h.Scan(name, string(ch.buf), from)
		prevScannedTo := ch.scannedTo
		ch.scannedTo = len(ch.buf)
		for i := range matches {
			m := matches[i]
			// The overlap window is rescanned so a term split across frames
			// is caught. A match that starts inside it was already reported
			// by the previous scan (redaction hides this — the rewrite
			// means it cannot be re-found — but observe leaves the text
			// intact, and every rescan reported the same hit again).
			if m.Offset < prevScannedTo && m.Offset+m.Length <= prevScannedTo {
				continue
			}
			if m.Offset < ch.released {
				// Already left the building. Report it (audit) but nothing
				// can be done; this only happens when Overlap < the match
				// length, which the engine prevents.
				res.Matches = append(res.Matches, m)
				continue
			}
			res.Matches = append(res.Matches, m)
			if m.Block {
				h.cut = true
				h.pending = nil
				res.Cut = true
				res.Blocked = &matches[i]
				return res
			}
			if m.Observe {
				continue
			}
			h.redact(name, m)
			res.Redactions++
		}
	}
	// Release frames whose every delta ends at least HoldBytes before the
	// end of its channel.
	res.Release = h.releaseReady(false)
	return res
}

// Flush releases everything pending at end of stream.
func (h *Holdback) Flush() [][]byte {
	if h.cut {
		return nil
	}
	return h.releaseReady(true)
}

func (h *Holdback) releaseReady(all bool) [][]byte {
	var out [][]byte
	n := 0
	for _, f := range h.pending {
		ready := true
		if !all {
			for _, d := range f.deltas {
				ch := h.channels[d.channel]
				if d.start+d.length > len(ch.buf)-h.HoldBytes {
					ready = false
					break
				}
			}
		}
		if !ready {
			break
		}
		out = append(out, h.render(f))
		for _, d := range f.deltas {
			ch := h.channels[d.channel]
			if end := d.start + d.length; end > ch.released {
				ch.released = end
			}
		}
		n++
	}
	h.pending = h.pending[n:]
	for _, b := range out {
		h.Released += int64(len(b))
	}
	return out
}

// render re-serialises a frame with any redactions applied to its deltas.
func (h *Holdback) render(f frame) []byte {
	if len(f.deltas) == 0 {
		return f.raw
	}
	// Fast path: if no delta's text changed, return the raw bytes.
	changed := false
	for _, d := range f.deltas {
		ch := h.channels[d.channel]
		if d.dirty(ch) {
			changed = true
			break
		}
	}
	if !changed {
		return f.raw
	}
	payload, prefix, suffix, ok := splitSSE(f.raw)
	if !ok {
		return f.raw
	}
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(payload, &tree); err != nil {
		return f.raw
	}
	for _, d := range f.deltas {
		ch := h.channels[d.channel]
		text := string(ch.buf[d.start : d.start+d.length])
		if !setPath(tree, d.jsonPath, text) {
			return f.raw
		}
	}
	enc, err := json.Marshal(tree)
	if err != nil {
		return f.raw
	}
	return append(append(append([]byte{}, prefix...), enc...), suffix...)
}

// dirty reports whether the delta's span in the channel buffer differs
// from what the frame originally carried. We track that with a per-channel
// dirty bitmap approximated by a redaction log; simplest correct approach:
// compare against the original text kept alongside.
func (d delta) dirty(ch *channel) bool {
	return ch.dirtyRanges != nil && ch.dirtyRanges.overlaps(d.start, d.start+d.length)
}

type ranges []struct{ a, b int }

func (r ranges) overlaps(a, b int) bool {
	for _, x := range r {
		if a < x.b && x.a < b {
			return true
		}
	}
	return false
}

// redact replaces a span in the channel buffer with its replacement. Because
// replacement length differs from span length, every later delta's start
// must shift; we keep delta starts stable by padding/truncating the
// replacement to the span length. A redaction marker like
// "[REDACTED:aws-key]" is truncated or right-padded with spaces to fit.
func (h *Holdback) redact(name string, m Match) {
	ch := h.channels[name]
	if m.Offset < 0 || m.Offset+m.Length > len(ch.buf) {
		return
	}
	rep := []byte(m.Replacement)
	if len(rep) > m.Length {
		rep = rep[:m.Length]
	} else if len(rep) < m.Length {
		rep = append(rep, bytes.Repeat([]byte{' '}, m.Length-len(rep))...)
	}
	copy(ch.buf[m.Offset:], rep)
	ch.dirtyRanges = append(ch.dirtyRanges, struct{ a, b int }{m.Offset, m.Offset + m.Length})
}

// --- SSE / JSON plumbing --------------------------------------------------------

// splitSSE returns the JSON payload of a "data: {...}" line with its
// surrounding bytes, or ok=false for non-data lines and [DONE].
func splitSSE(raw []byte) (payload, prefix, suffix []byte, ok bool) {
	trimmed := bytes.TrimRight(raw, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, nil, nil, false
	}
	rest := bytes.TrimLeft(trimmed[len("data:"):], " ")
	if bytes.Equal(rest, []byte("[DONE]")) || len(rest) == 0 || rest[0] != '{' {
		return nil, nil, nil, false
	}
	prefixLen := len(trimmed) - len(rest)
	return rest, raw[:prefixLen], raw[len(trimmed):], true
}

// extractDeltas parses a chat.completion.chunk frame and records every text
// contribution to its channel buffers.
func (h *Holdback) extractDeltas(f *frame) {
	payload, _, _, ok := splitSSE(f.raw)
	if !ok {
		return
	}
	var chunk struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int `json:"index"`
					Function struct {
						Arguments *string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	for ci, c := range chunk.Choices {
		base := "c" + itoa(c.Index)
		if c.Delta.Content != nil && *c.Delta.Content != "" {
			h.append(f, base, *c.Delta.Content, []string{"choices", itoa(ci), "delta", "content"})
		}
		if c.Delta.ReasoningContent != nil && *c.Delta.ReasoningContent != "" {
			h.append(f, base+":r", *c.Delta.ReasoningContent, []string{"choices", itoa(ci), "delta", "reasoning_content"})
		}
		for ti, tc := range c.Delta.ToolCalls {
			if tc.Function.Arguments != nil && *tc.Function.Arguments != "" {
				h.append(f, base+":t"+itoa(tc.Index), *tc.Function.Arguments,
					[]string{"choices", itoa(ci), "delta", "tool_calls", itoa(ti), "function", "arguments"})
			}
		}
	}
}

func (h *Holdback) append(f *frame, name, text string, path []string) {
	ch := h.channels[name]
	if ch == nil {
		ch = &channel{}
		h.channels[name] = ch
	}
	start := len(ch.buf)
	ch.buf = append(ch.buf, text...)
	f.deltas = append(f.deltas, delta{channel: name, start: start, length: len(text), jsonPath: path})
}

// setPath writes a string at a path of object keys / array indices.
func setPath(tree map[string]json.RawMessage, path []string, value string) bool {
	if len(path) == 0 {
		return false
	}
	key := path[0]
	if len(path) == 1 {
		enc, _ := json.Marshal(value)
		tree[key] = enc
		return true
	}
	raw, ok := tree[key]
	if !ok {
		return false
	}
	// Next segment: object or array?
	if isIndex(path[1]) {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return false
		}
		idx := atoi(path[1])
		if idx < 0 || idx >= len(arr) {
			return false
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(arr[idx], &obj); err != nil {
			return false
		}
		if !setPath(obj, path[2:], value) {
			return false
		}
		enc, err := json.Marshal(obj)
		if err != nil {
			return false
		}
		arr[idx] = enc
		aenc, err := json.Marshal(arr)
		if err != nil {
			return false
		}
		tree[key] = aenc
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if !setPath(obj, path[1:], value) {
		return false
	}
	enc, err := json.Marshal(obj)
	if err != nil {
		return false
	}
	tree[key] = enc
	return true
}

func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	var b strings.Builder
	if i == 0 {
		return "0"
	}
	if i < 0 {
		b.WriteByte('-')
		i = -i
	}
	var digits [20]byte
	n := len(digits)
	for i > 0 {
		n--
		digits[n] = byte('0' + i%10)
		i /= 10
	}
	b.Write(digits[n:])
	return b.String()
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
