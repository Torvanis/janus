package secgw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	lru "github.com/hashicorp/golang-lru/v2"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/detect"
	"github.com/torvanis/janus/internal/store"
)

// Engine owns the compiled detectors for one policy snapshot and runs
// checks against requests and responses. It is rebuilt whenever the
// snapshot changes (the config-cache cadence) and is safe for concurrent use.
type Engine struct {
	snap     *store.SecgwSnapshot
	secrets  *detect.SecretsDetector
	pii      *detect.PIIDetector
	terms    *detect.TermsDetector
	classify classify.Resolver
	logger   *slog.Logger

	// verdictCache memoises per-message deterministic scans keyed on
	// sha256(direction|policy set|role|content). Chat clients resend the
	// whole history every turn; without this a 40-turn conversation rescans
	// turn 1 forty times. Bounded LRU: an unbounded map here grew by one
	// entry per distinct message ever proxied and was only freed by a
	// restart. VerdictCacheSize entries at a few hundred bytes each is a
	// few tens of megabytes at most, and holds the working set of every
	// live conversation comfortably.
	verdictCache *lru.Cache[string, []Violation]
	maxSpan      int
}

// VerdictCacheSize bounds verdictCache. 65,536 entries ≈ the last few
// thousand active conversations' worth of message history.
const VerdictCacheSize = 1 << 16

// Violation is one matched check with the metadata the store and the
// capture matrix need. Text is populated in memory for redaction and is
// dropped before persistence unless the kind is capture-eligible.
type Violation struct {
	Kind           store.SecgwCheckKind
	RuleID         string
	Severity       string
	Direction      string
	Action         string
	Offset, Length int
	Text           string
	Replacement    string
	MessageIndex   int
	// Record is false for a hit in a message the client is REPLAYING
	// (everything before the final user turn). Chat clients resend the
	// whole history every turn, so a term in turn 1 is matched again on
	// every later request; it is still enforced (blocked/redacted) but
	// was recorded when it was new, and recording it again on every turn
	// buries the violations list under one message's echoes.
	Record          bool
	PolicyID        string
	BindingID       string
	ClassifierScore float64
	ClassifierModel string
}

// Hash is the sha256 of the matched span, always persisted so "same secret
// again?" is answerable without the secret.
func (v Violation) Hash() string {
	sum := sha256.Sum256([]byte(v.Text))
	return hex.EncodeToString(sum[:])
}

// CaptureText reports whether the capture matrix allows this violation's
// text to be persisted: only prompt_injection (the attack IS the
// intelligence) and PII (stored as the redacted marker, never the value).
func (v Violation) CaptureText(cfg store.SecgwCaptureConfig) (string, bool) {
	switch v.Kind {
	case store.SecgwCheckPromptInjection, store.SecgwCheckContentSafety:
		// Both model-backed kinds keep the judged text: for injection the
		// attack is the intelligence; for content safety the admin needs
		// to see WHAT the guard called S9 to tune the category set.
		if cfg.PromptInjectionBodiesEnabled() {
			return v.Text, true
		}
	case store.SecgwCheckPII:
		return v.Replacement, true
	}
	return "", false
}

// ToStore converts a violation for persistence, applying the capture matrix.
func (v Violation) ToStore(cfg store.SecgwCaptureConfig, encrypt func(string) (string, error)) *store.SecgwViolation {
	out := &store.SecgwViolation{
		Kind: string(v.Kind), RuleID: v.RuleID, Severity: v.Severity, Direction: v.Direction, Action: v.Action,
		MatchOffset: v.Offset, MatchLength: v.Length, MatchHash: v.Hash(), PolicyID: v.PolicyID, BindingID: v.BindingID,
		ClassifierScore: v.ClassifierScore, ClassifierModel: v.ClassifierModel,
	}
	if text, ok := v.CaptureText(cfg); ok && text != "" && encrypt != nil {
		if env, err := encrypt(text); err == nil {
			out.SetMatchTextEnvelope(env)
		}
	}
	return out
}

