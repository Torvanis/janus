// Package decisions translates between the two decision-API dialects Janus
// speaks, in both directions, for requests and responses:
//
//   - System One (TypeSafe api.typesafe.ai/v1/systemone, OpenRouter
//     /api/alpha/decisions, llama.cpp and Ollama /v1/systemone):
//     {model, state, questions: {id: {type: noul|choice|score, ...}}}
//     -> {model, answers: {id: {...}}, usage}.
//   - OpenAI Decisions (api.openai.com/v1/decisions):
//     {model, input, questions: [{type: predicate|choice|score, name, ...}]}
//     -> {model, answers: [...], usage}.
//
// Both describe the same three question kinds. A request is parsed into the
// canonical Request below and written out in the other dialect; answers come
// back the same way. Same-dialect traffic is never translated (the caller's
// bytes are forwarded). Anything that cannot be expressed in the target
// dialect is refused with a reason rather than silently dropped, except where
// the meaning survives in words (System One's yes/no criteria become part of
// an OpenAI predicate's instructions).
package decisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Dialects.
const (
	DialectSystemOne = "systemone"
	DialectOpenAI    = "openai"
)

// Canonical question kinds.
const (
	KindYesNo  = "yesno"
	KindChoice = "choice"
	KindScore  = "score"
)

// Choice is one option of a choice question. Value is a string or a bool
// (OpenAI allows boolean values; System One keys are always strings).
type Choice struct {
	Value       string
	Bool        bool // Value came in as a JSON boolean
	Description string
}

// Level is one level of a score question, lowest first.
type Level struct {
	Label       string
	Description string
}

// Question is one question in canonical form.
type Question struct {
	// Name identifies the answer. System One always has one (the map key);
	// OpenAI's is optional, and Unnamed records that it was absent.
	Name    string
	Unnamed bool
	Kind    string
	// Instructions is the question text.
	Instructions string
	// YesMeans / NoMeans spell out a yes/no question's boundary (System
	// One criteria {true,false} or instructions {counts_as_yes,_no}).
	YesMeans string
	NoMeans  string
	Choices  []Choice
	Levels   []Level
}

// Part is one piece of the evidence: text, or an image data URL.
type Part struct {
	Text     string
	ImageURL string
	Detail   string
}

// Request is a decision request in canonical form.
type Request struct {
	Model string
	// Parts is the evidence in order. A System One JSON state becomes one
	// text part holding its JSON (StateJSON keeps the original).
	Parts     []Part
	StateJSON json.RawMessage
	Questions []Question
	// SafetyIdentifier is OpenAI's opaque end-user id (dropped for System
	// One, which has no equivalent).
	SafetyIdentifier string
}

// Text joins the request's text parts (what a text-only guard reads).
func (r Request) Text() string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// Images counts image parts.
func (r Request) Images() int {
	n := 0
	for _, p := range r.Parts {
		if p.ImageURL != "" {
			n++
		}
	}
	return n
}

// Prob is one entry of an answer's distribution.
type Prob struct {
	Value       string
	Bool        bool
	Index       int // score level index
	Label       string
	Probability float64
}

// Answer is one answer in canonical form.
type Answer struct {
	Name    string
	Unnamed bool
	// Kind is KindYesNo / KindChoice / KindScore, or "refusal".
	Kind          string
	Probability   float64 // yes/no: P(yes)
	Choice        string
	ChoiceBool    bool
	Score         float64
	Confidence    *float64
	Probabilities []Prob
}

// KindRefusal is an answer the model declined to give (OpenAI only).
const KindRefusal = "refusal"

// Usage is what a decision call consumed.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	// CostUSD is set when the provider reports a charge (OpenRouter).
	CostUSD      float64
	CostReported bool
}

// Response is a decision response in canonical form.
type Response struct {
	Model   string
	Answers []Answer
	Usage   Usage
	// Extra keeps provider fields worth passing on (OpenRouter id/provider)
	// when the response is written back in System One form.
	Extra map[string]json.RawMessage
}

