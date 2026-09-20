package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func init() { Register(&Anthropic{}) }

// AnthropicVersion is the API version header value sent on every call.
const AnthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is used when a chat request names no output cap:
// the Messages API requires max_tokens, so the translation must never fail a
// request the caller considered valid.
const anthropicDefaultMaxTokens = 4096

// Anthropic speaks the native Messages API. Chat completions are translated
// both ways so downstream OpenAI SDKs work unchanged: Prepare rewrites the
// request (chatToMessages), TransformResponse rewrites a buffered Messages
// response into chat.completion, and the stream transformer rewrites the
// typed Messages SSE events into chat.completion.chunk frames.
type Anthropic struct {
	// now is the clock for the `created` field on translated responses;
	// tests pin it. Nil means time.Now.
	now func() time.Time
}

// Type returns the stored adapter enum value.
func (a *Anthropic) Type() string { return "anthropic" }

func (a *Anthropic) created() int64 {
	if a.now != nil {
		return a.now().Unix()
	}
	return time.Now().Unix()
}

// Discover lists models from the Anthropic models endpoint.
func (a *Anthropic) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(up.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	a.applyAuth(req.Header, up)
	body, err := doDiscovery(client, req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			// Anthropic has started returning a capabilities object on
			// each model. The field names are not yet stable across API
			// versions, so both spellings seen in the wild are accepted;
			// zero means the provider said nothing.
			MaxInputTokens int64 `json:"max_input_tokens"`
			Capabilities   struct {
				ContextWindow  int64 `json:"context_window"`
				MaxInputTokens int64 `json:"max_input_tokens"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse Anthropic model list: %w", err)
	}
	out := make([]DiscoveredModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		ctxWindow := m.Capabilities.ContextWindow
		if ctxWindow == 0 {
			ctxWindow = m.Capabilities.MaxInputTokens
		}
		if ctxWindow == 0 {
			ctxWindow = m.MaxInputTokens
		}
		// Every model Anthropic lists is a Messages (chat) model today, but
		// the id is still consulted so a future embedding or video entry
		// is not mislabelled the way video models once were on OpenAI.
		out = append(out, DiscoveredModel{Name: m.ID, Modalities: guessModalities(m.ID), ContextWindow: ctxWindow})
	}
	return out, nil
}

func (a *Anthropic) applyAuth(h http.Header, up Upstream) {
	if up.APIKey != "" {
		h.Set("x-api-key", up.APIKey)
	}
	h.Set("anthropic-version", AnthropicVersion)
	h.Del("Authorization")
}

// isChatCompletions reports whether the request is the one path this adapter
// translates. Every other path (native /v1/messages callers, /v1/models) is
// relayed byte-for-byte in both directions.
func isChatCompletions(req *Request) bool {
	return req != nil && strings.HasPrefix(req.Path, "/v1/chat/completions")
}

// Prepare maps OpenAI chat completions onto the Messages API. Other paths are
// forwarded unchanged so native Anthropic clients keep working.
func (a *Anthropic) Prepare(up Upstream, req *Request) (*Prepared, error) {
	header := cloneHeader(req.Header)
	a.applyAuth(header, up)

	if !isChatCompletions(req) {
		return &Prepared{URL: joinURL(up.BaseURL, req.Path), Header: header, Body: req.Body}, nil
	}
	body, err := chatToMessages(req.Body)
	if err != nil {
		return nil, err
	}
	return &Prepared{URL: joinURL(up.BaseURL, "/v1/messages"), Header: header, Body: body}, nil
}

// --- request translation -----------------------------------------------------

// chatCompletionRequest is the subset of the OpenAI chat request the
// translation acts on. Everything else the caller sent is examined through
// the raw map so unknown fields are handled deliberately (see chatToMessages).
type chatCompletionRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Stream              bool              `json:"stream"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	TopK                *int              `json:"top_k"`
	Stop                json.RawMessage   `json:"stop"`
	Tools               []json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
	User                string            `json:"user"`
	N                   *int              `json:"n"`
	Thinking            json.RawMessage   `json:"thinking"`
}

// chatMessage is one OpenAI chat message. Content is raw because it is either
// a string or an array of typed parts.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// chatToMessages converts an OpenAI chat request into the Anthropic Messages
// shape. The contract is translate-or-fail-loudly: every field the caller
// sent is either mapped onto its Messages equivalent, deliberately dropped
// because it is a hint with no Anthropic counterpart and no effect on the
// answer's meaning (seed, presence/frequency penalties, logprobs,
// stream_options — usage is always reported), or rejected with an error
// naming the field when honouring it silently is impossible (n > 1, audio
// input). Nothing is discarded by accident.
func chatToMessages(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var in chatCompletionRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat completion request: %w", err)
	}
	if in.N != nil && *in.N > 1 {
		return nil, fmt.Errorf("anthropic: n=%d is not supported by the Messages API; request one completion at a time", *in.N)
	}

	out := map[string]any{"model": in.Model, "stream": in.Stream}

	// Messages. System/developer prompts lift to the top-level field; the
	// remaining turns are translated block by block, and consecutive turns
	// with the same role are merged because the Messages API requires a
	// tool_result to sit in the single user turn that follows the tool_use.
	var systemParts []any
	systemAllText := true
	messages := []map[string]any{}
	for i, raw := range in.Messages {
		var m chatMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("parse chat message %d: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			blocks, err := systemBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			for _, b := range blocks {
				if _, isString := b.(string); !isString {
					systemAllText = false
				}
				systemParts = append(systemParts, b)
			}
			continue
		case "tool", "function":
			block, err := toolResultBlock(m)
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			appendTurn(&messages, "user", []any{block})
		case "assistant":
			blocks, err := assistantBlocks(m)
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			if len(blocks) == 0 {
				continue // an empty assistant turn carries nothing Anthropic accepts
			}
			appendTurn(&messages, "assistant", blocks)
		case "user":
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", i, err)
			}
			appendTurn(&messages, "user", blocks)
		default:
			return nil, fmt.Errorf("message %d: role %q has no Messages API equivalent", i, m.Role)
		}
	}
	trimTrailingAssistantWhitespace(messages)
	if len(systemParts) > 0 {
		if systemAllText {
			texts := make([]string, 0, len(systemParts))
			for _, p := range systemParts {
				texts = append(texts, p.(string))
			}
			out["system"] = strings.Join(texts, "\n\n")
		} else {
			blocks := make([]any, 0, len(systemParts))
			for _, p := range systemParts {
				if s, ok := p.(string); ok {
					blocks = append(blocks, map[string]any{"type": "text", "text": s})
				} else {
					blocks = append(blocks, p)
				}
			}
			out["system"] = blocks
		}
	}
	out["messages"] = messages

	// Output cap: max_completion_tokens is the current OpenAI spelling and
	// supersedes the deprecated max_tokens when both are present.
	switch {
	case in.MaxCompletionTokens != nil:
		out["max_tokens"] = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		out["max_tokens"] = *in.MaxTokens
	default:
		out["max_tokens"] = anthropicDefaultMaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		out["top_k"] = *in.TopK
	}
	if stops := stopSequences(in.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}

	// Tools: OpenAI wraps the schema under function.parameters; Anthropic
	// takes it flat as input_schema.
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for i, raw := range in.Tools {
			tool, err := anthropicTool(raw)
			if err != nil {
				return nil, fmt.Errorf("tools[%d]: %w", i, err)
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
		if choice, err := anthropicToolChoice(in.ToolChoice, in.ParallelToolCalls); err != nil {
			return nil, err
		} else if choice != nil {
			out["tool_choice"] = choice
		}
	}

	// response_format: the Messages API has no JSON mode, so the constraint
	// is expressed the way Anthropic documents it — as an instruction in
	// the system prompt (with the schema inlined for json_schema). The
	// request still succeeds and the model is told exactly what to emit.
	if instruction, err := responseFormatInstruction(in.ResponseFormat); err != nil {
		return nil, err
	} else if instruction != "" {
		switch sys := out["system"].(type) {
		case string:
			out["system"] = sys + "\n\n" + instruction
		case []any:
			out["system"] = append(sys, map[string]any{"type": "text", "text": instruction})
		default:
			out["system"] = instruction
		}
	}

	// user → metadata.user_id, the Messages API's abuse-attribution field.
	// (OpenAI's free-form `metadata` map is deliberately NOT forwarded:
	// Anthropic's metadata object accepts user_id only and rejects anything
	// else, and OpenAI metadata is bookkeeping for stored completions, which
	// the Messages API has no notion of.)
	if in.User != "" {
		out["metadata"] = map[string]any{"user_id": in.User}
	}
	// Anthropic-native extended thinking passes straight through for callers
	// that already speak it.
	if len(in.Thinking) > 0 {
		out["thinking"] = in.Thinking
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode Messages request: %w", err)
	}
	return encoded, nil
}

