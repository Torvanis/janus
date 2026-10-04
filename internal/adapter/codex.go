package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Codex speaks OpenAI's ChatGPT-plan inference surface
// (chatgpt.com/backend-api/codex), the endpoint a ChatGPT Plus/Pro sign-in
// can call. That surface accepts ONLY the Responses API, and only streamed,
// while almost every client a gateway serves speaks Chat Completions. The
// adapter therefore translates in both directions:
//
//   - /v1/chat/completions requests become Responses requests (messages →
//     input items, tools → Responses function tools, system messages →
//     instructions) and the Responses event stream is turned back into
//     chat.completion.chunk frames — or, when the caller did not ask to
//     stream, aggregated into one chat.completion object.
//   - /v1/responses requests are forwarded natively for clients that already
//     speak Responses.
//
// It is only ever used for personal subscriptions (it is not offered as an
// upstream type): the bearer credential is the user's own ChatGPT sign-in.
type Codex struct{}

func init() { Register(&Codex{}) }

// CodexAdapterType is the adapter_type string for the ChatGPT-plan surface.
const CodexAdapterType = "openai_codex"

// codexOriginator identifies Janus to OpenAI. OpenAI asks third-party
// harnesses on this endpoint to identify themselves rather than impersonate
// the Codex CLI.
const codexOriginator = "janus_gateway"

// Type implements Adapter.
func (c *Codex) Type() string { return CodexAdapterType }

// PersonalOnly implements PersonalOnly: never an upstream type.
func (c *Codex) PersonalOnly() bool { return true }

// Discover implements Adapter. Personal subscriptions list their models
// through the subscription provider, never through upstream discovery.
func (c *Codex) Discover(context.Context, Upstream, *http.Client) ([]DiscoveredModel, error) {
	return nil, errors.New("the ChatGPT plan surface is only reachable through a personal subscription")
}

// BuffersEventStream reports that a non-streaming chat request is still
// answered with an event stream (the surface cannot answer any other way),
// which the proxy must buffer and hand to TransformResponse whole.
func (c *Codex) BuffersEventStream(req *Request) bool { return req != nil && !req.Streaming }

