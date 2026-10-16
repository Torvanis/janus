package routing

import (
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/store"
)

func cfg() store.RouterConfig {
	return store.RouterConfig{
		Default: store.RouterDefault{ModelID: "frontier", Thinking: store.ThinkingPassthrough},
		Routes: []store.RouterRoute{
			{ID: "legal", ModelID: "onprem", Categories: []string{"legal"}},
			{ID: "hardcode", ModelID: "opus", Categories: []string{"coding"}, DifficultyMin: 4, Thinking: "high"},
			{ID: "easy", ModelID: "local", Categories: []string{"quick_facts", "translation"}, DifficultyMax: 2.5},
		},
	}
}

func cands(c store.RouterConfig) []Candidate {
	out := []Candidate{}
	for _, r := range c.Routes {
		out = append(out, Candidate{Route: r, Servable: true, ContextWindow: 32000})
	}
	return out
}

var minConf = map[string]float64{"legal": 0.5, "coding": 0.5, "quick_facts": 0.5, "translation": 0.5}

func TestChooseFirstMatchWins(t *testing.T) {
	c := cfg()
	d := Choose(c, cands(c), Answer{Category: "coding", Confidence: 0.9, Difficulty: 4.4}, minConf, Context{})
	if d.RouteID != "hardcode" || d.ModelID != "opus" || d.Thinking != "high" || d.Reason != store.RouteReasonMatched {
		t.Fatalf("got %+v", d)
	}
	// Skipped routes are traced with why.
	if len(d.Trace) != 2 || d.Trace[0].Outcome != "skipped" || !strings.Contains(d.Trace[0].Detail, "route wants legal") {
		t.Fatalf("trace %+v", d.Trace)
	}
}

func TestChooseDifficultyBand(t *testing.T) {
	c := cfg()
	d := Choose(c, cands(c), Answer{Category: "coding", Confidence: 0.9, Difficulty: 3.1}, minConf, Context{})
	if !d.IsDefault() || d.Reason != store.RouteReasonNoMatch {
		t.Fatalf("medium coding should fall to default, got %+v", d)
	}
	d = Choose(c, cands(c), Answer{Category: "translation", Confidence: 0.8, Difficulty: 1.4}, minConf, Context{})
	if d.RouteID != "easy" {
		t.Fatalf("easy translation should go local, got %+v", d)
	}
	d = Choose(c, cands(c), Answer{Category: "translation", Confidence: 0.8, Difficulty: 3}, minConf, Context{})
	if !d.IsDefault() {
		t.Fatalf("hard translation should stay on default, got %+v", d)
	}
}

func TestChooseLowConfidenceIsNamed(t *testing.T) {
	c := cfg()
	d := Choose(c, cands(c), Answer{Category: "legal", Confidence: 0.31}, minConf, Context{})
	if !d.IsDefault() || d.Reason != store.RouteReasonLowConfidence {
		t.Fatalf("got %+v", d)
	}
}

func TestChooseSkipsUnservableTooLargeUngrantedOverQuota(t *testing.T) {
	c := cfg()
	cs := cands(c)
	cs[0].Servable = false
	d := Choose(c, cs, Answer{Category: "legal", Confidence: 0.9}, minConf, Context{})
	if !d.IsDefault() {
		t.Fatalf("unservable route served: %+v", d)
	}
	cs = cands(c)
	d = Choose(c, cs, Answer{Category: "legal", Confidence: 0.9}, minConf, Context{EstimatedTokens: 31000})
	if !d.IsDefault() || !strings.Contains(d.Trace[0].Detail, "context window") {
		t.Fatalf("oversized request routed to small model: %+v", d)
	}
	c.Routes[0].RestrictToGranted = true
	c.Routes[0].SkipWhenOverQuota = true
	cs = cands(c)
	d = Choose(c, cs, Answer{Category: "legal", Confidence: 0.9}, minConf, Context{Granted: func(string) bool { return false }})
	if !d.IsDefault() || !strings.Contains(d.Trace[0].Detail, "not granted") {
		t.Fatalf("ungranted: %+v", d)
	}
	d = Choose(c, cs, Answer{Category: "legal", Confidence: 0.9}, minConf, Context{Granted: func(string) bool { return true }, OverQuota: func(string) bool { return true }})
	if !d.IsDefault() || !strings.Contains(d.Trace[0].Detail, "quota") {
		t.Fatalf("over quota: %+v", d)
	}
}