// Choice-count limits shared by both dialects (OpenAI documents 2..255; the
// System One servers accept the same range or fewer, see MaxOptions).
const (
	MinChoices = 2
	MaxChoices = 255
)

// ErrUnsupported marks a request that cannot be expressed in the target
// dialect. Its message is written for the caller.
var ErrUnsupported = errors.New("not supported")

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

// --- System One --------------------------------------------------------------

// ParseSystemOne reads a System One request. Question order follows the
// body, so answers can be returned in the order they were asked.
func ParseSystemOne(body []byte) (Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return Request{}, fmt.Errorf("the body is not a JSON object: %w", err)
	}
	var req Request
	_ = json.Unmarshal(top["model"], &req.Model)
	state := bytes.TrimSpace(top["state"])
	switch {
	case len(state) == 0 || string(state) == "null":
		return Request{}, errors.New("state is required")
	case state[0] == '"':
		var s string
		if err := json.Unmarshal(state, &s); err != nil {
			return Request{}, errors.New("state is not valid JSON")
		}
		req.Parts = []Part{{Text: s}}
	default:
		// A JSON object or array: the model reads it as JSON.
		req.StateJSON = append(json.RawMessage{}, state...)
		req.Parts = []Part{{Text: string(state)}}
	}
	keys, raws, err := orderedObject(top["questions"])
	if err != nil {
		return Request{}, fmt.Errorf("questions: %w", err)
	}
	if len(keys) == 0 {
		return Request{}, errors.New("ask at least one question")
	}
	for i, k := range keys {
		q, err := parseS1Question(k, raws[i])
		if err != nil {
			return Request{}, fmt.Errorf("questions.%s: %w", k, err)
		}
		req.Questions = append(req.Questions, q)
	}
	return req, nil
}

