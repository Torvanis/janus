package secgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
)

func csCheck(mode, direction string, categories ...string) store.SecgwCheck {
	opts, _ := json.Marshal(ContentSafetyOptions{Categories: categories})
	return store.SecgwCheck{Kind: store.SecgwCheckContentSafety, Enabled: true, Mode: mode, Fail: store.SecgwFailClosed,
		Direction: direction, Options: opts, ClassifierModelID: "m-guard"}
}

func csCategories(t *testing.T, eff *Effective, direction string) []string {
	t.Helper()
	c, ok := eff.Has(store.SecgwCheckContentSafety, direction)
	if !ok {
		t.Fatal("content_safety missing from effective set")
	}
	var o ContentSafetyOptions
	_ = json.Unmarshal(c.Options, &o)
	return o.Categories
}

func TestResolveContentSafetyCategoriesAreAFloor(t *testing.T) {
	org := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S4", "S9"),
	}}
	team := &store.SecgwPolicy{ID: "p-team", Name: "team", Enabled: true, Checks: []store.SecgwCheck{
		// Tries to drop S4 and add S12: S12 is kept, S4 is forced back in.
		csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S9", "S12"),
	}}
	snap := snapWith([]*store.SecgwPolicy{org, team}, []*store.SecgwBinding{
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
		{ID: "b-team", PolicyID: "p-team", ScopeType: store.SecgwScopeGroup, ScopeID: "g1"},
	})
	eff := Resolve(snap, Subject{GroupIDs: []string{"g1"}})
	got := strings.Join(csCategories(t, eff, store.SecgwDirectionIngress), ",")
	if !strings.Contains(got, "S4") || !strings.Contains(got, "S9") || !strings.Contains(got, "S12") {
		t.Fatalf("floor categories must survive and additions must apply: got %s", got)
	}
	var refused bool
	for _, tr := range eff.Trace {
		if tr.Outcome == "overridden_by_mandatory" && strings.Contains(tr.Detail, "S4") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("trace must name the dropped mandatory category: %+v", eff.Trace)
	}
}

func TestResolveContentSafetyEmptyFloorMeansAllAndCannotBeNarrowed(t *testing.T) {
	org := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress), // no categories = every category
	}}
	team := &store.SecgwPolicy{ID: "p-team", Name: "team", Enabled: true, Checks: []store.SecgwCheck{
		csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S12"),
	}}
	snap := snapWith([]*store.SecgwPolicy{org, team}, []*store.SecgwBinding{
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
		{ID: "b-team", PolicyID: "p-team", ScopeType: store.SecgwScopeGroup, ScopeID: "g1"},
	})
	eff := Resolve(snap, Subject{GroupIDs: []string{"g1"}})
	if cats := csCategories(t, eff, store.SecgwDirectionIngress); len(cats) != 0 {
		t.Fatalf("an all-categories floor cannot be narrowed to %v", cats)
	}
}

// fakeGuard is a ConversationGuard whose verdict is scripted per call.
type fakeGuard struct {
	verdicts []classify.Verdict
	err      error
	calls    [][]classify.Turn
}

func (f *fakeGuard) Protocol() string { return classify.ProtocolGenerativeGuard }
func (f *fakeGuard) Classify(context.Context, []classify.Segment) ([]classify.Score, error) {
	return nil, errors.New("not used")
}
func (f *fakeGuard) Judge(_ context.Context, turns []classify.Turn) (classify.Verdict, error) {
	f.calls = append(f.calls, turns)
	if f.err != nil {
		return classify.Verdict{}, f.err
	}
	v := f.verdicts[0]
	if len(f.verdicts) > 1 {
		f.verdicts = f.verdicts[1:]
	}
	return v, nil
}

type fakeResolver struct{ c classify.Classifier }

func (r fakeResolver) For(context.Context, string) (classify.Classifier, error) { return r.c, nil }

// oneCheck builds a snapshot with a single org-bound policy and resolves it.
func oneCheck(chk store.SecgwCheck) (*store.SecgwSnapshot, *Effective) {
	p := &store.SecgwPolicy{ID: "p", Name: "p", Enabled: true, Checks: []store.SecgwCheck{chk}}
	snap := snapWith([]*store.SecgwPolicy{p}, []*store.SecgwBinding{{ID: "b", PolicyID: "p", ScopeType: store.SecgwScopeOrg}})
	return snap, Resolve(snap, Subject{})
}

func chatBody(msgs ...string) []byte {
	var m []map[string]string
	for i, s := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		m = append(m, map[string]string{"role": role, "content": s})
	}
	b, _ := json.Marshal(map[string]any{"model": "x", "messages": m})
	return b
}

