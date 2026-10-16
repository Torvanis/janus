package decisions

import (
	"strings"
	"testing"
)

// OpenAI requests with duplicate question names, or an unnamed question
// whose generated key matches a named one, must never translate to a System
// One body with one key twice (a question silently lost).
func TestDuplicateOrCollidingNamesNeverDuplicateS1Keys(t *testing.T) {
	dup := `{"model":"m","input":"x","questions":[{"type":"predicate","name":"a","instructions":"Is it spam?"},{"type":"predicate","name":"a","instructions":"Is it urgent?"}]}`
	if _, err := ParseOpenAI([]byte(dup)); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("duplicate names must be refused: %v", err)
	}
	collide := `{"model":"m","input":"x","questions":[{"type":"predicate","name":"q1","instructions":"Is it spam?"},{"type":"predicate","instructions":"Is it urgent?"}]}`
	req, err := ParseOpenAI([]byte(collide))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeSystemOne(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	keys, _, err := orderedObject(questionsOf(t, out))
	if err != nil || len(keys) != 2 {
		t.Fatalf("translated body has ambiguous keys (%v): %s", err, out)
	}
	// The answer to each question comes back from its own key.
	resp := `{"model":"m","answers":{"q1":{"type":"noul","noul":0.1},"` + keys[1] + `":{"type":"noul","noul":0.9}},"usage":{}}`
	parsed, err := ParseSystemOneResponse([]byte(resp), req.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Answers[0].Probability != 0.1 || parsed.Answers[1].Probability != 0.9 {
		t.Errorf("answers crossed: %+v", parsed.Answers)
	}
	back := EncodeSystemOneResponse(parsed, req.Questions)
	if k, _, err := orderedObject(fieldOf(t, back, "answers")); err != nil || len(k) != 2 {
		t.Errorf("System One response has ambiguous keys (%v): %s", err, back)
	}
}

// Choice questions take 2..255 options with unique values, in both dialects.
func TestChoiceCountAndValuesAreValidated(t *testing.T) {
	for _, body := range []string{
		`{"state":"x","questions":{"a":{"type":"choice","instructions":"pick","criteria":{"only":"the one"}}}}`,
		`{"state":"x","questions":{"a":{"type":"choice","instructions":"pick","criteria":["only"]}}}`,
		`{"state":"x","questions":{"a":{"type":"choice","instructions":"pick","criteria":["a","a"]}}}`,
		`{"state":"x","questions":{"a":{"type":"choice","instructions":"pick","criteria":` + manyList(256) + `}}}`,
	} {
		if _, err := ParseSystemOne([]byte(body)); err == nil {
			t.Errorf("accepted: %.120s", body)
		}
	}
	if _, err := ParseSystemOne([]byte(`{"state":"x","questions":{"a":{"type":"choice","instructions":"pick","criteria":` + manyList(255) + `}}}`)); err != nil {
		t.Errorf("255 choices must be accepted: %v", err)
	}
	var b strings.Builder
	b.WriteString(`{"model":"m","input":"x","questions":[{"type":"choice","name":"a","instructions":"pick","choices":[`)
	for i := 0; i < 300; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"value":"v` + itoa3(i) + `"}`)
	}
	b.WriteString(`]}]}`)
	if _, err := ParseOpenAI([]byte(b.String())); err == nil {
		t.Errorf("a 300-option choice question was accepted (limit 255)")
	}
	if _, err := ParseOpenAI([]byte(`{"model":"m","input":"x","questions":[{"type":"choice","name":"a","instructions":"pick","choices":[{"value":"x"},{"value":"x"}]}]}`)); err == nil {
		t.Errorf("duplicate choice values were accepted")
	}
}

// QuestionText lists every caller text outside the evidence, and
// RewriteQuestionText rewrites only redactable fields.
func TestQuestionTextCoversEveryCallerString(t *testing.T) {
	s1 := `{"model":"m","state":"EVIDENCE","questions":{"n1":{"type":"noul","instructions":{"question":"Q","counts_as_yes":"Y","counts_as_no":"N"}},` +
		`"c1":{"type":"choice","instructions":"C","criteria":{"v1":"D1","v2":"D2"}},"s1":{"type":"score","instructions":"S","criteria":["L0",{"label":"L1","description":"LD"}]}},"extra":"E"}`
	fields, err := QuestionText([]byte(s1), DialectSystemOne)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range fields {
		got[f.Text] = f.Redactable
	}
	for _, want := range []string{"Q", "Y", "N", "C", "D1", "D2", "S", "L0", "L1", "LD", "E", "n1", "v1"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q not inspected", want)
		}
	}
	for _, never := range []string{"EVIDENCE", "m", "noul", "choice"} {
		if _, ok := got[never]; ok {
			t.Errorf("%q should not be in the question text", never)
		}
	}
	if got["v1"] || got["n1"] || !got["D1"] || !got["L0"] {
		t.Errorf("redactable flags wrong: %v", got)
	}
	idx := map[string]int{}
	for i, f := range fields {
		idx[f.Text] = i
	}
	out, err := RewriteQuestionText([]byte(s1), DialectSystemOne, map[int]string{idx["D1"]: "[R]", idx["L0"]: "[R0]"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseSystemOne(out)
	if err != nil {
		t.Fatal(err)
	}
	if req.Questions[1].Choices[0].Description != "[R]" || req.Questions[2].Levels[0].Label != "[R0]" || req.Text() != "EVIDENCE" {
		t.Errorf("rewrite: %s", out)
	}
	if _, err := RewriteQuestionText([]byte(s1), DialectSystemOne, map[int]string{idx["v1"]: "x"}); err == nil {
		t.Error("a choice value must not be rewritable")
	}

	oai := `{"model":"m","input":"EVIDENCE","safety_identifier":"SID","questions":[{"type":"choice","name":"NM","instructions":"I","choices":[{"value":"VAL","description":"DSC"},{"value":true}]}]}`
	fields, err = QuestionText([]byte(oai), DialectOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]bool{}
	for _, f := range fields {
		got[f.Text] = f.Redactable
	}
	for _, want := range []string{"SID", "NM", "I", "VAL", "DSC"} {
		if _, ok := got[want]; !ok {
			t.Errorf("openai: %q not inspected", want)
		}
	}
	if got["NM"] || got["VAL"] || !got["DSC"] || !got["I"] {
		t.Errorf("openai redactable flags wrong: %v", got)
	}
	if _, ok := got["EVIDENCE"]; ok {
		t.Error("openai: evidence must be left to the evidence scan")
	}
}

func manyList(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"o` + itoa3(i) + `"`)
	}
	b.WriteByte(']')
	return b.String()
}

func itoa3(i int) string {
	return string(rune('0'+i/100)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

func questionsOf(t *testing.T, body []byte) []byte { return fieldOf(t, body, "questions") }

func fieldOf(t *testing.T, body []byte, name string) []byte {
	t.Helper()
	keys, vals, err := orderedObject(body)
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range keys {
		if k == name {
			return vals[i]
		}
	}
	t.Fatalf("no %s", name)
	return nil
}