func parseS1Question(name string, raw json.RawMessage) (Question, error) {
	var q struct {
		Type         string          `json:"type"`
		Instructions json.RawMessage `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return Question{}, errors.New("not a question object")
	}
	out := Question{Name: name}
	// instructions: a string, or {question, counts_as_yes, counts_as_no}.
	if len(q.Instructions) > 0 && q.Instructions[0] == '{' {
		var o struct {
			Question    string `json:"question"`
			CountsAsYes string `json:"counts_as_yes"`
			CountsAsNo  string `json:"counts_as_no"`
		}
		_ = json.Unmarshal(q.Instructions, &o)
		out.Instructions, out.YesMeans, out.NoMeans = o.Question, o.CountsAsYes, o.CountsAsNo
	} else {
		_ = json.Unmarshal(q.Instructions, &out.Instructions)
	}
	switch q.Type {
	case "noul":
		out.Kind = KindYesNo
		if len(q.Criteria) > 0 && q.Criteria[0] == '{' {
			var c map[string]string
			if json.Unmarshal(q.Criteria, &c) == nil {
				if v := c["true"]; v != "" {
					out.YesMeans = v
				}
				if v := c["false"]; v != "" {
					out.NoMeans = v
				}
			}
		}
	case "choice":
		out.Kind = KindChoice
		keys, raws, err := orderedObject(q.Criteria)
		if err != nil {
			// criteria may also be a plain list of option names.
			var list []string
			if json.Unmarshal(q.Criteria, &list) != nil || len(list) == 0 {
				return Question{}, errors.New("a choice question needs criteria: {value: description}")
			}
			for _, v := range list {
				out.Choices = append(out.Choices, Choice{Value: v})
			}
			if err := checkChoices(out.Choices); err != nil {
				return Question{}, err
			}
			break
		}
		for i, k := range keys {
			var d string
			_ = json.Unmarshal(raws[i], &d)
			out.Choices = append(out.Choices, Choice{Value: k, Description: d})
		}
		if err := checkChoices(out.Choices); err != nil {
			return Question{}, err
		}
	case "score":
		out.Kind = KindScore
		var labels []json.RawMessage
		if err := json.Unmarshal(q.Criteria, &labels); err != nil || len(labels) < 2 {
			return Question{}, errors.New("a score question needs criteria: [at least two level labels, lowest first]")
		}
		for _, l := range labels {
			var s string
			if json.Unmarshal(l, &s) == nil {
				out.Levels = append(out.Levels, Level{Label: s})
				continue
			}
			var o struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			}
			if json.Unmarshal(l, &o) != nil || o.Label == "" {
				return Question{}, errors.New("each score level must be a label")
			}
			out.Levels = append(out.Levels, Level(o))
		}
	default:
		return Question{}, fmt.Errorf("unknown question type %q (noul, choice or score)", q.Type)
	}
	if strings.TrimSpace(out.Instructions) == "" {
		return Question{}, errors.New("instructions are required")
	}
	return out, nil
}

// EncodeSystemOne writes a System One request. Images cannot be expressed.
func EncodeSystemOne(r Request, model string) ([]byte, error) {
	if r.Images() > 0 {
		return nil, unsupported("this model reads text only; the request carries %d image(s)", r.Images())
	}
	type s1q struct {
		Type         string `json:"type"`
		Instructions any    `json:"instructions"`
		Criteria     any    `json:"criteria,omitempty"`
	}
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	writeJSON(&b, model)
	b.WriteString(`,"state":`)
	if len(r.StateJSON) > 0 {
		b.Write(r.StateJSON)
	} else {
		writeJSON(&b, r.Text())
	}
	b.WriteString(`,"questions":{`)
	keys := s1Keys(r.Questions)
	for i, q := range r.Questions {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSON(&b, keys[i])
		b.WriteByte(':')
		out := s1q{Instructions: q.Instructions}
		switch q.Kind {
		case KindYesNo:
			out.Type = "noul"
			if q.YesMeans != "" || q.NoMeans != "" {
				out.Instructions = map[string]string{"question": q.Instructions, "counts_as_yes": q.YesMeans, "counts_as_no": q.NoMeans}
			}
		case KindChoice:
			out.Type = "choice"
			out.Criteria = orderedStrings(q.Choices)
		case KindScore:
			out.Type = "score"
			labels := make([]string, len(q.Levels))
			for j, l := range q.Levels {
				labels[j] = l.Label
				if l.Description != "" {
					labels[j] = l.Label + ": " + l.Description
				}
			}
			out.Criteria = labels
		}
		writeJSON(&b, out)
	}
	b.WriteString(`}}`)
	return b.Bytes(), nil
}

// s1Keys are the System One keys for the questions: each one's name, or
// q<i> for a question the OpenAI caller left unnamed. A generated key never
// collides with a real name (or another generated key): it gets a suffix.
func s1Keys(qs []Question) []string {
	taken := make(map[string]bool, len(qs))
	for _, q := range qs {
		if !q.Unnamed && q.Name != "" {
			taken[q.Name] = true
		}
	}
	keys := make([]string, len(qs))
	for i, q := range qs {
		if !q.Unnamed && q.Name != "" {
			keys[i] = q.Name
			continue
		}
		k := "q" + strconv.Itoa(i)
		for n := 2; taken[k]; n++ {
			k = "q" + strconv.Itoa(i) + "_" + strconv.Itoa(n)
		}
		taken[k] = true
		keys[i] = k
	}
	return keys
}

// ParseSystemOneResponse reads a System One response for the questions asked
// (in that order).
func ParseSystemOneResponse(raw []byte, asked []Question) (Response, error) {
	var top struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			InputTokens  int64    `json:"input_tokens"`
			OutputTokens int64    `json:"output_tokens"`
			Cost         *float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return Response{}, fmt.Errorf("unparseable decision response: %w", err)
	}
	out := Response{Model: top.Model, Usage: Usage{InputTokens: top.Usage.InputTokens, OutputTokens: top.Usage.OutputTokens}}
	if top.Usage.Cost != nil {
		out.Usage.CostUSD, out.Usage.CostReported = *top.Usage.Cost, true
	}
	var extra map[string]json.RawMessage
	if json.Unmarshal(raw, &extra) == nil {
		for _, k := range []string{"model", "answers", "usage"} {
			delete(extra, k)
		}
		if len(extra) > 0 {
			out.Extra = extra
		}
	}
	keys := s1Keys(asked)
	for i, q := range asked {
		key := keys[i]
		a := Answer{Name: q.Name, Unnamed: q.Unnamed}
		rawA, ok := top.Answers[key]
		if !ok {
			return Response{}, fmt.Errorf("no answer for %s", key)
		}
		var ans struct {
			Type          string             `json:"type"`
			Choice        json.RawMessage    `json:"choice"`
			Noul          *float64           `json:"noul"`
			Score         *float64           `json:"score"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
			Legend        map[string]string  `json:"legend"`
		}
		if err := json.Unmarshal(rawA, &ans); err != nil {
			return Response{}, fmt.Errorf("answer %s: %w", key, err)
		}
		a.Confidence = ans.Confidence
		switch {
		case ans.Type == KindRefusal:
			a.Kind = KindRefusal
		case q.Kind == KindYesNo:
			if ans.Noul == nil {
				return Response{}, fmt.Errorf("answer %s has no yes/no probability", key)
			}
			a.Kind, a.Probability = KindYesNo, *ans.Noul
		case q.Kind == KindChoice:
			a.Kind = KindChoice
			_ = json.Unmarshal(ans.Choice, &a.Choice)
			for _, c := range q.Choices {
				p := Prob{Value: c.Value, Bool: c.Bool, Probability: ans.Probabilities[c.Value]}
				a.Probabilities = append(a.Probabilities, p)
				if c.Value == a.Choice {
					a.ChoiceBool = c.Bool
				}
			}
		case q.Kind == KindScore:
			a.Kind = KindScore
			if ans.Score != nil {
				a.Score = *ans.Score
			}
			for j, l := range q.Levels {
				a.Probabilities = append(a.Probabilities, Prob{Index: j, Label: l.Label, Probability: ans.Probabilities[strconv.Itoa(j)]})
			}
		}
		out.Answers = append(out.Answers, a)
	}
	return out, nil
}

