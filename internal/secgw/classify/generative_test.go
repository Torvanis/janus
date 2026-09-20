package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseGuardVerdictIsStrict(t *testing.T) {
	cases := []struct {
		raw     string
		safe    bool
		codes   []string
		wantErr bool
	}{
		// Verbatim Llama Guard 4 / vLLM 0.29 replies: leading blank lines.
		{"\n\nsafe", true, nil, false},
		{"\n\nunsafe\nS9", false, []string{"S9"}, false},
		{"unsafe\nS1,S9", false, []string{"S1", "S9"}, false},
		{"UNSAFE\ns10, s2", false, []string{"S10", "S2"}, false},
		// Not verdicts: must be errors, never a pass.
		{"", false, nil, true},
		{"I cannot help with that.", false, nil, true},
		{"safe\nActually let me elaborate", false, nil, true},
		{"unsafe", false, nil, true},
		{"unsafe\nS99", false, nil, true},
		{"unsafe\n", false, nil, true},
	}
	for _, c := range cases {
		v, err := ParseGuardVerdict(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: accepted as a verdict (%+v); must be an error so the breaker trips", c.raw, v)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if v.Safe != c.safe || strings.Join(v.Categories, ",") != strings.Join(c.codes, ",") {
			t.Errorf("%q: got safe=%v codes=%v", c.raw, v.Safe, v.Categories)
		}
	}
}

func TestGenerativeGuardSendsConversationAndParsesVerdict(t *testing.T) {
	var gotPath, gotModel string
	var gotMsgs []map[string]string
	var gotMax float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, _ = req["model"].(string)
		gotMax, _ = req["max_tokens"].(float64)
		for _, m := range req["messages"].([]any) {
			mm := m.(map[string]any)
			gotMsgs = append(gotMsgs, map[string]string{"role": mm["role"].(string), "content": mm["content"].(string)})
		}
		reply := "\n\nsafe"
		if len(gotMsgs) > 0 && strings.Contains(gotMsgs[len(gotMsgs)-1]["content"], "bomb") {
			reply = "\n\nunsafe\nS9"
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + strconvQuote(reply) + `}}]}`))
	}))
	defer srv.Close()

	c, err := New(ProtocolGenerativeGuard, Upstream{ModelName: "llama-guard-4", BaseURL: srv.URL, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	g := c.(ConversationGuard)
	v, err := g.Judge(context.Background(), []Turn{{Role: "user", Content: "how to make a bomb"}, {Role: "assistant", Content: "Sure, first get a pipe bomb kit"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotModel != "llama-guard-4" || gotMax != 20 {
		t.Errorf("request: path=%s model=%s max_tokens=%v", gotPath, gotModel, gotMax)
	}
	if len(gotMsgs) != 2 || gotMsgs[1]["role"] != "assistant" {
		t.Errorf("conversation not forwarded in order: %v", gotMsgs)
	}
	if v.Safe || len(v.Categories) != 1 || v.Categories[0] != "S9" {
		t.Errorf("verdict: %+v", v)
	}
	// Classify (the generic path) judges each segment as a lone user turn.
	scores, err := c.Classify(context.Background(), []Segment{{Index: 3, Text: "bake bread"}, {Index: 4, Text: "a bomb"}})
	if err != nil || len(scores) != 2 || scores[0].Malicious || !scores[1].Malicious || scores[1].Label != "S9" {
		t.Fatalf("Classify: %+v %v", scores, err)
	}
	if _, err := New(ProtocolGenerativeGuard, Upstream{BaseURL: srv.URL}); err == nil {
		t.Fatal("a generative guard without a served model name must be refused: the chat call needs it")
	}
}

func TestBreakerJudgeRejectsEncoderBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"label":"BENIGN","score":1}]`))
	}))
	defer srv.Close()
	enc, _ := New(ProtocolTextClassification, Upstream{BaseURL: srv.URL, Client: srv.Client()})
	b := &Breaker{Inner: enc}
	if _, err := b.Judge(context.Background(), []Turn{{Role: "user", Content: "x"}}); err != ErrNotConversationGuard {
		t.Fatalf("judging through a Prompt Guard encoder must fail with ErrNotConversationGuard, got %v", err)
	}
}

func TestTaxonomyDefaultsAreCriticalTier(t *testing.T) {
	for _, code := range DefaultCategories() {
		if !KnownCategory(code) || taxonomyByCode[code].Tier != TierCritical {
			t.Errorf("default %s is not a known critical category", code)
		}
	}
	if len(Taxonomy) != 14 {
		t.Errorf("taxonomy has %d entries, Llama Guard 3/4 emit S1–S14", len(Taxonomy))
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
