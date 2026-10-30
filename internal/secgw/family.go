package secgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/detect"
	"github.com/torvanis/janus/internal/store"
)

// Family safety: the built-in Community policy (see store/secgw_family.go
// and ~/audits/janus-chat/family-filter-design.md). It never goes through
// Resolve: it is one independent evaluation that applies, for covered
// users, in addition to whatever Business policies resolve to.
//
// Unlike every other content-safety path it BLOCKS replies too (the
// streaming side is stream.ParagraphGate, judged by FamilyJudgeReply), and
// one category outcome is not a refusal at all: "support" answers a child
// who is struggling with a kind message instead of forwarding the message.

// Family outcomes.
const (
	FamilyAllow   = ""
	FamilyBlock   = store.SecgwActionBlocked
	FamilySupport = store.SecgwActionSupported
	// FamilyWithhold (reply side): the reply was about to give a
	// schoolwork answer away; it stops there and gets a tutor's note.
	FamilyWithhold = "withheld"
)

// FamilyOutcome is one family verdict.
type FamilyOutcome struct {
	// Action is FamilyAllow, FamilyBlock or FamilySupport.
	Action string
	// Failed reports a fail-closed classifier outage (Action is block).
	Failed bool
	Err    error
	// Violation is the row to record (nil when allowed or failed).
	Violation *Violation
	Model     ModelStats
	// Instructions (ingress, allowed): what Child Safety adds to the
	// system prompt for this turn: the age range's base text, the tutor
	// rule, and the instructions of each matched "answer" category.
	Instructions string
	// Matched are the Child Safety categories that matched and were
	// answered appropriately (for the request log).
	Matched []string
	// ToolOutput: the verdict is about tool output (a page, search
	// results) shown to the child, not the child's own words. Never a
	// support answer, never a "your child asked" notice.
	ToolOutput bool
}

// errFamilyNoClassifier is a family check with nothing to call.
var errFamilyNoClassifier = errors.New("family safety has no usable safety model")

// FamilyCovers reports whether the family policy applies to a user right
// now (on, a classifier chosen, the user assigned directly or via a team).
func (e *Engine) FamilyCovers(userID string) bool {
	return e != nil && e.snap != nil && e.snap.Family.Covers(userID)
}

// FamilyProfile is the age range that applies to a covered user.
func (e *Engine) FamilyProfile(userID string) *store.ChildProfile {
	if e == nil || e.snap == nil {
		return nil
	}
	return e.snap.Family.Profile(userID)
}

// FamilyConfig returns the family configuration the engine was built with.
func (e *Engine) FamilyConfig() *store.SecgwFamilyConfig {
	if e == nil || e.snap == nil || e.snap.Family == nil {
		return nil
	}
	return e.snap.Family.Config
}

// familyCategories is what the check asks, from the SHIPPED definitions.
// On a request: the profile's categories that are not off, and the fixed
// safety categories. On a reply: the fixed safety categories, the
// categories the profile blocks (never feeling low: a kind reply to a sad
// child must never be cut), and "gives the answer away" while tutoring.
func familyCategories(p *store.ChildProfile, reply bool) []*store.SecgwCategory {
	var slugs []string
	if !reply {
		slugs = p.Asks()
	} else {
		for _, slug := range store.ChildSafetyCategorySlugs {
			if slug == store.ChildCategoryFeelingLow || slug == store.ChildCategoryHomeworkLearning || slug == store.ChildCategoryHomeworkShortcut {
				continue
			}
			if p.Outcome(slug) == store.ChildOutcomeBlock {
				slugs = append(slugs, slug)
			}
		}
		slugs = append(slugs, store.SecgwFamilySafetySlugs...)
		if p.Tutoring() {
			slugs = append(slugs, store.ChildCategoryFinalAnswer)
		}
	}
	var out []*store.SecgwCategory
	for _, slug := range slugs {
		if c, ok := store.ChildCategory(slug); ok {
			out = append(out, c)
		}
	}
	return out
}

