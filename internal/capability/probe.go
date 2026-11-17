package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// A probe measures what a provider's model list does not say, by sending
// the model a handful of tiny chat requests and reading what comes back.
//
// Rules it keeps:
//   - Only what no provider and no administrator has already said is
//     measured.
//   - A value is recorded only on evidence: the reply shows the behavior, or
//     the provider refuses the request and names the parameter. Many servers
//     (vLLM, Ollama, OpenRouter) accept and ignore parameters they do not
//     know, so a 200 alone proves nothing and records nothing.
//   - A request that fails for another reason (rate limit, timeout, 5xx)
//     leaves that capability unknown rather than guessing.

// ProbeRunning marks a probe that has been claimed and is in flight.
const ProbeRunning = "running"

// ProbeQueued marks a model waiting its turn in a "measure all" run.
const ProbeQueued = "queued"

// ProbeQueueTTL is how long a queued claim is honored.
const ProbeQueueTTL = 3 * time.Hour

// ProbeClaimTTL is how long a running claim is honored before another
// attempt may take over (the replica that claimed it may have died).
const ProbeClaimTTL = 6 * time.Minute

// MaxProbeRequests bounds what one probe may send.
const MaxProbeRequests = 30

// Reply is what one probe request came back with, in chat-completions terms.
type Reply struct {
	// Status is the provider's HTTP status.
	Status int
	// Error and Param are the provider's error message and the parameter it
	// names, on a refusal.
	Error string
	Param string
	// Text is the answer; Reasoning is reasoning text returned beside it.
	Text      string
	Reasoning string
	// ReasoningTokens is what the provider says it spent reasoning.
	ReasoningTokens int64
	// ReasoningCounted: the reply's usage accounts for every token the
	// model generated, so ReasoningTokens 0 is a count of none and not the
	// provider saying nothing (Gemini, which hides its thinking).
	ReasoningCounted bool
	// ToolCalls names the functions the model called.
	ToolCalls []string
	// InputTokens and OutputTokens are what the request cost.
	InputTokens  int64
	OutputTokens int64
	// Local: Janus's adapter refused to build the request, so nothing
	// reached the provider. It says what the route can carry, not what
	// the model takes.
	Local bool
}

// Caller sends one chat request body (without the model name) and returns
// the reply. An error means no answer arrived at all.
type Caller func(ctx context.Context, body map[string]any) (Reply, error)

// ProbeInput is what a probe needs to know before it starts.
type ProbeInput struct {
	// Known is what the provider and the administrator already said (the
	// state with any earlier probe left out).
	Known Profile
	// AdapterType picks which thinking switches are worth trying.
	AdapterType string
	// By is who asked: "enabled", "manual" or "all".
	By  string
	Now func() time.Time
}

// EffortLadder is every reasoning-effort value in use, least to most.
var EffortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

const (
	probeQuestion = "What is 17 + 26? Reply with the number only."
	probeToolName = "janus_probe_time"
	// A 64x64 solid green PNG.
	probeImage = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAATElEQVR42u3PQQkAAAgAseufwqhG8C0MVmA1/SYgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgcFlT0gCXrcLQywAAAABJRU5ErkJggg=="
)

func (r Reply) ok() bool { return r.Status/100 == 2 }

// reasoned reports whether the reply shows the model reasoning.
func (r Reply) reasoned() bool {
	return r.ReasoningTokens > 0 || strings.TrimSpace(r.Reasoning) != "" || strings.Contains(r.Text, "<think>")
}

