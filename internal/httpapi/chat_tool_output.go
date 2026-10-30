package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/store"
)

// Tool output in Chat — a page read_page fetched, Janus's web search
// results, the transcript or description of an attached file — is what a
// tool fed the model, not the person's words. It is judged here, when the
// tool makes it, with the person's effective policy:
//
//   - Child Safety (a covered person): as content shown to the child, with
//     the reply judge. Never a support answer, never a parent notice.
//   - Security Gateway: prompt injection and content safety.
//
// What is flagged is withheld from the model (a short notice takes its
// place) and the reply shows a "withheld" activity; the reply itself goes
// on. Violations are recorded as tool output, never as the person's
// words, and the proxy is told (secgw.WithToolOutputJudged) so it does not
// judge the same text again as the person's message.
//
// Provider-run tools (hosted web search, web_fetch) fetch upstream; only
// the reply they lead to is judged.

// chatToolOutputJudged marks a Chat proxy request whose tool output was
// judged here (see secgw.WithToolOutputJudged).
func chatToolOutputJudged(ctx context.Context) context.Context {
	return secgw.WithToolOutputJudged(ctx)
}

// chatActivityWithheld is the activity a withheld tool result shows.
const chatActivityWithheld = "withheld"

// chatToolJudge judges tool output for one reply run.
type chatToolJudge struct {
	s    *Server
	user *store.User
	plan chatRunPlan
	live *chatLiveRun
	eng  *secgw.Engine
	eff  *secgw.Effective // model-backed ingress checks only; nil when none
}

// chatToolJudgeFor prepares the judge, or nil when nothing judges tool
// output for this person (no engine, no Child Safety, no model check).
func (s *Server) chatToolJudgeFor(ctx context.Context, user *store.User, plan chatRunPlan, live *chatLiveRun) *chatToolJudge {
	eng, err := s.secgwEngine(ctx)
	if err != nil || eng == nil || user == nil {
		return nil
	}
	j := &chatToolJudge{s: s, user: user, plan: plan, live: live, eng: eng}
	groupIDs, _, _ := s.principalContext(ctx, user)
	resolved, _ := s.cachedResolveModel(ctx, plan.Model)
	eff := eng.Resolve(secgwSubject(&store.UsageEvent{UserID: user.ID}, groupIDs, resolved))
	if !eff.Empty() {
		only := &secgw.Effective{Checks: map[store.SecgwCheckKind]secgw.ResolvedCheck{}, Policy: eff.Policy}
		for _, k := range []store.SecgwCheckKind{store.SecgwCheckPromptInjection, store.SecgwCheckContentSafety} {
			if c, ok := eff.Has(k, store.SecgwDirectionIngress); ok {
				only.Checks[k] = c
			}
		}
		if !only.Empty() {
			j.eff = only
		}
	}
	if j.eff == nil && !eng.FamilyCovers(user.ID) {
		return nil
	}
	return j
}

// judge returns text, or the withheld notice in its place. what names the
// result for the model and the person ("the page at https://…").
func (j *chatToolJudge) judge(ctx context.Context, what, label, text string) string {
	return j.judgeAs(ctx, what, label, text, true)
}

// judgeAs is judge; record=false withholds without recording again.
func (j *chatToolJudge) judgeAs(ctx context.Context, what, label, text string, record bool) string {
	if j == nil || strings.TrimSpace(text) == "" {
		return text
	}
	if j.eng.FamilyCovers(j.user.ID) {
		out := j.eng.FamilyJudgeToolOutput(ctx, j.user.ID, j.plan.Question, text)
		if out.Err != nil {
			j.s.Logger.WarnContext(ctx, "chat: child safety could not judge tool output, withholding it", "error", out.Err.Error())
		}
		if out.Action != secgw.FamilyAllow {
			if out.Violation != nil && record {
				j.record(ctx, nil, []secgw.Violation{*out.Violation})
			}
			reason := "it is not suitable here"
			if out.Failed {
				reason = "it could not be checked"
			}
			return j.withhold(what, label, reason)
		}
	}
	if j.eff == nil {
		return text
	}
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "tool", "content": text}}})
	dec, err := j.eng.Ingress(ctx, j.eff, secgw.Request{Body: body})
	if err != nil {
		j.s.Logger.WarnContext(ctx, "chat: tool output check failed, withholding it", "error", err.Error())
		return j.withhold(what, label, "it could not be checked")
	}
	if record {
		j.record(ctx, j.eff.Policy, dec.Violations)
	}
	if dec.Action != store.SecgwActionBlocked {
		return text
	}
	reason := "it contained instructions aimed at the assistant"
	switch {
	case dec.ClassifierFailed:
		reason = "it could not be checked"
	case dec.BlockKind == store.SecgwCheckContentSafety:
		reason = "it is not allowed here"
	}
	return j.withhold(what, label, reason)
}