// appendTurn adds content blocks to the conversation, merging into the
// previous turn when the role repeats.
func appendTurn(messages *[]map[string]any, role string, blocks []any) {
	if n := len(*messages); n > 0 && (*messages)[n-1]["role"] == role {
		existing, _ := (*messages)[n-1]["content"].([]any)
		(*messages)[n-1]["content"] = append(existing, blocks...)
		return
	}
	*messages = append(*messages, map[string]any{"role": role, "content": blocks})
}

// trimTrailingAssistantWhitespace keeps a prefilled final assistant turn
// acceptable: the Messages API rejects trailing whitespace there.
func trimTrailingAssistantWhitespace(messages []map[string]any) {
	n := len(messages)
	if n == 0 || messages[n-1]["role"] != "assistant" {
		return
	}
	blocks, _ := messages[n-1]["content"].([]any)
	if len(blocks) == 0 {
		return
	}
	last, ok := blocks[len(blocks)-1].(map[string]any)
	if !ok || last["type"] != "text" {
		return
	}
	if text, ok := last["text"].(string); ok {
		last["text"] = strings.TrimRight(text, " \t\r\n")
	}
}

// systemBlocks returns a system message's content as strings (plain text)
// and/or Anthropic text blocks (when the caller sent typed parts, whose
// cache_control annotations must survive).
func systemBlocks(content json.RawMessage) ([]any, error) {
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return []any{text}, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("system content must be a string or an array of content parts")
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p["type"] != "text" {
			return nil, fmt.Errorf("system content part type %v is not supported; only text parts may appear in a system prompt", p["type"])
		}
		block := map[string]any{"type": "text", "text": p["text"]}
		if cc, ok := p["cache_control"]; ok {
			block["cache_control"] = cc
		}
		out = append(out, block)
	}
	return out, nil
}

