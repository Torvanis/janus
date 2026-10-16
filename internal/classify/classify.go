// Package classify labels requests with a category from an admin-curated
// taxonomy, for usage reporting today and request routing later.
//
// Two protocols reach a classifier:
//
//   - System One (TypeSafe Jev, Bespoke Nimble, Laya, and any server that
//     speaks POST /v1/systemone): the classifier scores every option and
//     returns calibrated probabilities. No text is generated, so it is fast
//     and its confidence is meaningful.
//   - Chat: any chat model, through the gateway's normal upstream adapters,
//     is shown the taxonomy and asked for a JSON answer. It works with every
//     provider (useful for small deployments with no decision model), at the
//     cost of generation latency and a self-reported confidence.
//
// The engine runs either inline (the label is ready before the request is
// forwarded, within a time budget) or async (a bounded background queue, so
// reporting never adds latency). A classifier failure never fails a request.
package classify

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Option is one answer the classifier may pick.
type Option struct {
	Slug        string
	Name        string
	Description string
}

// Question types.
const (
	// QuestionChoice asks which one of Options best describes the text.
	QuestionChoice = "choice"
	// QuestionYesNo asks a yes/no question and returns the probability of
	// yes (System One "noul"). Options are unused.
	QuestionYesNo = "noul"
)

// Question is one question for a classifier.
type Question struct {
	ID           string
	Type         string // QuestionChoice (default) or QuestionYesNo
	Instructions string
	// YesMeans and NoMeans spell out a yes/no question's boundary. Decision
	// models were measurably more accurate with both written out than
	// with the question alone.
	YesMeans string
	NoMeans  string
	Options  []Option
	// Optional marks a question another feature added to a call that is
	// being made anyway (Usage insights or Auto-routing riding on a
	// guard's call). A bad answer to an optional question is recorded in
	// Result.Missing instead of failing the whole call, so a reporting
	// question can never fail the guard it rode on.
	Optional bool
}

// IsYesNo reports whether q is a yes/no question.
func (q Question) IsYesNo() bool { return q.Type == QuestionYesNo }

// Answer is the classifier's answer to one question. A choice question sets
// Choice, Confidence and Probabilities; a yes/no question sets Yes, the
// probability of yes.
type Answer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
	Yes           float64
}

// Usage is what one classifier call consumed, when the provider reports it.
type Usage struct {
	TokensIn  int64
	TokensOut int64
	// TokensCached is the cached share of TokensIn (OpenAI Decisions
	// reports it), priced at the cached rate like a served decision.
	TokensCached int64
	// CostUSD is set when the provider reports a price (OpenRouter does).
	CostUSD float64
	// CostReported says the provider reported a price, even $0: a
	// reported zero is a price, not a missing one.
	CostReported bool
}

// Result is one classifier call.
type Result struct {
	Answers map[string]Answer
	Usage   Usage
	// Model is the model the provider says answered (e.g. jev-1.13.0).
	Model string
	// Calls is the number of requests the backend made to produce this
	// result (one per question in the separate call style).
	Calls int
	// Missing holds, per optional question id, why it has no usable
	// answer (an invalid choice, a refusal, too many options, or its own
	// request failing in the separate style). Required questions never
	// appear here: their failure fails the call.
	Missing map[string]error
}

// MissingErr returns the first reason one of ids has no usable answer.
func (r Result) MissingErr(ids []string) error {
	for _, id := range ids {
		if err, ok := r.Missing[id]; ok {
			return err
		}
	}
	return nil
}

func (r *Result) miss(id string, err error) {
	if r.Missing == nil {
		r.Missing = map[string]error{}
	}
	r.Missing[id] = err
}

// Backend classifies one text against one or more questions.
type Backend interface {
	Protocol() string
	Classify(ctx context.Context, text string, questions []Question) (Result, error)
}

// ErrNoAnswer is returned when a classifier answered but did not pick a
// valid option. An unparseable answer is an error, never a silent "other".
var ErrNoAnswer = errors.New("classifier returned no valid choice")

// Taxonomy is the enabled category tree handed to the engine.
type Taxonomy struct {
	Roots    []Option
	Children map[string][]Option
}