// Prepare implements Adapter.
func (c *Codex) Prepare(up Upstream, req *Request) (*Prepared, error) {
	header := cloneHeader(req.Header)
	for k, v := range CodexHeaders(up.APIKey) {
		header.Set(k, v)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	switch {
	case isChatCompletions(req):
		body, sessionKey, err := chatToResponses(req.Body, req.Model)
		if err != nil {
			return nil, err
		}
		if sessionKey != "" {
			header.Set("session_id", sessionKey)
		}
		return &Prepared{URL: joinURL(up.BaseURL, "/responses"), Header: header, Body: body}, nil
	case strings.HasPrefix(req.Path, "/v1/responses"):
		return &Prepared{URL: joinURL(up.BaseURL, "/responses"), Header: header, Body: req.Body}, nil
	}
	return nil, fmt.Errorf("a ChatGPT plan serves chat completions and responses only, not %s", req.Path)
}

// CodexHeaders returns the identity and workspace headers the ChatGPT plan
// surface requires for an access token. The workspace id and residency come
// from the token's own claims; a token that is not a JWT yields just the
// bearer header, which surfaces as a clear 401 from OpenAI.
func CodexHeaders(accessToken string) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + accessToken,
		"originator":    codexOriginator,
		"User-Agent":    "Janus-Gateway",
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return h
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return h
	}
	var claims struct {
		Auth struct {
			AccountID        string `json:"chatgpt_account_id"`
			DataResidency    string `json:"chatgpt_data_residency"`
			ComputeResidency string `json:"chatgpt_compute_residency"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return h
	}
	if claims.Auth.AccountID != "" {
		h["ChatGPT-Account-ID"] = claims.Auth.AccountID
	}
	if r := strings.TrimSpace(firstNonEmpty(claims.Auth.DataResidency, claims.Auth.ComputeResidency)); r != "" {
		h["x-openai-internal-codex-residency"] = r
	}
	return h
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// --- request translation -----------------------------------------------------

// codexDefaultInstructions is sent when the caller supplied no system
// message: the surface requires an instructions string.
const codexDefaultInstructions = "You are a helpful assistant."

// chatToResponses converts an OpenAI chat request into a Responses request
// for the ChatGPT plan surface. Contract, as for every translating adapter:
// translate or fail loudly. Fields the surface refuses and that only tune
// sampling (temperature, top_p, penalties, seed, stop, max tokens — the plan
// meters its own limits) are dropped; anything that would change what the
// caller receives and cannot be expressed (n > 1, audio output) is refused.
func chatToResponses(body []byte, model string) ([]byte, string, error) {
	var in struct {
		Messages          []json.RawMessage `json:"messages"`
		Tools             []json.RawMessage `json:"tools"`
		ToolChoice        json.RawMessage   `json:"tool_choice"`
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		ResponseFormat    json.RawMessage   `json:"response_format"`
		ReasoningEffort   string            `json:"reasoning_effort"`
		Reasoning         json.RawMessage   `json:"reasoning"`
		N                 *int              `json:"n"`
		Modalities        []string          `json:"modalities"`
		PromptCacheKey    string            `json:"prompt_cache_key"`
		User              string            `json:"user"`
		Verbosity         string            `json:"verbosity"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "", fmt.Errorf("parse chat request: %w", err)
	}
	if in.N != nil && *in.N > 1 {
		return nil, "", errors.New("a ChatGPT plan returns one choice per request; n must be 1")
	}
	for _, m := range in.Modalities {
		if m == "audio" {
			return nil, "", errors.New("a ChatGPT plan cannot return audio through chat completions")
		}
	}
	if len(in.Messages) == 0 {
		return nil, "", errors.New("messages must not be empty")
	}

	var instructions []string
	input := []any{}
	for i, raw := range in.Messages {
		var m chatMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			text, err := plainText(m.Content)
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			if strings.TrimSpace(text) != "" {
				instructions = append(instructions, text)
			}
		case "user":
			parts, err := responsesUserParts(m.Content)
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			input = append(input, map[string]any{"type": "message", "role": "user", "content": parts})
		case "assistant":
			text, err := plainText(m.Content)
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			if text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID,
					"name": tc.Function.Name, "arguments": args})
			}
		case "tool", "function":
			text, err := plainText(m.Content)
			if err != nil {
				return nil, "", fmt.Errorf("messages[%d]: %w", i, err)
			}
			if m.ToolCallID == "" {
				return nil, "", fmt.Errorf("messages[%d]: a tool message needs tool_call_id", i)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": text})
		default:
			return nil, "", fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}

	out := map[string]any{
		"model":        model,
		"instructions": strings.Join(instructions, "\n\n"),
		"input":        input,
		"store":        false,
		"stream":       true,
	}
	if out["instructions"] == "" {
		out["instructions"] = codexDefaultInstructions
	}
	if len(in.Tools) > 0 {
		tools := make([]any, 0, len(in.Tools))
		for i, raw := range in.Tools {
			t, err := responsesTool(raw)
			if err != nil {
				return nil, "", fmt.Errorf("tools[%d]: %w", i, err)
			}
			tools = append(tools, t)
		}
		out["tools"] = tools
		choice, err := responsesToolChoice(in.ToolChoice)
		if err != nil {
			return nil, "", err
		}
		out["tool_choice"] = choice
		parallel := true
		if in.ParallelToolCalls != nil {
			parallel = *in.ParallelToolCalls
		}
		out["parallel_tool_calls"] = parallel
	}
	switch {
	case len(in.Reasoning) > 0 && string(in.Reasoning) != "null":
		out["reasoning"] = json.RawMessage(in.Reasoning)
	case in.ReasoningEffort != "":
		out["reasoning"] = map[string]any{"effort": in.ReasoningEffort, "summary": "auto"}
	}
	if len(in.ResponseFormat) > 0 && string(in.ResponseFormat) != "null" {
		format, err := responsesTextFormat(in.ResponseFormat)
		if err != nil {
			return nil, "", err
		}
		if format != nil {
			out["text"] = map[string]any{"format": format}
		}
	}
	if in.Verbosity != "" {
		text, _ := out["text"].(map[string]any)
		if text == nil {
			text = map[string]any{}
		}
		text["verbosity"] = in.Verbosity
		out["text"] = text
	}
	sessionKey := in.PromptCacheKey
	if sessionKey == "" {
		sessionKey = in.User
	}
	if sessionKey != "" {
		out["prompt_cache_key"] = sessionKey
	}
	encoded, err := json.Marshal(out)
	return encoded, sessionKey, err
}