func familyTimeout(cfg *store.SecgwFamilyConfig) time.Duration {
	if cfg.TimeoutMs > 0 {
		return time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	return time.Duration(store.SecgwFamilyDefaultTimeoutMs) * time.Millisecond
}

// familyJudge asks the family classifier about each text (windows of one
// request or reply) and returns the categories that count, highest first.
// prompt is the child's message, given to a generative guard as the user
// turn when judging a reply.
func (e *Engine) familyJudge(ctx context.Context, cfg *store.SecgwFamilyConfig, p *store.ChildProfile, texts []string, reply bool, prompt string) (counted []CategoryScore, stats ModelStats, err error) {
	started := time.Now()
	defer func() { stats.Elapsed = time.Since(started) }()
	if e.classify == nil || cfg.ClassifierModelID == "" {
		return nil, stats, errFamilyNoClassifier
	}
	cl, err := e.classify.For(ctx, cfg.ClassifierModelID)
	if err != nil {
		return nil, stats, err
	}
	cats := familyCategories(p, reply)
	timeout := familyTimeout(cfg)
	feature := "family_safety"
	if d, ok := cl.(decisionClassifier); ok && d.IsDecision() {
		// "Gives the answer away" is its own yes/no question with both
		// sides spelled out. In the shared choice question a reply that
		// only confirms the child's own answer fitted no other option and
		// landed on it ("So x = 23?" -> "Yes!" was withheld).
		var answer *store.SecgwCategory
		rest := cats[:0:0]
		for _, c := range cats {
			if c.Slug == store.ChildCategoryFinalAnswer {
				answer = c
				continue
			}
			rest = append(rest, c)
		}
		qs := CategoryQuestions(rest, cfg.Ask, reply)
		if answer != nil {
			qs = append(qs, dec.Question{ID: CategoryYesNoPrefix + answer.Slug, Type: dec.QuestionYesNo,
				Instructions: "Does the assistant's reply give away the answer to a schoolwork problem that the child has not already given themselves?",
				YesMeans:     answer.Includes, NoMeans: answer.Excludes})
		}
		if reply && strings.TrimSpace(prompt) != "" {
			// The reply is judged with the child's message as context, so
			// "x = 5" reads as an answer given away, not a stray fact. Only
			// the reply is judged: the question is there to read it by.
			for i := range qs {
				qs[i].Instructions += " Judge only the assistant's reply; the child's message is shown for context."
			}
			for i := range texts {
				texts[i] = "The child asked: " + dec.TrimText(prompt, 1500) + "\n\nThe assistant replied: " + texts[i]
			}
		}
		type res struct {
			r   dec.Result
			err error
		}
		out := make([]res, len(texts))
		var wg sync.WaitGroup
		for i, t := range texts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(classify.WithGuardCall(dec.WithFeature(ctx, feature, "inline")), timeout)
				defer cancel()
				out[i].r, out[i].err = d.Decide(cctx, t, qs)
			}()
		}
		wg.Wait()
		best := map[string]float64{}
		for _, o := range out {
			switch {
			case o.r.Calls > 0:
				stats.Calls += o.r.Calls
			case cfg.Ask == store.SecgwAskSeparately:
				stats.Calls += len(qs)
			default:
				stats.Calls++
			}
			if o.err != nil {
				return nil, stats, o.err
			}
			probs, perr := CategoryProbabilities(o.r, rest, cfg.Ask)
			if perr != nil {
				return nil, stats, perr
			}
			if answer != nil {
				a, ok := o.r.Answers[CategoryYesNoPrefix+answer.Slug]
				if !ok {
					return nil, stats, dec.ErrNoAnswer
				}
				probs[answer.Slug] = a.Yes
			}
			for slug, p := range probs {
				best[slug] = max(best[slug], p)
			}
		}
		for _, s := range ScoreCategories(best, cats) {
			if s.Counts {
				counted = append(counted, s)
			}
		}
		return counted, stats, nil
	}
	guard, ok := cl.(interface {
		Judge(context.Context, []classify.Turn) (classify.Verdict, error)
	})
	if !ok {
		return nil, stats, classify.ErrNotConversationGuard
	}
	// Legacy generative guard: only the safety categories can be asked,
	// by their hazard codes.
	byCode := map[string]*store.SecgwCategory{}
	for _, c := range cats {
		if c.LegacyCode != "" {
			byCode[c.LegacyCode] = c
		}
	}
	seen := map[string]bool{}
	for _, t := range texts {
		turns := []classify.Turn{{Role: "user", Content: t}}
		if reply {
			turns = []classify.Turn{{Role: "user", Content: dec.TrimText(prompt, guardTextMaxChars)}, {Role: "assistant", Content: t}}
			if strings.TrimSpace(prompt) == "" {
				turns = turns[1:]
			}
		}
		cctx, cancel := context.WithTimeout(classify.WithGuardCall(dec.WithFeature(ctx, feature, "inline")), timeout)
		v, jerr := guard.Judge(cctx, turns)
		cancel()
		stats.Calls++
		if jerr != nil {
			return nil, stats, jerr
		}
		if v.Safe {
			continue
		}
		for _, code := range v.Categories {
			if c := byCode[code]; c != nil && !seen[c.Slug] {
				seen[c.Slug] = true
				counted = append(counted, CategoryScore{Slug: c.Slug, Name: c.Name, Probability: 1, MinConfidence: c.MinConfidence, Counts: true})
			}
		}
	}
	return counted, stats, nil
}