// Empty reports that there is nothing to classify against.
func (t Taxonomy) Empty() bool { return len(t.Roots) == 0 }

// Count is the number of options a flat question over the whole tree has:
// every root, plus every child of a root.
func (t Taxonomy) Count() int {
	n := len(t.Roots)
	for _, r := range t.Roots {
		n += len(t.Children[r.Slug])
	}
	return n
}

// FlatSeparator joins a parent and child slug in a flat option key.
const FlatSeparator = "|"

// FlatQuestion asks for the category and subcategory in ONE question by
// offering every leaf: each root as itself ("Coding": none of its
// subcategories) and each child as "Coding > Rust". Measured on decision
// models this was the most accurate way to ask, and it is one call; the
// caller must check the classifier accepts Count() options.
func (t Taxonomy) FlatQuestion() Question {
	var opts []Option
	for _, r := range t.Roots {
		opts = append(opts, r)
		for _, k := range t.Children[r.Slug] {
			desc := k.Description
			if desc == "" {
				desc = k.Name
			}
			opts = append(opts, Option{Slug: r.Slug + FlatSeparator + k.Slug, Name: r.Name + " > " + k.Name, Description: desc})
		}
	}
	return Question{ID: "category", Instructions: categoryInstructions, Options: opts}
}

// SplitFlat turns a flat option key into category and subcategory.
func SplitFlat(key string) (string, string) {
	if i := strings.Index(key, FlatSeparator); i >= 0 {
		return key[:i], key[i+len(FlatSeparator):]
	}
	return key, ""
}

const categoryInstructions = "What is the main kind of work this request to an AI assistant asks for? Judge the request itself, not the topic of any pasted material it refers to."

// HasSlug reports whether slug is an option among options.
func HasSlug(options []Option, slug string) bool {
	for _, o := range options {
		if o.Slug == slug {
			return true
		}
	}
	return false
}

// CategoryQuestion asks for the top-level category. A parent's description
// names its subcategories ("Includes: Reverse engineering, …"): the
// classifier picks the parent first, so it must know that work belongs
// there rather than in a neighbour it superficially resembles.
func (t Taxonomy) CategoryQuestion() Question {
	opts := make([]Option, len(t.Roots))
	for i, r := range t.Roots {
		opts[i] = r
		if kids := t.Children[r.Slug]; len(kids) > 0 {
			names := make([]string, len(kids))
			for j, k := range kids {
				names[j] = k.Name
			}
			desc := strings.TrimRight(r.Description, ". ")
			if desc != "" {
				desc += ". "
			}
			opts[i].Description = desc + "Includes: " + strings.Join(names, ", ") + "."
		}
	}
	return Question{ID: "category", Instructions: categoryInstructions, Options: opts}
}

// SubcategoryQuestion asks which refinement of parent applies. "none" is
// always offered so a parent with partial coverage is not forced to a child.
func (t Taxonomy) SubcategoryQuestion(parent Option) (Question, bool) {
	kids := t.Children[parent.Slug]
	if len(kids) == 0 {
		return Question{}, false
	}
	opts := append(append([]Option{}, kids...), Option{Slug: "none", Name: "None of these", Description: "None of the more specific options applies."})
	return Question{ID: "subcategory", Instructions: "This request was classified as " + parent.Name + ". Which more specific kind is it?", Options: opts}, true
}

// Timing records how long a classification took.
type Timing struct {
	Queue   time.Duration
	Elapsed time.Duration
}

// TrimText keeps the most recent maxChars characters, cutting at a rune
// boundary: the latest turn is what the request is about.
func TrimText(text string, maxChars int) string { return trimText(text, maxChars) }

func trimText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return "…" + string(runes[len(runes)-maxChars:])
}

// sanitizeError keeps errors short and free of credentials or bodies.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.Index(msg, "Bearer "); i >= 0 {
		msg = msg[:i] + "Bearer [redacted]"
	}
	return TruncateUTF8(msg, 300)
}

// TruncateUTF8 cuts s to at most n bytes without splitting a character, and
// drops any invalid bytes: PostgreSQL refuses invalid UTF-8 in a text
// column, so a cut mid-character would lose the whole row.
func TruncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
