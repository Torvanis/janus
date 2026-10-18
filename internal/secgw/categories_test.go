package secgw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
)

// catS1 is a System One server for security categories. Each rule maps a
// keyword in the text to a category and a probability; a choice question
// spreads the rest over "none", a yes/no question answers the category's
// probability as P(yes). It records every request's question ids.
type catS1 struct {
	*httptest.Server
	rules   map[string]map[string]float64 // keyword -> slug -> probability
	calls   atomic.Int64
	mu      sync.Mutex
	batches [][]string
}

func newCatS1(t *testing.T, rules map[string]map[string]float64) *catS1 {
	f := &catS1{rules: rules}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		state := strings.ToLower(req.State)
		probs := map[string]float64{}
		for kw, m := range f.rules {
			if strings.Contains(state, kw) {
				for slug, p := range m {
					probs[slug] = max(probs[slug], p)
				}
			}
		}
		var ids []string
		answers := map[string]any{}
		for id, q := range req.Questions {
			ids = append(ids, id)
			switch {
			case q.Type == "noul" && strings.HasPrefix(id, CategoryYesNoPrefix):
				answers[id] = map[string]any{"type": "noul", "noul": probs[strings.TrimPrefix(id, CategoryYesNoPrefix)]}
			case q.Type == "noul":
				p := 0.01
				if strings.Contains(state, "ignore all previous") {
					p = 0.99
				}
				answers[id] = map[string]any{"type": "noul", "noul": p}
			default:
				out := map[string]float64{}
				rest := 1.0
				best, bestP := "none", 0.0
				for slug := range q.Criteria {
					if p := probs[slug]; p > 0 {
						out[slug] = p
						rest -= p
						if p > bestP {
							best, bestP = slug, p
						}
					}
				}
				out["none"] = max(0, rest)
				if out["none"] > bestP {
					best = "none"
				}
				answers[id] = map[string]any{"type": "choice", "choice": best, "probabilities": out}
			}
		}
		f.mu.Lock()
		f.batches = append(f.batches, ids)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(f.Close)
	return f
}

