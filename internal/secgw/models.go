package secgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/detect"
	"github.com/torvanis/janus/internal/store"
)

// Model-backed ingress checks (prompt injection, content safety).
//
// Every model-backed check that applies to a request runs at the same
// time, so a request waits for the slowest guard, not the sum of them.
// Checks bound to the same decision model are asked together: one request
// when the model takes combined questions, one request per question in
// parallel when it does not (see classify.DefaultStyle). Another feature
// (Usage insights) can add its own questions to a decision call that is
// being made anyway (Request.Extra), so a label costs no extra round trip.

// Extra is another feature's question set offered to the guard stage.
type Extra struct {
	// ClassifierKey is the decision classifier the questions are for
	// ("upstream_id/model"); they ride along only on a call to it.
	ClassifierKey string
	// Text is the text the questions are about. They share a call only
	// when it is the same text the guards are judging.
	Text      string
	Questions []dec.Question
	// Feature labels the questions in traces.
	Feature string
	// AnyStyle lets the questions ride on a classifier that takes
	// questions separately too (one request per question, in parallel).
	// Set by features that must be answered before forwarding anyway
	// (auto-routing): asking alongside the guards is no slower than asking
	// after them. Reporting-only features leave it false so they never
	// add GPU work while a user waits.
	AnyStyle bool
	// Timeout is the most these questions may wait. They ride only on a
	// call that waits at least that long anyway (never extending a guard's
	// wait), and their answer counts as timed out when it arrives later.
	// Zero: whatever the guards wait.
	Timeout time.Duration

	// Filled by the engine when the questions rode along.
	Ran    bool
	Result dec.Result
	Err    error
	// Elapsed is the shared call's wall time.
	Elapsed time.Duration
}

// DecisionThresholdDefault is the injection threshold for decision models
// when the check sets none. Their yes-probabilities are calibrated
// differently from Prompt Guard's label scores: at 0.9 they miss most
// attacks, while around 0.3 they catch most with few false alarms on
// ordinary work requests.
const DecisionThresholdDefault = 0.3

// HarmThresholdDefault is the probability of "unsafe" at which a decision
// model's harm answer counts.
const HarmThresholdDefault = 0.5

// guardTextMaxChars bounds the text sent to a decision model in one call
// for a guard check. With the default long-text option ("tail") the most
// recent text is kept; with "windows" the whole text is judged in windows
// of this size.
const guardTextMaxChars = 12000

// guardWindowOverlap is how far consecutive windows overlap, so an attack
// straddling a boundary is seen whole by at least one of them.
const guardWindowOverlap = 1000

// guardMaxWindows caps the calls one check makes for one request in
// "windows" mode (about 180,000 characters). Longer text is not judged in
// full, so the check counts as failed and its fail stance applies, rather
// than passing text nobody read.
const guardMaxWindows = 16

var errNoResolver = errors.New("no classifier resolver configured")

// errTextTooLong reports text longer than guardMaxWindows windows.
var errTextTooLong = fmt.Errorf("text is longer than %d windows of %d characters; it was not judged in full", guardMaxWindows, guardTextMaxChars)

// decisionClassifier is what the engine needs from a resolved decision
// model (classify.Breaker implements it).
type decisionClassifier interface {
	Decide(ctx context.Context, text string, questions []dec.Question) (dec.Result, error)
	Key() string
	Style() string
	IsDecision() bool
}

// GuardCoverage is what a decision-model guard check reads: which messages
// (judge_scope) and how text longer than one call takes is handled
// (long_text). See store.SecgwJudge* and store.SecgwLongText*.
type GuardCoverage struct {
	JudgeScope string `json:"judge_scope,omitempty"`
	LongText   string `json:"long_text,omitempty"`
}

// coverageOf reads a check's coverage options; unset means the defaults
// (new turn, last 12,000 characters).
func coverageOf(c ResolvedCheck) GuardCoverage {
	var o GuardCoverage
	if len(c.Options) > 0 {
		_ = json.Unmarshal(c.Options, &o)
	}
	return o
}

