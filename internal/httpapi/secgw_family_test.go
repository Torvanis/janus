package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// familyDecider is a System One server for the Family safety tests:
// keyword -> category probability, both question shapes, a failure switch
// and a per-call delay. It counts calls and records what it was shown.
type familyDecider struct {
	*httptest.Server
	mu     sync.Mutex
	states []string
	fail   bool
	delay  time.Duration
}

func newFamilyDecider(t *testing.T) *familyDecider {
	f := &familyDecider{}
	rules := map[string]map[string]float64{
		"essay":      {"homework_shortcut": 0.9},
		"ignores me": {"feeling_low": 0.6},
		"vape":       {"not_for_children": 0.8},
		"nerve":      {"indiscriminate_weapons": 0.95},
		"solve":      {"homework_learning": 0.9},
		// A reply that gives a schoolwork answer away.
		"the answer is": {"final_answer": 0.9},
		"my address is": {"personal_info": 0.8},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		f.mu.Lock()
		f.states = append(f.states, req.State)
		fail, delay := f.fail, f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		st := strings.ToLower(req.State)
		// Like a real classifier, read earlier turns only to resolve what
		// the latest message refers to ("do that"): a follow-up on
		// another topic is judged on its own.
		if i := strings.LastIndex(st, "the latest message, which is the request to judge:"); i >= 0 {
			if latest := st[i:]; !strings.Contains(latest, " that") {
				st = latest
			}
		}
		probs := map[string]float64{}
		for kw, m := range rules {
			if strings.Contains(st, kw) {
				for k, v := range m {
					probs[k] = v
				}
			}
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			if q.Type == "choice" {
				if _, ok := q.Criteria["final_answer"]; ok {
					http.Error(w, "final_answer must be its own yes/no question", http.StatusBadRequest)
					return
				}
			}
			if q.Type == "noul" {
				answers[id] = map[string]any{"type": "noul", "noul": probs[strings.TrimPrefix(id, "seccat_")]}
				continue
			}
			out, rest, best, bestP := map[string]float64{}, 1.0, "none", 0.0
			for slug := range q.Criteria {
				if p := probs[slug]; p > 0 {
					out[slug], rest = p, rest-p
					if p > bestP {
						best, bestP = slug, p
					}
				}
			}
			out["none"] = rest
			if rest > bestP {
				best = "none"
			}
			answers[id] = map[string]any{"type": "choice", "choice": best, "probabilities": out}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-family", "answers": answers, "usage": map[string]any{"input_tokens": 50, "output_tokens": 1}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *familyDecider) set(fail bool, delay time.Duration) {
	f.mu.Lock()
	f.fail, f.delay = fail, delay
	f.mu.Unlock()
}

func (f *familyDecider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.states)
}

func (h *harness) familyModel(t *testing.T, f *familyDecider) *store.Model {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "family-decider", "llama_cpp", f.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "nimble-family", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := all[0]
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	return m
}

// strictKids is a custom age range that blocks every Child Safety
// category (the old Family safety behavior the earlier tests pin).
func strictKids(cats ...string) []map[string]any {
	little, kids, teens := map[string]any{"id": store.ChildProfileLittle, "name": "Little kids (8 and under)"},
		map[string]any{"id": store.ChildProfileKids, "name": "Kids (9-12)"}, map[string]any{"id": store.ChildProfileTeens, "name": "Teens (13-17)"}
	set := map[string]any{}
	on := map[string]bool{}
	for _, c := range cats {
		on[c] = true
	}
	for _, slug := range store.ChildSafetyCategorySlugs {
		o := store.ChildOutcomeOff
		if on[slug] || len(cats) == 0 {
			o = store.ChildOutcomeBlock
		}
		set[slug] = map[string]any{"outcome": o}
	}
	return []map[string]any{little, kids, teens, {"id": "cs_strict", "name": "Strict", "categories": set}}
}

// putFamily switches Child Safety on for the given assignments.
func (h *harness) putFamily(t *testing.T, sess *auth.Session, body map[string]any) map[string]any {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/child-safety", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put family: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decodeInto(t, rec, &out)
	return out
}

// otherUser makes a second, non-admin user with an API token.
func (h *harness) otherUser(t *testing.T, sub, email string) (*store.User, string) {
	t.Helper()
	ctx := context.Background()
	u, _, err := h.store.UpsertUserFromIdentity(ctx, sub, email, "Kid "+sub, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := h.store.CreateToken(ctx, u.ID, "kid token")
	if err != nil {
		t.Fatal(err)
	}
	return u, tok
}

func (h *harness) doWithToken(token, method, path string, body any) *httptest.ResponseRecorder {
	saved := h.token
	h.token = token
	defer func() { h.token = saved }()
	return h.do(method, path, body)
}

// familyLeaks are words the child's notice must never contain.
var familyLeaks = []string{"Family safety", "family_safety", "content_safety", "homework", "Homework", "weapons", "indiscriminate", "feeling_low", "policy:", `"param"`}

func assertKindNotice(t *testing.T, body string) {
	t.Helper()
	for _, leak := range familyLeaks {
		if strings.Contains(body, leak) {
			t.Fatalf("child's notice leaks %q: %s", leak, body)
		}
	}
}

func TestFamilySafetyCommunityCanEnableBlockButBusinessPolicyStill402(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	// An ordinary policy in block mode is Business: still 402 on Community.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", categoriesPolicy("Kids block", m.ID, "block", "homework_shortcut")); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("custom block policy on Community: %d %s", rec.Code, rec.Body.String())
	}
	// A custom policy that calls itself "Family safety" gets no exemption.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", categoriesPolicy("Family safety", m.ID, "block", "feeling_low")); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("look-alike policy on Community: %d %s", rec.Code, rec.Body.String())
	}
	// The built-in family policy switches on in block mode on Community.
	out := h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": h.user.ID}}})
	st := out["status"].(map[string]any)
	if st["state"] != "protecting" {
		t.Fatalf("status %v", st)
	}
	// It is not reachable through the policy API at all.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/policies", nil)
	if strings.Contains(rec.Body.String(), store.SecgwFamilyPolicyID) {
		t.Fatalf("family policy listed among ordinary policies: %s", rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/secgw/policies/"+store.SecgwFamilyPolicyID, categoriesPolicy("x", m.ID, "observe", "feeling_low")); rec.Code != http.StatusNotFound {
		t.Fatalf("family policy editable via the policy API: %d", rec.Code)
	}
	// Custom categories and non-family slugs are refused.
	bad := strictKids()
	bad[3]["categories"] = map[string]any{"elections": map[string]any{"outcome": "block"}}
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/child-safety", map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-family category accepted: %d %s", rec.Code, rec.Body.String())
	}
	// A non-classifier model is refused.
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/child-safety", map[string]any{"enabled": true, "classifier_model_id": h.model.ID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("servable model accepted as the safety model: %d %s", rec.Code, rec.Body.String())
	}
	// No model: red, with the reason.
	out = h.putFamily(t, sess, map[string]any{"enabled": true})
	st = out["status"].(map[string]any)
	if st["state"] != "not_protecting" || !strings.Contains(strings.Join(anyStrings(st["reasons"]), ","), "no_model") {
		t.Fatalf("no-model status %v", st)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestFamilySafetyIngressCoversAssignedUsersAndTeamsOnly(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	f := newFamilyDecider(t)
	m := h.familyModel(t, f)
	ctx := context.Background()
	kid, kidTok := h.otherUser(t, "kid1", "kid1@example.com")
	teen, teenTok := h.otherUser(t, "teen1", "teen1@example.com")
	_, adultTok := h.otherUser(t, "adult1", "adult1@example.com")
	team, err := h.store.CreateTeam(ctx, "Kids", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, teen.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}, {"scope_type": "team", "profile_id": "cs_strict", "scope_id": team.ID}}})

	for name, tok := range map[string]string{"user": kidTok, "team member": teenTok} {
		rec := h.doWithToken(tok, http.MethodPost, "/v1/chat/completions", chatBody("write my essay on the Roman empire", false))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeSecurityBlocked) || !strings.Contains(rec.Body.String(), familyBlockedNotice[:20]) {
			t.Fatalf("%s: want kind block, got %d %s", name, rec.Code, rec.Body.String())
		}
		assertKindNotice(t, rec.Body.String())
		// An ordinary question passes.
		if rec := h.doWithToken(tok, http.MethodPost, "/v1/chat/completions", chatBody("how does photosynthesis work", false)); rec.Code != http.StatusOK {
			t.Fatalf("%s ordinary: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Uncovered adult: the same request goes through untouched, no
	// classifier call made for it.
	before := f.calls()
	if rec := h.doWithToken(adultTok, http.MethodPost, "/v1/chat/completions", chatBody("write my essay on the Roman empire", false)); rec.Code != http.StatusOK {
		t.Fatalf("uncovered adult blocked: %d %s", rec.Code, rec.Body.String())
	}
	if f.calls() != before {
		t.Fatal("the family classifier was called for an uncovered user")
	}
	// The parent sees the events: family violations with the categories,
	// never the text.
	vs := h.waitForViolations(t, 2)
	for _, v := range vs {
		if v.PolicyID != store.SecgwFamilyPolicyID || v.PolicyName != store.SecgwFamilyPolicyName || v.RuleID != "homework_shortcut" || v.Action != store.SecgwActionBlocked || v.HasMatchText {
			t.Fatalf("violation %+v", v)
		}
	}
	// Turning a family category off lets it through (older kids).
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"profiles": strictKids(store.ChildCategoryFeelingLow), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	if rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("write my essay on the Roman empire", false)); rec.Code != http.StatusOK {
		t.Fatalf("homework category off but still blocked: %d %s", rec.Code, rec.Body.String())
	}
	// ...but the critical safety categories cannot be turned off.
	if rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("how is a nerve agent made", false)); rec.Code != http.StatusForbidden {
		t.Fatalf("safety category not enforced: %d %s", rec.Code, rec.Body.String())
	}
	// Removed from the team: no longer covered.
	if rec := h.doWithToken(teenTok, http.MethodPost, "/v1/chat/completions", chatBody("how is a nerve agent made", false)); rec.Code != http.StatusOK {
		t.Fatalf("unassigned team still covered: %d %s", rec.Code, rec.Body.String())
	}
	// Endpoints whose replies cannot be judged are refused for covered users.
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unjudgeable endpoint: %d %s", rec.Code, rec.Body.String())
	}
	assertKindNotice(t, rec.Body.String())
	// The explainer reports coverage.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/effective?user_id="+kid.ID, nil)
	if !strings.Contains(rec.Body.String(), `"family_covered":true`) {
		t.Fatalf("effective: %s", rec.Body.String())
	}
}