// userBlocks translates user content parts: text stays text, image_url
// becomes an image block (base64 data URLs and https URLs alike), and file
// parts carrying a PDF data URL become document blocks.
func userBlocks(content json.RawMessage) ([]any, error) {
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return []any{map[string]any{"type": "text", "text": ""}}, nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("user content must be a string or an array of content parts")
	}
	out := make([]any, 0, len(parts))
	for i, p := range parts {
		switch p["type"] {
		case "text":
			block := map[string]any{"type": "text", "text": p["text"]}
			if cc, ok := p["cache_control"]; ok {
				block["cache_control"] = cc
			}
			out = append(out, block)
		case "image_url":
			url := ""
			switch v := p["image_url"].(type) {
			case string:
				url = v
			case map[string]any:
				url, _ = v["url"].(string)
			}
			block, err := imageBlock(url)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			out = append(out, block)
		case "file":
			file, _ := p["file"].(map[string]any)
			data, _ := file["file_data"].(string)
			block, err := documentBlock(data)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			out = append(out, block)
		case "input_audio":
			return nil, fmt.Errorf("content[%d]: audio input is not supported by the Anthropic Messages API", i)
		// Anthropic-native block types pass through for callers that mix them in.
		case "image", "document", "tool_result", "tool_use":
			out = append(out, p)
		default:
			return nil, fmt.Errorf("content[%d]: part type %v has no Messages API equivalent", i, p["type"])
		}
	}
	return out, nil
}

// imageBlock builds an Anthropic image block from an OpenAI image_url value.
func imageBlock(url string) (map[string]any, error) {
	if url == "" {
		return nil, fmt.Errorf("image_url part has no url")
	}
	if strings.HasPrefix(url, "data:") {
		mediaType, data, ok := parseDataURL(url)
		if !ok {
			return nil, fmt.Errorf("image_url data URL must be base64-encoded as data:<media-type>;base64,<data>")
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mediaType, "data": data,
		}}, nil
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}, nil
}