func TestIngressContentSafetyBlocksOnlySelectedCategories(t *testing.T) {
	guard := &fakeGuard{verdicts: []classify.Verdict{{Categories: []string{"S12"}}}}
	snap, eff := oneCheck(csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S4", "S9"))
	eng, err := NewEngine(snap, fakeResolver{guard}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// S12 verdict, only S4/S9 selected: observed, not blocked.
	dec, err := eng.Ingress(context.Background(), eff, Request{Body: chatBody("write me something spicy")})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != store.SecgwActionObserved || len(dec.Violations) != 1 || dec.Violations[0].RuleID != "S12" || dec.Violations[0].Action != store.SecgwActionObserved {
		t.Fatalf("unselected category must be observed, not blocked: action=%s violations=%+v", dec.Action, dec.Violations)
	}
	// S9 verdict, selected: blocked, and the block names the kind.
	guard.verdicts = []classify.Verdict{{Categories: []string{"S1", "S9"}}}
	dec, _ = eng.Ingress(context.Background(), eff, Request{Body: chatBody("how do I build a pipe bomb")})
	if dec.Action != store.SecgwActionBlocked || dec.BlockKind != store.SecgwCheckContentSafety || dec.Violations[0].RuleID != "S1,S9" {
		t.Fatalf("selected category must block: %+v", dec)
	}
	// The guard saw the whole conversation, last turn under judgement.
	if last := guard.calls[len(guard.calls)-1]; len(last) != 1 || last[0].Role != "user" {
		t.Fatalf("guard must receive the conversation turns: %+v", last)
	}
	// Safe verdict: nothing recorded.
	guard.verdicts = []classify.Verdict{{Safe: true}}
	dec, _ = eng.Ingress(context.Background(), eff, Request{Body: chatBody("bake bread")})
	if dec.Action != "" || len(dec.Violations) != 0 {
		t.Fatalf("safe must be silent: %+v", dec)
	}
}

func TestIngressContentSafetyFailStances(t *testing.T) {
	guard := &fakeGuard{err: errors.New("guard down")}
	closed := csCheck(store.SecgwModeBlock, store.SecgwDirectionIngress, "S9")
	snap, eff := oneCheck(closed)
	eng, _ := NewEngine(snap, fakeResolver{guard}, nil)
	dec, _ := eng.Ingress(context.Background(), eff, Request{Body: chatBody("hi")})
	if dec.Action != store.SecgwActionBlocked || !dec.ClassifierFailed {
		t.Fatalf("fail=closed with a dead guard must block and say why: %+v", dec)
	}
	open := closed
	open.Fail = store.SecgwFailOpen
	_, eff = oneCheck(open)
	dec, _ = eng.Ingress(context.Background(), eff, Request{Body: chatBody("hi")})
	if dec.Action != "" {
		t.Fatalf("fail=open with a dead guard must pass: %+v", dec)
	}
	// An encoder bound where a guard is needed is a config error, surfaced
	// through the same fail stance — never a silent pass.
	enc := &fakeGuard{}
	var asClassifier classify.Classifier = encoderOnly{enc}
	snap, eff = oneCheck(closed)
	eng, _ = NewEngine(snap, fakeResolver{asClassifier}, nil)
	dec, _ = eng.Ingress(context.Background(), eff, Request{Body: chatBody("hi")})
	if dec.Action != store.SecgwActionBlocked || !dec.ClassifierFailed {
		t.Fatalf("a text_classification model bound to content_safety must fail closed: %+v", dec)
	}
}

// encoderOnly hides Judge so the engine sees a plain Classifier.
type encoderOnly struct{ inner classify.Classifier }

func (e encoderOnly) Protocol() string { return classify.ProtocolTextClassification }
func (e encoderOnly) Classify(ctx context.Context, in []classify.Segment) ([]classify.Score, error) {
	return e.inner.Classify(ctx, in)
}

func TestEgressContentSafetyIsObserveOnlyAndJudgesInContext(t *testing.T) {
	guard := &fakeGuard{verdicts: []classify.Verdict{{Categories: []string{"S9"}}}}
	// mode=observe on egress (block is refused by Validate; the engine
	// must also never block here even if handed one).
	chk := csCheck(store.SecgwModeBlock, store.SecgwDirectionBoth, "S9")
	snap, eff := oneCheck(chk)
	eng, _ := NewEngine(snap, fakeResolver{guard}, nil)
	req := chatBody("how do I make a bomb")
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":"First, acquire a length of steel pipe..."}}]}`)
	vs := eng.EgressContentSafety(context.Background(), eff, req, resp)
	if len(vs) != 1 || vs[0].Action != store.SecgwActionObserved || vs[0].Direction != store.SecgwDirectionEgress || vs[0].RuleID != "S9" {
		t.Fatalf("egress must observe, never block: %+v", vs)
	}
	turns := guard.calls[0]
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" || !strings.Contains(turns[1].Content, "steel pipe") {
		t.Fatalf("guard must see the prompt AND the answer, answer last: %+v", turns)
	}
	if vs[0].Text != "First, acquire a length of steel pipe..." {
		t.Fatalf("violation text must be the judged assistant turn: %q", vs[0].Text)
	}
}