// plainText flattens chat content (a string or an array of text parts)
// into one string. Non-text parts are refused: they cannot be carried on
// the roles this is used for.
func plainText(content json.RawMessage) (string, error) {
	if len(content) == 0 || string(content) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", errors.New("content must be a string or an array of parts")
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			b.WriteString(p.Text)
		default:
			return "", fmt.Errorf("content part type %q is not supported on this message role", p.Type)
		}
	}
	return b.String(), nil
}

func responsesUserParts(content json.RawMessage) ([]any, error) {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []any{map[string]any{"type": "input_text", "text": s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url"`
		File struct {
			FileData string `json:"file_data"`
			Filename string `json:"filename"`
		} `json:"file"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, errors.New("content must be a string or an array of parts")
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			out = append(out, map[string]any{"type": "input_text", "text": p.Text})
		case "image_url":
			part := map[string]any{"type": "input_image", "image_url": p.ImageURL.URL}
			if p.ImageURL.Detail != "" {
				part["detail"] = p.ImageURL.Detail
			}
			out = append(out, part)
		case "file":
			if p.File.FileData == "" {
				return nil, errors.New("file parts must carry file_data")
			}
			part := map[string]any{"type": "input_file", "file_data": p.File.FileData}
			if p.File.Filename != "" {
				part["filename"] = p.File.Filename
			}
			out = append(out, part)
		default:
			return nil, fmt.Errorf("content part type %q is not supported", p.Type)
		}
	}
	return out, nil
}

func responsesTool(raw json.RawMessage) (map[string]any, error) {
	var t struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if t.Type != "function" || t.Function.Name == "" {
		return nil, errors.New("only function tools are supported")
	}
	out := map[string]any{"type": "function", "name": t.Function.Name, "description": t.Function.Description}
	if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
		out["parameters"] = t.Function.Parameters
	} else {
		out["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if t.Function.Strict != nil {
		out["strict"] = *t.Function.Strict
	}
	return out, nil
}

func responsesToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "auto", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		}
		return nil, fmt.Errorf("tool_choice %q is not supported", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
		return nil, errors.New("tool_choice must be auto, none, required, or a named function")
	}
	return map[string]any{"type": "function", "name": obj.Function.Name}, nil
}

func responsesTextFormat(raw json.RawMessage) (any, error) {
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("response_format: %w", err)
	}
	switch rf.Type {
	case "", "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		name := rf.JSONSchema.Name
		if name == "" {
			name = "response"
		}
		f := map[string]any{"type": "json_schema", "name": name, "schema": rf.JSONSchema.Schema}
		if rf.JSONSchema.Strict != nil {
			f["strict"] = *rf.JSONSchema.Strict
		}
		return f, nil
	}
	return nil, fmt.Errorf("response_format type %q is not supported", rf.Type)
}

// --- response translation ----------------------------------------------------

// codexEvent is the subset of Responses stream events the translation reads.
type codexEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	OutputIndex int             `json:"output_index"`
	Item        codexOutputItem `json:"item"`
	Response    struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *codexUsage `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

type codexOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type codexUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *codexUsage) usage() Usage {
	// Responses reports cached input as a subset of input_tokens, the
	// OpenAI convention (CachedDisjoint=false).
	return Usage{TokensIn: u.InputTokens, TokensOut: u.OutputTokens, TokensCached: u.InputTokensDetails.CachedTokens, Reported: true}
}

func (u *codexUsage) openAI() map[string]any {
	return map[string]any{
		"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.InputTokensDetails.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.OutputTokensDetails.ReasoningTokens},
	}
}

// codexTranslator turns a Responses event sequence into chat-completion
// output. One instance per response; used line by line when streaming and
// over the whole buffered stream otherwise.
type codexTranslator struct {
	model    string
	id       string
	created  int64
	started  bool
	finished bool
	// toolIndex maps a Responses output_index to its chat tool_call index.
	toolIndex map[int]int
	tools     []map[string]any // aggregated tool calls (buffered mode)
	text      strings.Builder
	reasoning strings.Builder
	usage     *codexUsage
	finish    string
	err       error
}

func newCodexTranslator(model string) *codexTranslator {
	return &codexTranslator{model: model, id: "chatcmpl-janus", created: time.Now().Unix(), toolIndex: map[int]int{}}
}

// event applies one Responses event and returns the chat deltas it
// produces (streaming mode emits each as a chunk).
func (t *codexTranslator) event(ev codexEvent) []map[string]any {
	switch ev.Type {
	case "response.created", "response.in_progress":
		if ev.Response.ID != "" {
			t.id = "chatcmpl-" + strings.TrimPrefix(ev.Response.ID, "resp_")
		}
		if ev.Response.Model != "" {
			t.model = ev.Response.Model
		}
	case "response.output_text.delta":
		t.text.WriteString(ev.Delta)
		return []map[string]any{{"content": ev.Delta}}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		t.reasoning.WriteString(ev.Delta)
		return []map[string]any{{"reasoning_content": ev.Delta}}
	case "response.output_item.added":
		if ev.Item.Type == "function_call" {
			idx := len(t.toolIndex)
			t.toolIndex[ev.OutputIndex] = idx
			t.tools = append(t.tools, map[string]any{"id": ev.Item.CallID, "type": "function",
				"function": map[string]any{"name": ev.Item.Name, "arguments": ""}})
			return []map[string]any{{"tool_calls": []any{map[string]any{"index": idx, "id": ev.Item.CallID, "type": "function",
				"function": map[string]any{"name": ev.Item.Name, "arguments": ""}}}}}
		}
	case "response.function_call_arguments.delta":
		idx, ok := t.toolIndex[ev.OutputIndex]
		if !ok {
			return nil
		}
		fn := t.tools[idx]["function"].(map[string]any)
		fn["arguments"] = fn["arguments"].(string) + ev.Delta
		return []map[string]any{{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]any{"arguments": ev.Delta}}}}}
	case "response.output_item.done":
		// A function call whose arguments arrived only in the done item
		// (no deltas) still has to reach the caller.
		if ev.Item.Type == "function_call" {
			idx, ok := t.toolIndex[ev.OutputIndex]
			if !ok {
				return nil
			}
			fn := t.tools[idx]["function"].(map[string]any)
			if fn["arguments"].(string) == "" && ev.Item.Arguments != "" {
				fn["arguments"] = ev.Item.Arguments
				return []map[string]any{{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]any{"arguments": ev.Item.Arguments}}}}}
			}
		}
	case "response.completed", "response.incomplete":
		t.finished = true
		t.usage = ev.Response.Usage
		switch {
		case len(t.tools) > 0:
			t.finish = "tool_calls"
		case ev.Response.IncompleteDetails.Reason == "max_output_tokens":
			t.finish = "length"
		case ev.Response.IncompleteDetails.Reason == "content_filter":
			t.finish = "content_filter"
		default:
			t.finish = "stop"
		}
	case "response.failed":
		t.finished = true
		msg := "the ChatGPT plan reported a failed response"
		if ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		}
		t.err = errors.New(msg)
	case "error":
		t.finished = true
		msg := ev.Message
		if msg == "" {
			msg = "the ChatGPT plan returned an error"
		}
		t.err = errors.New(msg)
	}
	return nil
}