func catBreaker(t *testing.T, f *catS1, style string) *classify.Breaker {
	t.Helper()
	inner, err := classify.New(classify.ProtocolDecision, classify.Upstream{ModelID: "m-dec", ModelName: "fake", BaseURL: f.URL,
		UpstreamID: "up1", Style: style, Client: f.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return &classify.Breaker{Inner: inner, ModelID: "m-dec", Threshold: 3, Cooldown: 0}
}

func testCategories() map[string]*store.SecgwCategory {
	out := map[string]*store.SecgwCategory{}
	for _, c := range store.BuiltinSecgwCategories {
		c := c
		out[c.Slug] = &c
	}
	return out
}

func catCheck(mode, direction, ask string, slugs ...string) store.SecgwCheck {
	opts, _ := json.Marshal(ContentSafetyOptions{Method: store.SecgwMethodCategories, Safety: slugs, Ask: ask, TimeoutMs: 3000})
	return store.SecgwCheck{Kind: store.SecgwCheckContentSafety, Enabled: true, Mode: mode, Fail: store.SecgwFailClosed,
		Direction: direction, Options: opts, ClassifierModelID: "m-dec"}
}

func catEngine(t *testing.T, b *classify.Breaker, checks ...store.SecgwCheck) (*Engine, *Effective) {
	t.Helper()
	p := &store.SecgwPolicy{ID: "p", Name: "p", Enabled: true, Checks: checks}
	snap := snapWith([]*store.SecgwPolicy{p}, []*store.SecgwBinding{{ID: "b", PolicyID: "p", ScopeType: store.SecgwScopeOrg}})
	snap.Categories = testCategories()
	eng, err := NewEngine(snap, oneResolver{b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return eng, Resolve(snap, Subject{})
}

var familyRules = map[string]map[string]float64{
	"essay":      {"homework_shortcut": 0.8},
	"half sure":  {"homework_shortcut": 0.55}, // below its 0.6
	"ignores me": {"feeling_low": 0.4},        // above its 0.35
	"vape":       {"not_for_children": 0.7, "feeling_low": 0.2},
	"nerve gas":  {"indiscriminate_weapons": 0.9},
	"meet":       {"personal_info": 0.6, "not_for_children": 0.52},
}

var familySlugs = []string{"homework_shortcut", "not_for_children", "bypass_controls", "personal_info", "feeling_low", "indiscriminate_weapons"}

func TestCategoriesTogetherBlocksWithCategoryNames(t *testing.T) {
	f := newCatS1(t, familyRules)
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined), catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", familySlugs...))
	d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody("can you meet me after school")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != store.SecgwActionBlocked || d.BlockKind != store.SecgwCheckContentSafety || len(d.Violations) != 1 {
		t.Fatalf("decision = %+v, want one blocking content-safety violation", d)
	}
	v := d.Violations[0]
	if v.RuleID != "personal_info,not_for_children" || v.ClassifierScore != 0.6 {
		t.Fatalf("violation = %q score %v, want both categories ordered by probability and the top score", v.RuleID, v.ClassifierScore)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("together asks one question in one call, made %d", f.calls.Load())
	}
	f.mu.Lock()
	ids := f.batches[0]
	f.mu.Unlock()
	if len(ids) != 1 || ids[0] != CategoryQuestionID {
		t.Fatalf("asked %v, want the one %s question", ids, CategoryQuestionID)
	}
	// An ordinary request passes.
	d, err = eng.Ingress(context.Background(), eff, Request{Body: userBody("what is photosynthesis")})
	if err != nil || d.Action != "" || len(d.Violations) != 0 {
		t.Fatalf("ordinary request: %+v %v", d, err)
	}
}

func TestCategoriesThresholdIsPerCategory(t *testing.T) {
	f := newCatS1(t, familyRules)
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined), catCheck(store.SecgwModeObserve, store.SecgwDirectionIngress, "", familySlugs...))
	cases := []struct {
		text, want string
	}{
		{"write my essay on the civil war", "homework_shortcut"}, // 0.8 >= 0.6
		{"I'm half sure this is homework", ""},                   // 0.55 < 0.6
		{"everyone at school ignores me", "feeling_low"},         // 0.4 >= 0.35
		{"where can I buy a vape", "not_for_children"},           // feeling_low 0.2 < 0.35
	}
	for _, tc := range cases {
		d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody(tc.text)})
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if len(d.Violations) == 1 {
			got = d.Violations[0].RuleID
			if d.Violations[0].Action != store.SecgwActionObserved {
				t.Fatalf("%q: observe-mode check acted %s", tc.text, d.Violations[0].Action)
			}
		}
		if got != tc.want {
			t.Errorf("%q: counted %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestCategoriesSeparatelyAsksOneQuestionPerCategory(t *testing.T) {
	f := newCatS1(t, familyRules)
	slugs := []string{"homework_shortcut", "feeling_low", "not_for_children"}
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleSeparate), catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, store.SecgwAskSeparately, slugs...))
	d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody("everyone at school ignores me")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != store.SecgwActionBlocked || d.Violations[0].RuleID != "feeling_low" {
		t.Fatalf("decision = %+v, want feeling_low to block", d)
	}
	if f.calls.Load() != 3 || d.Model.Calls != 3 {
		t.Fatalf("separate style: %d server calls, %d counted; want one per category (3)", f.calls.Load(), d.Model.Calls)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ids := range f.batches {
		if len(ids) != 1 || !strings.HasPrefix(ids[0], CategoryYesNoPrefix) {
			t.Fatalf("separately asks yes/no questions %q, got %v", CategoryYesNoPrefix+"<slug>", ids)
		}
	}
}