// refuses reports whether the provider refused the request and named one of
// the given parameters or words.
func (r Reply) refuses(words ...string) bool {
	if r.Status != 400 && r.Status != 422 && r.Status != 415 {
		return false
	}
	text := strings.ToLower(r.Param + " " + r.Error)
	for _, w := range words {
		if strings.Contains(text, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func (r Reply) quote() string {
	msg := strings.Join(strings.Fields(r.Error), " ")
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", r.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", r.Status, msg)
}

type prober struct {
	ctx    context.Context
	call   Caller
	in     ProbeInput
	facts  Facts
	run    ProbeRun
	capKey string
	// quiet is merged into content probes so a reasoning model does not
	// spend the whole output cap thinking.
	quiet map[string]any
	// levels are the reasoning_effort values the model takes.
	levels []string
}

func (p *prober) known(key string) bool {
	_, ok := p.in.Known.resolved[key]
	return ok
}

func (p *prober) set(key string, value any, note string) {
	if p.known(key) {
		return
	}
	p.measured(key, value, note)
}

// measured records a value even where the record already names one: for
// what had to be tried through this route to be relied on.
func (p *prober) measured(key string, value any, note string) {
	n, err := Normalize(key, value)
	if err != nil {
		return
	}
	if len(note) > 240 {
		note = note[:240] + "…"
	}
	p.facts[key] = Fact{Value: n, At: p.in.Now(), Note: note}
}

// send makes one request. sent is false when the budget is spent or no
// answer arrived; the caller then leaves its capability unknown.
func (p *prober) send(body map[string]any) (r Reply, sent bool) {
	if p.run.Requests >= MaxProbeRequests || p.ctx.Err() != nil {
		return Reply{}, false
	}
	ctx, cancel := context.WithTimeout(p.ctx, 60*time.Second)
	defer cancel()
	p.run.Requests++
	r, err := p.call(ctx, body)
	p.run.InputTokens += r.InputTokens
	p.run.OutputTokens += r.OutputTokens
	if err != nil {
		return Reply{}, false
	}
	return r, true
}

func (p *prober) body(content any, cap int, extra map[string]any) map[string]any {
	b := map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}, p.capKey: cap}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (p *prober) quietBody(content any, cap int, extra map[string]any) map[string]any {
	b := p.body(content, cap, extra)
	for k, v := range p.quiet {
		if _, taken := b[k]; !taken {
			b[k] = v
		}
	}
	return b
}

// RunProbe measures one model. It returns the facts it could establish and
// the summary of the run; on ProbeFailed the facts are empty.
func RunProbe(ctx context.Context, call Caller, in ProbeInput) (Facts, ProbeRun) {
	if in.Now == nil {
		in.Now = func() time.Time { return time.Now().UTC() }
	}
	p := &prober{ctx: ctx, call: call, in: in, facts: Facts{}, capKey: adapter.OutputCapMaxTokens}
	p.run = ProbeRun{At: in.Now(), By: in.By, Status: ProbeOK}
	if v := in.Known.Text(adapter.CapOutputCap); v != "" {
		p.capKey = v
	}

	base, ok := p.baseline()
	if !ok {
		p.run.Status = ProbeFailed
		return Facts{}, p.run
	}
	levels, levelsKnown := p.effortLevels(base)
	p.levels = levels
	if in.AdapterType == "gemini" {
		p.geminiThinkingOff(base, levels)
	} else {
		p.thinkingOff(base, levels, levelsKnown)
	}
	p.vision()
	p.sound()
	p.video()
	p.tools()
	p.sampling()
	p.structured()
	if p.run.Requests >= MaxProbeRequests {
		p.run.Detail = "stopped at the request limit; some capabilities were not measured"
	}
	return p.facts, p.run
}

// baseline asks a plain question. It settles which parameter limits output
// and whether the model reasons when nothing is said about reasoning.
func (p *prober) baseline() (Reply, bool) {
	r, sent := p.send(p.body(probeQuestion, 256, nil))
	if !sent {
		p.run.Detail = "the model did not answer"
		return r, false
	}
	if !r.ok() && r.refuses("max_tokens", "max_completion_tokens") && !p.known(adapter.CapOutputCap) {
		refusal := r.quote()
		other := adapter.OutputCapMaxCompletionTokens
		if p.capKey == other {
			other = adapter.OutputCapMaxTokens
		}
		p.capKey = other
		if r, sent = p.send(p.body(probeQuestion, 256, nil)); !sent {
			p.run.Detail = "the model did not answer"
			return r, false
		}
		if r.ok() {
			p.set(adapter.CapOutputCap, p.capKey, "the other parameter was refused ("+refusal+")")
		}
	} else if r.ok() {
		p.set(adapter.CapOutputCap, p.capKey, "a request limited with "+p.capKey+" was answered")
	}
	if !r.ok() {
		p.run.Detail = "a plain request was refused (" + r.quote() + ")"
		return r, false
	}
	if known, said := p.in.Known.Bool(adapter.CapThinking); said && !known {
		return r, true
	}
	// A reply without reasoning proves nothing: models that reason by
	// default decide per question, and this one is easy.
	if r.reasoned() {
		p.set(adapter.CapThinking, true, "a plain reply came with reasoning")
		p.set(adapter.CapThinkingDefault, true, "it reasoned when the request said nothing about reasoning")
	}
	return r, true
}

var reLadderWord = regexp.MustCompile(`\b(none|minimal|low|medium|high|xhigh|max)\b`)

func sortLadder(levels []string) []string {
	rank := map[string]int{}
	for i, l := range EffortLadder {
		rank[l] = i
	}
	out := append([]string{}, levels...)
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i]] < rank[out[j]] })
	return out
}