func (t *codexTranslator) chunk(delta map[string]any, finish any, usage map[string]any) []byte {
	choices := []any{}
	if delta != nil {
		choices = append(choices, map[string]any{"index": 0, "delta": delta, "finish_reason": finish})
	}
	c := map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model, "choices": choices}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// NewStreamTransformer implements Adapter.
func (c *Codex) NewStreamTransformer(req *Request, _ http.Header) StreamTransformer {
	if !isChatCompletions(req) {
		return PassthroughStream{}
	}
	return &codexStream{t: newCodexTranslator(reqModel(req))}
}

type codexStream struct{ t *codexTranslator }

func (s *codexStream) TransformStreamLine(line []byte) ([]byte, error) {
	if !s.t.started && !isSSEFraming(line) {
		// Not an event stream: an error body (JSON) under a 4xx/5xx
		// status. Relay it as-is so the caller sees OpenAI's reason.
		return line, nil
	}
	payload, ok := sseData(line)
	if !ok || s.t.finished {
		return nil, nil
	}
	var ev codexEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("parse ChatGPT plan event: %w", err)
	}
	var out []byte
	if !s.t.started && ev.Type != "" {
		s.t.started = true
		s.t.event(ev) // created carries the id and model the first chunk repeats
		out = append(out, s.t.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)...)
		if ev.Type == "response.created" || ev.Type == "response.in_progress" {
			return out, nil
		}
	}
	for _, d := range s.t.event(ev) {
		out = append(out, s.t.chunk(d, nil, nil)...)
	}
	if s.t.err != nil {
		return out, s.t.err
	}
	if s.t.finished {
		out = append(out, s.t.chunk(map[string]any{}, s.t.finish, nil)...)
		if s.t.usage != nil {
			out = append(out, s.t.chunk(nil, nil, s.t.usage.openAI())...)
		}
		out = append(out, []byte("data: [DONE]\n\n")...)
	}
	return out, nil
}

