// Package routing decides which route of an auto-routing router serves a
// request. It is pure: the classifier call, grants, quotas and the catalog
// are supplied by the caller, so every rule here is unit-testable and the
// same code answers live requests, shadow traffic and the admin's Try box.
//
// A router asks a decision classifier at most two questions about the new
// turn of a request:
//
//   - category: one flat choice over every enabled routing category plus
//     "none of these". Every category is offered, not only the ones the
//     router uses, so the classifier has the near neighbours it needs to
//     tell them apart (offering only "Legal" and "none" pulls borderline
//     requests into Legal).
//   - difficulty: a five-level complexity scale, asked only when some route
//     depends on it. Its answer is the expected level (sum of level x
//     probability), a number between 1 and 5.
//
// Routes are then tried in order; the first that fits serves:
//
//	model servable -> categories match (with the category's minimum
//	confidence) -> difficulty within the route's band -> request fits the
//	model's context window -> caller granted (when the route requires it)
//	-> caller under quota on the model (when the route skips on quota)
//
// Nothing fitting means the default serves. A failed, slow or absent
// classifier answer means the default serves; a router never fails a
// request because it could not decide.
package routing

import (
	"math"
	"strconv"
	"strings"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/store"
)

// Question ids. They are namespaced so they can share a decision call with
// the Security Gateway's guard questions and Usage insights.
const (
	QuestionCategory   = "routing_category"
	QuestionDifficulty = "routing_difficulty"
	// QuestionCodeDifficulty is the coding-specific scale, asked alongside
	// the general one when the difficulty style is "category".
	QuestionCodeDifficulty = "routing_difficulty_code"
)

// DefaultRouteID identifies the default route on decisions.
const DefaultRouteID = "default"

const categoryInstructions = "Which kind of work does this request to an AI assistant ask for? Judge the request itself, not the topic of any pasted material it refers to."

const difficultyInstructions = "How complex is this request for an AI assistant to answer well?"

// Levels of the general difficulty question (measured AUC 0.83-0.85 at
// separating prompts a small local model answers well from those that need
// a frontier model).
var difficultyLevels = []classify.Option{
	{Slug: "l1", Name: "Trivial", Description: "a quick fact, a short rewrite or a routine one-step task anyone could do."},
	{Slug: "l2", Name: "Easy", Description: "routine work a competent generalist does without special knowledge."},
	{Slug: "l3", Name: "Moderate", Description: "needs solid knowledge of the field or several careful steps."},
	{Slug: "l4", Name: "Hard", Description: "needs real expertise, multi-step reasoning, or weighing subtle trade-offs."},
	{Slug: "l5", Name: "Expert", Description: "would challenge a top specialist; deep reasoning, novel analysis or subtle correctness."},
}

// Levels of the coding difficulty question (AUC 0.87-0.89 on coding work).
var codeLevels = []classify.Option{
	{Slug: "l1", Name: "Trivial", Description: "one-line fix, syntax question, or a tiny snippet."},
	{Slug: "l2", Name: "Easy", Description: "a short self-contained function or a common bug with an obvious cause."},
	{Slug: "l3", Name: "Moderate", Description: "a non-trivial function or module, or a bug that needs some tracing."},
	{Slug: "l4", Name: "Hard", Description: "multi-part design, concurrency, performance or a subtle bug across components."},
	{Slug: "l5", Name: "Expert", Description: "architecture-level design, tricky correctness (lock-free, numerical, security) or deep debugging with little information."},
}

// Category is one routing category as the classifier sees it.
type Category struct {
	Slug          string
	Name          string
	Description   string
	MinConfidence float64
}

// CategoriesFrom turns stored routing categories into classifier options.
// Linked categories take their description from the Usage-insights category
// they point at (insights maps slug -> name, description).
func CategoriesFrom(cats []store.RoutingCategory, insights map[string][2]string) []Category {
	out := []Category{}
	for _, c := range cats {
		if !c.Enabled {
			continue
		}
		cat := Category{Slug: c.Slug, Name: c.Name, MinConfidence: c.MinConfidence}
		if c.LinkedSlug != "" {
			if ins, ok := insights[c.LinkedSlug]; ok {
				cat.Description = ins[1]
				if cat.Name == "" {
					cat.Name = ins[0]
				}
			}
		} else {
			cat.Description = c.Includes
			if c.Excludes != "" {
				cat.Description += " NOT: " + c.Excludes
			}
		}
		out = append(out, cat)
	}
	return out
}