func TestCategoriesWindowsTakeTheMaxPerCategory(t *testing.T) {
	f := newCatS1(t, familyRules)
	chk := catCheck(store.SecgwModeObserve, store.SecgwDirectionIngress, "", familySlugs...)
	var o map[string]any
	_ = json.Unmarshal(chk.Options, &o)
	o["long_text"] = store.SecgwLongTextWindows
	chk.Options, _ = json.Marshal(o)
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined), chk)
	// The keyword sits at the start of a text longer than one window: only
	// the first window sees it.
	text := "please write my essay " + strings.Repeat("lorem ipsum ", 2000)
	d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody(text)})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() < 2 {
		t.Fatalf("windows mode made %d calls, want one per window", f.calls.Load())
	}
	if len(d.Violations) != 1 || d.Violations[0].RuleID != "homework_shortcut" || d.Violations[0].ClassifierScore != 0.8 {
		t.Fatalf("violations = %+v, want the first window's homework_shortcut at 0.8", d.Violations)
	}
}

func TestCategoriesEgressIsObservedAndAsksAboutTheReply(t *testing.T) {
	f := newCatS1(t, familyRules)
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined), catCheck(store.SecgwModeObserve, store.SecgwDirectionBoth, "", familySlugs...))
	// Validate refuses block on egress; the engine must still observe if
	// handed one.
	eff.Checks[store.SecgwCheckContentSafety] = func() ResolvedCheck {
		c := eff.Checks[store.SecgwCheckContentSafety]
		c.Mode = store.SecgwModeBlock
		return c
	}()
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":"Here is how to make nerve gas..."}}]}`)
	vs := eng.EgressContentSafety(context.Background(), eff, userBody("tell me a story"), resp)
	if len(vs) != 1 || vs[0].Action != store.SecgwActionObserved || vs[0].Direction != store.SecgwDirectionEgress || vs[0].RuleID != "indiscriminate_weapons" {
		t.Fatalf("egress = %+v, want one observed indiscriminate_weapons", vs)
	}
	q := CategoryQuestions([]*store.SecgwCategory{testCategories()["feeling_low"]}, "", true)
	if !strings.Contains(q[0].Instructions, "reply from an AI assistant") {
		t.Fatalf("egress question %q must ask about the reply", q[0].Instructions)
	}
}

func TestCategoriesShareTheCallWithInjection(t *testing.T) {
	f := newCatS1(t, familyRules)
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined),
		injCheck(store.SecgwFailClosed, 3000), catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", familySlugs...))
	d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody("write my essay")})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 || d.Action != store.SecgwActionBlocked {
		t.Fatalf("calls %d action %s, want one combined call that blocks", f.calls.Load(), d.Action)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches[0]) != 2 {
		t.Fatalf("one call should carry injection and categories, got %v", f.batches[0])
	}
}