// TransformResponse implements Adapter. A non-streaming chat request was
// sent upstream as a stream (the only form the surface answers); here the
// whole event stream is folded into one chat.completion object. Error bodies
// (JSON, not a stream) are passed through for the proxy to relay.
func (c *Codex) TransformResponse(req *Request, body []byte) ([]byte, error) {
	if !isChatCompletions(req) || !looksLikeEventStream(body) {
		return body, nil
	}
	t := newCodexTranslator(reqModel(req))
	forEachSSEData(body, func(payload []byte) {
		var ev codexEvent
		if json.Unmarshal(payload, &ev) == nil {
			t.event(ev)
		}
	})
	if t.err != nil {
		return nil, t.err
	}
	if !t.finished {
		return nil, errors.New("the ChatGPT plan stream ended before the response completed")
	}
	msg := map[string]any{"role": "assistant", "content": t.text.String()}
	if t.reasoning.Len() > 0 {
		msg["reasoning_content"] = t.reasoning.String()
	}
	if len(t.tools) > 0 {
		calls := make([]any, len(t.tools))
		for i, tc := range t.tools {
			calls[i] = tc
		}
		msg["tool_calls"] = calls
		if t.text.Len() == 0 {
			msg["content"] = nil
		}
	}
	out := map[string]any{
		"id": t.id, "object": "chat.completion", "created": t.created, "model": t.model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": t.finish}},
	}
	if t.usage != nil {
		out["usage"] = t.usage.openAI()
	}
	return json.Marshal(out)
}

// isSSEFraming reports whether a line belongs to an event stream (a field,
// a comment, or the blank line that ends an event).
func isSSEFraming(line []byte) bool {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return true
	}
	for _, p := range []string{"event:", "data:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(t, []byte(p)) {
			return true
		}
	}
	return false
}

func looksLikeEventStream(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

func forEachSSEData(body []byte, fn func([]byte)) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if payload, ok := sseData(sc.Bytes()); ok {
			fn(payload)
		}
	}
}

// ExtractUsage implements Adapter: from the completed event of a buffered
// stream, or the usage object of a JSON body.
func (c *Codex) ExtractUsage(body []byte) (Usage, bool) {
	if !looksLikeEventStream(body) {
		var r struct {
			Usage *codexUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil && r.Usage != nil {
			return r.Usage.usage(), true
		}
		return Usage{}, false
	}
	col := &codexCollector{}
	forEachSSEData(body, col.feedPayload)
	return col.usage, col.usage.Reported
}

// NewStreamCollector implements Adapter.
func (c *Codex) NewStreamCollector() StreamCollector { return &codexCollector{} }

type codexCollector struct{ usage Usage }

func (c *codexCollector) Feed(line []byte) {
	if payload, ok := sseData(line); ok {
		c.feedPayload(payload)
	}
}

func (c *codexCollector) feedPayload(payload []byte) {
	var ev codexEvent
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	if (ev.Type == "response.completed" || ev.Type == "response.incomplete") && ev.Response.Usage != nil {
		c.usage.MergeStream(ev.Response.Usage.usage())
	}
}

func (c *codexCollector) Usage() Usage { return c.usage }