// familyViolation builds the row for a family verdict. The judged text is
// NEVER kept (capture is always off for the family policy): admins cannot
// read chats, and this policy must not become a way around that.
func familyViolation(cfg *store.SecgwFamilyConfig, direction, action string, counted []CategoryScore, length int) *Violation {
	slugs := make([]string, len(counted))
	for i, s := range counted {
		slugs[i] = s.Slug
	}
	return &Violation{Kind: store.SecgwCheckContentSafety, RuleID: strings.Join(slugs, ","), Severity: string(detect.SeverityHigh),
		Direction: direction, Action: action, Length: length, Record: true, MessageIndex: -1,
		PolicyID: store.SecgwFamilyPolicyID, ClassifierScore: counted[0].Probability, ClassifierModel: cfg.ClassifierModelID}
}

// familyIngressText is what the family check judges on a request: the new
// turn of a chat body, or the prompt/input of a completions or embeddings
// body (the raw body when it has neither).
func familyIngressText(body []byte, media bool) string {
	if msgs, ok := parseMessages(body); ok {
		// Tool output is never the child's words: it is judged as content
		// shown to the child (FamilyJudgeToolOutput), never as theirs.
		return scopeText(userWordsOnly(msgs, media), store.SecgwJudgeNewTurn)
	}
	var raw struct {
		Prompt json.RawMessage `json:"prompt"`
		Input  json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &raw) == nil {
		var parts []string
		for _, f := range []json.RawMessage{raw.Prompt, raw.Input} {
			parts = append(parts, stringsOf(f)...)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n")
		}
	}
	return string(body)
}

// stringsOf reads a JSON string or array of strings.
func stringsOf(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	return nil
}

// familyWindows splits text into the windows one family check judges:
// every window of the text (never just its tail), or errTextTooLong.
func familyWindows(text string) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	ws := classify.Chunk(text, guardTextMaxChars, guardWindowOverlap)
	if len(ws) > guardMaxWindows {
		return nil, errTextTooLong
	}
	return ws, nil
}

// FamilyIngress judges a covered user's request against their age range.
// Support beats block beats answering: in the first two nothing is
// forwarded, and a kind answer is never the wrong one to a message that
// also reads as a child in distress. An allowed request carries the
// instructions to add (always at least the age range's base text).
func (e *Engine) FamilyIngress(ctx context.Context, userID string, body []byte) FamilyOutcome {
	cfg := e.FamilyConfig()
	p := e.FamilyProfile(userID)
	if cfg == nil || p == nil {
		return FamilyOutcome{}
	}
	text := familyIngressText(body, ToolOutputJudged(ctx))
	texts, err := familyWindows(text)
	if err != nil {
		return FamilyOutcome{Action: FamilyBlock, Failed: true, Err: err}
	}
	if len(texts) == 1 {
		// A follow-up is judged with its conversation as context: "how
		// do I do that?" right after asking about turning parental
		// controls off is about turning them off. A new turn long enough
		// to need several windows is judged window by window, as before.
		if msgs, ok := parseMessages(body); ok {
			texts[0] = dec.WithContext(earlierTurns(msgs), texts[0], 0)
		}
	}
	out := FamilyOutcome{}
	var counted []CategoryScore
	if len(texts) > 0 {
		var stats ModelStats
		counted, stats, err = e.familyJudge(ctx, cfg, p, texts, false, "")
		out.Model = stats
		if err != nil {
			out.Model.Failed = 1
			out.Action, out.Failed, out.Err = FamilyBlock, true, err
			return out
		}
	}
	var blocked, answered []CategoryScore
	support := false
	for _, c := range counted {
		switch {
		case c.Slug == "self_harm":
			support = true
			blocked = append(blocked, c)
		case c.Slug == store.ChildCategoryFeelingLow && p.Outcome(c.Slug) == store.ChildOutcomeBlock:
			support = true
			blocked = append(blocked, c)
		case p.Outcome(c.Slug) == store.ChildOutcomeAnswer:
			answered = append(answered, c)
		case p.Outcome(c.Slug) == store.ChildOutcomeBlock, isSafetySlug(c.Slug):
			blocked = append(blocked, c)
		}
	}
	if len(blocked) > 0 {
		out.Action = FamilyBlock
		if support {
			out.Action = FamilySupport
		}
		out.Violation = familyViolation(cfg, store.SecgwDirectionIngress, out.Action, blocked, len(text))
		return out
	}
	if !ToolOutputJudged(ctx) {
		// Tool output a client sends (not judged where the tool ran, as
		// Chat does): judged as content shown to the child, like a reply.
		if msgs, ok := parseMessages(body); ok {
			if t := toolOutputText(msgs); t != "" {
				tool := e.FamilyJudgeToolOutput(ctx, userID, FamilyPrompt(body), t)
				out.Model.Elapsed += tool.Model.Elapsed
				out.Model.Calls += tool.Model.Calls
				out.Model.Failed += tool.Model.Failed
				if tool.Action != FamilyAllow {
					tool.Model = out.Model
					return tool
				}
			}
		}
	}
	parts := []string{p.BaseText()}
	if p.Tutoring() {
		parts = append(parts, store.TutorRule())
	}
	for _, c := range answered {
		out.Matched = append(out.Matched, c.Slug)
		if t := strings.TrimSpace(p.InstructionsFor(c.Slug)); t != "" {
			parts = append(parts, t)
		}
	}
	out.Instructions = strings.Join(parts, "\n\n")
	return out
}