// NewEngine compiles detectors for every check kind any policy in the
// snapshot references. Options are the union across policies: a
// policy-specific subset is applied at scan time.
func NewEngine(snap *store.SecgwSnapshot, resolver classify.Resolver, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cache, err := lru.New[string, []Violation](VerdictCacheSize)
	if err != nil {
		return nil, fmt.Errorf("verdict cache: %w", err)
	}
	e := &Engine{snap: snap, classify: resolver, logger: logger, verdictCache: cache}
	if snap.Empty() {
		return e, nil
	}
	needs := map[store.SecgwCheckKind]bool{}
	var secretsOpts detect.SecretsOptions
	piiOpts := detect.PIIOptions{}
	for _, p := range snap.Policies {
		for _, c := range p.Checks {
			if !c.Enabled {
				continue
			}
			needs[c.Kind] = true
			switch c.Kind {
			case store.SecgwCheckSecrets:
				var o struct {
					DisableRules   []string `json:"disable_rules"`
					ExtraRulesTOML string   `json:"extra_rules_toml"`
				}
				_ = json.Unmarshal(c.Options, &o)
				secretsOpts.DisableRules = append(secretsOpts.DisableRules, o.DisableRules...)
				if o.ExtraRulesTOML != "" {
					secretsOpts.ExtraRulesTOML += "\n" + o.ExtraRulesTOML
				}
			case store.SecgwCheckPII:
				var o struct {
					AllowBareSSN bool `json:"allow_bare_ssn"`
				}
				_ = json.Unmarshal(c.Options, &o)
				piiOpts.AllowBareSSN = piiOpts.AllowBareSSN || o.AllowBareSSN
			}
		}
	}
	if needs[store.SecgwCheckSecrets] {
		// Disable rules are policy-scoped at scan time; the compiled
		// detector carries the union of extra rules only.
		if e.secrets, err = detect.NewSecretsDetector(detect.SecretsOptions{ExtraRulesTOML: secretsOpts.ExtraRulesTOML}); err != nil {
			return nil, fmt.Errorf("secrets detector: %w", err)
		}
		e.maxSpan = max(e.maxSpan, e.secrets.MaxSpan())
	}
	if needs[store.SecgwCheckPII] {
		e.pii = detect.NewPIIDetector(piiOpts)
		e.maxSpan = max(e.maxSpan, e.pii.MaxSpan())
	}
	if needs[store.SecgwCheckTerms] && len(snap.TermLists) > 0 {
		lists := make([]detect.TermList, 0, len(snap.TermLists))
		for _, l := range snap.TermLists {
			lists = append(lists, detect.TermList{ID: l.ID, Name: l.Name, Mode: detect.TermMatchMode(l.MatchMode),
				Severity: detect.Severity(l.Severity), Terms: l.Terms, Allow: l.Allow})
		}
		if e.terms, err = detect.NewTermsDetector(lists); err != nil {
			return nil, fmt.Errorf("terms detector: %w", err)
		}
		e.maxSpan = max(e.maxSpan, e.terms.MaxSpan())
	}
	return e, nil
}

// Snapshot returns the snapshot this engine was built from.
func (e *Engine) Snapshot() *store.SecgwSnapshot { return e.snap }

// MaxSpan is the longest match any compiled detector can produce; the
// streaming hold-back sizes its rescan overlap from it.
func (e *Engine) MaxSpan() int { return e.maxSpan }

// Resolve is a convenience over the package-level Resolve for this snapshot.
func (e *Engine) Resolve(sub Subject) *Effective { return Resolve(e.snap, sub) }

