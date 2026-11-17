package secgw

import (
	"encoding/json"
	"strings"

	"github.com/torvanis/janus/internal/secgw/jsontext"
)

// A request or reply is JSON whose layout depends on the endpoint. The chat
// layout is read message by message (parseMessages, responseTexts), which is
// what gives a violation its message and its turn. Everything else in a
// chat body, and every string of any other JSON body, is read with
// jsontext: a Responses input, a prompt, an embeddings input, an Anthropic
// system field, a tool definition. So no text reaches a provider or a
// caller unread because of where in the document it sits, and a redaction
// is written to the value it was found in.

// conversationKeys are the top-level arrays whose elements are the turns of
// a conversation, in order.
var conversationKeys = map[string]bool{"messages": true, "input": true, "contents": true}

// requestCovered reports a string of a chat request that parseMessages
// already reads.
func requestCovered(t *jsontext.Text) bool {
	if t.At(0) != "messages" || t.Index(1) < 0 {
		return false
	}
	switch len(t.Path) {
	case 3:
		return t.At(2) == "content"
	case 5:
		return t.At(2) == "content" && t.At(4) == "text" && (t.Type == "text" || t.Type == "")
	case 6:
		return t.At(2) == "tool_calls" && t.At(4) == "function" && t.At(5) == "arguments"
	}
	return false
}

// responseCovered reports a string of a chat completion that responseTexts
// already reads.
func responseCovered(t *jsontext.Text) bool {
	if t.At(0) != "choices" || t.Index(1) < 0 || t.At(2) != "message" {
		return false
	}
	switch len(t.Path) {
	case 4:
		return t.At(3) == "content" || t.At(3) == "reasoning_content"
	case 6:
		return t.At(3) == "content" && t.At(5) == "text" && (t.Type == "text" || t.Type == "")
	case 7:
		return t.At(3) == "tool_calls" && t.At(5) == "function" && t.At(6) == "arguments"
	}
	return false
}

// requestBody is a request read for inspection.
type requestBody struct {
	// msgs is every text: the chat messages first (when the body has
	// them), then the rest of the document.
	msgs []Message
	// chat is how many of msgs are chat messages; isChat says the body has
	// the chat layout at all.
	chat   int
	isChat bool
	// doc is the decoded document, nil when the body is not JSON.
	doc *jsontext.Doc
}

// readRequest reads a request body. A body that is not JSON (a multipart
// upload, binary) is one text, as sent; nothing in it can be rewritten.
func readRequest(body []byte) requestBody {
	chat, isChat := parseMessages(body)
	doc, isJSON := jsontext.Parse(body)
	if !isJSON {
		if isChat {
			return requestBody{msgs: chat, chat: len(chat), isChat: true}
		}
		return requestBody{msgs: []Message{{Index: 0, Role: "body", Text: string(body)}}}
	}
	out := requestBody{msgs: chat, chat: len(chat), isChat: isChat, doc: doc}
	// One past the last turn: text outside the conversation (instructions,
	// a prompt, tool definitions) is sent with every request and is always
	// part of what this request adds.
	turns := 0
	for _, t := range doc.Texts {
		if conversationKeys[t.At(0)] && t.Index(1) >= turns {
			turns = t.Index(1) + 1
		}
	}
	for _, m := range chat {
		if m.Index >= turns {
			turns = m.Index + 1
		}
	}
	for _, t := range doc.Texts {
		if isChat && requestCovered(t) {
			continue
		}
		m := Message{Index: turns, Role: docRole(t), Text: t.Value, Tool: t.Tool, ref: t, outside: true}
		if conversationKeys[t.At(0)] && t.Index(1) >= 0 {
			m.Index, m.outside = t.Index(1), false
		}
		out.msgs = append(out.msgs, m)
	}
	return out
}

// docRole is whose words a string of a document is: the role of the object
// it sits in, the assistant for what a model wrote earlier, otherwise the
// caller.
func docRole(t *jsontext.Text) string {
	switch t.Role {
	case "model":
		return "assistant"
	case "":
		switch t.Type {
		case "function_call", "output_text", "refusal", "reasoning", "summary_text", "thinking", "tool_use":
			return "assistant"
		}
		return "user"
	}
	return t.Role
}

// judged is the part of a request the model-backed checks read: the
// conversation and the system text. Tool definitions, metadata and the
// like are not anyone's message; the deterministic checks still read them.
func (b requestBody) judged() []Message {
	if b.doc == nil {
		return b.msgs
	}
	out := make([]Message, 0, len(b.msgs))
	for i, m := range b.msgs {
		if i < b.chat || !m.outside || !b.isChat || m.Role == "system" {
			out = append(out, m)
		}
	}
	return out
}

// rewrite returns the body with every changed text written back.
func (b requestBody) rewrite(body []byte) ([]byte, error) {
	if b.doc != nil {
		for i := b.chat; i < len(b.msgs); i++ {
			if b.msgs[i].ref != nil {
				b.doc.Set(b.msgs[i].ref, b.msgs[i].Text)
			}
		}
		if b.doc.Changed() {
			out, err := b.doc.Marshal()
			if err != nil {
				return nil, err
			}
			body = out
		}
	}
	if b.isChat {
		return rewriteMessages(body, b.msgs[:b.chat])
	}
	return body, nil
}

// canRewrite reports whether a redaction can be applied to this body.
func (b requestBody) canRewrite() bool { return b.doc != nil || b.isChat }

// replyText is the answer a buffered reply shows the caller, whatever API
// it came from: the first chat choice, a Responses output, a completion, an
// Anthropic message.
func replyText(body []byte) string {
	if texts, ok := responseTexts(body); ok {
		for _, t := range texts {
			if t.Path == "content" && strings.TrimSpace(t.Text) != "" {
				return t.Text
			}
		}
	}
	var raw struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	for _, c := range raw.Choices {
		if strings.TrimSpace(c.Text) != "" {
			return c.Text
		}
	}
	var parts []string
	for _, item := range raw.Output {
		for _, c := range item.Content {
			if c.Type == "output_text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	if len(parts) == 0 && raw.Role == "assistant" {
		if text, shape := flattenContent(raw.Content); shape != "none" {
			return text
		}
	}
	return strings.Join(parts, "\n")
}