// withhold shows the withheld activity and returns the model's notice.
func (j *chatToolJudge) withhold(what, label, reason string) string {
	j.live.extras.addActivity(chatActivity{Type: chatActivityWithheld, Status: "done", Query: label})
	return "[" + upperFirst(what) + " was withheld: " + reason + ". Do not guess what it said; use other sources or tell the user it could not be used.]"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// record keeps tool-output violations, marked as tool output, on the
// reply's request (no usage event: nothing was sent to a model).
func (j *chatToolJudge) record(ctx context.Context, policy *store.SecgwPolicy, vs []secgw.Violation) {
	var capture store.SecgwCaptureConfig
	if policy != nil {
		capture = policy.Capture
	}
	encrypt := func(text string) (string, error) { return j.s.Cipher.Encrypt(text) }
	var rows []*store.SecgwViolation
	for _, v := range vs {
		if !v.Record {
			continue
		}
		v.Direction = store.SecgwDirectionToolOutput
		row := v.ToStore(capture, encrypt)
		row.RequestID, row.UserID, row.ModelName = j.plan.RequestID, j.user.ID, j.plan.Model
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return
	}
	if err := j.s.Store.InsertSecgwViolations(ctx, rows); err != nil {
		j.s.Logger.ErrorContext(ctx, "chat: persist tool output violations", "error", err.Error())
	}
	if j.s.Metrics != nil {
		for _, v := range vs {
			j.s.Metrics.SecgwViolations.WithLabelValues(string(v.Kind), v.Action, store.SecgwDirectionToolOutput).Inc()
		}
	}
}

// judgeMediaText judges the transcripts and descriptions of attached files
// (made now or kept from an earlier reply) in every message sent, so one
// withheld once is not shown to the model again on a later turn. Only the
// new message's verdicts are recorded; earlier ones were when they were new.
func (j *chatToolJudge) judgeMediaText(ctx context.Context, history []chatWireMessage) {
	if j == nil {
		return
	}
	last := -1
	for i := len(history) - 1; i >= 0 && last < 0; i-- {
		if history[i].Role == "user" {
			last = i
		}
	}
	for i := range history {
		parts, ok := history[i].Content.([]chatPart)
		if !ok || history[i].Role != "user" {
			continue
		}
		for k := range parts {
			name, body, ok := chatMediaTextOf(parts[k].Text)
			if !ok {
				continue
			}
			if out := j.judgeAs(ctx, "the text made of the attached file "+name, name, body, i == last); out != body {
				parts[k] = chatPart{Type: "text", Text: out}
			}
		}
	}
}

// chatMediaTextOf reads a part made by chatMediaTextPart.
func chatMediaTextOf(text string) (name, body string, ok bool) {
	head, rest, found := strings.Cut(text, "\n")
	if !found || !strings.HasPrefix(head, `<file name="`) || !strings.HasSuffix(rest, "\n</file>") ||
		!(strings.HasSuffix(head, `" kind="transcript">`) || strings.HasSuffix(head, `" kind="description">`)) {
		return "", "", false
	}
	name = strings.TrimPrefix(head, `<file name="`)
	name = name[:strings.Index(name, `" kind="`)]
	return name, strings.TrimSuffix(rest, "\n</file>"), true
}