// documentBlock builds an Anthropic document block from an OpenAI file part's
// data URL.
func documentBlock(fileData string) (map[string]any, error) {
	mediaType, data, ok := parseDataURL(fileData)
	if !ok {
		return nil, fmt.Errorf("file part must carry file_data as a base64 data URL; file ids cannot be forwarded to Anthropic")
	}
	return map[string]any{"type": "document", "source": map[string]any{
		"type": "base64", "media_type": mediaType, "data": data,
	}}, nil
}

// parseDataURL splits data:<media-type>;base64,<data>.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return "", "", false
	}
	meta, data := url[len("data:"):comma], url[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if mediaType == "" || data == "" {
		return "", "", false
	}
	return mediaType, data, true
}

// assistantBlocks translates an assistant turn: text content (string or
// parts) followed by one tool_use block per tool_call, with the JSON-encoded
// arguments string decoded into the object Anthropic expects.
func assistantBlocks(m chatMessage) ([]any, error) {
	blocks := []any{}
	if len(m.Content) > 0 && !bytes.Equal(m.Content, []byte("null")) {
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		} else {
			var parts []map[string]any
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				return nil, fmt.Errorf("assistant content must be a string or an array of content parts")
			}
			for _, p := range parts {
				switch p["type"] {
				case "text":
					if t, _ := p["text"].(string); t != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": t})
					}
				case "refusal":
					// OpenAI-only annotation; nothing to replay.
				case "tool_use", "thinking", "redacted_thinking":
					blocks = append(blocks, p)
				default:
					return nil, fmt.Errorf("assistant content part type %v has no Messages API equivalent", p["type"])
				}
			}
		}
	}
	for i, call := range m.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			return nil, fmt.Errorf("tool_calls[%d]: type %q is not supported", i, call.Type)
		}
		input := map[string]any{}
		if args := strings.TrimSpace(call.Function.Arguments); args != "" {
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				return nil, fmt.Errorf("tool_calls[%d]: arguments must be a JSON object: %w", i, err)
			}
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input,
		})
	}
	return blocks, nil
}

// toolResultBlock translates a role:tool message into the tool_result block
// Anthropic expects inside the next user turn.
func toolResultBlock(m chatMessage) (map[string]any, error) {
	if m.ToolCallID == "" {
		return nil, fmt.Errorf("tool message has no tool_call_id to answer")
	}
	block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID}
	if len(m.Content) == 0 || bytes.Equal(m.Content, []byte("null")) {
		return block, nil
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		block["content"] = text
		return block, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, fmt.Errorf("tool content must be a string or an array of content parts")
	}
	content := make([]any, 0, len(parts))
	for i, p := range parts {
		switch p["type"] {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": p["text"]})
		case "image_url":
			url := ""
			switch v := p["image_url"].(type) {
			case string:
				url = v
			case map[string]any:
				url, _ = v["url"].(string)
			}
			img, err := imageBlock(url)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			content = append(content, img)
		case "image":
			content = append(content, p)
		default:
			return nil, fmt.Errorf("content[%d]: tool result part type %v has no Messages API equivalent", i, p["type"])
		}
	}
	block["content"] = content
	return block, nil
}