// scanText runs the deterministic detectors named by eff over one text in
// the given direction. It never runs classifiers.
func (e *Engine) scanText(eff *Effective, direction, text string) []Violation {
	var out []Violation
	add := func(check ResolvedCheck, ms []detect.Match) {
		for _, m := range ms {
			out = append(out, Violation{Kind: check.Kind, RuleID: m.RuleID, Severity: string(m.Severity), Direction: direction,
				Offset: m.Offset, Length: m.Length, Text: m.Text, Replacement: m.Replacement,
				PolicyID: check.PolicyID, BindingID: check.BindingID})
		}
	}
	if c, ok := eff.Has(store.SecgwCheckSecrets, direction); ok && e.secrets != nil {
		ms := e.secrets.Detect(text)
		if len(c.Options) > 0 {
			var o struct {
				DisableRules []string `json:"disable_rules"`
			}
			if json.Unmarshal(c.Options, &o) == nil && len(o.DisableRules) > 0 {
				ms = filterRules(ms, o.DisableRules)
			}
		}
		add(c, ms)
	}
	if c, ok := eff.Has(store.SecgwCheckPII, direction); ok && e.pii != nil {
		ms := e.pii.Detect(text)
		if len(c.Options) > 0 {
			var o struct {
				Classes []string `json:"classes"`
			}
			if json.Unmarshal(c.Options, &o) == nil && len(o.Classes) > 0 {
				ms = keepRules(ms, o.Classes)
			}
		}
		add(c, ms)
	}
	if c, ok := eff.Has(store.SecgwCheckTerms, direction); ok && e.terms != nil {
		ms := e.terms.Detect(text)
		if len(c.Options) > 0 {
			var o struct {
				TermListIDs []string `json:"term_list_ids"`
			}
			if json.Unmarshal(c.Options, &o) == nil && len(o.TermListIDs) > 0 {
				ms = keepRules(ms, o.TermListIDs)
			}
		}
		add(c, ms)
	}
	return out
}

func filterRules(ms []detect.Match, drop []string) []detect.Match {
	set := map[string]bool{}
	for _, d := range drop {
		set[d] = true
	}
	out := ms[:0]
	for _, m := range ms {
		if !set[m.RuleID] {
			out = append(out, m)
		}
	}
	return out
}

func keepRules(ms []detect.Match, keep []string) []detect.Match {
	set := map[string]bool{}
	for _, k := range keep {
		set[k] = true
	}
	out := ms[:0]
	for _, m := range ms {
		if set[m.RuleID] {
			out = append(out, m)
		}
	}
	return out
}

// ScanCached memoises scanText per (role, content, direction, policy set).
func (e *Engine) scanCached(eff *Effective, direction, role, text string) []Violation {
	key := cacheKey(eff, direction, role, text)
	if v, ok := e.verdictCache.Get(key); ok {
		return cloneViolations(v)
	}
	vs := e.scanText(eff, direction, text)
	e.verdictCache.Add(key, cloneViolations(vs))
	return vs
}

