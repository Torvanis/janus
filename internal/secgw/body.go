package secgw

import (
	"encoding/json"
	"strings"
)

// parseMessages flattens the OpenAI chat body into per-message text. Only
// text-bearing parts are collected: image_url and audio parts are skipped
// (v1 is text-only). Returns ok=false when the body is not a chat request.
func parseMessages(body []byte) ([]Message, bool) {
	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Messages == nil {
		return nil, false
	}
	out := make([]Message, 0, len(raw.Messages))
	for i, m := range raw.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			// tool_calls arguments are model-authored on the assistant
			// turn and user-forwarded on replay: scan them as text.
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		text, path := flattenContent(msg.Content)
		var extra []string
		for _, tc := range msg.ToolCalls {
			if tc.Function.Arguments != "" {
				extra = append(extra, tc.Function.Arguments)
			}
		}
		if len(extra) > 0 {
			// Tool arguments are appended with a separator that cannot occur
			// in JSON so offsets into the content prefix stay valid.
			out = append(out, Message{Index: i, Role: msg.Role, Text: text, contentPath: path})
			out = append(out, Message{Index: i, Role: msg.Role + ".tool_calls", Text: strings.Join(extra, "\n"), contentPath: "tool_calls"})
			continue
		}
		out = append(out, Message{Index: i, Role: msg.Role, Text: text, contentPath: path})
	}
	return out, true
}

// flattenContent returns the text of a content field and how it was shaped.
// For content arrays, text parts are joined with "\n" — offsets within a
// single part remain valid for that part; rewriteMessages re-splits.
func flattenContent(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", "none"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, "string"
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", "none"
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" || (p.Type == "" && p.Text != "") {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n"), "parts"
}

// rewriteMessages writes redacted texts back into the body. It rewrites
// only messages whose text changed and preserves every other byte of the
// request by operating on the generic JSON tree.
func rewriteMessages(body []byte, msgs []Message) ([]byte, error) {
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, err
	}
	var rawMsgs []json.RawMessage
	if err := json.Unmarshal(tree["messages"], &rawMsgs); err != nil {
		return nil, err
	}
	changed := false
	for _, m := range msgs {
		if m.Index < 0 || m.Index >= len(rawMsgs) {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(rawMsgs[m.Index], &obj); err != nil {
			continue
		}
		switch m.contentPath {
		case "string":
			cur, _ := flattenContent(obj["content"])
			if cur == m.Text {
				continue
			}
			enc, _ := json.Marshal(m.Text)
			obj["content"] = enc
		case "parts":
			var parts []map[string]json.RawMessage
			if err := json.Unmarshal(obj["content"], &parts); err != nil {
				continue
			}
			// Re-split the joined text back onto the text parts in order.
			pieces := strings.Split(m.Text, "\n")
			pi := 0
			origTexts := make([]string, 0, len(parts))
			for _, p := range parts {
				var t string
				if json.Unmarshal(p["type"], &t) == nil && t == "text" {
					var s string
					_ = json.Unmarshal(p["text"], &s)
					origTexts = append(origTexts, s)
				}
			}
			// If the redacted text still splits into the same number of
			// pieces as there were text parts, write back per part; else
			// collapse into the first text part (rare: a redaction spanning
			// a newline).
			if len(pieces) != len(origTexts) {
				pieces = []string{m.Text}
			}
			for i := range parts {
				var t string
				if json.Unmarshal(parts[i]["type"], &t) != nil || t != "text" {
					continue
				}
				if pi < len(pieces) {
					enc, _ := json.Marshal(pieces[pi])
					parts[i]["text"] = enc
				} else {
					enc, _ := json.Marshal("")
					parts[i]["text"] = enc
				}
				pi++
			}
			enc, err := json.Marshal(parts)
			if err != nil {
				return nil, err
			}
			obj["content"] = enc
		case "tool_calls":
			// Redaction inside tool-call arguments is applied by rewriting
			// each argument string in order.
			var tcs []map[string]json.RawMessage
			if err := json.Unmarshal(obj["tool_calls"], &tcs); err != nil {
				continue
			}
			pieces := strings.Split(m.Text, "\n")
			for i := range tcs {
				var fn map[string]json.RawMessage
				if json.Unmarshal(tcs[i]["function"], &fn) != nil {
					continue
				}
				if i < len(pieces) {
					enc, _ := json.Marshal(pieces[i])
					fn["arguments"] = enc
					fenc, _ := json.Marshal(fn)
					tcs[i]["function"] = fenc
				}
			}
			enc, err := json.Marshal(tcs)
			if err != nil {
				return nil, err
			}
			obj["tool_calls"] = enc
		default:
			continue
		}
		enc, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		rawMsgs[m.Index] = enc
		changed = true
	}
	if !changed {
		return body, nil
	}
	enc, err := json.Marshal(rawMsgs)
	if err != nil {
		return nil, err
	}
	tree["messages"] = enc
	return json.Marshal(tree)
}

// responseText is one scannable text channel of a buffered response.
type responseText struct {
	Choice int
	Path   string // "content" | "reasoning" | "tool:<i>"
	Text   string
}

// responseTexts extracts every model-authored text channel from a
// non-streaming chat completion: message.content, reasoning_content, and
// each tool call's arguments. Non-chat bodies return ok=false.
func responseTexts(body []byte) ([]responseText, bool) {
	var raw struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Choices == nil {
		return nil, false
	}
	var out []responseText
	for i, ch := range raw.Choices {
		if text, path := flattenContent(ch.Message.Content); path != "none" && text != "" {
			out = append(out, responseText{Choice: i, Path: "content", Text: text})
		}
		if ch.Message.ReasoningContent != "" {
			out = append(out, responseText{Choice: i, Path: "reasoning", Text: ch.Message.ReasoningContent})
		}
		for j, tc := range ch.Message.ToolCalls {
			if tc.Function.Arguments != "" {
				out = append(out, responseText{Choice: i, Path: "tool:" + itoa(j), Text: tc.Function.Arguments})
			}
		}
	}
	return out, true
}

// rewriteResponseTexts writes redacted channel texts back into the body.
func rewriteResponseTexts(body []byte, texts []responseText) ([]byte, error) {
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, err
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(tree["choices"], &choices); err != nil {
		return nil, err
	}
	for _, t := range texts {
		if t.Choice < 0 || t.Choice >= len(choices) {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(choices[t.Choice]["message"], &msg); err != nil {
			continue
		}
		switch {
		case t.Path == "content":
			enc, _ := json.Marshal(t.Text)
			msg["content"] = enc
		case t.Path == "reasoning":
			enc, _ := json.Marshal(t.Text)
			msg["reasoning_content"] = enc
		case strings.HasPrefix(t.Path, "tool:"):
			idx := atoi(strings.TrimPrefix(t.Path, "tool:"))
			var tcs []map[string]json.RawMessage
			if json.Unmarshal(msg["tool_calls"], &tcs) != nil || idx < 0 || idx >= len(tcs) {
				continue
			}
			var fn map[string]json.RawMessage
			if json.Unmarshal(tcs[idx]["function"], &fn) != nil {
				continue
			}
			enc, _ := json.Marshal(t.Text)
			fn["arguments"] = enc
			fenc, _ := json.Marshal(fn)
			tcs[idx]["function"] = fenc
			tenc, _ := json.Marshal(tcs)
			msg["tool_calls"] = tenc
		}
		menc, err := json.Marshal(msg)
		if err != nil {
			return nil, err
		}
		choices[t.Choice]["message"] = menc
	}
	enc, err := json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	tree["choices"] = enc
	return json.Marshal(tree)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