// stopSequences normalises OpenAI's string-or-array stop field.
func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// anthropicTool converts one OpenAI function tool definition.
func anthropicTool(raw json.RawMessage) (map[string]any, error) {
	var tool struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		} `json:"function"`
		// Anthropic-native tool definitions (name/input_schema at top
		// level, or server tools like web_search) pass through.
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, fmt.Errorf("parse tool: %w", err)
	}
	if tool.Type != "function" && tool.Type != "" {
		if tool.Name == "" {
			return nil, fmt.Errorf("tool type %q is not supported", tool.Type)
		}
		var native map[string]any
		_ = json.Unmarshal(raw, &native)
		return native, nil
	}
	if tool.Type == "" && len(tool.InputSchema) > 0 && tool.Name != "" {
		var native map[string]any
		_ = json.Unmarshal(raw, &native)
		return native, nil
	}
	if tool.Function.Name == "" {
		return nil, fmt.Errorf("function tool has no name")
	}
	out := map[string]any{"name": tool.Function.Name}
	if tool.Function.Description != "" {
		out["description"] = tool.Function.Description
	}
	if len(tool.Function.Parameters) > 0 && !bytes.Equal(tool.Function.Parameters, []byte("null")) {
		out["input_schema"] = tool.Function.Parameters
	} else {
		out["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return out, nil
}

// anthropicToolChoice maps OpenAI's tool_choice (and parallel_tool_calls)
// onto the Messages tool_choice object.
func anthropicToolChoice(raw json.RawMessage, parallel *bool) (map[string]any, error) {
	var choice map[string]any
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var mode string
		if err := json.Unmarshal(raw, &mode); err == nil {
			switch mode {
			case "auto":
				choice = map[string]any{"type": "auto"}
			case "none":
				choice = map[string]any{"type": "none"}
			case "required":
				choice = map[string]any{"type": "any"}
			default:
				return nil, fmt.Errorf("tool_choice %q is not supported", mode)
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, fmt.Errorf("parse tool_choice: %w", err)
			}
			switch obj.Type {
			case "function":
				if obj.Function.Name == "" {
					return nil, fmt.Errorf("tool_choice.function.name is required")
				}
				choice = map[string]any{"type": "tool", "name": obj.Function.Name}
			case "auto", "any", "none":
				choice = map[string]any{"type": obj.Type}
			case "tool":
				choice = map[string]any{"type": "tool", "name": obj.Name}
			default:
				return nil, fmt.Errorf("tool_choice type %q is not supported", obj.Type)
			}
		}
	}
	if parallel != nil && !*parallel {
		if choice == nil {
			choice = map[string]any{"type": "auto"}
		}
		if choice["type"] != "none" {
			choice["disable_parallel_tool_use"] = true
		}
	}
	return choice, nil
}

// responseFormatInstruction renders OpenAI's response_format as the system
// instruction Anthropic documents for JSON output. text (the default) yields
// nothing.
func responseFormatInstruction(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return "", fmt.Errorf("parse response_format: %w", err)
	}
	switch format.Type {
	case "", "text":
		return "", nil
	case "json_object":
		return "Respond only with a single valid JSON object. Do not include any prose, explanation, or Markdown code fences before or after the JSON.", nil
	case "json_schema":
		if len(format.JSONSchema.Schema) == 0 {
			return "", fmt.Errorf("response_format.json_schema.schema is required")
		}
		return "Respond only with a single valid JSON object that conforms exactly to the following JSON Schema. Do not include any prose, explanation, or Markdown code fences before or after the JSON.\n\n" +
			string(format.JSONSchema.Schema), nil
	default:
		return "", fmt.Errorf("response_format type %q is not supported", format.Type)
	}
}

// --- response translation ----------------------------------------------------

// anthropicContentBlock is one block of a Messages response.
type anthropicContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Thinking string          `json:"thinking"`
}

// anthropicMessage is a non-streaming Messages response.
type anthropicMessage struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsageBlock     `json:"usage"`
}

// openAIFinishReason maps Anthropic stop reasons onto OpenAI finish reasons.
func openAIFinishReason(stop string) string {
	switch stop {
	case "":
		return ""
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

// openAIUsage renders an Anthropic usage block in the OpenAI shape. Cache
// reads land in prompt_tokens_details.cached_tokens, the field OpenAI SDKs
// already know how to read.
func openAIUsage(u anthropicUsageBlock) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadInputTokens}
	}
	return out
}

// TransformResponse wraps a Messages response as an OpenAI chat.completion:
// text blocks concatenate into choices[0].message.content, tool_use blocks
// become tool_calls with JSON-encoded arguments, thinking blocks surface as
// reasoning_content, and stop_reason maps to finish_reason. Error bodies
// ({"type":"error",…}) already nest error.message where OpenAI clients look
// for it and are relayed unchanged, as is anything on a non-translated path.
func (a *Anthropic) TransformResponse(req *Request, body []byte) ([]byte, error) {
	if !isChatCompletions(req) || len(body) == 0 {
		return body, nil
	}
	var msg anthropicMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.Type != "message" {
		return body, nil
	}
	message := map[string]any{"role": "assistant"}
	var text, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id": block.ID, "type": "function",
				"function": map[string]any{"name": block.Name, "arguments": toolArguments(block.Input)},
			})
		}
	}
	if text.Len() > 0 || len(toolCalls) == 0 {
		message["content"] = text.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finish := openAIFinishReason(msg.StopReason)
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{
		"id":      msg.ID,
		"object":  "chat.completion",
		"created": a.created(),
		"model":   msg.Model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   openAIUsage(msg.Usage),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode chat.completion response: %w", err)
	}
	return encoded, nil
}