// CategoryQuestion is the flat choice over every category plus "none".
func CategoryQuestion(cats []Category) classify.Question {
	opts := make([]classify.Option, 0, len(cats)+1)
	for _, c := range cats {
		opts = append(opts, classify.Option{Slug: c.Slug, Name: c.Name, Description: c.Description})
	}
	opts = append(opts, classify.Option{Slug: store.RoutingCategoryNone, Name: "None of these", Description: "The request fits none of the other options."})
	return classify.Question{ID: QuestionCategory, Instructions: categoryInstructions, Options: opts}
}

// DifficultyQuestion is the general five-level complexity question.
func DifficultyQuestion() classify.Question {
	return classify.Question{ID: QuestionDifficulty, Instructions: difficultyInstructions, Options: difficultyLevels}
}

// CodeDifficultyQuestion is the coding-specific five-level question.
func CodeDifficultyQuestion() classify.Question {
	return classify.Question{ID: QuestionCodeDifficulty, Instructions: "How complex is this coding task?", Options: codeLevels}
}

// NeedsDifficulty reports whether any route depends on difficulty.
func NeedsDifficulty(cfg store.RouterConfig) bool {
	if cfg.Default.Thinking == store.ThinkingScale {
		return true
	}
	for _, r := range cfg.Routes {
		if r.DifficultyMin > 0 || r.DifficultyMax > 0 || r.Thinking == store.ThinkingScale {
			return true
		}
	}
	return false
}

// NeedsCategory reports whether any route depends on the category.
func NeedsCategory(cfg store.RouterConfig) bool {
	for _, r := range cfg.Routes {
		if len(r.Categories) > 0 {
			return true
		}
	}
	return false
}

// NeedsDecision reports whether the router has anything to decide: a
// router with no routes always serves its default.
func NeedsDecision(cfg store.RouterConfig) bool {
	return len(cfg.Routes) > 0 && (NeedsCategory(cfg) || NeedsDifficulty(cfg))
}

// Questions returns the questions a router needs answered.
func Questions(cfg store.RouterConfig, cats []Category, codeStyle bool) []classify.Question {
	var qs []classify.Question
	if NeedsCategory(cfg) {
		qs = append(qs, CategoryQuestion(cats))
	}
	if NeedsDifficulty(cfg) {
		qs = append(qs, DifficultyQuestion())
		if codeStyle {
			qs = append(qs, CodeDifficultyQuestion())
		}
	}
	return qs
}

// Answer is what the classifier said, reduced to what routing uses.
type Answer struct {
	// Category is the chosen category slug ("none" or "" when not asked).
	Category string
	// Confidence is the probability of Category.
	Confidence float64
	// Probabilities are every category's probability.
	Probabilities map[string]float64
	// Difficulty is the expected level 1-5 (0 = not asked).
	Difficulty float64
}

// expectedLevel turns a level distribution into its expected value.
func expectedLevel(a classify.Answer) float64 {
	if len(a.Probabilities) == 0 {
		if n, err := strconv.Atoi(strings.TrimPrefix(a.Choice, "l")); err == nil {
			return float64(n)
		}
		return 0
	}
	sum, total := 0.0, 0.0
	for k, p := range a.Probabilities {
		n, err := strconv.Atoi(strings.TrimPrefix(k, "l"))
		if err != nil {
			continue
		}
		sum += float64(n) * p
		total += p
	}
	if total == 0 {
		return 0
	}
	return math.Round(sum/total*100) / 100
}

// Interpret extracts the routing answer from a classifier result.
func Interpret(res classify.Result) Answer {
	var out Answer
	if a, ok := res.Answers[QuestionCategory]; ok {
		out.Category, out.Probabilities = a.Choice, a.Probabilities
		out.Confidence = a.Confidence
		if p, ok := a.Probabilities[a.Choice]; ok {
			out.Confidence = p
		}
	}
	if a, ok := res.Answers[QuestionDifficulty]; ok {
		out.Difficulty = expectedLevel(a)
	}
	if a, ok := res.Answers[QuestionCodeDifficulty]; ok && out.Category == "coding" {
		if d := expectedLevel(a); d > 0 {
			out.Difficulty = d
		}
	}
	return out
}

