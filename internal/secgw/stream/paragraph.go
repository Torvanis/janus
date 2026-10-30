package stream

import (
	"bytes"
	"encoding/json"
)

// ParagraphGate is the reply hold-back for the Family safety policy: unlike
// Holdback (a fixed byte window scanned by deterministic rules), it holds
// every frame until the text it carries has been JUDGED by a classifier,
// one chunk at a time. A chunk ends at a paragraph break; without one, at
// the last sentence or line end once SoftBytes have accumulated, or hard at
// HardBytes. A frame is released only when every text delta in it ends at
// or before its channel's cleared offset, so nothing unjudged ever leaves.
// On a block the pending queue is dropped and nothing more is released.
//
// Judged text: content, reasoning (reasoning_content / reasoning) and the
// legacy completions "text" field. Tool-call arguments are not judged (they
// are not shown to the user) but keep their place in the queue.
type ParagraphGate struct {
	SoftBytes int
	HardBytes int
	// Judge decides one chunk of a channel's text given the cleared text
	// before it. block=true cuts the stream; tag rides on the result.
	Judge func(channel, prior, chunk string) (block bool, tag any)

	pending  []gateFrame
	channels map[string]*gateChannel
	order    []string
	cut      bool
	// Judged counts Judge calls.
	Judged int
}

type gateFrame struct {
	raw    []byte
	deltas []gateDelta
}

type gateDelta struct {
	channel string
	end     int
}

type gateChannel struct {
	buf     []byte
	cleared int
}

// Gate defaults: one judgement per paragraph, bounded so a reply with no
// paragraph breaks is still judged (and released) in pieces.
const (
	DefaultSoftBytes = 600
	DefaultHardBytes = 1500
)

// NewParagraphGate builds a gate with the default chunk bounds.
func NewParagraphGate(judge func(channel, prior, chunk string) (bool, any)) *ParagraphGate {
	return &ParagraphGate{SoftBytes: DefaultSoftBytes, HardBytes: DefaultHardBytes, Judge: judge, channels: map[string]*gateChannel{}}
}

// GateResult is what Feed and Flush return.
type GateResult struct {
	Release [][]byte
	Cut     bool
	// Tag is the blocking judgement's tag when Cut.
	Tag any
}

// Feed accepts one transformed SSE frame.
func (g *ParagraphGate) Feed(raw []byte) GateResult {
	if g.cut {
		return GateResult{Cut: true}
	}
	f := gateFrame{raw: raw}
	touched := g.extract(&f)
	g.pending = append(g.pending, f)
	for _, name := range touched {
		if res, cut := g.judgeReady(name, false); cut {
			return res
		}
	}
	return GateResult{Release: g.release(false)}
}

// Flush judges whatever text is still held and releases the rest of the
// queue (end of stream), unless the tail is blocked.
func (g *ParagraphGate) Flush() GateResult {
	if g.cut {
		return GateResult{Cut: true}
	}
	for _, name := range g.order {
		if res, cut := g.judgeReady(name, true); cut {
			return res
		}
	}
	return GateResult{Release: g.release(true)}
}

// judgeReady judges every complete chunk of a channel (and, at the end,
// the remainder).
func (g *ParagraphGate) judgeReady(name string, final bool) (GateResult, bool) {
	ch := g.channels[name]
	for {
		end := g.boundary(ch.buf[ch.cleared:])
		if end <= 0 {
			if !final || ch.cleared >= len(ch.buf) {
				return GateResult{}, false
			}
			end = len(ch.buf) - ch.cleared
		}
		g.Judged++
		block, tag := g.Judge(name, string(ch.buf[:ch.cleared]), string(ch.buf[ch.cleared:ch.cleared+end]))
		if block {
			g.cut = true
			g.pending = nil
			return GateResult{Cut: true, Tag: tag}, true
		}
		ch.cleared += end
	}
}

// boundary is where the next chunk of seg ends (0 = not yet).
func (g *ParagraphGate) boundary(seg []byte) int {
	if i := bytes.LastIndex(seg, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	if len(seg) < g.SoftBytes {
		return 0
	}
	best := -1
	for _, sep := range [][]byte{[]byte(". "), []byte("! "), []byte("? "), []byte("\n")} {
		if i := bytes.LastIndex(seg, sep); i >= 0 && i+len(sep) > best {
			best = i + len(sep)
		}
	}
	if best > 0 {
		return best
	}
	if len(seg) <= g.HardBytes {
		return 0
	}
	cut := g.HardBytes
	for cut > 0 && (seg[cut]&0xC0) == 0x80 {
		cut-- // never split a UTF-8 sequence
	}
	return cut
}

func (g *ParagraphGate) release(all bool) [][]byte {
	var out [][]byte
	n := 0
	for _, f := range g.pending {
		if !all {
			ready := true
			for _, d := range f.deltas {
				if d.end > g.channels[d.channel].cleared {
					ready = false
					break
				}
			}
			if !ready {
				break
			}
		}
		out = append(out, f.raw)
		n++
	}
	g.pending = g.pending[n:]
	return out
}

// extract records a frame's judged text and returns the channels it grew.
func (g *ParagraphGate) extract(f *gateFrame) []string {
	payload, _, _, ok := splitSSE(f.raw)
	if !ok {
		return nil
	}
	var chunk struct {
		Choices []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return nil
	}
	var touched []string
	add := func(name, text string) {
		if text == "" {
			return
		}
		ch := g.channels[name]
		if ch == nil {
			ch = &gateChannel{}
			g.channels[name] = ch
			g.order = append(g.order, name)
		}
		ch.buf = append(ch.buf, text...)
		f.deltas = append(f.deltas, gateDelta{channel: name, end: len(ch.buf)})
		touched = append(touched, name)
	}
	for _, c := range chunk.Choices {
		base := "c" + itoa(c.Index)
		if c.Delta.Content != nil {
			add(base, *c.Delta.Content)
		}
		add(base, c.Text)
		if c.Delta.ReasoningContent != nil {
			add(base+":r", *c.Delta.ReasoningContent)
		}
		if c.Delta.Reasoning != nil {
			add(base+":r", *c.Delta.Reasoning)
		}
	}
	return touched
}
