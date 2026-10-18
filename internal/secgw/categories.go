package secgw

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/detect"
	"github.com/torvanis/janus/internal/store"
)

// Content safety with security categories (method "categories").
//
// Instead of the fixed hazard question, a decision model is asked about
// the categories the check selects (store.SecgwCategory): built-in safety
// categories, the Family defaults and the admin's own. Each category
// counts when the classifier's probability for it reaches the category's
// minimum confidence. The legacy method (Llama Guard compatible) is
// untouched: HarmQuestion, safetyFromDecision and judgeContentSafety.

// CategoryQuestionID is the id of the "together" question (one choice
// between every selected category and "none of these"). When more
// categories are asked than one question takes (the try box asking every
// enabled category), further questions get a numeric suffix.
const CategoryQuestionID = "security_categories"

// CategoryYesNoPrefix prefixes the id of each "separately" question.
const CategoryYesNoPrefix = "seccat_"

// errNoCategories reports a categories check whose categories are all
// gone (deleted or turned off since it was saved).
var errNoCategories = errors.New("none of this check's security categories exist or are turned on")

// errCategoriesNeedDecision reports a categories check bound to a
// classifier that is not a decision model.
var errCategoriesNeedDecision = errors.New("security categories need a decision model")

// usesCategories reports whether a content-safety check asks with
// security categories.
func (o ContentSafetyOptions) usesCategories() bool { return o.Method == store.SecgwMethodCategories }

func contentSafetyOptions(c ResolvedCheck) ContentSafetyOptions {
	var o ContentSafetyOptions
	if len(c.Options) > 0 {
		_ = json.Unmarshal(c.Options, &o)
	}
	return o
}

// categoryDescription is what the classifier reads for one category:
// what it includes, then what it does not.
func categoryDescription(c *store.SecgwCategory) string {
	d := strings.TrimSpace(c.Includes)
	if x := strings.TrimSpace(c.Excludes); x != "" {
		if d != "" {
			d += " "
		}
		d += "Not: " + x
	}
	return d
}

// CategoryQuestions builds the questions that ask about cats: one choice
// question ("together", the default) or one yes/no question per category
// ("separately"). reply asks about an assistant's reply instead of a
// request. A together question carries at most store.MaxSecgwCategoriesPerCheck
// categories; more are split over several questions.
func CategoryQuestions(cats []*store.SecgwCategory, ask string, reply bool) []dec.Question {
	subject := "request to an AI assistant"
	short := "request"
	if reply {
		subject, short = "reply from an AI assistant", "reply"
	}
	var out []dec.Question
	if ask == store.SecgwAskSeparately {
		for _, c := range cats {
			no := strings.TrimSpace(c.Excludes)
			if no == "" {
				no = "An ordinary " + short + " that does not fall under " + c.Name + "."
			}
			out = append(out, dec.Question{ID: CategoryYesNoPrefix + c.Slug, Type: dec.QuestionYesNo,
				Instructions: "Does this " + short + " fall under " + c.Name + "?",
				YesMeans:     strings.TrimSpace(c.Includes), NoMeans: no})
		}
		return out
	}
	for start := 0; start < len(cats); start += store.MaxSecgwCategoriesPerCheck {
		end := min(len(cats), start+store.MaxSecgwCategoriesPerCheck)
		opts := []dec.Option{{Slug: store.SecgwCategoryNone, Name: "None of these", Description: "An ordinary " + short + "."}}
		for _, c := range cats[start:end] {
			opts = append(opts, dec.Option{Slug: c.Slug, Name: c.Name, Description: categoryDescription(c)})
		}
		id := CategoryQuestionID
		if start > 0 {
			id += "_" + strconv.Itoa(start/store.MaxSecgwCategoriesPerCheck+1)
		}
		out = append(out, dec.Question{ID: id, Type: dec.QuestionChoice,
			Instructions: "Which of these categories does this " + subject + " fall under?", Options: opts})
	}
	return out
}

// CategoryProbabilities reads one call's answers to CategoryQuestions: the
// probability of each category. A question with no answer leaves its
// categories out (callers treat a missing answer as a failure).
func CategoryProbabilities(res dec.Result, cats []*store.SecgwCategory, ask string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, q := range CategoryQuestions(cats, ask, false) {
		a, ok := res.Answers[q.ID]
		if !ok {
			if err := res.MissingErr([]string{q.ID}); err != nil {
				return nil, err
			}
			return nil, dec.ErrNoAnswer
		}
		if q.IsYesNo() {
			out[strings.TrimPrefix(q.ID, CategoryYesNoPrefix)] = a.Yes
			continue
		}
		if len(a.Probabilities) == 0 {
			// A server that reports only the choice: its confidence is
			// the chosen category's probability.
			for _, o := range q.Options {
				if o.Slug == a.Choice && o.Slug != store.SecgwCategoryNone {
					out[o.Slug] = a.Confidence
				}
			}
			continue
		}
		for _, o := range q.Options {
			if o.Slug != store.SecgwCategoryNone {
				out[o.Slug] = a.Probabilities[o.Slug]
			}
		}
	}
	return out, nil
}

