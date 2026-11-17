// Package jsontext finds the text in a JSON document whatever its layout,
// and writes changed text back.
//
// The Security Gateway's checks read text. A request or reply reaches the
// gateway as JSON whose layout depends on the endpoint (chat messages, a
// Responses input, a prompt, an embeddings input, a provider's own format).
// Scanning the raw bytes misses text that JSON escaping disguises (a secret
// written with \u escapes) and cannot rewrite anything; reading only the
// layouts the gateway knows misses the rest. Parse decodes the document and
// lists every string value in it, so a check sees the text as the provider
// will and a redaction can replace exactly that value.
package jsontext

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Text is one string value of a document.
type Text struct {
	// Path is the keys and array indexes that lead to the value.
	Path []string
	// Value is the decoded string.
	Value string
	// Role is the "role" of the nearest enclosing object that has one
	// ("user", "assistant", "system", "tool"), or "" when none does.
	Role string
	// Tool marks text inside a tool result (what a tool fed the model).
	Tool bool
	// Type is the "type" of the object the value sits in directly, or "".
	Type string

	set func(string)
}

// Doc is a decoded document and its text.
type Doc struct {
	root    any
	Texts   []*Text
	changed bool
}

// Parse decodes a JSON object or array and lists its text. ok is false for
// anything else (not JSON, or a bare scalar).
func Parse(body []byte) (*Doc, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // numbers are written back exactly as they came
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, false
	}
	if decoder.More() {
		return nil, false
	}
	switch root.(type) {
	case map[string]any, []any:
	default:
		return nil, false
	}
	doc := &Doc{root: root}
	doc.walk(root, nil, "", false)
	return doc, true
}

// Set replaces a text's value in the document.
func (d *Doc) Set(t *Text, value string) {
	if t.Value == value {
		return
	}
	t.Value = value
	t.set(value)
	d.changed = true
}

// Root is the decoded document (a map[string]any or []any).
func (d *Doc) Root() any { return d.root }

// Changed reports whether any text was replaced.
func (d *Doc) Changed() bool { return d.changed }

// Marshal encodes the document. Object keys come out sorted; values other
// than replaced text are unchanged.
func (d *Doc) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(d.root); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// toolResultTypes are the "type" values of objects that hold a tool's
// output, across the provider layouts.
var toolResultTypes = map[string]bool{
	"tool_result": true, "function_call_output": true, "computer_call_output": true,
	"local_shell_call_output": true, "custom_tool_call_output": true, "mcp_call_output": true,
	"web_search_tool_result": true, "web_fetch_tool_result": true, "code_execution_tool_result": true,
	"functionResponse": true,
}

func (d *Doc) walk(node any, path []string, role string, tool bool) {
	switch v := node.(type) {
	case map[string]any:
		if r, ok := v["role"].(string); ok && r != "" {
			role = r
			tool = r == "tool" || r == "function"
		}
		if t, ok := v["type"].(string); ok && toolResultTypes[t] {
			tool = true
		}
		if _, ok := v["functionResponse"]; ok {
			tool = true
		}
		kind, _ := v["type"].(string)
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childRole := role
			if len(path) == 0 && (k == "instructions" || k == "system" || k == "systemInstruction" || k == "system_instruction") {
				childRole = "system"
			}
			if s, ok := v[k].(string); ok {
				if s != "" && !structural(k) && !Binary(s) {
					d.Texts = append(d.Texts, &Text{Path: child(path, k), Value: s, Role: childRole, Tool: tool, Type: kind,
						set: func(value string) { v[k] = value }})
				}
				continue
			}
			d.walk(v[k], child(path, k), childRole, tool)
		}
	case []any:
		for i := range v {
			if s, ok := v[i].(string); ok {
				if s != "" && !Binary(s) {
					d.Texts = append(d.Texts, &Text{Path: child(path, strconv.Itoa(i)), Value: s, Role: role, Tool: tool,
						set: func(value string) { v[i] = value }})
				}
				continue
			}
			d.walk(v[i], child(path, strconv.Itoa(i)), role, tool)
		}
	}
}

func child(path []string, key string) []string {
	out := make([]string, len(path)+1)
	copy(out, path)
	out[len(path)] = key
	return out
}

// structural reports a key whose value is an identifier or an enumeration
// and never text: scanning it finds nothing a person wrote, and rewriting
// it would break the request (a redacted file id names no file).
func structural(key string) bool {
	switch key {
	case "id", "model", "type", "role", "object", "status", "signature", "encrypted_content", "system_fingerprint":
		return true
	}
	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids")
}

// binaryMin is the length from which an unbroken run of base64 characters
// is taken for encoded media, not text.
const binaryMin = 1024

// Binary reports a string that carries encoded media (a data: URL, or a
// long unbroken base64 run such as an inline image or sound), which no text
// check can read.
func Binary(s string) bool {
	if strings.HasPrefix(s, "data:") {
		head := s
		if len(head) > 128 {
			head = head[:128]
		}
		if strings.Contains(head, ";base64,") {
			return true
		}
	}
	if len(s) < binaryMin {
		return false
	}
	for i := 0; i < binaryMin; i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// At returns the path element at i, or "".
func (t *Text) At(i int) string {
	if i < 0 || i >= len(t.Path) {
		return ""
	}
	return t.Path[i]
}

// Index returns the path element at i as an array index, or -1.
func (t *Text) Index(i int) int {
	n, err := strconv.Atoi(t.At(i))
	if err != nil || n < 0 {
		return -1
	}
	return n
}