func isSafetySlug(slug string) bool {
	for _, s := range store.SecgwFamilySafetySlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// FamilyPrompt is the child's new turn, for judging a reply with a
// generative guard (which reads an answer in the context of its question).
// A request that only carries tool results has no new words of the child;
// the question is then the child's last message.
func FamilyPrompt(body []byte) string {
	if t := familyIngressText(body, false); strings.TrimSpace(t) != "" {
		return t
	}
	msgs, _ := parseMessages(body)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && strings.TrimSpace(msgs[i].Text) != "" {
			return msgs[i].Text
		}
	}
	return ""
}

// familyReplyContext is how much of the already-cleared reply a chunk is
// judged with, so a paragraph is read in context.
const familyReplyContext = 4000

// FamilyJudgeReply judges one chunk of a reply (streaming or not) with the
// text before it as context. Any counted category blocks: support is for
// the child's own words, never for what a model says.
func (e *Engine) FamilyJudgeReply(ctx context.Context, userID, prompt, prior, chunk string) FamilyOutcome {
	cfg := e.FamilyConfig()
	p := e.FamilyProfile(userID)
	if cfg == nil || p == nil || strings.TrimSpace(chunk) == "" {
		return FamilyOutcome{}
	}
	if len(prior) > familyReplyContext {
		cut := len(prior) - familyReplyContext
		for cut < len(prior) && (prior[cut]&0xC0) == 0x80 {
			cut++
		}
		prior = prior[cut:]
	}
	texts, err := familyWindows(prior + chunk)
	if err != nil {
		return FamilyOutcome{Action: FamilyBlock, Failed: true, Err: err}
	}
	counted, stats, err := e.familyJudge(ctx, cfg, p, texts, true, prompt)
	out := FamilyOutcome{Model: stats}
	if err != nil {
		out.Model.Failed = 1
		out.Action, out.Failed, out.Err = FamilyBlock, true, err
		return out
	}
	if len(counted) == 0 {
		return out
	}
	out.Action = FamilyBlock
	if len(counted) == 1 && counted[0].Slug == store.ChildCategoryFinalAnswer {
		// Only an answer given away: stop there with a tutor's note.
		out.Action = FamilyWithhold
	}
	out.Violation = familyViolation(cfg, store.SecgwDirectionEgress, store.SecgwActionBlocked, counted, len(chunk))
	return out
}

// FamilyJudgeToolOutput judges tool output (a fetched page, search
// results, a transcript) that is about to be shown to a covered child's
// model: as content shown to the child, with the reply judge, never as the
// child's own words. Anything flagged blocks (the caller withholds it); the
// violation is recorded as tool output. Never a support answer.
func (e *Engine) FamilyJudgeToolOutput(ctx context.Context, userID, prompt, text string) FamilyOutcome {
	out := e.FamilyJudgeReply(ctx, userID, prompt, "", text)
	out.ToolOutput = true
	if out.Action == FamilyWithhold {
		out.Action = FamilyBlock
	}
	if out.Violation != nil {
		out.Violation.Direction = store.SecgwDirectionToolOutput
	}
	return out
}

// toolOutputText is the tool output in the new turn.
func toolOutputText(msgs []Message) string {
	from := guardTurnStart(msgs)
	var parts []string
	for _, m := range msgs {
		if m.Tool && m.Index >= from && strings.TrimSpace(m.Text) != "" {
			parts = append(parts, m.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// FamilyReplyText is the reply text a buffered response shows the user:
// every choice's content and reasoning, and legacy completions text.
func FamilyReplyText(body []byte) string {
	var parts []string
	if texts, ok := responseTexts(body); ok {
		for _, t := range texts {
			if t.Path == "content" || t.Path == "reasoning" {
				parts = append(parts, t.Text)
			}
		}
	}
	var legacy struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &legacy) == nil {
		for _, c := range legacy.Choices {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