func cacheKey(eff *Effective, direction, role, text string) string {
	h := sha256.New()
	h.Write([]byte(direction))
	h.Write([]byte{0})
	h.Write([]byte(role))
	h.Write([]byte{0})
	// The policy identity matters: the same text under a different check
	// set yields a different verdict.
	for _, k := range store.SecgwCheckKinds() {
		if c, ok := eff.Checks[k]; ok {
			h.Write([]byte(c.PolicyID))
			h.Write([]byte(c.Mode))
			h.Write(c.Options)
		}
		h.Write([]byte{0})
	}
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

func cloneViolations(in []Violation) []Violation {
	if in == nil {
		return nil
	}
	out := make([]Violation, len(in))
	copy(out, in)
	return out
}

// --- ingress ------------------------------------------------------------------

// Message is one chat message with its text flattened.
type Message struct {
	Index int
	Role  string
	Text  string
	// contentPath is how to write a redacted string back: "string" when
	// content was a plain string, "parts" when it was a content array.
	contentPath string
}

// Request is a downstream call awaiting inspection.
type Request struct {
	Body           []byte
	IsServiceToken bool
	Subject        Subject
}

// Recorded counts the violations that will be persisted (Record == true).
// The usage event carries this so its "N violations" badge joins to the
// rows actually written.
func Recorded(vs []Violation) int {
	n := 0
	for _, v := range vs {
		if v.Record {
			n++
		}
	}
	return n
}

// newTurnStart returns the message index from which this request's NEW
// content begins: everything after the last assistant message. On the first
// turn that is 0 (whole request is new). Non-chat bodies have one message
// at index 0, also new.
func newTurnStart(msgs []Message) int {
	from := 0
	for _, m := range msgs {
		if m.Role == "assistant" || strings.HasPrefix(m.Role, "assistant.") {
			from = m.Index + 1
		}
	}
	return from
}

// Decision is the ingress outcome.
type Decision struct {
	Action     string // "" (nothing matched) | observed | redacted | blocked
	Body       []byte // rewritten body when Action == redacted
	Violations []Violation
	Redactions int
	// BlockKind names the check that blocked, for the error param.
	BlockKind store.SecgwCheckKind
	// ClassifierFailed reports a fail-closed classifier outage: the
	// request is blocked because the judge was unavailable, not because
	// anything matched. Distinguishable in the audit trail.
	ClassifierFailed bool
}

// Ingress evaluates a request against its effective checks.
func (e *Engine) Ingress(ctx context.Context, eff *Effective, req Request) (Decision, error) {
	dec := Decision{}
	if eff.Empty() {
		return dec, nil
	}
	direction := store.SecgwDirectionIngress

	// 1. shape — cheapest, no text scan.
	if c, ok := eff.Has(store.SecgwCheckShape, direction); ok {
		var lim detect.ShapeLimits
		_ = json.Unmarshal(c.Options, &shapeOptions{&lim})
		for _, m := range detect.CheckShape(detect.ShapeInput{Body: req.Body, IsServiceToken: req.IsServiceToken}, lim) {
			dec.Violations = append(dec.Violations, Violation{Kind: c.Kind, RuleID: m.RuleID, Severity: string(m.Severity), Record: true,
				Direction: direction, Text: m.Text, PolicyID: c.PolicyID, BindingID: c.BindingID, MessageIndex: -1})
		}
		if len(dec.Violations) > 0 && c.Mode == store.SecgwModeBlock {
			return e.block(dec, c.Kind), nil
		}
	}

	// 2. deterministic text checks, per message.
	msgs, parsed := parseMessages(req.Body)
	if !parsed {
		// Not a chat-shaped body (embeddings, audio, images). Scan the raw
		// body as one text so a secret in an embeddings input is still seen.
		msgs = []Message{{Index: 0, Role: "body", Text: string(req.Body)}}
	}
	var blockKind store.SecgwCheckKind
	redacted := false
	newFrom := newTurnStart(msgs)
	for i := range msgs {
		vs := e.scanCached(eff, direction, msgs[i].Role, msgs[i].Text)
		if len(vs) == 0 {
			continue
		}
		var spans []detect.Match
		for j := range vs {
			vs[j].MessageIndex = msgs[i].Index
			vs[j].Record = msgs[i].Index >= newFrom
			c := eff.Checks[vs[j].Kind]
			switch c.Mode {
			case store.SecgwModeBlock:
				vs[j].Action = store.SecgwActionBlocked
				if blockKind == "" {
					blockKind = vs[j].Kind
				}
			case store.SecgwModeRedact:
				vs[j].Action = store.SecgwActionRedacted
				spans = append(spans, detect.Match{Offset: vs[j].Offset, Length: vs[j].Length, RuleID: vs[j].RuleID, Replacement: vs[j].Replacement})
			default:
				vs[j].Action = store.SecgwActionObserved
			}
		}
		dec.Violations = append(dec.Violations, vs...)
		if len(spans) > 0 && blockKind == "" {
			msgs[i].Text = detect.Redact(msgs[i].Text, spans)
			dec.Redactions += len(spans)
			redacted = true
		}
	}
	if blockKind != "" {
		return e.block(dec, blockKind), nil
	}

	// 3. prompt injection — model-backed, only over what survived.
	if c, ok := eff.Has(store.SecgwCheckPromptInjection, direction); ok {
		vs, failed, err := e.classifyInjection(ctx, c, msgs)
		if err != nil {
			e.logger.WarnContext(ctx, "secgw: classifier call failed", "error", err.Error(), "model", c.ClassifierModelID, "fail", c.Fail)
		}
		if failed && c.Fail == store.SecgwFailClosed {
			dec.ClassifierFailed = true
			return e.block(dec, c.Kind), nil
		}
		for j := range vs {
			if c.Mode == store.SecgwModeBlock {
				vs[j].Action = store.SecgwActionBlocked
			} else {
				vs[j].Action = store.SecgwActionObserved
			}
		}
		dec.Violations = append(dec.Violations, vs...)
		if len(vs) > 0 && c.Mode == store.SecgwModeBlock {
			return e.block(dec, c.Kind), nil
		}
	}

	// 4. content safety — a conversation guard judges the LAST turn with
	// the earlier ones as context. Same block/observe/fail semantics as
	// prompt injection; the difference is the verdict is a category set.
	if c, ok := eff.Has(store.SecgwCheckContentSafety, direction); ok && parsed {
		vs, failed, err := e.judgeContentSafety(ctx, c, direction, msgs)
		if err != nil {
			e.logger.WarnContext(ctx, "secgw: content guard call failed", "error", err.Error(), "model", c.ClassifierModelID, "fail", c.Fail)
		}
		if failed && c.Fail == store.SecgwFailClosed {
			dec.ClassifierFailed = true
			return e.block(dec, c.Kind), nil
		}
		dec.Violations = append(dec.Violations, vs...)
		for _, v := range vs {
			if v.Action == store.SecgwActionBlocked {
				return e.block(dec, c.Kind), nil
			}
		}
	}

	if redacted && parsed {
		body, err := rewriteMessages(req.Body, msgs)
		if err != nil {
			return dec, fmt.Errorf("rewrite redacted body: %w", err)
		}
		dec.Body = body
		dec.Action = store.SecgwActionRedacted
	} else if len(dec.Violations) > 0 {
		dec.Action = store.SecgwActionObserved
	}
	return dec, nil
}

func (e *Engine) block(dec Decision, kind store.SecgwCheckKind) Decision {
	dec.Action = store.SecgwActionBlocked
	dec.BlockKind = kind
	dec.Body = nil
	dec.Redactions = 0
	return dec
}

// shapeOptions adapts the JSON option names to detect.ShapeLimits.
type shapeOptions struct{ l *detect.ShapeLimits }

func (o *shapeOptions) UnmarshalJSON(b []byte) error {
	var raw struct {
		MaxMessages                 int  `json:"max_messages"`
		MaxBodyBytes                int  `json:"max_body_bytes"`
		MaxImageParts               int  `json:"max_image_parts"`
		MaxTools                    int  `json:"max_tools"`
		DenySystemFromServiceTokens bool `json:"deny_system_from_service_tokens"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*o.l = detect.ShapeLimits{MaxMessages: raw.MaxMessages, MaxBodyBytes: raw.MaxBodyBytes, MaxImageParts: raw.MaxImageParts,
		MaxTools: raw.MaxTools, DenySystemFromServiceTokens: raw.DenySystemFromServiceTokens}
	return nil
}

// classifyInjection chunks each message to the classifier's window, scores
// the chunks in one batched call, and reports one violation per message
// whose max score crosses the threshold.
func (e *Engine) classifyInjection(ctx context.Context, c ResolvedCheck, msgs []Message) (vs []Violation, failed bool, err error) {
	if e.classify == nil {
		return nil, true, fmt.Errorf("no classifier resolver configured")
	}
	opts := classify.InjectionOptions{Threshold: 0.9, ChunkChars: 1500, OverlapChars: 200, Timeout: 250 * time.Millisecond}
	if len(c.Options) > 0 {
		var o struct {
			Threshold  float64 `json:"threshold"`
			ChunkChars int     `json:"chunk_chars"`
			TimeoutMs  int     `json:"timeout_ms"`
		}
		if json.Unmarshal(c.Options, &o) == nil {
			if o.Threshold > 0 {
				opts.Threshold = o.Threshold
			}
			if o.ChunkChars > 0 {
				opts.ChunkChars = o.ChunkChars
			}
			if o.TimeoutMs > 0 {
				opts.Timeout = time.Duration(o.TimeoutMs) * time.Millisecond
			}
		}
	}
	cl, err := e.classify.For(ctx, c.ClassifierModelID)
	if err != nil {
		return nil, true, err
	}
	var segs []classify.Segment
	owner := []int{}
	for i, m := range msgs {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		for _, chunk := range classify.Chunk(m.Text, opts.ChunkChars, opts.OverlapChars) {
			segs = append(segs, classify.Segment{Index: len(segs), Text: chunk})
			owner = append(owner, i)
		}
	}
	if len(segs) == 0 {
		return nil, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	scores, err := cl.Classify(cctx, segs)
	if err != nil {
		return nil, true, err
	}
	best := map[int]classify.Score{}
	for _, s := range scores {
		if s.Index < 0 || s.Index >= len(owner) {
			continue
		}
		if !s.Malicious {
			continue
		}
		mi := owner[s.Index]
		if prev, ok := best[mi]; !ok || s.Score > prev.Score {
			best[mi] = s
		}
	}
	for mi, s := range best {
		if s.Score < opts.Threshold {
			continue
		}
		vs = append(vs, Violation{Kind: c.Kind, RuleID: s.Label, Severity: string(detect.SeverityHigh), Direction: store.SecgwDirectionIngress,
			MessageIndex: msgs[mi].Index, Text: msgs[mi].Text, Length: len(msgs[mi].Text), PolicyID: c.PolicyID, BindingID: c.BindingID,
			Record:          msgs[mi].Index >= newTurnStart(msgs),
			ClassifierScore: s.Score, ClassifierModel: c.ClassifierModelID})
	}
	return vs, false, nil
}

// --- egress (buffered) --------------------------------------------------------

// judgeContentSafety hands the conversation to a generative guard with
// the last message as the turn under judgement, and maps the verdict onto
// the policy's selected categories.
//
// Direction decides the action. On ingress the check may block. On egress
// the v1 ruling is observe-only: a generative verdict cannot be produced
// inside a streaming hold-back window, so a buffered response is judged
// and LOGGED — never withheld — and the action is always observed
// regardless of mode. Validate refuses mode=block on egress at save time
// so an admin is told, rather than discovering it in a violations page
// full of "observed".
func (e *Engine) judgeContentSafety(ctx context.Context, c ResolvedCheck, direction string, msgs []Message) (vs []Violation, failed bool, err error) {
	if e.classify == nil {
		return nil, true, fmt.Errorf("no classifier resolver configured")
	}
	var opts ContentSafetyOptions
	if len(c.Options) > 0 {
		_ = json.Unmarshal(c.Options, &opts)
	}
	timeout := 5 * time.Second
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	cl, err := e.classify.For(ctx, c.ClassifierModelID)
	if err != nil {
		return nil, true, err
	}
	guard, ok := cl.(interface {
		Judge(context.Context, []classify.Turn) (classify.Verdict, error)
	})
	if !ok {
		return nil, true, classify.ErrNotConversationGuard
	}
	var turns []classify.Turn
	last := -1
	for i, m := range msgs {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		turns = append(turns, classify.Turn{Role: m.Role, Content: m.Text})
		last = i
	}
	if len(turns) == 0 {
		return nil, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := guard.Judge(cctx, turns)
	if err != nil {
		return nil, true, err
	}
	if v.Safe {
		return nil, false, nil
	}
	action := store.SecgwActionObserved
	if direction == store.SecgwDirectionIngress && c.Mode == store.SecgwModeBlock && opts.Enforces(v.Categories) {
		action = store.SecgwActionBlocked
	}
	// One violation per verdict, RuleID = the category list ("S9,S2") so
	// the violations page can render each code as a chip. Always recorded:
	// the guard judges the NEW turn (the last message), never a replayed
	// one, so the replay de-duplication that applies to span matches does
	// not apply here.
	return []Violation{{
		Kind: c.Kind, RuleID: strings.Join(v.Categories, ","), Severity: string(detect.SeverityHigh), Direction: direction,
		Action: action, MessageIndex: msgs[last].Index, Text: msgs[last].Text, Length: len(msgs[last].Text), Record: true,
		PolicyID: c.PolicyID, BindingID: c.BindingID, ClassifierScore: 1, ClassifierModel: c.ClassifierModelID,
	}}, false, nil
}

// EgressContentSafety judges a buffered response in the context of the
// request that produced it. Observe-only by ruling (see
// judgeContentSafety); the caller appends the violations to its sink and
// never changes the response. reqBody is the ORIGINAL request so the
// guard sees the user prompt the answer responds to — Llama Guard
// moderates an assistant turn differently with that context.
func (e *Engine) EgressContentSafety(ctx context.Context, eff *Effective, reqBody, respBody []byte) []Violation {
	c, ok := eff.Has(store.SecgwCheckContentSafety, store.SecgwDirectionEgress)
	if !ok {
		return nil
	}
	texts, ok := responseTexts(respBody)
	if !ok || len(texts) == 0 {
		return nil
	}
	msgs, _ := parseMessages(reqBody)
	// Append the first choice's content as the assistant turn under
	// judgement. Reasoning and tool-call paths are not moderated here:
	// the taxonomy is about what a user is shown.
	for _, t := range texts {
		if t.Path == "content" && strings.TrimSpace(t.Text) != "" {
			msgs = append(msgs, Message{Index: len(msgs), Role: "assistant", Text: t.Text})
			break
		}
	}
	vs, _, err := e.judgeContentSafety(ctx, c, store.SecgwDirectionEgress, msgs)
	if err != nil {
		e.logger.WarnContext(ctx, "secgw: egress content guard call failed", "error", err.Error(), "model", c.ClassifierModelID)
	}
	return vs
}

// ScanEgressText runs the deterministic egress detectors over the tail of a
// streaming channel's accumulated text, from offset `from` (which the
// hold-back buffer has already stepped back by the overlap). Offsets in
// the returned violations are into the full text.
func (e *Engine) ScanEgressText(eff *Effective, full string, from int) []Violation {
	if from < 0 {
		from = 0
	}
	if from > len(full) {
		return nil
	}
	// Step back to a rune boundary so a multi-byte character is never split.
	for from > 0 && from < len(full) && !utf8.RuneStart(full[from]) {
		from--
	}
	vs := e.scanText(eff, store.SecgwDirectionEgress, full[from:])
	for i := range vs {
		vs[i].Offset += from
		vs[i].Record = true
	}
	return vs
}

// EgressResult is the buffered-egress outcome.
type EgressResult struct {
	Action     string
	Body       []byte
	Violations []Violation
	Redactions int
	BlockKind  store.SecgwCheckKind
}

// Egress scans an OpenAI-shaped, non-streaming response body.
func (e *Engine) Egress(eff *Effective, body []byte) EgressResult {
	res := EgressResult{Body: body}
	if eff.Empty() {
		return res
	}
	direction := store.SecgwDirectionEgress
	texts, ok := responseTexts(body)
	if !ok || len(texts) == 0 {
		return res
	}
	var blockKind store.SecgwCheckKind
	changed := false
	for i := range texts {
		vs := e.scanText(eff, direction, texts[i].Text)
		if len(vs) == 0 {
			continue
		}
		var spans []detect.Match
		for j := range vs {
			vs[j].Record = true // a response is always new content
			c := eff.Checks[vs[j].Kind]
			switch c.Mode {
			case store.SecgwModeBlock:
				vs[j].Action = store.SecgwActionBlocked
				if blockKind == "" {
					blockKind = vs[j].Kind
				}
			case store.SecgwModeRedact:
				vs[j].Action = store.SecgwActionRedacted
				spans = append(spans, detect.Match{Offset: vs[j].Offset, Length: vs[j].Length, RuleID: vs[j].RuleID, Replacement: vs[j].Replacement})
			default:
				vs[j].Action = store.SecgwActionObserved
			}
		}
		res.Violations = append(res.Violations, vs...)
		if len(spans) > 0 && blockKind == "" {
			texts[i].Text = detect.Redact(texts[i].Text, spans)
			res.Redactions += len(spans)
			changed = true
		}
	}
	if blockKind != "" {
		res.Action = store.SecgwActionBlocked
		res.BlockKind = blockKind
		return res
	}
	if changed {
		if out, err := rewriteResponseTexts(body, texts); err == nil {
			res.Body = out
			res.Action = store.SecgwActionRedacted
		}
	} else if len(res.Violations) > 0 {
		res.Action = store.SecgwActionObserved
	}
	return res
}

// BuiltinSecretRules lists the embedded secret rules for the admin UI.
func BuiltinSecretRules() ([]detect.RuleInfo, error) {
	d, err := detect.NewSecretsDetector(detect.SecretsOptions{})
	if err != nil {
		return nil, err
	}
	return d.Rules(), nil
}

// PIIClasses lists the PII classes a policy may select.
func PIIClasses() []string {
	return []string{string(detect.ClassPAN), string(detect.ClassIBAN), string(detect.ClassSSNUS), string(detect.ClassNPI), string(detect.ClassEmail), string(detect.ClassPhone)}
}