// Candidate is a route's runtime view: the configuration plus what the
// caller supplies about the target model.
type Candidate struct {
	Route store.RouterRoute
	// Servable is false when the target is missing or disabled.
	Servable bool
	// ContextWindow of the target model (0 = unknown).
	ContextWindow int64
}

// Context is what selection knows about the request.
type Context struct {
	// EstimatedTokens is a rough size of the request (bytes / 4).
	EstimatedTokens int64
	// Granted reports whether the caller holds a grant on a route's model
	// (consulted only for routes with RestrictToGranted).
	Granted func(modelID string) bool
	// OverQuota reports whether the caller is over a quota on a route's
	// model (consulted only for routes with SkipWhenOverQuota).
	OverQuota func(modelID string) bool
}

// Step is one line of the decision trace the admin's Try box shows.
type Step struct {
	RouteID string `json:"route_id"`
	Index   int    `json:"index"`
	Outcome string `json:"outcome"` // matched, skipped
	Detail  string `json:"detail"`
}

// Decision is which route serves and why.
type Decision struct {
	// RouteID is the chosen route's id, or DefaultRouteID.
	RouteID string
	// Index is the chosen route's position (-1 for the default).
	Index int
	// ModelID is the model or managed model that serves.
	ModelID string
	// Thinking is the reasoning effort to send ("" = leave the request's).
	Thinking string
	// Reason is a store.RouteReason* value.
	Reason string
	Trace  []Step
}

// IsDefault reports whether the default serves.
func (d Decision) IsDefault() bool { return d.RouteID == DefaultRouteID }

// categoryMatches reports whether the answer counts as one of slugs.
func categoryMatches(route store.RouterRoute, ans Answer, minConf map[string]float64) (bool, string) {
	if len(route.Categories) == 0 {
		return true, "any category"
	}
	if ans.Category == "" {
		return false, "category was not classified"
	}
	for _, slug := range route.Categories {
		if slug != ans.Category {
			continue
		}
		min := minConf[slug]
		if ans.Confidence < min {
			return false, "category " + slug + " at " + pct(ans.Confidence) + " is below its " + pct(min) + " minimum"
		}
		return true, "category " + slug + " at " + pct(ans.Confidence)
	}
	return false, "category is " + ans.Category + ", route wants " + strings.Join(route.Categories, ", ")
}

func pct(v float64) string { return strconv.Itoa(int(math.Round(v*100))) + "%" }

func difficultyFits(route store.RouterRoute, d float64) (bool, string) {
	if route.DifficultyMin == 0 && route.DifficultyMax == 0 {
		return true, ""
	}
	if d == 0 {
		return false, "difficulty was not classified"
	}
	ds := strconv.FormatFloat(d, 'f', 1, 64)
	if route.DifficultyMin > 0 && d < route.DifficultyMin {
		return false, "difficulty " + ds + " is below " + strconv.FormatFloat(route.DifficultyMin, 'f', 1, 64)
	}
	if route.DifficultyMax > 0 && d > route.DifficultyMax {
		return false, "difficulty " + ds + " is above " + strconv.FormatFloat(route.DifficultyMax, 'f', 1, 64)
	}
	return true, "difficulty " + ds
}