// effortLevels finds the reasoning_effort values the model accepts. A value
// that cannot exist is sent first: a server that answers it does not check
// the parameter, and then acceptance of a real value means nothing.
func (p *prober) effortLevels(base Reply) (levels []string, known bool) {
	if l, ok := p.in.Known.List(adapter.CapThinkingLevels); ok {
		return l, true
	}
	if v, said := p.in.Known.Bool(adapter.CapThinking); said && !v {
		return nil, false
	}
	canary, sent := p.send(p.body(probeQuestion, 64, map[string]any{"reasoning_effort": "janus-probe"}))
	if !sent || canary.ok() || (canary.Status != 400 && canary.Status != 422) {
		// Not checked by the server, or no usable answer: unknown.
		return nil, false
	}
	note := ""
	// Many providers answer the impossible value with the list they take.
	seen := map[string]bool{}
	for _, w := range reLadderWord.FindAllString(strings.ToLower(canary.Error), -1) {
		if !seen[w] {
			seen[w] = true
			levels = append(levels, w)
		}
	}
	switch {
	case len(levels) >= 2 && p.in.AdapterType == "gemini":
		// Google's OpenAI-compatible route lists the values the route
		// takes, the same for every model; a model then refuses a level it
		// does not have ("Thinking level MINIMAL is not supported for this
		// model"). So each listed value is sent and a refusal is recorded.
		listed := sortLadder(levels)
		levels = nil
		var refused []string
		for _, level := range listed {
			r, sent := p.send(p.body(probeQuestion, 64, map[string]any{"reasoning_effort": level}))
			switch {
			case !sent:
				return nil, false
			case r.ok():
				levels = append(levels, level)
			case r.refuses("reasoning", "thinking", "level", "effort", "budget"):
				refused = append(refused, level+": "+r.quote())
			default:
				// Refused for some other reason, or failing: no partial list.
				return nil, false
			}
		}
		note = "each value the provider listed (" + strings.Join(listed, ", ") + ") was sent and answered"
		if len(refused) > 0 {
			note = "each value the provider listed was sent; refused " + strings.Join(refused, "; ")
		}
	case len(levels) >= 2:
		note = "the provider listed them when refusing an invalid value (" + canary.quote() + ")"
	default:
		levels = nil
		for _, level := range EffortLadder {
			r, sent := p.send(p.body(probeQuestion, 64, map[string]any{"reasoning_effort": level}))
			switch {
			case !sent:
				return nil, false
			case r.ok():
				levels = append(levels, level)
			case r.Status == 400 || r.Status == 422:
			default:
				// Rate limited or failing: do not record a partial list.
				return nil, false
			}
		}
		note = "each value was sent; the others were refused"
		if len(levels) == 0 {
			note = "every reasoning_effort value was refused (" + canary.quote() + ")"
		}
	}
	levels = sortLadder(levels)
	p.set(adapter.CapThinkingLevels, levels, note)
	real := 0
	for _, l := range levels {
		if l != "none" {
			real++
		}
	}
	switch {
	case real > 0:
		p.set(adapter.CapThinking, true, "it accepts reasoning effort levels")
	case !base.reasoned():
		p.set(adapter.CapThinking, false, "it takes no reasoning effort and a plain reply came with no reasoning")
	}
	return levels, true
}