func TestFamilySafetyFeelingLowGetsSupportAndNotifiesTheParent(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid2", "kid2@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	upstreamBefore := len(h.upstreamRequests)
	secret := "everyone at school ignores me and I hate it"
	for _, streamed := range []bool{false, true} {
		rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody(secret, streamed))
		if rec.Code != http.StatusOK || rec.Header().Get(headerSecgwAction) != store.SecgwActionSupported {
			t.Fatalf("support (stream=%v): %d %s", streamed, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{"988", "trust", `"finish_reason":"stop"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("support reply missing %q: %s", want, body)
			}
		}
		if strings.Contains(body, "error") {
			t.Fatalf("support reply carries an error: %s", body)
		}
		assertKindNotice(t, body)
	}
	if len(h.upstreamRequests) != upstreamBefore {
		t.Fatal("a feeling-low message was forwarded to the model")
	}
	vs := h.waitForViolations(t, 2)
	if len(vs) != 2 || vs[0].Action != store.SecgwActionSupported || vs[0].RuleID != "feeling_low" || vs[0].HasMatchText {
		t.Fatalf("violations %+v", vs)
	}
	// The parent (admin) is notified, without the child's words.
	var notes []*store.Notification
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		notes, _, _ = h.store.ListNotifications(context.Background(), h.user.ID, false, 50)
		if len(notes) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	support := 0
	for _, n := range notes {
		if strings.Contains(n.Title, "may need support") && strings.Contains(n.Title, kid.Name) {
			support++
		}
		if strings.Contains(n.Body, "ignores me") || strings.Contains(n.Title, "ignores me") {
			t.Fatalf("notification quotes the child: %+v", n)
		}
	}
	if support != 2 {
		t.Fatalf("want 2 support notifications (never throttled), got %d: %+v", support, notes)
	}
}

func TestFamilySafetyFailsClosedForCoveredUsersOnly(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	f := newFamilyDecider(t)
	m := h.familyModel(t, f)
	kid, kidTok := h.otherUser(t, "kid3", "kid3@example.com")
	_, adultTok := h.otherUser(t, "adult3", "adult3@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	f.set(true, 0)
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("what is two plus two", false))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyUnavailableNotice[:24]) {
		t.Fatalf("covered user with the classifier down: %d %s", rec.Code, rec.Body.String())
	}
	assertKindNotice(t, rec.Body.String())
	if rec := h.doWithToken(adultTok, http.MethodPost, "/v1/chat/completions", chatBody("what is two plus two", false)); rec.Code != http.StatusOK {
		t.Fatalf("uncovered user affected by the family classifier outage: %d %s", rec.Code, rec.Body.String())
	}
	// A model that is gone fails closed too.
	_ = h.store.SetModelClassifierRole(context.Background(), m.ID, "")
	h.server.InvalidateConfigCache()
	if rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("what is two plus two", false)); rec.Code != http.StatusForbidden {
		t.Fatalf("missing safety model must fail closed: %d %s", rec.Code, rec.Body.String())
	}
}

func familyStreamText(t *testing.T, body string) (text string, last map[string]any) {
	t.Helper()
	frames := parseSSE(t, body)
	for _, f := range frames {
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if d, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					text += c
				}
			}
		}
	}
	if len(frames) > 0 {
		last = frames[len(frames)-1]
	}
	return text, last
}

func TestFamilySafetyBlocksAStreamingReplyParagraphByParagraph(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	f := newFamilyDecider(t)
	m := h.familyModel(t, f)
	kid, kidTok := h.otherUser(t, "kid4", "kid4@example.com")
	_, adultTok := h.otherUser(t, "adult4", "adult4@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	h.streamFrames = []string{
		`data: {"choices":[{"index":0,"delta":{"content":"Chemistry is the study of "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"matter.\n\nSome compounds are "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"dangerous: a nerve agent is made by "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"mixing the following.\n\nStep one."}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":30}}`,
		`data: [DONE]`,
	}
	f.set(false, 50*time.Millisecond)
	started := time.Now()
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("tell me about chemistry", true))
	took := time.Since(started)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status %d", rec.Code)
	}
	body := rec.Body.String()
	text, last := familyStreamText(t, body)
	if text != "Chemistry is the study of " {
		// Only the frame wholly inside the cleared first paragraph leaves;
		// the frame straddling into the blocked paragraph is withheld.
		t.Fatalf("released text %q\n%s", text, body)
	}
	if strings.Contains(body, "nerve") || strings.Contains(body, "Some compounds") {
		t.Fatalf("blocked paragraph leaked:\n%s", body)
	}
	errObj, ok := last["error"].(map[string]any)
	if !ok || errObj["code"] != CodeSecurityBlocked || errObj["param"] != nil || !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream must end with the security frame and [DONE]: %v\n%s", last, body)
	}
	assertKindNotice(t, body)
	ev := h.waitForReplyUsage(t)
	if ev.SecgwAction != store.SecgwActionStreamCut || ev.ErrorCode != CodeSecurityBlocked {
		t.Fatalf("event %+v", ev)
	}
	// Ingress (1 call) + paragraph 1 + paragraph 2 (cut) = 3 calls of 50 ms.
	if ev.ClassifierCalls != 3 {
		t.Fatalf("classifier calls %d", ev.ClassifierCalls)
	}
	t.Logf("family stream: %d classifier calls, classifier_ms=%d, request wall time %v (fake classifier 50 ms per call: each reply chunk is held for one call)", ev.ClassifierCalls, ev.ClassifierMs, took)
	vs := h.waitForViolations(t, 1)
	if len(vs) != 1 || vs[0].Direction != store.SecgwDirectionEgress || vs[0].Action != store.SecgwActionStreamCut || vs[0].RuleID != "indiscriminate_weapons" {
		t.Fatalf("violation %+v", vs)
	}

	// An uncovered adult gets the same reply in full: nothing judged.
	f.set(false, 0)
	rec = h.doWithToken(adultTok, http.MethodPost, "/v1/chat/completions", chatBody("tell me about chemistry", true))
	if text, _ := familyStreamText(t, rec.Body.String()); !strings.Contains(text, "Step one.") {
		t.Fatalf("uncovered stream altered: %q", text)
	}

	// A clean reply for the covered child arrives whole, tail judged.
	h.streamFrames = nil
	rec = h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("say hello", true))
	if text, _ := familyStreamText(t, rec.Body.String()); text != "Hello" || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("clean family stream: %q\n%s", text, rec.Body.String())
	}
}