func TestAnyCategoryRouteAndScaleThinking(t *testing.T) {
	c := store.RouterConfig{Default: store.RouterDefault{ModelID: "opus", Thinking: "medium"},
		Routes: []store.RouterRoute{{ID: "max", ModelID: "opus", DifficultyMin: 4.5, Thinking: "max"}, {ID: "s", ModelID: "opus", Thinking: store.ThinkingScale}}}
	d := Choose(c, cands(c), Answer{Difficulty: 1.2}, nil, Context{})
	if d.RouteID != "s" || d.Thinking != "low" {
		t.Fatalf("got %+v", d)
	}
	d = Choose(c, cands(c), Answer{Difficulty: 4.8}, nil, Context{})
	if d.RouteID != "max" || d.Thinking != "max" {
		t.Fatalf("got %+v", d)
	}
	if NeedsCategory(c) || !NeedsDifficulty(c) || !NeedsDecision(c) {
		t.Fatal("needs")
	}
	if got := Thinking(store.ThinkingPassthrough, 5); got != "" {
		t.Fatalf("passthrough = %q", got)
	}
}

func TestInterpretExpectedDifficultyAndCodeOverride(t *testing.T) {
	res := classify.Result{Answers: map[string]classify.Answer{
		QuestionCategory:       {Choice: "coding", Confidence: 0.7, Probabilities: map[string]float64{"coding": 0.82, "none": 0.18}},
		QuestionDifficulty:     {Choice: "l2", Probabilities: map[string]float64{"l1": 0.5, "l2": 0.5}},
		QuestionCodeDifficulty: {Choice: "l4", Probabilities: map[string]float64{"l4": 1}},
	}}
	a := Interpret(res)
	if a.Category != "coding" || a.Confidence != 0.82 || a.Difficulty != 4 {
		t.Fatalf("got %+v", a)
	}
	res.Answers[QuestionCategory] = classify.Answer{Choice: "writing", Probabilities: map[string]float64{"writing": 1}}
	if a := Interpret(res); a.Difficulty != 1.5 {
		t.Fatalf("non-coding should use general scale, got %v", a.Difficulty)
	}
}

func TestQuestionsOfferEveryCategoryPlusNone(t *testing.T) {
	cats := CategoriesFrom([]store.RoutingCategory{
		{Slug: "legal", Name: "Legal", Includes: "law", Excludes: "forms", Enabled: true},
		{Slug: "off", Name: "Off", Includes: "x", Enabled: false},
		{Slug: "coding", Name: "Coding", LinkedSlug: "coding", Enabled: true},
	}, map[string][2]string{"coding": {"Coding", "Writing source code."}})
	q := CategoryQuestion(cats)
	if len(q.Options) != 3 || q.Options[2].Slug != store.RoutingCategoryNone {
		t.Fatalf("options %+v", q.Options)
	}
	if q.Options[0].Description != "law NOT: forms" || q.Options[1].Description != "Writing source code." {
		t.Fatalf("descriptions %+v", q.Options)
	}
	qs := Questions(cfg(), cats, true)
	if len(qs) != 3 {
		t.Fatalf("want category + 2 difficulty questions, got %d", len(qs))
	}
}

func TestByRouteIDSticky(t *testing.T) {
	c := cfg()
	d, ok := ByRouteID(c, cands(c), "easy", 0, Context{})
	if !ok || d.ModelID != "local" || d.Reason != store.RouteReasonSticky {
		t.Fatalf("got %+v", d)
	}
	if _, ok := ByRouteID(c, cands(c), "gone", 0, Context{}); ok {
		t.Fatal("deleted route must not be sticky")
	}
	d, _ = ByRouteID(c, cands(c), DefaultRouteID, 0, Context{})
	if !d.IsDefault() {
		t.Fatal("default sticky")
	}
}