type thinkingSwitch struct {
	name string
	body map[string]any
}

func offSwitches(adapterType string, levels []string, levelsKnown bool) []thinkingSwitch {
	effortNone := thinkingSwitch{adapter.ThinkingOffEffortNone, map[string]any{"reasoning_effort": "none"}}
	enable := thinkingSwitch{adapter.ThinkingOffEnableThinking, map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}}
	think := thinkingSwitch{adapter.ThinkingOffThink, map[string]any{"think": false}}
	disabled := thinkingSwitch{adapter.ThinkingOffDisabled, map[string]any{"thinking": map[string]any{"type": "disabled"}}}
	reasoning := thinkingSwitch{adapter.ThinkingOffReasoningFalse, map[string]any{"reasoning": map[string]any{"enabled": false}}}
	var out []thinkingSwitch
	// reasoning_effort "none" is tried unless the model is known to refuse it.
	takesNone := !levelsKnown
	for _, l := range levels {
		takesNone = takesNone || l == "none"
	}
	if takesNone {
		out = append(out, effortNone)
	}
	switch adapterType {
	case "ollama":
		out = append(out, think)
	case "anthropic":
		out = append(out, disabled)
	case "vlm", "llama_cpp":
		out = append(out, enable)
	case "openai_compatible":
		out = append(out, enable, disabled, reasoning)
	}
	return out
}

// thinkingOff finds the switch that stops a model reasoning, judged by the
// reply and not by the request being accepted. When the model does not
// reason by default it looks for the switch that turns reasoning on.
func (p *prober) thinkingOff(base Reply, levels []string, levelsKnown bool) {
	if v, said := p.in.Known.Bool(adapter.CapThinking); said && !v {
		return
	}
	if off := p.in.Known.Text(adapter.CapThinkingOff); off != "" {
		// A named switch is tried, not trusted: what a provider's list says
		// about its own API need not hold for the route Janus calls it by.
		var named *thinkingSwitch
		for _, adapterType := range []string{"openai_compatible", "ollama"} {
			for _, sw := range offSwitches(adapterType, nil, false) {
				if sw.name == off {
					named = &sw
				}
			}
		}
		if named == nil || !base.reasoned() {
			// "never", or nothing to turn off on this question.
			p.quietFromLevels(levels)
			return
		}
		if r, sent := p.send(p.body(probeQuestion, 256, named.body)); !sent || !r.ok() || !r.reasoned() {
			if sent && r.ok() {
				p.measured(adapter.CapThinkingOff, named.name, "with it the reply came with no reasoning")
				p.quiet = named.body
			} else {
				p.quietFromLevels(levels)
			}
			return
		}
		// It did not stop the model reasoning: look for the one that does.
	}
	if !base.reasoned() {
		if p.offByEffortNone(levels) {
			p.quiet = map[string]any{"reasoning_effort": "none"}
			return
		}
		p.thinkingOn()
		p.quietFromLevels(levels)
		return
	}
	var tried []string
	conclusive := true
	for _, sw := range offSwitches(p.in.AdapterType, levels, levelsKnown) {
		r, sent := p.send(p.body(probeQuestion, 256, sw.body))
		switch {
		case !sent:
			conclusive = false
		case r.ok() && !r.reasoned():
			p.measured(adapter.CapThinkingOff, sw.name, "with it the reply came with no reasoning")
			p.quiet = sw.body
			return
		case r.ok() || r.Status == 400 || r.Status == 422:
			tried = append(tried, sw.name)
		default:
			conclusive = false
		}
	}
	if conclusive && len(tried) > 0 {
		p.measured(adapter.CapThinkingOff, adapter.ThinkingOffNever, "it still reasoned, or refused, with each of: "+strings.Join(tried, ", "))
	}
	p.quietFromLevels(levels)
}