// EncodeSystemOneResponse writes a response in System One form, keyed by the
// names the caller asked with.
func EncodeSystemOneResponse(r Response, asked []Question) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	writeJSON(&b, r.Model)
	b.WriteString(`,"answers":{`)
	keys := s1Keys(asked)
	for i := range r.Answers {
		a := r.Answers[i]
		if i > 0 {
			b.WriteByte(',')
		}
		key := "q" + strconv.Itoa(i)
		if i < len(keys) {
			key = keys[i]
		}
		writeJSON(&b, key)
		b.WriteByte(':')
		switch a.Kind {
		case KindYesNo:
			writeJSON(&b, map[string]any{"type": "noul", "noul": a.Probability})
		case KindChoice:
			probs := &orderedFloats{}
			for _, p := range a.Probabilities {
				probs.add(p.Value, p.Probability)
			}
			b.WriteString(`{"type":"choice","choice":`)
			writeJSON(&b, a.Choice)
			b.WriteString(`,"probabilities":`)
			probs.write(&b)
			if a.Confidence != nil {
				b.WriteString(`,"confidence":`)
				writeJSON(&b, *a.Confidence)
			}
			b.WriteByte('}')
		case KindScore:
			probs, legend := &orderedFloats{}, &orderedFloats{strs: true}
			for _, p := range a.Probabilities {
				probs.add(strconv.Itoa(p.Index), p.Probability)
				legend.addStr(strconv.Itoa(p.Index), p.Label)
			}
			b.WriteString(`{"type":"score","score":`)
			writeJSON(&b, a.Score)
			b.WriteString(`,"legend":`)
			legend.write(&b)
			b.WriteString(`,"probabilities":`)
			probs.write(&b)
			if a.Confidence != nil {
				b.WriteString(`,"confidence":`)
				writeJSON(&b, *a.Confidence)
			}
			b.WriteByte('}')
		default:
			b.WriteString(`{"type":"refusal"}`)
		}
	}
	b.WriteString(`},"usage":`)
	u := map[string]any{"input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens}
	if r.Usage.CostReported {
		u["cost"] = r.Usage.CostUSD
	}
	writeJSON(&b, u)
	for _, k := range sortedKeys(r.Extra) {
		b.WriteByte(',')
		writeJSON(&b, k)
		b.WriteByte(':')
		b.Write(r.Extra[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// --- OpenAI -------------------------------------------------------------------

// ParseOpenAI reads an OpenAI Decisions request.
func ParseOpenAI(body []byte) (Request, error) {
	var top struct {
		Model            string            `json:"model"`
		Input            json.RawMessage   `json:"input"`
		Questions        []json.RawMessage `json:"questions"`
		SafetyIdentifier *string           `json:"safety_identifier"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return Request{}, fmt.Errorf("the body is not a decisions request: %w", err)
	}
	req := Request{Model: top.Model}
	if top.SafetyIdentifier != nil {
		req.SafetyIdentifier = *top.SafetyIdentifier
	}
	in := bytes.TrimSpace(top.Input)
	switch {
	case len(in) == 0 || string(in) == "null":
		return Request{}, errors.New("input is required")
	case in[0] == '"':
		var s string
		_ = json.Unmarshal(in, &s)
		req.Parts = []Part{{Text: s}}
	case in[0] == '[':
		var msgs []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(in, &msgs); err != nil {
			return Request{}, errors.New("input must be a string or a list of user messages")
		}
		for _, m := range msgs {
			if m.Role != "" && m.Role != "user" {
				return Request{}, fmt.Errorf("input messages must have role user (got %q)", m.Role)
			}
			c := bytes.TrimSpace(m.Content)
			if len(c) > 0 && c[0] == '"' {
				var s string
				_ = json.Unmarshal(c, &s)
				req.Parts = append(req.Parts, Part{Text: s})
				continue
			}
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
				Detail   string `json:"detail"`
			}
			if err := json.Unmarshal(c, &parts); err != nil {
				return Request{}, errors.New("message content must be a string or a list of input_text / input_image parts")
			}
			for _, p := range parts {
				switch p.Type {
				case "input_text":
					req.Parts = append(req.Parts, Part{Text: p.Text})
				case "input_image":
					req.Parts = append(req.Parts, Part{ImageURL: p.ImageURL, Detail: p.Detail})
				default:
					return Request{}, fmt.Errorf("unsupported content part %q (input_text or input_image)", p.Type)
				}
			}
		}
	default:
		return Request{}, errors.New("input must be a string or a list of user messages")
	}
	if len(top.Questions) == 0 {
		return Request{}, errors.New("ask at least one question")
	}
	names := map[string]bool{}
	for i, raw := range top.Questions {
		q, err := parseOAIQuestion(raw)
		if err != nil {
			return Request{}, fmt.Errorf("questions[%d]: %w", i, err)
		}
		if !q.Unnamed {
			if names[q.Name] {
				return Request{}, fmt.Errorf("questions[%d]: the name %q is used by another question; question names must be unique", i, q.Name)
			}
			names[q.Name] = true
		}
		req.Questions = append(req.Questions, q)
	}
	return req, nil
}

// checkChoices enforces the choice-count limits and unique values: a
// duplicate value would be one key twice in a System One body.
func checkChoices(cs []Choice) error {
	if len(cs) < MinChoices {
		return fmt.Errorf("a choice question needs at least %d choices", MinChoices)
	}
	if len(cs) > MaxChoices {
		return fmt.Errorf("a choice question takes at most %d choices (got %d)", MaxChoices, len(cs))
	}
	seen := make(map[string]bool, len(cs))
	for _, c := range cs {
		if seen[c.Value] {
			return fmt.Errorf("the choice value %q is listed twice", c.Value)
		}
		seen[c.Value] = true
	}
	return nil
}

// MostChoices is the largest number of choices any question offers.
func (r Request) MostChoices() int {
	n := 0
	for _, q := range r.Questions {
		n = max(n, len(q.Choices))
	}
	return n
}

func parseOAIQuestion(raw json.RawMessage) (Question, error) {
	var q struct {
		Type         string  `json:"type"`
		Name         *string `json:"name"`
		Instructions string  `json:"instructions"`
		Choices      []struct {
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"choices"`
		Levels []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"levels"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return Question{}, errors.New("not a question object")
	}
	out := Question{Instructions: q.Instructions}
	if q.Name != nil {
		out.Name = *q.Name
	} else {
		out.Unnamed = true
	}
	switch q.Type {
	case "predicate":
		out.Kind = KindYesNo
	case "choice":
		out.Kind = KindChoice
		for _, c := range q.Choices {
			v := bytes.TrimSpace(c.Value)
			ch := Choice{Description: c.Description}
			switch string(v) {
			case "true", "false":
				ch.Value, ch.Bool = string(v), true
			default:
				if json.Unmarshal(v, &ch.Value) != nil {
					return Question{}, errors.New("a choice value must be a string or a boolean")
				}
			}
			out.Choices = append(out.Choices, ch)
		}
		if err := checkChoices(out.Choices); err != nil {
			return Question{}, err
		}
	case "score":
		out.Kind = KindScore
		if len(q.Levels) < 2 {
			return Question{}, errors.New("a score question needs at least two levels")
		}
		for _, l := range q.Levels {
			out.Levels = append(out.Levels, Level{Label: l.Label, Description: l.Description})
		}
	default:
		return Question{}, fmt.Errorf("unknown question type %q (predicate, choice or score)", q.Type)
	}
	if strings.TrimSpace(out.Instructions) == "" {
		return Question{}, errors.New("instructions are required")
	}
	return out, nil
}

// EncodeOpenAI writes an OpenAI Decisions request. System One's yes/no
// boundary is folded into the predicate's instructions, which is the only
// place OpenAI has for it.
func EncodeOpenAI(r Request, model string) ([]byte, error) {
	type oaiChoice struct {
		Value       any    `json:"value"`
		Description string `json:"description,omitempty"`
	}
	type oaiLevel struct {
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}
	type oaiQ struct {
		Type         string      `json:"type"`
		Name         *string     `json:"name,omitempty"`
		Instructions string      `json:"instructions"`
		Choices      []oaiChoice `json:"choices,omitempty"`
		Levels       []oaiLevel  `json:"levels,omitempty"`
	}
	body := map[string]any{"model": model}
	if r.Images() == 0 {
		body["input"] = r.Text()
	} else {
		parts := []map[string]string{}
		for _, p := range r.Parts {
			if p.ImageURL != "" {
				m := map[string]string{"type": "input_image", "image_url": p.ImageURL}
				if p.Detail != "" {
					m["detail"] = p.Detail
				}
				parts = append(parts, m)
			} else {
				parts = append(parts, map[string]string{"type": "input_text", "text": p.Text})
			}
		}
		body["input"] = []map[string]any{{"role": "user", "content": parts}}
	}
	qs := make([]oaiQ, 0, len(r.Questions))
	keys := s1Keys(r.Questions)
	for i, q := range r.Questions {
		out := oaiQ{Instructions: q.Instructions}
		if !q.Unnamed {
			name := keys[i]
			out.Name = &name
		}
		switch q.Kind {
		case KindYesNo:
			out.Type = "predicate"
			if q.YesMeans != "" {
				out.Instructions += "\nCounts as yes: " + q.YesMeans
			}
			if q.NoMeans != "" {
				out.Instructions += "\nCounts as no: " + q.NoMeans
			}
		case KindChoice:
			out.Type = "choice"
			for _, c := range q.Choices {
				var v any = c.Value
				if c.Bool {
					v = c.Value == "true"
				}
				out.Choices = append(out.Choices, oaiChoice{Value: v, Description: c.Description})
			}
		case KindScore:
			out.Type = "score"
			for _, l := range q.Levels {
				out.Levels = append(out.Levels, oaiLevel(l))
			}
		}
		qs = append(qs, out)
	}
	body["questions"] = qs
	if r.SafetyIdentifier != "" {
		body["safety_identifier"] = r.SafetyIdentifier
	}
	return json.Marshal(body)
}

// ParseOpenAIResponse reads an OpenAI Decisions response. Answers arrive in
// question order.
func ParseOpenAIResponse(raw []byte, asked []Question) (Response, error) {
	var top struct {
		Model   string `json:"model"`
		Answers []struct {
			Type          string          `json:"type"`
			Name          *string         `json:"name"`
			Probability   *float64        `json:"probability"`
			Choice        json.RawMessage `json:"choice"`
			Score         *float64        `json:"score"`
			Confidence    *float64        `json:"confidence"`
			Probabilities []struct {
				Value       json.RawMessage `json:"value"`
				Label       string          `json:"label"`
				Probability float64         `json:"probability"`
			} `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return Response{}, fmt.Errorf("unparseable decision response: %w", err)
	}
	if len(top.Answers) != len(asked) {
		return Response{}, fmt.Errorf("asked %d questions, got %d answers", len(asked), len(top.Answers))
	}
	out := Response{Model: top.Model, Usage: Usage{InputTokens: top.Usage.InputTokens, OutputTokens: top.Usage.OutputTokens,
		CachedTokens: top.Usage.InputTokensDetails.CachedTokens}}
	for i, a := range top.Answers {
		q := asked[i]
		ans := Answer{Name: q.Name, Unnamed: q.Unnamed, Confidence: a.Confidence}
		switch a.Type {
		case "refusal":
			ans.Kind = KindRefusal
		case "predicate":
			if a.Probability == nil {
				return Response{}, fmt.Errorf("answer %d has no probability", i)
			}
			ans.Kind, ans.Probability = KindYesNo, *a.Probability
		case "choice":
			ans.Kind = KindChoice
			ans.Choice, ans.ChoiceBool = rawValue(a.Choice)
			for _, p := range a.Probabilities {
				v, isBool := rawValue(p.Value)
				ans.Probabilities = append(ans.Probabilities, Prob{Value: v, Bool: isBool, Probability: p.Probability})
			}
		case "score":
			ans.Kind = KindScore
			if a.Score != nil {
				ans.Score = *a.Score
			}
			for j, p := range a.Probabilities {
				idx := j
				_ = json.Unmarshal(p.Value, &idx)
				label := p.Label
				if label == "" && idx < len(q.Levels) {
					label = q.Levels[idx].Label
				}
				ans.Probabilities = append(ans.Probabilities, Prob{Index: idx, Label: label, Probability: p.Probability})
			}
		default:
			return Response{}, fmt.Errorf("answer %d has unknown type %q", i, a.Type)
		}
		out.Answers = append(out.Answers, ans)
	}
	return out, nil
}

// EncodeOpenAIResponse writes a response in OpenAI Decisions form.
func EncodeOpenAIResponse(r Response) []byte {
	answers := make([]map[string]any, 0, len(r.Answers))
	for _, a := range r.Answers {
		var name any
		if !a.Unnamed {
			name = a.Name
		}
		m := map[string]any{"name": name}
		switch a.Kind {
		case KindYesNo:
			m["type"], m["probability"] = "predicate", a.Probability
		case KindChoice:
			m["type"], m["choice"] = "choice", choiceValue(a.Choice, a.ChoiceBool)
			probs := make([]map[string]any, 0, len(a.Probabilities))
			for _, p := range a.Probabilities {
				probs = append(probs, map[string]any{"value": choiceValue(p.Value, p.Bool), "probability": p.Probability})
			}
			m["probabilities"] = probs
			m["confidence"] = confidenceOr(a)
		case KindScore:
			m["type"], m["score"] = "score", a.Score
			probs := make([]map[string]any, 0, len(a.Probabilities))
			for _, p := range a.Probabilities {
				probs = append(probs, map[string]any{"value": p.Index, "label": p.Label, "probability": p.Probability})
			}
			m["probabilities"] = probs
			m["confidence"] = confidenceOr(a)
		default:
			m["type"] = "refusal"
		}
		answers = append(answers, m)
	}
	body, _ := json.Marshal(map[string]any{
		"model":   r.Model,
		"answers": answers,
		"usage": map[string]any{
			"input_tokens":          r.Usage.InputTokens,
			"input_tokens_details":  map[string]int64{"cached_tokens": r.Usage.CachedTokens, "cache_write_tokens": 0},
			"output_tokens":         r.Usage.OutputTokens,
			"output_tokens_details": map[string]int64{"reasoning_tokens": 0},
			"total_tokens":          r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	})
	return body
}

// confidenceOr is the reported confidence, or the top probability when the
// provider gave none (OpenAI's schema requires the field).
func confidenceOr(a Answer) float64 {
	if a.Confidence != nil {
		return *a.Confidence
	}
	top := 0.0
	for _, p := range a.Probabilities {
		if p.Probability > top {
			top = p.Probability
		}
	}
	return top
}

func choiceValue(v string, isBool bool) any {
	if isBool {
		return v == "true"
	}
	return v
}

func rawValue(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "true" || t == "false" {
		return t, true
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s, false
}

// --- helpers ------------------------------------------------------------------

// orderedObject returns a JSON object's keys and values in document order.
func orderedObject(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, errors.New("expected an object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("expected an object")
	}
	var keys []string
	var vals []json.RawMessage
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if seen[k] {
			return nil, nil, fmt.Errorf("duplicate key %q", k)
		}
		seen[k] = true
		keys = append(keys, k)
		vals = append(vals, v)
	}
	return keys, vals, nil
}

// orderedStrings marshals choices as an object in their order.
type orderedStrings []Choice

func (o orderedStrings) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, c := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSON(&b, c.Value)
		b.WriteByte(':')
		writeJSON(&b, c.Description)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type orderedFloats struct {
	keys []string
	nums []float64
	strs bool
	vals []string
}

func (o *orderedFloats) add(k string, v float64) {
	o.keys = append(o.keys, k)
	o.nums = append(o.nums, v)
}
func (o *orderedFloats) addStr(k, v string) { o.keys = append(o.keys, k); o.vals = append(o.vals, v) }
func (o *orderedFloats) write(b *bytes.Buffer) {
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSON(b, k)
		b.WriteByte(':')
		if o.strs {
			writeJSON(b, o.vals[i])
		} else {
			writeJSON(b, o.nums[i])
		}
	}
	b.WriteByte('}')
}

func writeJSON(b *bytes.Buffer, v any) {
	raw, _ := json.Marshal(v)
	b.Write(raw)
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Endpoint is the decision URL for a provider base URL in a dialect.
func Endpoint(baseURL, dialect string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if dialect == DialectOpenAI {
		switch {
		case strings.HasSuffix(base, "/decisions"):
			return base
		case strings.HasSuffix(base, "/v1"):
			return base + "/decisions"
		default:
			return base + "/v1/decisions"
		}
	}
	switch {
	case strings.HasSuffix(base, "/systemone"), strings.HasSuffix(base, "/decisions"):
		return base
	case strings.Contains(base, "openrouter.ai"):
		base = strings.TrimSuffix(base, "/v1")
		if !strings.HasSuffix(base, "/api") {
			base += "/api"
		}
		return base + "/alpha/decisions"
	case strings.HasSuffix(base, "/v1"):
		return base + "/systemone"
	default:
		return base + "/v1/systemone"
	}
}

// DefaultDialect is the dialect a provider speaks when the admin has not
// said: OpenAI's own API (and Azure OpenAI) speak the OpenAI dialect, every
// other known decision server speaks System One.
func DefaultDialect(baseURL string) string {
	b := strings.ToLower(baseURL)
	if strings.Contains(b, "api.openai.com") || strings.Contains(b, ".openai.azure.com") {
		return DialectOpenAI
	}
	return DialectSystemOne
}

// WireModel is the model name a decision API answers to for a catalog name:
// Janus lists OpenAI's decision model as "<model>:decisions" next to the chat
// model, and OpenRouter lists Jev as typesafe/jev-router while its decisions
// endpoint answers only to ~typesafe/jev-latest.
func WireModel(name, baseURL string) string {
	if strings.Contains(baseURL, "openrouter.ai") && name == "typesafe/jev-router" {
		return "~typesafe/jev-latest"
	}
	return strings.TrimSuffix(name, DecisionsSuffix)
}

// DecisionsSuffix marks the catalog row of a model served on a decision
// endpoint when the same model also serves chat.
const DecisionsSuffix = ":decisions"