// NewTurnText is GuardText for a raw request body: the text of the new turn
// that decision models judge. Empty when the body is not a chat request.
func NewTurnText(body []byte) string {
	msgs, ok := parseMessages(body)
	if !ok {
		return ""
	}
	return GuardText(msgs)
}

// GuardText is the text decision models judge for ingress checks with the
// default coverage: the new turn (everything after the last assistant
// reply), its last 12,000 characters. Other features ask about this same
// text so they can share the guard's call.
func GuardText(msgs []Message) string {
	return dec.TrimText(scopeText(msgs, store.SecgwJudgeNewTurn), guardTextMaxChars)
}

func isAssistant(role string) bool { return strings.HasPrefix(role, "assistant") }

// guardTurnStart is where the new turn begins for decision-model guards:
// after the last assistant reply, ignoring trailing assistant messages. A
// request ending in an assistant prefill ("Sure, here it is:") has no text
// after its last assistant message; without this the guard judged nothing
// at all and the request passed unjudged, unrecorded.
func guardTurnStart(msgs []Message) int {
	end := len(msgs)
	for end > 0 && isAssistant(msgs[end-1].Role) {
		end--
	}
	return newTurnStart(msgs[:end])
}

// scopeText joins the non-assistant messages a check judges: the new turn,
// or the whole conversation the caller sent.
func scopeText(msgs []Message, scope string) string {
	from := guardTurnStart(msgs)
	if scope == store.SecgwJudgeConversation {
		from = 0
	}
	var parts []string
	for _, m := range msgs {
		if m.Index < from || isAssistant(m.Role) {
			continue
		}
		if t := strings.TrimSpace(m.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// guardTexts is what one check sends to its decision model: one text by
// default (the tail), or every window of the text in "windows" mode.
func guardTexts(msgs []Message, cov GuardCoverage) ([]string, error) {
	full := scopeText(msgs, cov.JudgeScope)
	if strings.TrimSpace(full) == "" {
		return nil, nil
	}
	if cov.LongText != store.SecgwLongTextWindows {
		return []string{dec.TrimText(full, guardTextMaxChars)}, nil
	}
	windows := classify.Chunk(full, guardTextMaxChars, guardWindowOverlap)
	if len(windows) > guardMaxWindows {
		return nil, errTextTooLong
	}
	return windows, nil
}

// ModelStats reports what the model-backed checks cost one request.
type ModelStats struct {
	// Elapsed is the wall time of the model-backed stage (checks run in
	// parallel, so this is the slowest one, not the sum).
	Elapsed time.Duration
	// Calls is the number of classifier requests made.
	Calls int
	// Failed is the number of checks whose classifier failed (error,
	// timeout, open breaker), whatever their fail stance did about it.
	Failed int
}

type checkOutcome struct {
	vs     []Violation
	failed bool
	err    error
	ran    bool
}

// decisionGroup is one call to a decision model: every question bound for
// the same model about the same text.
type decisionGroup struct {
	cl        decisionClassifier
	text      string
	questions []dec.Question
	// timeout is the longest of the guard checks' timeouts in the group;
	// each member still judges its answer against its own timeout.
	timeout  time.Duration
	features []string
	byID     map[string]string
	extras   []*Extra
	res      dec.Result
	err      error
	elapsed  time.Duration
}

// memberResult is one group's outcome as seen by one member: the call's
// error, or a timeout when the answer came after the member's own deadline
// (a longer-waiting member kept the call open).
func (g *decisionGroup) memberResult(timeout time.Duration) (dec.Result, error) {
	if g.err != nil {
		return g.res, g.err
	}
	if timeout > 0 && g.elapsed > timeout {
		return dec.Result{}, fmt.Errorf("classifier answered after this check's %v timeout: %w", timeout, context.DeadlineExceeded)
	}
	return g.res, nil
}

// decisionCheck is one guard check's share of the decision calls: one group
// per text it judges (one, or one per window).
type decisionCheck struct {
	groups  []*decisionGroup
	timeout time.Duration
}

// runModelChecks runs the applicable model-backed ingress checks in
// parallel.
func (e *Engine) runModelChecks(ctx context.Context, eff *Effective, msgs []Message, parsed bool, extras []*Extra) (inj, safe checkOutcome, stats ModelStats) {
	injC, hasInj := eff.Has(store.SecgwCheckPromptInjection, store.SecgwDirectionIngress)
	safeC, hasSafe := eff.Has(store.SecgwCheckContentSafety, store.SecgwDirectionIngress)
	hasSafe = hasSafe && parsed
	if !hasInj && !hasSafe {
		return
	}
	started := time.Now()
	defer func() { stats.Elapsed = time.Since(started) }()
	if e.classify == nil {
		fail := checkOutcome{failed: true, err: errNoResolver, ran: true}
		if hasInj {
			inj = fail
		}
		if hasSafe {
			safe = fail
		}
		return
	}

	groups := map[string]*decisionGroup{}
	var order []string
	addDecision := func(cl decisionClassifier, text string, q dec.Question, timeout time.Duration, feature string) *decisionGroup {
		key := cl.Key() + "\x00" + text
		g := groups[key]
		if g == nil {
			g = &decisionGroup{cl: cl, text: text, byID: map[string]string{}}
			groups[key] = g
			order = append(order, key)
		}
		g.questions = append(g.questions, q)
		g.features = append(g.features, feature)
		g.byID[q.ID] = feature
		if timeout > g.timeout {
			g.timeout = timeout
		}
		return g
	}
	// planDecision adds a check's question to the group of every text it
	// judges. A failure to plan (text too long to judge in full) is the
	// check's failure.
	planDecision := func(d decisionClassifier, c ResolvedCheck, q dec.Question, timeout time.Duration, kind store.SecgwCheckKind) (*decisionCheck, error) {
		texts, err := guardTexts(msgs, coverageOf(c))
		if err != nil {
			return nil, err
		}
		dc := &decisionCheck{timeout: timeout}
		seen := map[string]bool{}
		for _, t := range texts {
			if seen[t] {
				continue // a repeated window: one question per call
			}
			seen[t] = true
			dc.groups = append(dc.groups, addDecision(d, t, q, timeout, string(kind)))
		}
		return dc, nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	legacyCalls := 0
	var injDec, safeDec *decisionCheck
	if hasInj {
		inj.ran = true
		cl, err := e.classify.For(ctx, injC.ClassifierModelID)
		d, isDec := cl.(decisionClassifier)
		switch {
		case err != nil:
			inj = checkOutcome{failed: true, err: err, ran: true}
		case isDec && d.IsDecision():
			if injDec, err = planDecision(d, injC, dec.InjectionQuestion(), injectionTimeout(injC), store.SecgwCheckPromptInjection); err != nil {
				inj = checkOutcome{failed: true, err: err, ran: true}
			}
		default:
			wg.Add(1)
			go func() {
				defer wg.Done()
				vs, failed, err := e.classifyInjection(dec.WithFeature(ctx, string(store.SecgwCheckPromptInjection), "inline"), injC, msgs)
				mu.Lock()
				inj = checkOutcome{vs: vs, failed: failed, err: err, ran: true}
				legacyCalls++
				mu.Unlock()
			}()
		}
	}
	if hasSafe {
		safe.ran = true
		cl, err := e.classify.For(ctx, safeC.ClassifierModelID)
		d, isDec := cl.(decisionClassifier)
		switch {
		case err != nil:
			safe = checkOutcome{failed: true, err: err, ran: true}
		case isDec && d.IsDecision():
			if safeDec, err = planDecision(d, safeC, dec.HarmQuestion(), safetyTimeout(safeC), store.SecgwCheckContentSafety); err != nil {
				safe = checkOutcome{failed: true, err: err, ran: true}
			}
		default:
			wg.Add(1)
			go func() {
				defer wg.Done()
				vs, failed, err := e.judgeContentSafety(dec.WithFeature(ctx, string(store.SecgwCheckContentSafety), "inline"), safeC, store.SecgwDirectionIngress, msgs)
				mu.Lock()
				safe = checkOutcome{vs: vs, failed: failed, err: err, ran: true}
				legacyCalls++
				mu.Unlock()
			}()
		}
	}
	// Another feature's questions ride along when they are for a decision
	// model already being called, about the same text, the model takes
	// combined questions (so they add no request and no wait), and they
	// may wait as long as the guards do: a rider never makes a guard wait
	// past its own timeout. A rider with a longer budget asks on its own.
	for _, extra := range extras {
		if extra == nil || len(extra.Questions) == 0 {
			continue
		}
		g := groups[extra.ClassifierKey+"\x00"+extra.Text]
		if g == nil || !(extra.AnyStyle || g.cl.Style() == dec.StyleCombined) {
			continue
		}
		if extra.Timeout > g.timeout {
			continue
		}
		// A rider's questions are optional on this call: a bad answer
		// to them (an option that was not offered, a refusal, too many
		// options) must fail only that feature, never the guard.
		for _, q := range extra.Questions {
			q.Optional = true
			g.questions = append(g.questions, q)
		}
		g.features = append(g.features, extra.Feature)
		for _, q := range extra.Questions {
			g.byID[q.ID] = extra.Feature
		}
		g.extras = append(g.extras, extra)
	}
	for _, key := range order {
		g := groups[key]
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Marked as a guard call: only these move the breaker that
			// decides whether the guards on this model are down.
			cctx := classify.WithGuardCall(dec.WithQuestionFeatures(dec.WithFeature(ctx, strings.Join(uniq(g.features), "+"), "inline"), g.byID))
			cctx, cancel := context.WithTimeout(cctx, g.timeout)
			defer cancel()
			began := time.Now()
			g.res, g.err = g.cl.Decide(cctx, g.text, g.questions)
			g.elapsed = time.Since(began)
		}()
	}
	wg.Wait()

	stats.Calls = legacyCalls
	for _, key := range order {
		g := groups[key]
		switch {
		case g.res.Calls > 0:
			stats.Calls += g.res.Calls
		case g.cl.Style() == dec.StyleSeparate:
			stats.Calls += len(g.questions)
		default:
			stats.Calls++
		}
		for _, extra := range g.extras {
			res, err := g.memberResult(extra.Timeout)
			extra.Ran, extra.Result, extra.Err, extra.Elapsed = true, res, err, g.elapsed
			if extra.Err == nil {
				ids := make([]string, len(extra.Questions))
				for i, q := range extra.Questions {
					ids[i] = q.ID
				}
				extra.Err = g.res.MissingErr(ids)
			}
		}
	}
	if injDec != nil {
		inj = e.injectionFromDecision(injC, msgs, injDec)
	}
	if safeDec != nil {
		safe = e.safetyFromDecision(safeC, store.SecgwDirectionIngress, msgs, safeDec)
	}
	for _, o := range []checkOutcome{inj, safe} {
		if o.failed {
			stats.Failed++
		}
	}
	return inj, safe, stats
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func injectionTimeout(c ResolvedCheck) time.Duration {
	t := 250 * time.Millisecond
	var o struct {
		TimeoutMs int `json:"timeout_ms"`
	}
	if len(c.Options) > 0 && json.Unmarshal(c.Options, &o) == nil && o.TimeoutMs > 0 {
		t = time.Duration(o.TimeoutMs) * time.Millisecond
	}
	return t
}

func safetyTimeout(c ResolvedCheck) time.Duration {
	var o ContentSafetyOptions
	if len(c.Options) > 0 {
		_ = json.Unmarshal(c.Options, &o)
	}
	if o.TimeoutMs > 0 {
		return time.Duration(o.TimeoutMs) * time.Millisecond
	}
	return 5 * time.Second
}

// GuardTimeout is how long a model-backed ingress check waits for its
// classifier (its timeout_ms option, else the kind's default).
func GuardTimeout(c ResolvedCheck) time.Duration {
	if c.Kind == store.SecgwCheckContentSafety {
		return safetyTimeout(c)
	}
	return injectionTimeout(c)
}

// CanCarry reports whether a decision-model check's call can carry another
// feature's questions about GuardText that may wait up to timeout: the
// check judges that same text (default coverage) and waits at least as
// long. Callers use it to decide up front whether to ride or ask on their
// own; the engine applies the same rule.
func CanCarry(c ResolvedCheck, timeout time.Duration) bool {
	cov := coverageOf(c)
	if cov.JudgeScope == store.SecgwJudgeConversation {
		return false
	}
	return timeout <= GuardTimeout(c)
}

// lastNewMessage is the message a decision-model verdict is attributed to:
// the last non-assistant message of the new turn, or of the whole
// conversation when the new turn has none.
func lastNewMessage(msgs []Message) (Message, bool) {
	from := guardTurnStart(msgs)
	var fallback *Message
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if isAssistant(m.Role) || strings.TrimSpace(m.Text) == "" {
			continue
		}
		if m.Index >= from {
			return m, true
		}
		if fallback == nil {
			fallback = &msgs[i]
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Message{}, false
}

// answers collects a check's answer from every group (window) it was asked
// in. Any window failing fails the check: part of the text went unjudged.
func (dc *decisionCheck) answers(id string) ([]dec.Answer, error) {
	out := make([]dec.Answer, 0, len(dc.groups))
	for _, g := range dc.groups {
		res, err := g.memberResult(dc.timeout)
		if err != nil {
			return nil, err
		}
		a, ok := res.Answers[id]
		if !ok {
			return nil, dec.ErrNoAnswer
		}
		out = append(out, a)
	}
	return out, nil
}

func (e *Engine) injectionFromDecision(c ResolvedCheck, msgs []Message, dc *decisionCheck) checkOutcome {
	if len(dc.groups) == 0 {
		return checkOutcome{ran: true}
	}
	answers, err := dc.answers(dec.InjectionQuestion().ID)
	if err != nil {
		return checkOutcome{failed: true, err: err, ran: true}
	}
	yes := 0.0
	for _, a := range answers {
		yes = max(yes, a.Yes)
	}
	threshold := DecisionThresholdDefault
	var o struct {
		Threshold float64 `json:"threshold"`
	}
	if len(c.Options) > 0 && json.Unmarshal(c.Options, &o) == nil && o.Threshold > 0 {
		threshold = o.Threshold
	}
	if yes < threshold {
		return checkOutcome{ran: true}
	}
	m, ok := lastNewMessage(msgs)
	if !ok {
		return checkOutcome{ran: true}
	}
	return checkOutcome{ran: true, vs: []Violation{{Kind: c.Kind, RuleID: "INJECTION", Severity: string(detect.SeverityHigh), Direction: store.SecgwDirectionIngress,
		MessageIndex: m.Index, Text: m.Text, Length: len(m.Text), PolicyID: c.PolicyID, BindingID: c.BindingID, Record: true,
		ClassifierScore: yes, ClassifierModel: c.ClassifierModelID}}}
}

// worstHarm is the most unsafe of a check's window answers.
func worstHarm(answers []dec.Answer) (pUnsafe float64, codes []string) {
	for _, a := range answers {
		p, cs := dec.HarmVerdict(a)
		if len(cs) > 0 && (len(codes) == 0 || p > pUnsafe) {
			pUnsafe, codes = p, cs
		}
	}
	return pUnsafe, codes
}

func (e *Engine) safetyFromDecision(c ResolvedCheck, direction string, msgs []Message, dc *decisionCheck) checkOutcome {
	if len(dc.groups) == 0 {
		return checkOutcome{ran: true}
	}
	answers, err := dc.answers(dec.HarmQuestion().ID)
	if err != nil {
		return checkOutcome{failed: true, err: err, ran: true}
	}
	pUnsafe, codes := worstHarm(answers)
	if pUnsafe < HarmThresholdDefault || len(codes) == 0 {
		return checkOutcome{ran: true}
	}
	var opts ContentSafetyOptions
	if len(c.Options) > 0 {
		_ = json.Unmarshal(c.Options, &opts)
	}
	var m Message
	if direction == store.SecgwDirectionEgress {
		// The judged text is the response, appended as the last message.
		if len(msgs) == 0 {
			return checkOutcome{ran: true}
		}
		m = msgs[len(msgs)-1]
	} else {
		var ok bool
		if m, ok = lastNewMessage(msgs); !ok {
			return checkOutcome{ran: true}
		}
	}
	action := store.SecgwActionObserved
	if direction == store.SecgwDirectionIngress && c.Mode == store.SecgwModeBlock && opts.Enforces(codes) {
		action = store.SecgwActionBlocked
	}
	return checkOutcome{ran: true, vs: []Violation{{Kind: c.Kind, RuleID: strings.Join(codes, ","), Severity: string(detect.SeverityHigh),
		Direction: direction, Action: action, MessageIndex: m.Index, Text: m.Text, Length: len(m.Text), Record: true,
		PolicyID: c.PolicyID, BindingID: c.BindingID, ClassifierScore: pUnsafe, ClassifierModel: c.ClassifierModelID}}}
}

// responseHarmQuestion is the harm question asked about an assistant's
// reply rather than a request: same id and options, so verdicts read the
// same on both directions.
func responseHarmQuestion() dec.Question {
	q := dec.HarmQuestion()
	q.Instructions = "Is this reply from an AI assistant safe to show the user? If not, which hazard category does it fall under?"
	return q
}

// egressSafetyFromDecision judges a response with a decision model: the
// harm question about the assistant's reply (in windows when the check's
// long_text option asks for it, else its last 12,000 characters).
// Observe-only like every egress content-safety verdict.
func (e *Engine) egressSafetyFromDecision(ctx context.Context, c ResolvedCheck, d decisionClassifier, msgs []Message) ([]Violation, error) {
	reply := msgs[len(msgs)-1].Text
	texts := []string{dec.TrimText(reply, guardTextMaxChars)}
	if coverageOf(c).LongText == store.SecgwLongTextWindows {
		texts = classify.Chunk(reply, guardTextMaxChars, guardWindowOverlap)
		if len(texts) > guardMaxWindows {
			return nil, errTextTooLong
		}
	}
	timeout := safetyTimeout(c)
	q := responseHarmQuestion()
	dc := &decisionCheck{timeout: timeout}
	var wg sync.WaitGroup
	for _, t := range texts {
		g := &decisionGroup{cl: d, text: t, questions: []dec.Question{q}, timeout: timeout}
		dc.groups = append(dc.groups, g)
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(classify.WithGuardCall(dec.WithFeature(ctx, string(store.SecgwCheckContentSafety), "inline")), timeout)
			defer cancel()
			began := time.Now()
			g.res, g.err = d.Decide(cctx, g.text, g.questions)
			g.elapsed = time.Since(began)
		}()
	}
	wg.Wait()
	o := e.safetyFromDecision(c, store.SecgwDirectionEgress, msgs, dc)
	return o.vs, o.err
}

// compile-time check that the breaker satisfies decisionClassifier.
var _ decisionClassifier = (*classify.Breaker)(nil)