// geminiThinkQuestion makes a Gemini model think even at its lowest level,
// so a reply with no thinking says the switch worked and not that the
// question was too easy (gemini-3.8-flash skips thinking on 17 + 26 at
// thinkingBudget 0 but not on this).
const geminiThinkQuestion = "What is 37 × 43? Reply with the number only."

// geminiThinkingOff measures whether a Gemini model's thinking can be
// turned off. Janus has one way to ask, reasoning_effort "none": Google's
// OpenAI-compatible route (plain chat requests) and Janus's native adapter
// (requests with Google-run tools, which is how Chat calls Gemini) both
// turn it into thinkingBudget 0. Some models then think not at all, others
// at their lowest level, and some refuse a budget of 0. Both routes are
// asked; Gemini hides its thinking, so a reply counts as without thinking
// only when its usage shows zero thinking tokens (ReasoningCounted).
//   - It thinks, or the budget is refused, on either route: "never".
//   - Zero on both routes, and the model shown to think when not asked
//     to stop: thinking_budget_zero.
//   - Anything else leaves it unknown.
func (p *prober) geminiThinkingOff(base Reply, levels []string) {
	if v, said := p.in.Known.Bool(adapter.CapThinking); said && !v {
		return
	}
	if p.in.Known.Text(adapter.CapThinkingOff) == adapter.ThinkingOffNever {
		p.quietFromLevels(levels)
		return
	}
	off := map[string]any{"reasoning_effort": "none"}
	routes := []struct {
		name string
		body map[string]any
	}{
		{"the OpenAI-compatible route", p.body(geminiThinkQuestion, 256, off)},
		// A Google-run tool sends the request to the native API, as Chat's
		// requests go; this question gives the model nothing to fetch.
		{"the native route", p.body(geminiThinkQuestion, 256, map[string]any{"reasoning_effort": "none", "tools": []any{map[string]any{"type": "web_fetch"}}})},
	}
	var silent []string
	for _, route := range routes {
		r, sent := p.send(route.body)
		switch {
		case !sent:
		case r.ok() && r.reasoned():
			p.measured(adapter.CapThinkingOff, adapter.ThinkingOffNever,
				fmt.Sprintf("with thinkingBudget 0 it still thought on %s (%d thinking tokens)", route.name, r.ReasoningTokens))
			p.quietFromLevels(levels)
			return
		case r.refuses("budget", "thinking") && !r.refuses("url_context", "tool"):
			p.measured(adapter.CapThinkingOff, adapter.ThinkingOffNever, "thinkingBudget 0 was refused on "+route.name+" ("+r.quote()+")")
			p.quietFromLevels(levels)
			return
		case r.ok() && r.ReasoningCounted:
			silent = append(silent, route.name)
		}
	}
	if len(silent) != len(routes) {
		p.quietFromLevels(levels)
		return
	}
	// No thinking with the switch means something only if the model thinks
	// without it: by default on the plain question, or at its top level.
	thinks := base.reasoned()
	how := "by default"
	if !thinks {
		extra := map[string]any{}
		how = "by default on the same question"
		if top := topLevel(levels); top != "" {
			extra["reasoning_effort"], how = top, "at "+top+" on the same question"
		}
		r, sent := p.send(p.body(geminiThinkQuestion, 256, extra))
		thinks = sent && r.ok() && r.reasoned()
	}
	if !thinks {
		p.quietFromLevels(levels)
		return
	}
	p.set(adapter.CapThinking, true, "it thought "+how)
	p.measured(adapter.CapThinkingOff, adapter.ThinkingOffBudgetZero,
		"it thought "+how+" and spent 0 thinking tokens with thinkingBudget 0 on "+strings.Join(silent, " and "))
	p.quiet = off
}

