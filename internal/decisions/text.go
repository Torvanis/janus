package decisions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// TextField is one piece of caller-supplied text in a decision request
// outside the evidence: a question's instructions, yes/no boundaries,
// choice values and descriptions, level labels, question names, the
// safety identifier, and any field Janus does not interpret (forwarded
// as sent on same-dialect requests). The Security Gateway inspects them
// like the evidence.
type TextField struct {
	// Path locates the field ("questions.urgent.instructions").
	Path string
	Text string
	// Redactable is false where rewriting the text would change what the
	// answers refer to (question names, choice values, object keys) or
	// what the request means (question types): a redaction there has to
	// block instead.
	Redactable bool
}

// QuestionText lists the caller-supplied text of a request body in the
// inbound dialect, in document order, excluding the model name and the
// evidence (state / input), which are inspected separately.
func QuestionText(body []byte, dialect string) ([]TextField, error) {
	var out []TextField
	_, err := walkText(body, dialect, func(f TextField) string {
		out = append(out, f)
		return f.Text
	})
	return out, err
}

// RewriteQuestionText returns body with the fields QuestionText listed
// replaced, by their position in that list. Only redactable fields may be
// replaced; anything else is an error. Every other byte keeps its meaning
// and key order.
func RewriteQuestionText(body []byte, dialect string, replaced map[int]string) ([]byte, error) {
	i := 0
	var bad error
	out, err := walkText(body, dialect, func(f TextField) string {
		defer func() { i++ }()
		s, ok := replaced[i]
		if !ok || s == f.Text {
			return f.Text
		}
		if !f.Redactable && bad == nil {
			bad = fmt.Errorf("%s cannot be rewritten", f.Path)
		}
		return s
	})
	if err != nil {
		return nil, err
	}
	if bad != nil {
		return nil, bad
	}
	return out, nil
}

// walkText rebuilds body, offering every caller text field (as defined
// above) to visit and writing back what visit returns.
func walkText(body []byte, dialect string, visit func(TextField) string) ([]byte, error) {
	evidence := "state"
	if dialect == DialectOpenAI {
		evidence = "input"
	}
	keys, vals, err := orderedObject(body)
	if err != nil {
		return nil, err
	}
	// System One choice questions list their values as criteria keys or a
	// criteria array; score questions list level labels the same way.
	scoreQ := map[string]bool{}
	if dialect != DialectOpenAI {
		for i, k := range keys {
			if k != "questions" {
				continue
			}
			qk, qv, err := orderedObject(vals[i])
			if err != nil {
				break
			}
			for j, name := range qk {
				var t struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(qv[j], &t)
				scoreQ[name] = t.Type == "score"
			}
		}
	}
	w := &textWalker{dialect: dialect, visit: visit, scoreQ: scoreQ}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSON(&b, k)
		b.WriteByte(':')
		if k == "model" || k == evidence {
			b.Write(vals[i])
			continue
		}
		if err := w.walk(&b, vals[i], []string{k}); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type textWalker struct {
	dialect string
	visit   func(TextField) string
	scoreQ  map[string]bool
}

func (w *textWalker) walk(b *bytes.Buffer, raw json.RawMessage, path []string) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		b.Write(raw)
		return nil
	}
	switch t[0] {
	case '{':
		keys, vals, err := orderedObject(t)
		if err != nil {
			return err
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			// Keys are names the answers or the model refer to: inspected,
			// never rewritten.
			w.visit(TextField{Path: strings.Join(append(path, k), ".") + " (key)", Text: k})
			writeJSON(b, k)
			b.WriteByte(':')
			if err := w.walk(b, vals[i], append(path[:len(path):len(path)], k)); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(t, &items); err != nil {
			return err
		}
		b.WriteByte('[')
		for i, it := range items {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := w.walk(b, it, append(path[:len(path):len(path)], "*")); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return err
		}
		if w.isType(path) {
			// A question type is one of a few keywords the parser checks.
			b.Write(t)
			return nil
		}
		writeJSON(b, w.visit(TextField{Path: strings.Join(path, "."), Text: s, Redactable: w.redactable(path)}))
	default:
		b.Write(t)
	}
	return nil
}

func (w *textWalker) isType(path []string) bool {
	return len(path) == 3 && path[0] == "questions" && path[2] == "type"
}

// redactable reports whether a string value at path may be rewritten
// without changing what the answers refer to.
func (w *textWalker) redactable(path []string) bool {
	if len(path) < 3 || path[0] != "questions" {
		return true // safety_identifier, fields Janus does not interpret
	}
	if w.dialect == DialectOpenAI {
		// questions.*.name, questions.*.choices.*.value
		if len(path) == 3 && path[2] == "name" {
			return false
		}
		if len(path) >= 5 && path[2] == "choices" && path[4] == "value" {
			return false
		}
		return true
	}
	// System One: questions.<name>.criteria.* is a list of choice values
	// (choice) or of level labels (score); only labels may be rewritten.
	if len(path) == 4 && path[2] == "criteria" && path[3] == "*" {
		return w.scoreQ[path[1]]
	}
	return true
}