func TestFamilySafetyJudgesBufferedRepliesBeforeReturning(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid5", "kid5@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	h.upstreamReply = `{"id":"c","choices":[{"message":{"role":"assistant","content":"Here is how to make a nerve agent."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("chemistry homework help", false))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "nerve") || !strings.Contains(rec.Body.String(), familyReplyStopNotice[:20]) {
		t.Fatalf("buffered reply: %d %s", rec.Code, rec.Body.String())
	}
	assertKindNotice(t, rec.Body.String())
	ev := h.waitForReplyUsage(t)
	if ev.SecgwAction != store.SecgwActionBlocked || ev.TokensOut != 9 {
		t.Fatalf("blocked reply must still be metered: %+v", ev)
	}
}

func TestFamilySafetyBusinessEgressStaysObserveOnly(t *testing.T) {
	// A Business categories policy on both directions still only observes
	// replies, even with Family safety on for someone else.
	h := newHarness(t)
	withBusinessLicense(t, h)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, _ := h.otherUser(t, "kid6", "kid6@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	body := categoriesPolicy("Biz", m.ID, "block", "indiscriminate_weapons")
	body["checks"].([]map[string]any)[0]["direction"] = "both"
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body.String())
	}
	var p struct {
		Policy store.SecgwPolicy `json:"policy"`
	}
	decodeInto(t, rec, &p)
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": p.Policy.ID, "scope_type": "org"}); rec.Code != http.StatusCreated {
		t.Fatalf("bind: %d", rec.Code)
	}
	h.upstreamReply = `{"id":"c","choices":[{"message":{"role":"assistant","content":"Here is how to make a nerve agent."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	rec = h.do(http.MethodPost, "/v1/chat/completions", chatBody("chemistry question", false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nerve agent") {
		t.Fatalf("Business egress must observe, not block: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFamilySafetyChatShowsBlockedAndSupport(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.familyModel(t, newFamilyDecider(t))
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": h.user.ID}}})
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)

	// Blocked message: terminal "blocked" with the kind notice.
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "write my essay about volcanoes"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusBlocked || done["error_message"] != familyBlockedNotice {
		t.Fatalf("blocked chat: %v", done)
	}
	// The blocked turn is not resent as context: the next message passes.
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "what is a volcano"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("next turn after a block: %v", done)
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if strings.Contains(string(last.Body), "essay") {
		t.Fatalf("blocked turn resent as context: %s", last.Body)
	}

	// Support: an ordinary assistant message with the supportive text.
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "everyone ignores me"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete || !strings.Contains(done["content"].(string), "988") {
		t.Fatalf("support chat: %v", done)
	}

	// A reply stopped mid-stream ends as "blocked" too.
	h.streamFrames = []string{
		`data: {"choices":[{"index":0,"delta":{"content":"Sure.\n\n"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"A nerve agent is made like this.\n\n"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":10}}`,
		`data: [DONE]`,
	}
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "tell me about chemistry"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusBlocked || strings.Contains(done["content"].(string), "nerve") || done["error_message"] != familyReplyStopNotice {
		t.Fatalf("stopped reply in chat: %v", done)
	}
}

// lastUpstreamSystem is the system prompt the model was sent last.
func (h *harness) lastUpstreamSystem(t *testing.T) string {
	t.Helper()
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(last.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, m := range body.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// Answer appropriately: the message goes to the model with the age range's
// text, the tutor rule and the matched category's instructions; nothing
// is blocked, and an ordinary turn still gets the base text and tutor rule.
func TestChildSafetyAnswersAppropriatelyWithInstructions(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid7", "kid7@example.com")
	profiles := strictKids()
	profiles[1]["categories"] = map[string]any{"personal_info": map[string]any{"outcome": "answer", "instructions": "PARENT PRIVACY WORDS"}}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": kid.ID}}})

	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("hi! my address is 12 Elm Street", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer appropriately must not block: %d %s", rec.Code, rec.Body.String())
	}
	sys := h.lastUpstreamSystem(t)
	for _, want := range []string{"You are talking with a child", "PARENT PRIVACY WORDS", "NEVER give the final answer"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, sys)
		}
	}
	var ev *store.UsageEvent
	for _, e := range h.waitForUsageEvents(1) {
		if e.ClientApp != "janus-security-gateway" {
			ev = e
		}
	}
	if ev == nil || !strings.Contains(ev.RouteReason, "child=personal_info") || ev.SecgwAction == store.SecgwActionBlocked {
		t.Fatalf("event %+v", ev)
	}
	// An ordinary turn: base text and tutor rule, no category text.
	if rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("what is a volcano", false)); rec.Code != http.StatusOK {
		t.Fatalf("ordinary: %d", rec.Code)
	}
	sys = h.lastUpstreamSystem(t)
	if !strings.Contains(sys, "You are talking with a child") || strings.Contains(sys, "PARENT PRIVACY WORDS") || !strings.Contains(sys, "NEVER give the final answer") {
		t.Fatalf("ordinary turn system prompt:\n%s", sys)
	}
	// The adult's request is untouched.
	_, adultTok := h.otherUser(t, "adult7", "adult7@example.com")
	if rec := h.doWithToken(adultTok, http.MethodPost, "/v1/chat/completions", chatBody("what is a volcano", false)); rec.Code != http.StatusOK || h.lastUpstreamSystem(t) != "" {
		t.Fatalf("adult got child instructions: %q", h.lastUpstreamSystem(t))
	}
}

// The wrong-answer trick: a child "tries", the model corrects them with
// the final answer. The reply stops before the answer and ends with the
// tutor's note, streamed or buffered; the parent is not alerted.
func TestChildSafetyWithholdsAFinalAnswer(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid8", "kid8@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": kid.ID}}})
	h.streamFrames = []string{
		`data: {"choices":[{"index":0,"delta":{"content":"Good try! Let's check it.\n\n"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"Not quite. The answer is x = 5.\n\n"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":20}}`,
		`data: [DONE]`,
	}
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("solve 3x + 5 = 20. I think x = 6", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status %d", rec.Code)
	}
	text, _ := familyStreamText(t, rec.Body.String())
	if strings.Contains(text, "x = 5") || !strings.Contains(text, "Good try!") || !strings.Contains(text, familyWithheldNote) {
		t.Fatalf("streamed reply %q", text)
	}
	if strings.Contains(rec.Body.String(), `"error"`) || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("a withheld answer ends as a normal reply:\n%s", rec.Body.String())
	}
	assertKindNotice(t, text)
	// Buffered.
	h.streamFrames = nil
	h.upstreamReply = `{"id":"c","choices":[{"message":{"role":"assistant","content":"The answer is x = 5."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	rec = h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("solve 3x + 5 = 20. I think x = 6", false))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "x = 5") || !strings.Contains(rec.Body.String(), "you'll learn more") {
		t.Fatalf("buffered: %d %s", rec.Code, rec.Body.String())
	}
	vs := h.waitForViolations(t, 2)
	for _, v := range vs {
		if v.RuleID != store.ChildCategoryFinalAnswer || v.Direction != store.SecgwDirectionEgress || v.Action != store.SecgwActionWithheld {
			t.Fatalf("violation %+v", v)
		}
	}
	// The usage event says "withheld" (Chat shows a tutor note, not a
	// security warning).
	var withheld int
	for _, e := range h.waitForUsageEvents(2) {
		if e.SecgwAction == store.SecgwActionWithheld {
			withheld++
		}
	}
	if withheld != 2 {
		t.Fatalf("withheld usage events: %d", withheld)
	}
	// Checking the child's own answer is validation, not a give-away: the
	// reply is asked "gives the answer away" as its own yes/no question,
	// and a confirmation passes through untouched.
	h.upstreamReply = `{"id":"c","choices":[{"message":{"role":"assistant","content":"Yes! x = 23 is right. Great job subtracting 9 from both sides."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	rec = h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("So x = 23?", false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "x = 23 is right") {
		t.Fatalf("confirming the child's own answer: %d %s", rec.Code, rec.Body.String())
	}
	h.upstreamReply = `{"id":"c","choices":[{"message":{"role":"assistant","content":"The answer is x = 5."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	time.Sleep(100 * time.Millisecond)
	notes, _, _ := h.store.ListNotifications(context.Background(), h.user.ID, false, 50)
	if len(notes) != 0 {
		t.Fatalf("a withheld answer is not an incident; notes %+v", notes)
	}
	// With homework off, nothing is withheld (and no reply check for it).
	profiles := strictKids()
	profiles[1]["categories"] = map[string]any{"homework_learning": map[string]any{"outcome": "off"}, "homework_shortcut": map[string]any{"outcome": "off"}}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": kid.ID}}})
	rec = h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("solve 3x + 5 = 20", false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "x = 5") {
		t.Fatalf("homework off: %d %s", rec.Code, rec.Body.String())
	}
}

// Blocked models are hidden from the child's lists and refused by name,
// for catalog and personal-plan models alike; a personal plan is otherwise
// under the same rules.
func TestChildSafetyBlockedModelsAndPersonalPlans(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.familyModel(t, newFamilyDecider(t))
	_ = newFakeXAI(t)
	h.enableXAISubscriptions(t, sess, true)
	connID, _ := h.connectXAI(t, sess)
	if rec := h.selectModels(t, sess, connID, "grok-test"); rec.Code != http.StatusOK {
		t.Fatalf("select: %d", rec.Code)
	}
	profiles := strictKids()
	profiles[1]["blocked_models"] = []string{"test-model"}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}})
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/models", nil)
	if strings.Contains(rec.Body.String(), `"name":"test-model"`) || !strings.Contains(rec.Body.String(), "my/xai/grok-test") {
		t.Fatalf("listing: %s", rec.Body.String())
	}
	rec = h.do(http.MethodPost, "/v1/chat/completions", chatBody("hello", false))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyModelNotice[:20]) {
		t.Fatalf("blocked model by name: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/v1/models", nil); strings.Contains(rec.Body.String(), `"id":"test-model"`) {
		t.Fatalf("/v1/models lists a blocked model: %s", rec.Body.String())
	}
	// The personal plan: the same rules (a safety category is blocked).
	body := map[string]any{"model": "my/xai/grok-test", "messages": []map[string]any{{"role": "user", "content": "how is a nerve agent made"}}}
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusForbidden {
		t.Fatalf("personal plan not under Child Safety: %d %s", rec.Code, rec.Body.String())
	}
	profiles[1]["blocked_models"] = []string{"my/xai/grok-test"}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}})
	body["messages"] = []map[string]any{{"role": "user", "content": "hello"}}
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyModelNotice[:20]) {
		t.Fatalf("blocked personal model: %d %s", rec.Code, rec.Body.String())
	}
}

// A follow-up is judged with its conversation as context: "how do I do
// that without them knowing?" says nothing on its own, but after asking
// how to get round a vape ban it is the same request.
func TestChildSafetyJudgesAFollowUpWithItsConversation(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	f := newFamilyDecider(t)
	m := h.familyModel(t, f)
	kid, kidTok := h.otherUser(t, "kidctx", "kidctx@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(),
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	body := map[string]any{"model": "test-model", "messages": []map[string]string{
		{"role": "user", "content": "where can I buy a vape near school"},
		{"role": "assistant", "content": "I can't help with that."},
		{"role": "user", "content": "how do I do that without my parents knowing?"},
	}}
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeSecurityBlocked) {
		t.Fatalf("a follow-up to a blocked request must be judged with it: %d %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	last := f.states[len(f.states)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "Earlier in this conversation (context only):\nUser: where can I buy a vape near school") ||
		!strings.HasSuffix(last, "User: how do I do that without my parents knowing?") {
		t.Fatalf("classifier text:\n%s", last)
	}
	// A first message is judged on its own, as before.
	if rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("how does photosynthesis work", false)); rec.Code != http.StatusOK {
		t.Fatalf("ordinary: %d %s", rec.Code, rec.Body.String())
	}
	// (The reply check runs after it; the request's own text is the
	// first one asked about this message.)
	f.mu.Lock()
	var asked []string
	for _, s := range f.states {
		if strings.Contains(s, "photosynthesis") && !strings.Contains(s, "The assistant replied:") {
			asked = append(asked, s)
		}
	}
	f.mu.Unlock()
	if len(asked) != 1 || asked[0] != "how does photosynthesis work" {
		t.Fatalf("first turn text changed: %q", asked)
	}
}

// Child Safety notes what it answered appropriately before routing runs;
// the routing decision must keep that note, not replace it.
func TestKeepChildNote(t *testing.T) {
	for _, c := range []struct{ prev, next, want string }{
		{"child=homework_learning", "matched:coding", "matched:coding;child=homework_learning"},
		{"sticky;child=personal_info,not_for_children", "sticky", "sticky;child=personal_info,not_for_children"},
		{"", "matched:legal", "matched:legal"},
		{"child=feeling_low", "", "child=feeling_low"},
		{"matched:old", "no_match", "no_match"},
	} {
		if got := keepChildNote(c.prev, c.next); got != c.want {
			t.Errorf("keepChildNote(%q, %q) = %q, want %q", c.prev, c.next, got, c.want)
		}
	}
	// And instructions added afterwards keep it too.
	if got := withInstructionsNote("matched:coding;child=homework_learning", "route"); got != "matched:coding;child=homework_learning;instructions=route" {
		t.Errorf("withInstructionsNote dropped the child note: %q", got)
	}
}

// waitForReplyUsage returns the reply's own usage event. The safety
// classifier's metering rows are written concurrently with it, so the first
// event in the list is not reliably the reply.
func (h *harness) waitForReplyUsage(t *testing.T) *store.UsageEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, e := range h.waitForUsageEventsNoWait() {
			if e.ClientApp != "janus-security-gateway" {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply usage event among %d", len(h.waitForUsageEventsNoWait()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