// toolArguments renders a tool_use input object as the JSON string OpenAI
// clients expect in function.arguments.
func toolArguments(input json.RawMessage) string {
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		return "{}"
	}
	return string(input)
}

// NewStreamTransformer returns the per-stream Messages→chat.completion.chunk
// rewriter for translated chat calls, and a pass-through otherwise.
func (a *Anthropic) NewStreamTransformer(req *Request, _ http.Header) StreamTransformer {
	if !isChatCompletions(req) {
		return PassthroughStream{}
	}
	return &anthropicStreamTransformer{adapter: a, model: reqModel(req), toolIndex: map[int]int{}}
}

func reqModel(req *Request) string {
	if req == nil {
		return ""
	}
	return req.Model
}

// anthropicStreamTransformer rewrites the typed Messages SSE events.
//
//	message_start        → chunk with delta.role (carries id/model for the stream)
//	content_block_start  → tool_use: chunk opening tool_calls[k] (id, name); text: nothing
//	content_block_delta  → text_delta: delta.content; input_json_delta:
//	                       tool_calls[k].function.arguments; thinking_delta:
//	                       delta.reasoning_content
//	message_delta        → chunk with finish_reason (stop_reason mapped)
//	message_stop         → usage-only chunk (stream_options.include_usage shape) + [DONE]
//	ping, content_block_stop, signature_delta → swallowed
//	error                → data: {"error": …}
//
// `event:` lines and the blank separators are swallowed; every emitted frame
// is a complete `data: …\n\n`. Lines that are not SSE at all (an upstream
// JSON error body answering a stream:true request) pass through unchanged.
type anthropicStreamTransformer struct {
	adapter *Anthropic
	id      string
	model   string
	created int64
	// toolIndex maps Anthropic content-block indexes to sequential OpenAI
	// tool_call indexes: a text block at 0 and a tool block at 1 must
	// surface as tool_calls[0], not tool_calls[1].
	toolIndex    map[int]int
	inputTokens  int64
	cachedTokens int64
	outputTokens int64
	sawSSE       bool
	done         bool
}

type anthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	Message      *anthropicMessage      `json:"message"`
	ContentBlock *anthropicContentBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsageBlock `json:"usage"`
	Error json.RawMessage      `json:"error"`
}

func (t *anthropicStreamTransformer) TransformStreamLine(line []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(line)
	switch {
	case len(trimmed) == 0:
		if t.sawSSE {
			return nil, nil // frame separators are re-emitted with each translated frame
		}
		return line, nil
	case bytes.HasPrefix(trimmed, []byte("event:")), bytes.HasPrefix(trimmed, []byte(":")):
		t.sawSSE = true
		return nil, nil
	case !bytes.HasPrefix(trimmed, []byte("data:")):
		return line, nil // not SSE (e.g. a JSON error body): relay as-is
	}
	t.sawSSE = true
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		return nil, nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return t.finish(), nil
	}
	var ev anthropicStreamEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("parse Anthropic stream event: %w", err)
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			t.id = ev.Message.ID
			if ev.Message.Model != "" {
				t.model = ev.Message.Model
			}
			t.inputTokens = ev.Message.Usage.InputTokens
			t.cachedTokens = ev.Message.Usage.CacheReadInputTokens
			t.outputTokens = ev.Message.Usage.OutputTokens
		}
		t.created = t.adapter.created()
		return t.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			k := len(t.toolIndex)
			t.toolIndex[ev.Index] = k
			return t.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": k, "id": ev.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
			}}}, nil, nil)
		}
		return nil, nil
	case "content_block_delta":
		if ev.Delta == nil {
			return nil, nil
		}
		switch {
		case ev.Delta.Type == "input_json_delta":
			k, ok := t.toolIndex[ev.Index]
			if !ok {
				k = len(t.toolIndex)
				t.toolIndex[ev.Index] = k
			}
			return t.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": k, "function": map[string]any{"arguments": ev.Delta.PartialJSON},
			}}}, nil, nil)
		case ev.Delta.Type == "thinking_delta":
			return t.chunk(map[string]any{"reasoning_content": ev.Delta.Thinking}, nil, nil)
		case ev.Delta.Type == "signature_delta":
			return nil, nil
		case ev.Delta.Type == "text_delta", ev.Delta.Type == "":
			return t.chunk(map[string]any{"content": ev.Delta.Text}, nil, nil)
		default:
			return nil, nil
		}
	case "message_delta":
		if ev.Usage != nil {
			t.outputTokens = max64(t.outputTokens, ev.Usage.OutputTokens)
			if ev.Usage.InputTokens > 0 {
				t.inputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				t.cachedTokens = ev.Usage.CacheReadInputTokens
			}
		}
		finish := "stop"
		if ev.Delta != nil {
			if mapped := openAIFinishReason(ev.Delta.StopReason); mapped != "" {
				finish = mapped
			}
		}
		return t.chunk(map[string]any{}, &finish, nil)
	case "message_stop":
		return t.finish(), nil
	case "error":
		body := ev.Error
		if len(body) == 0 {
			body = payload
		}
		return append(append([]byte(`data: {"error":`), body...), '}', '\n', '\n'), nil
	default: // ping, content_block_stop, unknown future events
		return nil, nil
	}
}

