package classify

import (
	"encoding/json"
	"strings"
)

// Text scopes (mirrors store.ClassificationScope*; duplicated so this
// package has no store dependency).
const (
	ScopeLastUser = "last_user"
	ScopeAllUser  = "all_user"
	ScopeAll      = "conversation"
)

// ExtractText pulls the classifiable text out of an OpenAI-shaped request
// body: chat completions (messages), the Responses API (input, instructions)
// and legacy completions (prompt). Images, audio and files are skipped.
// It returns "" when the body carries no text.
func ExtractText(body []byte, scope string, includeSystem bool) string {
	var raw struct {
		Messages     []json.RawMessage `json:"messages"`
		Input        json.RawMessage   `json:"input"`
		Instructions string            `json:"instructions"`
		System       json.RawMessage   `json:"system"`
		Prompt       json.RawMessage   `json:"prompt"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	type turn struct{ role, text string }
	var turns []turn
	addMsg := func(m json.RawMessage) {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			return
		}
		if t := partsText(msg.Content); t != "" {
			turns = append(turns, turn{msg.Role, t})
		}
	}
	for _, m := range raw.Messages {
		addMsg(m)
	}
	if len(raw.Input) > 0 {
		var s string
		if json.Unmarshal(raw.Input, &s) == nil {
			turns = append(turns, turn{"user", s})
		} else {
			var items []json.RawMessage
			if json.Unmarshal(raw.Input, &items) == nil {
				for _, it := range items {
					addMsg(it)
				}
			}
		}
	}
	if len(raw.Prompt) > 0 {
		var s string
		if json.Unmarshal(raw.Prompt, &s) == nil {
			turns = append(turns, turn{"user", s})
		}
	}
	system := strings.TrimSpace(raw.Instructions)
	if t := partsText(raw.System); t != "" { // Anthropic-shaped bodies
		system = strings.TrimSpace(system + "\n" + t)
	}

	var out []string
	if includeSystem {
		for _, t := range turns {
			if t.role == "system" || t.role == "developer" {
				system = strings.TrimSpace(system + "\n" + t.text)
			}
		}
		if system != "" {
			out = append(out, "System instructions:\n"+system)
		}
	}
	switch scope {
	case ScopeAll:
		for _, t := range turns {
			if t.role == "system" || t.role == "developer" {
				continue
			}
			out = append(out, roleLabel(t.role)+": "+t.text)
		}
	case ScopeAllUser:
		for _, t := range turns {
			if t.role == "user" {
				out = append(out, t.text)
			}
		}
	default: // last user turn
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].role == "user" {
				out = append(out, turns[i].text)
				break
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n\n"))
}

func roleLabel(role string) string {
	switch role {
	case "assistant":
		return "Assistant"
	case "tool", "function":
		return "Tool"
	default:
		return "User"
	}
}

// partsText flattens a content field: a string, or an array of parts of
// which only text parts are kept (text, input_text, output_text).
func partsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text", "":
			if t := strings.TrimSpace(p.Text); t != "" {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, "\n")
}