// topLevel is the highest effort in levels other than "none", or "".
func topLevel(levels []string) string {
	sorted := sortLadder(levels)
	for i := len(sorted) - 1; i >= 0; i-- {
		if sorted[i] != "none" {
			return sorted[i]
		}
	}
	return ""
}

// offByEffortNone covers a model that takes reasoning_effort "none" but did
// not reason on the plain question: it is asked to reason at its highest
// level, and if it does, "none" must then stop it.
func (p *prober) offByEffortNone(levels []string) bool {
	levels = sortLadder(levels)
	if len(levels) < 2 || levels[0] != "none" {
		return false
	}
	top := levels[len(levels)-1]
	high, sent := p.send(p.body(probeQuestion, 256, map[string]any{"reasoning_effort": top}))
	if !sent || !high.ok() || !high.reasoned() {
		return false
	}
	none, sent := p.send(p.body(probeQuestion, 256, map[string]any{"reasoning_effort": "none"}))
	if !sent || !none.ok() || none.reasoned() {
		return false
	}
	p.set(adapter.CapThinkingOff, adapter.ThinkingOffEffortNone, "it reasoned at "+top+" and not at none")
	return true
}

// quietFromLevels asks for the least reasoning the model accepts when it
// cannot be turned off.
func (p *prober) quietFromLevels(levels []string) {
	for _, l := range sortLadder(levels) {
		if l != "none" {
			p.quiet = map[string]any{"reasoning_effort": l}
			return
		}
	}
}

// thinkingOn looks for reasoning that is off by default: self-hosted
// servers switch it per request.
func (p *prober) thinkingOn() {
	if p.known(adapter.CapThinking) {
		return
	}
	if _, found := p.facts[adapter.CapThinking]; found {
		return
	}
	var on map[string]any
	var off string
	switch p.in.AdapterType {
	case "ollama":
		on, off = map[string]any{"think": true}, adapter.ThinkingOffThink
	case "vlm", "llama_cpp", "openai_compatible":
		on, off = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}, adapter.ThinkingOffEnableThinking
	default:
		return
	}
	r, sent := p.send(p.body(probeQuestion, 256, on))
	if sent && r.ok() && r.reasoned() {
		p.set(adapter.CapThinking, true, "it reasoned when asked to")
		p.set(adapter.CapThinkingOff, off, "it reasons only when the request turns reasoning on")
	}
}