// finish emits the trailing usage-only chunk and the [DONE] sentinel exactly
// once.
func (t *anthropicStreamTransformer) finish() []byte {
	if t.done {
		return nil
	}
	t.done = true
	usage := openAIUsage(anthropicUsageBlock{
		InputTokens: t.inputTokens, OutputTokens: t.outputTokens, CacheReadInputTokens: t.cachedTokens,
	})
	frame, _ := t.chunk(nil, nil, usage)
	return append(frame, []byte("data: [DONE]\n\n")...)
}

// chunk renders one chat.completion.chunk frame. A nil delta yields the
// empty-choices usage frame OpenAI sends when stream_options.include_usage
// is on.
func (t *anthropicStreamTransformer) chunk(delta map[string]any, finish *string, usage map[string]any) ([]byte, error) {
	if t.created == 0 {
		t.created = t.adapter.created()
	}
	out := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
	}
	if delta == nil {
		out["choices"] = []any{}
	} else {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		if finish != nil {
			choice["finish_reason"] = *finish
		}
		out["choices"] = []map[string]any{choice}
	}
	if usage != nil {
		out["usage"] = usage
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode chat.completion.chunk: %w", err)
	}
	return append(append([]byte("data: "), encoded...), '\n', '\n'), nil
}

// --- usage extraction --------------------------------------------------------

// anthropicCacheCreation is the per-TTL breakdown of prompt-cache write
// tokens that newer Anthropic API versions nest under usage.cache_creation.
type anthropicCacheCreation struct {
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
}

// anthropicUsageBlock is one usage object as Anthropic emits it — either at
// the top level of a Messages response / message_delta frame, or nested under
// message on a message_start frame.
type anthropicUsageBlock struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	// CacheCreation is the modern per-TTL breakdown. A pointer so its
	// presence is distinguishable from an all-zero breakdown: when the
	// object is present it is authoritative and the legacy total below is
	// ignored (the API sends both, and summing them would double-bill).
	CacheCreation *anthropicCacheCreation `json:"cache_creation"`
	// CacheCreationInputTokens is the legacy un-broken-down total. Older
	// API versions send only this; it maps to the 5-minute bucket, the TTL
	// that legacy cache writes always had.
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// cacheWrites resolves the two cache-write buckets from one usage block,
// applying the breakdown-wins-over-legacy rule.
func (b anthropicUsageBlock) cacheWrites() (w5m, w1h int64) {
	if b.CacheCreation != nil {
		return b.CacheCreation.Ephemeral5mInputTokens, b.CacheCreation.Ephemeral1hInputTokens
	}
	return b.CacheCreationInputTokens, 0
}

type anthropicUsage struct {
	Usage      anthropicUsageBlock `json:"usage"`
	StopReason string              `json:"stop_reason"`
	// Streamed message_delta events nest stop_reason under delta.
	Delta *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message *struct {
		Usage anthropicUsageBlock `json:"usage"`
	} `json:"message"`
}

