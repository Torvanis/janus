package decisions

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Shapes below are the live responses captured on 2026-10-08 (OpenAI
// /v1/decisions with gpt-6-luna; OpenRouter Jev), trimmed.
const oaiLive = `{"model":"gpt-6-luna","answers":[{"type":"choice","name":"department","choice":"billing","probabilities":[{"value":"billing","probability":1.0},{"value":"technical","probability":0.0}],"confidence":1.0},{"type":"predicate","name":"urgent","probability":0.25},{"type":"score","name":"anger","score":0.91,"probabilities":[{"value":0,"label":"Calm","probability":0.09},{"value":1,"label":"Annoyed","probability":0.91},{"value":2,"label":"Furious","probability":0.0}],"confidence":0.87}],"usage":{"input_tokens":370,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":370}}`

const s1Live = `{"model":"typesafe/jev-1.13-20260917","answers":{"department":{"type":"choice","choice":"billing","probabilities":{"billing":1,"technical":0},"confidence":1},"urgent":{"type":"noul","noul":0.67},"anger":{"type":"score","score":1.05,"legend":{"0":"Calm","1":"Annoyed","2":"Furious"},"probabilities":{"0":0.0,"1":0.95,"2":0.05},"confidence":0.92}},"usage":{"input_tokens":323,"output_tokens":48,"cost":0.000013566},"id":"gen-dec-1","provider":"TypeSafe"}`

const s1Req = `{"model":"~typesafe/jev-latest","state":"I was charged twice.","questions":{
 "department":{"type":"choice","instructions":"Which department?","criteria":{"billing":"Payments","technical":"Bugs"}},
 "urgent":{"type":"noul","instructions":"Is this urgent?","criteria":{"true":"Explicitly time-sensitive","false":"No urgency"}},
 "anger":{"type":"score","instructions":"How angry?","criteria":["Calm","Annoyed","Furious"]}}}`

const oaiReq = `{"model":"gpt-6-luna","input":"I was charged twice.","questions":[
 {"type":"choice","name":"department","instructions":"Which department?","choices":[{"value":"billing","description":"Payments"},{"value":"technical","description":"Bugs"}]},
 {"type":"predicate","name":"urgent","instructions":"Is this urgent?"},
 {"type":"score","name":"anger","instructions":"How angry?","levels":[{"label":"Calm"},{"label":"Annoyed"},{"label":"Furious","description":"Shouting"}]}]}`

func TestSystemOneRequestToOpenAI(t *testing.T) {
	r, err := ParseSystemOne([]byte(s1Req))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{r.Questions[0].Name, r.Questions[1].Name, r.Questions[2].Name}; strings.Join(got, ",") != "department,urgent,anger" {
		t.Fatalf("question order must follow the body: %v", got)
	}
	out, err := EncodeOpenAI(r, "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Model     string
		Input     string
		Questions []struct {
			Type, Name, Instructions string
			Choices                  []struct{ Value, Description string }
			Levels                   []struct{ Label string }
		}
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.Model != "gpt-6-luna" || m.Input != "I was charged twice." || len(m.Questions) != 3 {
		t.Fatalf("bad translation: %s", out)
	}
	if q := m.Questions[0]; q.Type != "choice" || q.Choices[1].Value != "technical" || q.Choices[1].Description != "Bugs" {
		t.Fatalf("choice: %+v", q)
	}
	if q := m.Questions[1]; q.Type != "predicate" || !strings.Contains(q.Instructions, "Counts as yes: Explicitly time-sensitive") {
		t.Fatalf("the yes/no boundary must survive in the predicate's words: %+v", q)
	}
	if q := m.Questions[2]; q.Type != "score" || len(q.Levels) != 3 || q.Levels[2].Label != "Furious" {
		t.Fatalf("score: %+v", q)
	}
}

func TestOpenAIResponseBackToSystemOne(t *testing.T) {
	r, _ := ParseSystemOne([]byte(s1Req))
	resp, err := ParseOpenAIResponse([]byte(oaiLive), r.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 370 || resp.Answers[1].Probability != 0.25 || resp.Answers[2].Score != 0.91 {
		t.Fatalf("parse: %+v", resp)
	}
	out := EncodeSystemOneResponse(resp, r.Questions)
	var m struct {
		Answers map[string]struct {
			Type          string
			Choice        string
			Noul          *float64
			Score         *float64
			Probabilities map[string]float64
			Legend        map[string]string
			Confidence    *float64
		}
		Usage struct {
			InputTokens int64 `json:"input_tokens"`
		}
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if a := m.Answers["department"]; a.Type != "choice" || a.Choice != "billing" || a.Probabilities["billing"] != 1 || a.Confidence == nil {
		t.Fatalf("choice: %+v", a)
	}
	if a := m.Answers["urgent"]; a.Type != "noul" || a.Noul == nil || *a.Noul != 0.25 {
		t.Fatalf("predicate must come back as noul: %+v", a)
	}
	if a := m.Answers["anger"]; a.Type != "score" || *a.Score != 0.91 || a.Probabilities["1"] != 0.91 || a.Legend["2"] != "Furious" {
		t.Fatalf("score: %+v", a)
	}
	if m.Usage.InputTokens != 370 {
		t.Fatalf("usage: %s", out)
	}
	if !strings.Contains(string(out), `"answers":{"department"`) {
		t.Fatalf("answers keep the asked order: %s", out)
	}
}

func TestOpenAIRequestToSystemOneAndBack(t *testing.T) {
	r, err := ParseOpenAI([]byte(oaiReq))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeSystemOne(r, "~typesafe/jev-latest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), `{"model":"~typesafe/jev-latest","state":"I was charged twice.","questions":{"department":{"type":"choice"`) {
		t.Fatalf("questions must keep their order: %s", out)
	}
	if !strings.Contains(string(out), `"criteria":{"billing":"Payments","technical":"Bugs"}`) ||
		!strings.Contains(string(out), `"criteria":["Calm","Annoyed","Furious: Shouting"]`) {
		t.Fatalf("criteria: %s", out)
	}
	resp, err := ParseSystemOneResponse([]byte(s1Live), r.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.CostReported || resp.Usage.CostUSD != 0.000013566 {
		t.Fatalf("provider cost must be kept: %+v", resp.Usage)
	}
	back := EncodeOpenAIResponse(resp)
	var m struct {
		Answers []struct {
			Type, Name    string
			Probability   *float64
			Choice        any
			Score         *float64
			Confidence    *float64
			Probabilities []map[string]any
		}
	}
	if err := json.Unmarshal(back, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Answers) != 3 || m.Answers[0].Name != "department" || m.Answers[0].Choice != "billing" {
		t.Fatalf("choice: %s", back)
	}
	if a := m.Answers[1]; a.Type != "predicate" || *a.Probability != 0.67 {
		t.Fatalf("noul must come back as predicate: %s", back)
	}
	if a := m.Answers[2]; a.Type != "score" || *a.Score != 1.05 || a.Probabilities[1]["label"] != "Annoyed" || a.Confidence == nil {
		t.Fatalf("score: %s", back)
	}
}

func TestUnnamedAndBooleanChoices(t *testing.T) {
	r, err := ParseOpenAI([]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"questions":[{"type":"choice","instructions":"x","choices":[{"value":true},{"value":false}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Questions[0].Unnamed || !r.Questions[0].Choices[0].Bool {
		t.Fatalf("%+v", r.Questions[0])
	}
	s1, _ := EncodeSystemOne(r, "jev")
	if !strings.Contains(string(s1), `"q0":{"type":"choice"`) {
		t.Fatalf("unnamed questions get a stable key: %s", s1)
	}
	resp, err := ParseSystemOneResponse([]byte(`{"model":"jev","answers":{"q0":{"type":"choice","choice":"true","probabilities":{"true":0.9,"false":0.1}}},"usage":{"input_tokens":5}}`), r.Questions)
	if err != nil {
		t.Fatal(err)
	}
	out := string(EncodeOpenAIResponse(resp))
	if !strings.Contains(out, `"choice":true`) || !strings.Contains(out, `"name":null`) || !strings.Contains(out, `"confidence":0.9`) {
		t.Fatalf("boolean values and unnamed answers must round-trip: %s", out)
	}
}

func TestImagesCannotGoToSystemOne(t *testing.T) {
	r, err := ParseOpenAI([]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}],"questions":[{"type":"predicate","name":"d","instructions":"Damaged?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Images() != 1 || r.Text() != "look" {
		t.Fatalf("%+v", r)
	}
	if _, err := EncodeSystemOne(r, "jev"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("images to a text-only dialect must be refused, got %v", err)
	}
	out, err := EncodeOpenAI(r, "gpt-6-luna")
	if err != nil || !strings.Contains(string(out), `"input_image"`) {
		t.Fatalf("images must pass through to OpenAI: %v %s", err, out)
	}
}

func TestRefusalsAndErrors(t *testing.T) {
	r, _ := ParseOpenAI([]byte(oaiReq))
	resp, err := ParseOpenAIResponse([]byte(`{"model":"gpt-6-luna","answers":[{"type":"refusal","name":"department"},{"type":"predicate","name":"urgent","probability":0.1},{"type":"refusal","name":"anger"}],"usage":{"input_tokens":3}}`), r.Questions)
	if err != nil || resp.Answers[0].Kind != KindRefusal {
		t.Fatalf("refusals are answers: %v %+v", err, resp)
	}
	if out := string(EncodeSystemOneResponse(resp, r.Questions)); !strings.Contains(out, `"department":{"type":"refusal"}`) {
		t.Fatalf("%s", out)
	}
	for _, bad := range []string{`{"state":"x","questions":{}}`, `{"questions":{"a":{"type":"noul","instructions":"x"}}}`,
		`{"state":"x","questions":{"a":{"type":"rank","instructions":"x"}}}`, `{"state":"x","questions":{"a":{"type":"score","instructions":"x","criteria":["one"]}}}`} {
		if _, err := ParseSystemOne([]byte(bad)); err == nil {
			t.Fatalf("must refuse %s", bad)
		}
	}
	if _, err := ParseOpenAI([]byte(`{"model":"m","input":[{"role":"system","content":"x"}],"questions":[{"type":"predicate","instructions":"x"}]}`)); err == nil {
		t.Fatal("only user messages are evidence")
	}
}

func TestJSONStateIsKept(t *testing.T) {
	r, err := ParseSystemOne([]byte(`{"model":"jev","state":{"title":"A","body":"B"},"questions":{"x":{"type":"noul","instructions":"y"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := EncodeSystemOne(r, "jev")
	if !strings.Contains(string(out), `"state":{"title":"A","body":"B"}`) {
		t.Fatalf("a JSON state passes as JSON: %s", out)
	}
	oai, _ := EncodeOpenAI(r, "gpt-6-luna")
	if !strings.Contains(string(oai), `"input":"{\"title\":\"A\",\"body\":\"B\"}"`) {
		t.Fatalf("OpenAI reads it as JSON text: %s", oai)
	}
}

func TestEndpointsAndDialects(t *testing.T) {
	cases := []struct{ base, dialect, want string }{
		{"https://api.openai.com/v1", DialectOpenAI, "https://api.openai.com/v1/decisions"},
		{"https://api.openai.com", DialectOpenAI, "https://api.openai.com/v1/decisions"},
		{"https://openrouter.ai/api", DialectSystemOne, "https://openrouter.ai/api/alpha/decisions"},
		{"https://api.typesafe.ai", DialectSystemOne, "https://api.typesafe.ai/v1/systemone"},
		{"http://nimble:8000/v1", DialectSystemOne, "http://nimble:8000/v1/systemone"},
	}
	for _, c := range cases {
		if got := Endpoint(c.base, c.dialect); got != c.want {
			t.Errorf("Endpoint(%s,%s)=%s want %s", c.base, c.dialect, got, c.want)
		}
	}
	if DefaultDialect("https://api.openai.com/v1") != DialectOpenAI || DefaultDialect("https://openrouter.ai/api") != DialectSystemOne {
		t.Fatal("dialect detection")
	}
	if WireModel("gpt-6-luna:decisions", "https://api.openai.com/v1") != "gpt-6-luna" || WireModel("typesafe/jev-router", "https://openrouter.ai/api") != "~typesafe/jev-latest" {
		t.Fatal("wire model")
	}
}