// vision sends a small green image and asks for its color.
func (p *prober) vision() {
	if p.known(adapter.CapInImage) {
		return
	}
	content := []any{
		map[string]any{"type": "text", "text": "What single color fills this image? Reply with one word."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": probeImage}},
	}
	r, sent := p.send(p.quietBody(content, 256, nil))
	switch {
	case !sent:
	case r.ok() && strings.Contains(strings.ToLower(r.Text), "green"):
		p.set(adapter.CapInImage, true, "it named the color of a test image")
	case r.refuses("image", "vision", "multimodal", "multi-modal", "modalit", "content"):
		p.set(adapter.CapInImage, false, "a request with an image was refused ("+r.quote()+")")
	}
}

// sound sends a short spoken number the way Chat sends attached sound and
// asks which number it is. Only the right number is a yes; a refusal that
// names sound is a no; a wrong number, a reply that hears nothing, or a
// refusal Janus itself made (the route has no way to send sound) records
// nothing.
func (p *prober) sound() {
	if p.known(adapter.CapInAudio) || !adapter.CarriesSoundAndVideo(p.in.AdapterType) {
		return
	}
	content := []any{
		map[string]any{"type": "text", "text": "What number is spoken in this sound clip? Reply with the number only."},
		probeSoundPart(),
	}
	r, sent := p.send(p.mediaBody(content))
	switch {
	case !sent, r.Local:
	case r.ok():
		if n, ok := answerNumber(r.Text); ok && n == probeSpokenNumber {
			p.set(adapter.CapInAudio, true, "it named the number spoken in a test clip")
		}
	case r.refuses("audio", "sound", "modalit", "multimodal", "multi-modal", "mime",
		// OpenAI chat models that take only text and images say so
		// without naming sound ("Content blocks are expected to be either
		// text or image_url type.").
		"content blocks are expected"):
		p.set(adapter.CapInAudio, false, "a request with sound was refused ("+r.quote()+")")
	}
}

// video sends a one-second clip of a single color the way Chat sends
// attached video and asks for the color, with the same evidence rules as
// sound.
func (p *prober) video() {
	if p.known(adapter.CapInVideo) || !adapter.CarriesSoundAndVideo(p.in.AdapterType) {
		return
	}
	content := []any{
		map[string]any{"type": "text", "text": "What single color fills this video? Reply with one word."},
		probeVideoPart(),
	}
	r, sent := p.send(p.mediaBody(content))
	switch {
	case !sent, r.Local:
	case r.ok():
		if namesColor(r.Text, probeVideoColor) {
			p.set(adapter.CapInVideo, true, "it named the color of a test video")
		}
	case r.refuses("video", "modalit", "multimodal", "multi-modal", "mime"):
		p.set(adapter.CapInVideo, false, "a request with a video was refused ("+r.quote()+")")
	}
}

// mediaBody is a sound or video question, asked with the least thinking.
// The question is about perceiving, not reasoning, and on Gemini thinking
// made the answer wrong: gemini-2.5-flash, asked at its lowest level
// (thinkingBudget 1,024) or by default, named the test number as "41",
// "451", "461"; at thinkingBudget 0 it said "47" every time. So a Gemini
// model whose levels hold "none" is asked with none (the adapter sends
// thinkingBudget 0 on the native route these parts take), even where none
// was not shown to stop its thinking; others get the usual quiet setting.
func (p *prober) mediaBody(content []any) map[string]any {
	b := p.quietBody(content, 256, nil)
	if p.in.AdapterType == "gemini" && slices.Contains(p.levels, "none") {
		b["reasoning_effort"] = "none"
	}
	return b
}

// tools offers one function that takes no arguments and asks for it.
func (p *prober) tools() {
	if p.known(adapter.CapTools) {
		return
	}
	extra := map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{
		"name": probeToolName, "description": "Returns the current time.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}}
	r, sent := p.send(p.body("Call the "+probeToolName+" tool now.", 512, extra))
	switch {
	case !sent:
	case r.ok() && contains(r.ToolCalls, probeToolName):
		p.set(adapter.CapTools, true, "it called a test function")
	case r.refuses("/v1/responses", "responses api"):
		// The provider says where tools do work for this model.
		p.set(adapter.CapTools, true, "the provider takes its tools on the Responses API ("+r.quote()+")")
		p.set(adapter.CapToolsEndpoint, adapter.ToolsEndpointResponses, "chat completions refused tools and named the Responses API ("+r.quote()+")")
	case r.refuses("reasoning_effort", "reasoning"):
		// Refused because of reasoning, not because of tools: no verdict.
	case r.refuses("tool", "function"):
		p.set(adapter.CapTools, false, "a request offering a function was refused ("+r.quote()+")")
	}
}

// SamplingParams are the sampling settings a provider may refuse outright;
// the same list the proxy may leave out of a request.
var SamplingParams = []string{"temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty"}

var samplingProbeValue = map[string]any{"temperature": 0.7, "top_p": 0.9, "top_k": 40, "frequency_penalty": 0.1, "presence_penalty": 0.1}

// sampling sends each sampling setting on its own.
func (p *prober) sampling() {
	if p.known(adapter.CapRefusedParams) {
		return
	}
	refused := []string{}
	var notes []string
	for _, param := range SamplingParams {
		r, sent := p.send(p.quietBody(probeQuestion, 64, map[string]any{param: samplingProbeValue[param]}))
		switch {
		case !sent:
			return
		case r.ok():
		case r.refuses(param):
			refused = append(refused, param)
			notes = append(notes, r.quote())
		default:
			return
		}
	}
	note := "each of " + strings.Join(SamplingParams, ", ") + " was accepted"
	if len(notes) > 0 {
		note = strings.Join(notes, "; ")
	}
	p.set(adapter.CapRefusedParams, refused, note)
}

// structured asks for an answer that fits a JSON schema.
func (p *prober) structured() {
	if p.known(adapter.CapStructured) {
		return
	}
	extra := map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "janus_probe", "strict": true,
		"schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []any{"ok"}, "additionalProperties": false}}}}
	r, sent := p.send(p.quietBody("Set ok to true.", 256, extra))
	switch {
	case !sent:
	case r.ok() && fitsProbeSchema(r.Text):
		p.set(adapter.CapStructured, true, "the reply fitted the schema it was given")
	case r.refuses("response_format", "json_schema", "schema", "structured"):
		p.set(adapter.CapStructured, false, "a request with a JSON schema was refused ("+r.quote()+")")
	}
}

func fitsProbeSchema(text string) bool {
	text = strings.TrimSpace(text)
	var v map[string]any
	if json.Unmarshal([]byte(text), &v) != nil || len(v) != 1 {
		return false
	}
	_, ok := v["ok"].(bool)
	return ok
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Claim marks a probe as started. It reports false when another probe of
// this model is already in flight.
func (s *State) Claim(by string, now time.Time) bool {
	if s.inFlight(now) {
		return false
	}
	s.Probed = &ProbeRun{At: now, Status: ProbeRunning, By: by}
	return true
}

// Queue is Claim for a model that waits its turn.
func (s *State) Queue(by string, now time.Time) bool {
	if !s.Claim(by, now) {
		return false
	}
	s.Probed.Status = ProbeQueued
	return true
}

// Begin turns a queued claim into a running one.
func (s *State) Begin(by string, now time.Time) {
	s.Probed = &ProbeRun{At: now, Status: ProbeRunning, By: by}
}

func (s *State) inFlight(now time.Time) bool {
	if s.Probed == nil {
		return false
	}
	switch s.Probed.Status {
	case ProbeRunning:
		return now.Sub(s.Probed.At) < ProbeClaimTTL
	case ProbeQueued:
		return now.Sub(s.Probed.At) < ProbeQueueTTL
	}
	return false
}

// Finish records a probe's outcome. A probe that learned nothing (failed or
// skipped) keeps what an earlier probe measured.
func (s *State) Finish(facts Facts, run ProbeRun) {
	s.Probed = &run
	if run.Status != ProbeOK {
		return
	}
	if len(facts) == 0 {
		facts = nil
	}
	s.Probe = facts
}

// WithoutProbe is the state as the provider and the administrator describe
// it, which is what a new probe starts from.
func (s State) WithoutProbe() State {
	s.Probe = nil
	return s
}

// Settle turns a claim nobody finished (the replica running it stopped)
// into a failed run, so the model does not read as being measured forever.
func (s *State) Settle(now time.Time) {
	if s.Probed != nil && (s.Probed.Status == ProbeRunning || s.Probed.Status == ProbeQueued) && !s.inFlight(now) {
		run := *s.Probed
		run.Status, run.Detail = ProbeFailed, "the measurement was interrupted before it finished"
		s.Probed = &run
	}
}