// Choose picks the route for an answer. minConf maps category slug to its
// minimum confidence.
func Choose(cfg store.RouterConfig, cands []Candidate, ans Answer, minConf map[string]float64, rc Context) Decision {
	var trace []Step
	lowConfidence := false
	for i, c := range cands {
		r := c.Route
		skip := func(why string) {
			trace = append(trace, Step{RouteID: r.ID, Index: i, Outcome: "skipped", Detail: why})
		}
		if !c.Servable {
			skip("its model is not available")
			continue
		}
		ok, why := categoryMatches(r, ans, minConf)
		if !ok {
			if strings.Contains(why, "below its") {
				lowConfidence = true
			}
			skip(why)
			continue
		}
		dok, dwhy := difficultyFits(r, ans.Difficulty)
		if !dok {
			skip(dwhy)
			continue
		}
		if ok, why := servesRequest(c, rc); !ok {
			skip(why)
			continue
		}
		detail := why
		if dwhy != "" {
			detail += ", " + dwhy
		}
		trace = append(trace, Step{RouteID: r.ID, Index: i, Outcome: "matched", Detail: detail})
		return Decision{RouteID: r.ID, Index: i, ModelID: r.ModelID, Thinking: Thinking(r.Thinking, ans.Difficulty),
			Reason: store.RouteReasonMatched, Trace: trace}
	}
	reason := store.RouteReasonNoMatch
	if lowConfidence {
		reason = store.RouteReasonLowConfidence
	}
	return Default(cfg, ans.Difficulty, reason, trace)
}

// Default is the decision that serves the router's default.
func Default(cfg store.RouterConfig, difficulty float64, reason string, trace []Step) Decision {
	trace = append(trace, Step{RouteID: DefaultRouteID, Index: -1, Outcome: "matched", Detail: "default"})
	return Decision{RouteID: DefaultRouteID, Index: -1, ModelID: cfg.Default.ModelID, Thinking: Thinking(cfg.Default.Thinking, difficulty),
		Reason: reason, Trace: trace}
}

// servesRequest applies the per-request checks a route must pass whatever
// the classifier said: the request fits the model's context window, the
// caller is granted the model when the route requires it, and the caller is
// not over a quota on it when the route skips itself then. Choose and
// sticky turns share it, so a remembered conversation is held to the same
// rules as a fresh decision (a revoked grant takes effect on the next turn).
func servesRequest(c Candidate, rc Context) (bool, string) {
	r := c.Route
	if c.ContextWindow > 0 && rc.EstimatedTokens > c.ContextWindow*9/10 {
		return false, "request (~" + strconv.FormatInt(rc.EstimatedTokens, 10) + " tokens) is too large for its " + strconv.FormatInt(c.ContextWindow, 10) + "-token context window"
	}
	if r.RestrictToGranted && rc.Granted != nil && !rc.Granted(r.ModelID) {
		return false, "caller is not granted this route's model"
	}
	if r.SkipWhenOverQuota && rc.OverQuota != nil && rc.OverQuota(r.ModelID) {
		return false, "caller is over a quota on this route's model"
	}
	return true, ""
}

// ByRouteID returns the decision for a remembered route (stickiness). A
// route that no longer exists, has become unservable, or no longer serves
// this request (it outgrew the context window, the caller lost the grant a
// restricted route needs, or is over a quota the route skips on) is not
// sticky: the caller then decides afresh.
func ByRouteID(cfg store.RouterConfig, cands []Candidate, routeID string, difficulty float64, rc Context) (Decision, bool) {
	if routeID == DefaultRouteID {
		return Default(cfg, difficulty, store.RouteReasonSticky, nil), true
	}
	for i, c := range cands {
		if c.Route.ID != routeID || !c.Servable {
			continue
		}
		if ok, _ := servesRequest(c, rc); !ok {
			return Decision{}, false
		}
		return Decision{RouteID: routeID, Index: i, ModelID: c.Route.ModelID, Thinking: Thinking(c.Route.Thinking, difficulty),
			Reason: store.RouteReasonSticky, Trace: []Step{{RouteID: routeID, Index: i, Outcome: "matched", Detail: "same conversation as an earlier turn"}}}, true
	}
	return Decision{}, false
}

// Thinking resolves a route's thinking setting for a request: passthrough
// leaves the caller's choice (""), scale maps difficulty to an effort, and a
// fixed level is sent as is.
func Thinking(setting string, difficulty float64) string {
	switch setting {
	case "", store.ThinkingPassthrough:
		return ""
	case store.ThinkingScale:
		switch {
		case difficulty == 0:
			return ""
		case difficulty < 2.5:
			return "low"
		case difficulty < 3.5:
			return "medium"
		default:
			return "high"
		}
	}
	return setting
}