func TestCategoriesMissingOrWrongClassifierFailsTheCheck(t *testing.T) {
	f := newCatS1(t, familyRules)
	// Every selected category is gone from the snapshot.
	eng, eff := catEngine(t, catBreaker(t, f, dec.StyleCombined), catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", "deleted_one"))
	d, err := eng.Ingress(context.Background(), eff, Request{Body: userBody("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != store.SecgwActionBlocked || !d.ClassifierFailed {
		t.Fatalf("a fail-closed blocking check with no categories must fail closed: %+v", d)
	}
	// Bound to a Llama Guard style guard: the categories method cannot run.
	p := &store.SecgwPolicy{ID: "p", Name: "p", Enabled: true, Checks: []store.SecgwCheck{catCheck(store.SecgwModeObserve, store.SecgwDirectionIngress, "", "feeling_low")}}
	snap := snapWith([]*store.SecgwPolicy{p}, []*store.SecgwBinding{{ID: "b", PolicyID: "p", ScopeType: store.SecgwScopeOrg}})
	snap.Categories = testCategories()
	eng2, _ := NewEngine(snap, fakeResolver{&fakeGuard{verdicts: []classify.Verdict{{Safe: true}}}}, nil)
	d, err = eng2.Ingress(context.Background(), Resolve(snap, Subject{}), Request{Body: userBody("hello")})
	if err != nil || d.Action == store.SecgwActionBlocked || d.Model.Failed != 1 {
		t.Fatalf("observe check on a guard model: %+v %v; want failed, never blocked", d, err)
	}
}

func TestCategoriesFloorMerge(t *testing.T) {
	floor := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", "violent_crimes", "self_harm"),
	}}
	team := &store.SecgwPolicy{ID: "p-team", Name: "team", Enabled: true, Checks: []store.SecgwCheck{
		catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", "self_harm", "homework_shortcut"),
	}}
	snap := snapWith([]*store.SecgwPolicy{floor, team}, []*store.SecgwBinding{
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
		{ID: "b-team", PolicyID: "p-team", ScopeType: store.SecgwScopeGroup, ScopeID: "g1"},
	})
	eff := Resolve(snap, Subject{GroupIDs: []string{"g1"}})
	o := contentSafetyOptions(eff.Checks[store.SecgwCheckContentSafety])
	got := strings.Join(o.Safety, ",")
	for _, want := range []string{"violent_crimes", "self_harm", "homework_shortcut"} {
		if !strings.Contains(got, want) {
			t.Fatalf("merged categories %s, want the union with %s", got, want)
		}
	}
	// A team check on the legacy method cannot replace a categories floor.
	team.Checks = []store.SecgwCheck{csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S12")}
	eff = Resolve(snap, Subject{GroupIDs: []string{"g1"}})
	o = contentSafetyOptions(eff.Checks[store.SecgwCheckContentSafety])
	if o.Method != store.SecgwMethodCategories || strings.Join(o.Safety, ",") != "violent_crimes,self_harm" {
		t.Fatalf("method change must take the floor's options: %+v", o)
	}
	found := false
	for _, tr := range eff.Trace {
		if tr.Outcome == "overridden_by_mandatory" && strings.Contains(tr.Detail, "method set by the mandatory policy") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trace must explain the method: %+v", eff.Trace)
	}
	// And the other way round: a legacy floor keeps its method.
	floor.Checks = []store.SecgwCheck{csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S4")}
	team.Checks = []store.SecgwCheck{catCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "", "homework_shortcut")}
	eff = Resolve(snap, Subject{GroupIDs: []string{"g1"}})
	o = contentSafetyOptions(eff.Checks[store.SecgwCheckContentSafety])
	if o.usesCategories() || strings.Join(o.Categories, ",") != "S4" {
		t.Fatalf("legacy floor overridden: %+v", o)
	}
}

func TestCategoryQuestionsSplitAndDescribe(t *testing.T) {
	all := testCategories()
	var cats []*store.SecgwCategory
	for _, c := range store.BuiltinSecgwCategories {
		cats = append(cats, all[c.Slug])
	}
	for i := 0; i < 10; i++ {
		c := *all["feeling_low"]
		c.Slug = c.Slug + "_" + string(rune('a'+i))
		cats = append(cats, &c)
	}
	qs := CategoryQuestions(cats, "", false)
	if len(qs) != 2 || qs[0].ID != CategoryQuestionID || qs[1].ID != CategoryQuestionID+"_2" || len(qs[0].Options) != store.MaxSecgwCategoriesPerCheck+1 {
		t.Fatalf("29 categories should split into 25 + 4 options plus none: %d questions", len(qs))
	}
	for _, o := range qs[0].Options {
		if o.Slug == "homework_shortcut" && !strings.Contains(o.Description, "Not: Asking for an explanation") {
			t.Fatalf("description must carry the exclusions: %q", o.Description)
		}
		if strings.HasPrefix(o.Name, "S") && len(o.Name) <= 3 {
			t.Fatalf("option named by code %q", o.Name)
		}
	}
	if CategoryCalls(29, "") != 2 || CategoryCalls(5, store.SecgwAskSeparately) != 5 || CategoryCalls(5, "") != 1 {
		t.Fatal("call arithmetic")
	}
}
