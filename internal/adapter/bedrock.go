package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func init() { Register(&Bedrock{}) }

// Bedrock talks to AWS Bedrock Runtime with SigV4 request signing.
//
// Credentials are stored in the upstream's encrypted key field as
// "ACCESS_KEY_ID:SECRET_ACCESS_KEY" (optionally ":SESSION_TOKEN"). The region is
// taken from the base URL host, e.g.
// https://bedrock-runtime.us-east-1.amazonaws.com.
//
// NOTE: AWS Bedrock is a paid cloud service; the customer supplies their own AWS
// account and credentials. Janus never provisions one.
type Bedrock struct{}

// Type returns the stored adapter enum value.
func (a *Bedrock) Type() string { return "bedrock" }

// Discover lists foundation models via the Bedrock control plane.
func (a *Bedrock) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	region := regionFromHost(up.BaseURL)
	endpoint := fmt.Sprintf("https://bedrock.%s.amazonaws.com/foundation-models", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build Bedrock discovery request: %w", err)
	}
	creds, err := parseAWSCredentials(up.APIKey)
	if err != nil {
		return nil, err
	}
	if err := signAWSV4(req, nil, creds, region, "bedrock", time.Now().UTC()); err != nil {
		return nil, err
	}
	body, err := doDiscovery(client, req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		ModelSummaries []struct {
			ModelID          string   `json:"modelId"`
			OutputModalities []string `json:"outputModalities"`
		} `json:"modelSummaries"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse Bedrock model list: %w", err)
	}
	out := make([]DiscoveredModel, 0, len(payload.ModelSummaries))
	for _, m := range payload.ModelSummaries {
		if m.ModelID == "" {
			continue
		}
		out = append(out, DiscoveredModel{Name: m.ModelID, Modalities: bedrockModalities(m.OutputModalities)})
	}
	return out, nil
}

func bedrockModalities(out []string) []string {
	mapped := []string{}
	for _, m := range out {
		switch strings.ToUpper(m) {
		case "IMAGE":
			mapped = append(mapped, ModalityImage)
		case "EMBEDDING":
			mapped = append(mapped, ModalityEmbedding)
		case "VIDEO":
			mapped = append(mapped, ModalityVideo)
		case "SPEECH":
			mapped = append(mapped, ModalityTTS)
		default:
			mapped = append(mapped, ModalityChat)
		}
	}
	if len(mapped) == 0 {
		return []string{ModalityChat}
	}
	return mapped
}

// Prepare targets the Bedrock Converse API and signs the request.
func (a *Bedrock) Prepare(up Upstream, req *Request) (*Prepared, error) {
	creds, err := parseAWSCredentials(up.APIKey)
	if err != nil {
		return nil, err
	}
	region := regionFromHost(up.BaseURL)
	action := "converse"
	if req.Streaming {
		action = "converse-stream"
	}
	target := fmt.Sprintf("%s/model/%s/%s", strings.TrimRight(up.BaseURL, "/"), url.PathEscape(req.Model), action)

	body, err := chatToConverse(req.Body)
	if err != nil {
		return nil, err
	}
	header := cloneHeader(req.Header)
	header.Set("Content-Type", "application/json")

	signable, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build Bedrock request: %w", err)
	}
	signable.Header = header
	if err := signAWSV4(signable, body, creds, region, "bedrock", time.Now().UTC()); err != nil {
		return nil, err
	}
	return &Prepared{URL: target, Header: signable.Header, Body: body}, nil
}

// chatToConverse maps an OpenAI chat request onto the Bedrock Converse shape.
func chatToConverse(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var in struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		MaxTokens   *int     `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat completion request: %w", err)
	}
	messages := []map[string]any{}
	system := []map[string]any{}
	for _, m := range in.Messages {
		text, ok := m.Content.(string)
		if !ok {
			encoded, err := json.Marshal(m.Content)
			if err != nil {
				return nil, fmt.Errorf("encode message content: %w", err)
			}
			text = string(encoded)
		}
		if m.Role == "system" {
			system = append(system, map[string]any{"text": text})
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": []map[string]any{{"text": text}}})
	}
	out := map[string]any{"messages": messages}
	if len(system) > 0 {
		out["system"] = system
	}
	inference := map[string]any{}
	if in.MaxTokens != nil {
		inference["maxTokens"] = *in.MaxTokens
	}
	if in.Temperature != nil {
		inference["temperature"] = *in.Temperature
	}
	if len(inference) > 0 {
		out["inferenceConfig"] = inference
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode Converse request: %w", err)
	}
	return encoded, nil
}