// ExtractUsage reads token counts from a Messages response.
func (a *Anthropic) ExtractUsage(body []byte) (Usage, bool) {
	var env anthropicUsage
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, false
	}
	w5m, w1h := env.Usage.cacheWrites()
	u := Usage{
		TokensIn:           env.Usage.InputTokens,
		TokensOut:          env.Usage.OutputTokens,
		TokensCached:       env.Usage.CacheReadInputTokens,
		TokensCacheWrite5m: w5m,
		TokensCacheWrite1h: w1h,
		FinishReason:       env.StopReason,
	}
	if env.Message != nil {
		u.TokensIn = max64(u.TokensIn, env.Message.Usage.InputTokens)
		u.TokensOut = max64(u.TokensOut, env.Message.Usage.OutputTokens)
		u.TokensCached = max64(u.TokensCached, env.Message.Usage.CacheReadInputTokens)
		m5m, m1h := env.Message.Usage.cacheWrites()
		u.TokensCacheWrite5m = max64(u.TokensCacheWrite5m, m5m)
		u.TokensCacheWrite1h = max64(u.TokensCacheWrite1h, m1h)
	}
	if env.Delta != nil && env.Delta.StopReason != "" {
		u.FinishReason = env.Delta.StopReason
	}
	u.Reported = u.TokensIn > 0 || u.TokensOut > 0
	// Anthropic reports cache reads and cache writes DISJOINT from
	// input_tokens; the cost function must add them rather than subtract.
	u.CachedDisjoint = true
	return u, u.Reported
}

// NewStreamCollector accumulates the typed Anthropic SSE events, where input
// tokens arrive on message_start and output tokens on message_delta.
func (a *Anthropic) NewStreamCollector() StreamCollector { return &anthropicStreamCollector{} }

type anthropicStreamCollector struct{ usage Usage }

func (c *anthropicStreamCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	var env anthropicUsage
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	// One frame's view of the usage. message_start carries input, cache
	// reads and cache writes under message.usage; message_delta repeats the
	// running totals (and the output count) at the top level.
	frame := Usage{
		TokensIn:     env.Usage.InputTokens,
		TokensOut:    env.Usage.OutputTokens,
		TokensCached: env.Usage.CacheReadInputTokens,
	}
	frame.TokensCacheWrite5m, frame.TokensCacheWrite1h = env.Usage.cacheWrites()
	if env.Message != nil {
		frame.TokensIn = max64(frame.TokensIn, env.Message.Usage.InputTokens)
		frame.TokensOut = max64(frame.TokensOut, env.Message.Usage.OutputTokens)
		frame.TokensCached = max64(frame.TokensCached, env.Message.Usage.CacheReadInputTokens)
		m5m, m1h := env.Message.Usage.cacheWrites()
		frame.TokensCacheWrite5m = max64(frame.TokensCacheWrite5m, m5m)
		frame.TokensCacheWrite1h = max64(frame.TokensCacheWrite1h, m1h)
	}
	// A frame counts as a report when it carries ANY billable dimension, so
	// cache-write counts are never dropped even on a frame with no plain
	// input/output tokens (a fully cached prompt).
	frame.Reported = frame.TokensIn > 0 || frame.TokensOut > 0 || frame.TokensCached > 0 ||
		frame.TokensCacheWrite5m > 0 || frame.TokensCacheWrite1h > 0
	frame.FinishReason = env.StopReason
	// Streamed message_delta events nest stop_reason under delta.
	if env.Delta != nil && env.Delta.StopReason != "" {
		frame.FinishReason = env.Delta.StopReason
	}
	// Shared merge: every dimension takes the largest observed value (the
	// final frame is authoritative), flags are sticky — the same rule the
	// OpenAI, Bedrock and Vertex collectors apply, so no adapter can drift in
	// what it keeps from a trailing frame.
	c.usage.MergeStream(frame)
	c.usage.CachedDisjoint = true
}

func (c *anthropicStreamCollector) Usage() Usage {
	// Same disjoint cache convention as the buffered path (see ExtractUsage).
	c.usage.CachedDisjoint = true
	return c.usage
}