// CategoryScore is one category's probability and whether it counts.
type CategoryScore struct {
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	Probability   float64 `json:"probability"`
	MinConfidence float64 `json:"min_confidence"`
	Counts        bool    `json:"counts"`
}

// ScoreCategories applies each category's minimum confidence, ordered by
// probability (highest first, then name).
func ScoreCategories(probs map[string]float64, cats []*store.SecgwCategory) []CategoryScore {
	out := make([]CategoryScore, 0, len(cats))
	for _, c := range cats {
		p := probs[c.Slug]
		out = append(out, CategoryScore{Slug: c.Slug, Name: c.Name, Probability: p, MinConfidence: c.MinConfidence, Counts: p >= c.MinConfidence})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Probability != out[j].Probability {
			return out[i].Probability > out[j].Probability
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// checkCategories resolves a categories check's slugs against the
// snapshot, in the check's order, skipping any that are gone.
func (e *Engine) checkCategories(o ContentSafetyOptions) []*store.SecgwCategory {
	var out []*store.SecgwCategory
	seen := map[string]bool{}
	for _, slug := range o.Safety {
		if seen[slug] {
			continue
		}
		seen[slug] = true
		if e.snap == nil || e.snap.Categories == nil {
			continue
		}
		if c := e.snap.Categories[slug]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// categoriesFromDecision turns a categories check's answers (one group per
// window) into at most one violation: the categories that count, ordered
// by probability, max per category across windows.
func (e *Engine) categoriesFromDecision(c ResolvedCheck, direction string, msgs []Message, dc *decisionCheck, cats []*store.SecgwCategory, ask string) checkOutcome {
	if len(dc.groups) == 0 {
		return checkOutcome{ran: true}
	}
	best := map[string]float64{}
	for _, g := range dc.groups {
		res, err := g.memberResult(dc.timeout)
		if err != nil {
			return checkOutcome{failed: true, err: err, ran: true}
		}
		probs, err := CategoryProbabilities(res, cats, ask)
		if err != nil {
			return checkOutcome{failed: true, err: err, ran: true}
		}
		for slug, p := range probs {
			best[slug] = max(best[slug], p)
		}
	}
	var counted []CategoryScore
	for _, s := range ScoreCategories(best, cats) {
		if s.Counts {
			counted = append(counted, s)
		}
	}
	if len(counted) == 0 {
		return checkOutcome{ran: true}
	}
	var m Message
	if direction == store.SecgwDirectionEgress {
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
	slugs := make([]string, len(counted))
	for i, s := range counted {
		slugs[i] = s.Slug
	}
	action := store.SecgwActionObserved
	if direction == store.SecgwDirectionIngress && c.Mode == store.SecgwModeBlock {
		action = store.SecgwActionBlocked
	}
	return checkOutcome{ran: true, vs: []Violation{{Kind: c.Kind, RuleID: strings.Join(slugs, ","), Severity: string(detect.SeverityHigh),
		Direction: direction, Action: action, MessageIndex: m.Index, Text: m.Text, Length: len(m.Text), Record: true,
		PolicyID: c.PolicyID, BindingID: c.BindingID, ClassifierScore: counted[0].Probability, ClassifierModel: c.ClassifierModelID}}}
}

// egressCategoriesFromDecision asks a categories check's questions about
// an assistant's reply. Observe-only, like every egress content-safety
// verdict.
func (e *Engine) egressCategoriesFromDecision(ctx context.Context, c ResolvedCheck, d decisionClassifier, msgs []Message, o ContentSafetyOptions) ([]Violation, error) {
	cats := e.checkCategories(o)
	if len(cats) == 0 {
		return nil, errNoCategories
	}
	reply := msgs[len(msgs)-1].Text
	texts := []string{dec.TrimText(reply, guardTextMaxChars)}
	if coverageOf(c).LongText == store.SecgwLongTextWindows {
		texts = classify.Chunk(reply, guardTextMaxChars, guardWindowOverlap)
		if len(texts) > guardMaxWindows {
			return nil, errTextTooLong
		}
	}
	timeout := safetyTimeout(c)
	qs := CategoryQuestions(cats, o.Ask, true)
	dc := &decisionCheck{timeout: timeout}
	var wg sync.WaitGroup
	for _, t := range texts {
		g := &decisionGroup{cl: d, text: t, questions: qs, timeout: timeout}
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
	out := e.categoriesFromDecision(c, store.SecgwDirectionEgress, msgs, dc, cats, o.Ask)
	return out.vs, out.err
}

// CategoryCalls is how many classifier calls one categories check makes
// per text it judges: one together, one per category separately (sent in
// parallel). The policy editor shows the same arithmetic.
func CategoryCalls(n int, ask string) int {
	if ask == store.SecgwAskSeparately {
		return n
	}
	if n == 0 {
		return 0
	}
	return (n + store.MaxSecgwCategoriesPerCheck - 1) / store.MaxSecgwCategoriesPerCheck
}