// ExtractUsage reads the Converse usage block.
func (a *Bedrock) ExtractUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage struct {
			InputTokens      int64 `json:"inputTokens"`
			OutputTokens     int64 `json:"outputTokens"`
			CacheReadTokens  int64 `json:"cacheReadInputTokens"`
			CacheWriteTokens int64 `json:"cacheWriteInputTokens"`
		} `json:"usage"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, false
	}
	u := Usage{
		TokensIn: env.Usage.InputTokens, TokensOut: env.Usage.OutputTokens,
		TokensCached: env.Usage.CacheReadTokens, FinishReason: env.StopReason,
		// Converse reports one undifferentiated cache-write count; Bedrock
		// bills it at the 5-minute cache-write rate, so that is the tier it
		// lands in rather than being parsed and thrown away.
		TokensCacheWrite5m: env.Usage.CacheWriteTokens,
	}
	u.Reported = u.TokensIn > 0 || u.TokensOut > 0
	// Like the Anthropic API it fronts, Converse counts cache reads and
	// writes OUTSIDE inputTokens; the cost function must add them.
	u.CachedDisjoint = true
	return u, u.Reported
}

// NewStreamCollector scans relayed frames for the trailing metadata event.
func (a *Bedrock) NewStreamCollector() StreamCollector { return &bedrockStreamCollector{adapter: a} }

type bedrockStreamCollector struct {
	adapter *Bedrock
	usage   Usage
	decoder eventStreamDecoder
	mode    bedrockStreamMode
	broken  bool
}

// Feed accepts either JSON frames (bare or as SSE data lines — the shape a
// JSON-over-SSE proxy in front of Bedrock produces) or the binary
// application/vnd.amazon.eventstream chunks the real converse-stream endpoint
// emits. A frame's first byte tells them apart: a JSON object starts with
// '{', an event-stream frame with the high byte of its length (0x00).
func (c *bedrockStreamCollector) Feed(line []byte) {
	if c.mode == bedrockStreamUnknown {
		c.mode = detectBedrockStreamMode(line)
	}
	switch c.mode {
	case bedrockStreamUnknown:
		return
	case bedrockStreamJSON:
		payload, ok := sseData(line)
		if !ok {
			payload = bytes.TrimSpace(line)
		}
		if len(payload) > 0 && payload[0] == '{' {
			c.record(payload)
		}
		return
	}
	if c.broken {
		return
	}
	frames, err := c.decoder.Feed(line)
	if err != nil {
		c.broken = true // out of sync; further bytes cannot be trusted
	}
	for _, f := range frames {
		c.record(f.Payload)
	}
}

func (c *bedrockStreamCollector) record(payload []byte) {
	if u, found := c.adapter.ExtractUsage(payload); found {
		c.usage.MergeStream(u)
		c.usage.CachedDisjoint = true
	}
	var stop struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(payload, &stop) == nil && stop.StopReason != "" {
		c.usage.FinishReason = stop.StopReason
	}
}

func (c *bedrockStreamCollector) Usage() Usage { return c.usage }

// --- response translation ----------------------------------------------------

// converseContentBlock is one block of a Converse output message.
type converseContentBlock struct {
	Text    string `json:"text"`
	ToolUse *struct {
		ToolUseID string          `json:"toolUseId"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"toolUse"`
	ReasoningContent *struct {
		ReasoningText struct {
			Text string `json:"text"`
		} `json:"reasoningText"`
	} `json:"reasoningContent"`
}

// converseFinishReason maps Converse stopReason onto OpenAI finish_reason.
func converseFinishReason(stop string) string {
	switch stop {
	case "":
		return ""
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "guardrail_intervened", "content_filtered":
		return "content_filter"
	default: // end_turn, stop_sequence
		return "stop"
	}
}

func converseUsage(in, out, cached int64) map[string]any {
	u := map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
	if cached > 0 {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return u
}

// TransformResponse wraps a Converse response as an OpenAI chat.completion.
// Bodies that are not Converse output (errors, or an OpenAI-shaped body from
// a compatible front) are relayed unchanged.
func (a *Bedrock) TransformResponse(req *Request, body []byte) ([]byte, error) {
	if req == nil || !strings.HasPrefix(req.Path, "/v1/chat/completions") || len(body) == 0 {
		return body, nil
	}
	var resp struct {
		Output *struct {
			Message struct {
				Content []converseContentBlock `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string `json:"stopReason"`
		Usage      struct {
			InputTokens          int64 `json:"inputTokens"`
			OutputTokens         int64 `json:"outputTokens"`
			CacheReadInputTokens int64 `json:"cacheReadInputTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Output == nil {
		return body, nil
	}
	message := map[string]any{"role": "assistant"}
	var text, reasoning strings.Builder
	var toolCalls []map[string]any
	for _, block := range resp.Output.Message.Content {
		switch {
		case block.ToolUse != nil:
			toolCalls = append(toolCalls, map[string]any{
				"id": block.ToolUse.ToolUseID, "type": "function",
				"function": map[string]any{"name": block.ToolUse.Name, "arguments": toolArguments(block.ToolUse.Input)},
			})
		case block.ReasoningContent != nil:
			reasoning.WriteString(block.ReasoningContent.ReasoningText.Text)
		default:
			text.WriteString(block.Text)
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
	finish := converseFinishReason(resp.StopReason)
	if finish == "" {
		finish = "stop"
	}
	created := time.Now().Unix()
	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-bedrock-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   converseUsage(resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadInputTokens),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode chat.completion response: %w", err)
	}
	return encoded, nil
}

// NewStreamTransformer rewrites converse-stream's binary event-stream frames
// into chat.completion.chunk SSE. Because the translated output IS SSE, the
// upstream Content-Type (application/vnd.amazon.eventstream) is corrected to
// text/event-stream before the proxy copies headers to the client.
func (a *Bedrock) NewStreamTransformer(req *Request, respHeader http.Header) StreamTransformer {
	if req == nil || !strings.HasPrefix(req.Path, "/v1/chat/completions") {
		return PassthroughStream{}
	}
	if respHeader != nil && strings.Contains(strings.ToLower(respHeader.Get("Content-Type")), "vnd.amazon.eventstream") {
		respHeader.Set("Content-Type", "text/event-stream")
	}
	id := ""
	if respHeader != nil {
		id = respHeader.Get("X-Amzn-Requestid")
	}
	if id == "" {
		id = fmt.Sprintf("bedrock-%d", time.Now().UnixNano())
	}
	return &bedrockStreamTransformer{
		id: "chatcmpl-" + id, model: req.Model, created: time.Now().Unix(), toolIndex: map[int]int{},
	}
}

// bedrockStreamTransformer maps ConverseStream events:
//
//	messageStart      → chunk with delta.role
//	contentBlockStart → start.toolUse: chunk opening tool_calls[k]
//	contentBlockDelta → delta.text: content; delta.toolUse.input: arguments;
//	                    delta.reasoningContent.text: reasoning_content
//	messageStop       → chunk with finish_reason
//	metadata          → usage-only chunk + [DONE]
//	exception frames  → data: {"error": …}
//
// JSON frames (bare or SSE data lines) are accepted too, for JSON-over-SSE
// fronts and for tests.
type bedrockStreamTransformer struct {
	id        string
	model     string
	created   int64
	decoder   eventStreamDecoder
	mode      bedrockStreamMode
	toolIndex map[int]int
	in, out   int64
	cached    int64
	done      bool
}

func (t *bedrockStreamTransformer) TransformStreamLine(line []byte) ([]byte, error) {
	if t.mode == bedrockStreamUnknown {
		t.mode = detectBedrockStreamMode(line)
	}
	switch t.mode {
	case bedrockStreamUnknown:
		return nil, nil // leading whitespace before the first frame
	case bedrockStreamJSON:
		payload, ok := sseData(line)
		if !ok {
			payload = bytes.TrimSpace(line)
			if len(payload) == 0 || payload[0] != '{' {
				return nil, nil // event:/comment lines and blank separators
			}
		}
		out, known, err := t.event("", payload)
		if err != nil {
			return nil, err
		}
		if !known {
			return line, nil // not a ConverseStream event (e.g. an error body): relay as-is
		}
		return out, nil
	}
	// Binary event-stream: every byte belongs to the framing, so the whole
	// line — including a bare '\n', which is just a 0x0A byte inside a frame
	// — is handed to the decoder.
	frames, err := t.decoder.Feed(line)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, f := range frames {
		if f.Headers[":message-type"] == "exception" || f.Headers[":message-type"] == "error" {
			kind := f.Headers[":exception-type"]
			if kind == "" {
				kind = f.Headers[":error-code"]
			}
			var msg struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(f.Payload, &msg)
			frame, _ := json.Marshal(map[string]any{"error": map[string]any{
				"message": msg.Message, "type": kind, "code": kind,
			}})
			out = append(out, append(append([]byte("data: "), frame...), '\n', '\n')...)
			continue
		}
		chunk, _, err := t.event(f.Headers[":event-type"], f.Payload)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// bedrockStreamMode tells JSON frames (bare or SSE-wrapped) from the binary
// event-stream framing. Decided once, on the first non-blank line: a JSON
// object starts with '{' and SSE with a field name, while an event-stream
// frame starts with the high byte of its 32-bit length (0x00).
type bedrockStreamMode int

const (
	bedrockStreamUnknown bedrockStreamMode = iota
	bedrockStreamJSON
	bedrockStreamBinary
)

func detectBedrockStreamMode(line []byte) bedrockStreamMode {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return bedrockStreamUnknown
	}
	switch {
	case trimmed[0] == '{', trimmed[0] == ':',
		bytes.HasPrefix(trimmed, []byte("data:")), bytes.HasPrefix(trimmed, []byte("event:")):
		return bedrockStreamJSON
	default:
		return bedrockStreamBinary
	}
}

// event translates one decoded ConverseStream event. eventType may be empty
// for JSON frames, in which case the payload's fields identify it; known
// reports whether the payload was recognised as a ConverseStream event at all.
func (t *bedrockStreamTransformer) event(eventType string, payload []byte) (out []byte, known bool, err error) {
	var ev struct {
		Role              string `json:"role"`
		ContentBlockIndex int    `json:"contentBlockIndex"`
		Start             *struct {
			ToolUse *struct {
				ToolUseID string `json:"toolUseId"`
				Name      string `json:"name"`
			} `json:"toolUse"`
		} `json:"start"`
		Delta *struct {
			Text    *string `json:"text"`
			ToolUse *struct {
				Input string `json:"input"`
			} `json:"toolUse"`
			ReasoningContent *struct {
				Text string `json:"text"`
			} `json:"reasoningContent"`
		} `json:"delta"`
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens          int64 `json:"inputTokens"`
			OutputTokens         int64 `json:"outputTokens"`
			CacheReadInputTokens int64 `json:"cacheReadInputTokens"`
		} `json:"usage"`
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil, false, fmt.Errorf("parse Bedrock stream event: %w", err)
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, false, fmt.Errorf("parse Bedrock stream event: %w", err)
	}
	if _, isStop := probe["contentBlockIndex"]; eventType == "" && !isStop &&
		ev.Role == "" && ev.Start == nil && ev.Delta == nil && ev.StopReason == "" && ev.Usage == nil {
		return nil, false, nil
	}
	wrap := func(frame []byte, err error) ([]byte, bool, error) { return frame, true, err }
	switch {
	case eventType == "messageStart" || (eventType == "" && ev.Role != ""):
		return wrap(t.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
	case ev.Start != nil && ev.Start.ToolUse != nil:
		k := len(t.toolIndex)
		t.toolIndex[ev.ContentBlockIndex] = k
		return wrap(t.chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": k, "id": ev.Start.ToolUse.ToolUseID, "type": "function",
			"function": map[string]any{"name": ev.Start.ToolUse.Name, "arguments": ""},
		}}}, nil, nil))
	case ev.Delta != nil:
		switch {
		case ev.Delta.ToolUse != nil:
			k, ok := t.toolIndex[ev.ContentBlockIndex]
			if !ok {
				k = len(t.toolIndex)
				t.toolIndex[ev.ContentBlockIndex] = k
			}
			return wrap(t.chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": k, "function": map[string]any{"arguments": ev.Delta.ToolUse.Input},
			}}}, nil, nil))
		case ev.Delta.ReasoningContent != nil:
			return wrap(t.chunk(map[string]any{"reasoning_content": ev.Delta.ReasoningContent.Text}, nil, nil))
		case ev.Delta.Text != nil:
			return wrap(t.chunk(map[string]any{"content": *ev.Delta.Text}, nil, nil))
		}
		return nil, true, nil
	case ev.StopReason != "":
		finish := converseFinishReason(ev.StopReason)
		return wrap(t.chunk(map[string]any{}, &finish, nil))
	case ev.Usage != nil:
		t.in, t.out, t.cached = ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadInputTokens
		if t.done {
			return nil, true, nil
		}
		t.done = true
		frame, err := t.chunk(nil, nil, converseUsage(t.in, t.out, t.cached))
		if err != nil {
			return nil, true, err
		}
		return append(frame, []byte("data: [DONE]\n\n")...), true, nil
	default: // contentBlockStop
		return nil, true, nil
	}
}

func (t *bedrockStreamTransformer) chunk(delta map[string]any, finish *string, usage map[string]any) ([]byte, error) {
	out := map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model}
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

// --- SigV4 -------------------------------------------------------------------

type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func parseAWSCredentials(raw string) (awsCredentials, error) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return awsCredentials{}, fmt.Errorf("bedrock credentials must be stored as ACCESS_KEY_ID:SECRET_ACCESS_KEY (optionally :SESSION_TOKEN)")
	}
	creds := awsCredentials{AccessKeyID: parts[0], SecretAccessKey: parts[1]}
	if len(parts) > 2 {
		creds.SessionToken = strings.Join(parts[2:], ":")
	}
	return creds, nil
}

func regionFromHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "us-east-1"
	}
	segments := strings.Split(u.Hostname(), ".")
	for i, s := range segments {
		if s == "amazonaws" && i >= 1 {
			return segments[i-1]
		}
	}
	return "us-east-1"
}

// signAWSV4 applies AWS Signature Version 4 to req in place.
func signAWSV4(req *http.Request, body []byte, creds awsCredentials, region, service string, now time.Time) error {
	if req.URL == nil {
		return fmt.Errorf("sign request: missing URL")
	}
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256.Sum256(body)
	payloadHex := hex.EncodeToString(payloadHash[:])

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHex)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	signedNames := []string{}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if lower == "host" || strings.HasPrefix(lower, "x-amz-") || lower == "content-type" {
			signedNames = append(signedNames, lower)
		}
	}
	sort.Strings(signedNames)

	var canonicalHeaders strings.Builder
	for _, name := range signedNames {
		value := req.Header.Get(name)
		if name == "host" {
			value = req.URL.Host
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	signedHeaders := strings.Join(signedNames, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURIPath(req.URL.EscapedPath()),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHex,
	}, "\n")

	crHash := sha256.Sum256([]byte(canonicalRequest))
	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, scope, signedHeaders, signature))
	return nil
}

func canonicalURIPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
