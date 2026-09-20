package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

func init() {
	Register(&OpenAICompatible{name: "openai_compatible"})
	// vLLM and llama.cpp speak the OpenAI wire format end to end (discovery,
	// SSE streaming, usage reporting), so they reuse this adapter unchanged.
	//
	// "vlm" is the public adapter_type value persisted in upstream rows.
	// Keep this spelling: renaming it to "vllm" would break stored configurations.
	Register(&OpenAICompatible{name: "vlm"})
	Register(&OpenAICompatible{name: "llama_cpp"})
}

// OpenAICompatible is the reference adapter. It forwards bodies verbatim and
// injects bearer credentials, which is exactly the contract self-hosted engines
// (vLLM, llama.cpp) and OpenAI-compatible gateways implement.
type OpenAICompatible struct{ name string }

// Type returns the stored adapter enum value.
func (a *OpenAICompatible) Type() string { return a.name }

// Discover lists models from the provider's /v1/models endpoint.
func (a *OpenAICompatible) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(up.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	body, err := doDiscovery(client, req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			// vLLM reports the served context length here.
			MaxModelLen int64 `json:"max_model_len"`
			// llama.cpp nests it under meta: n_ctx is what the server was
			// actually started with, n_ctx_train what the model was trained
			// for. The served value is the one a caller must respect.
			Meta struct {
				NCtx      int64 `json:"n_ctx"`
				NCtxTrain int64 `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse model list from %s: %w", up.Name, err)
	}
	var raw struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &raw)
	out := make([]DiscoveredModel, 0, len(payload.Data))
	for i, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		ctxWindow := m.MaxModelLen
		if ctxWindow == 0 {
			ctxWindow = m.Meta.NCtx
		}
		if ctxWindow == 0 {
			ctxWindow = m.Meta.NCtxTrain
		}
		if ctxWindow == 0 {
			ctxWindow = m.ContextLength
		}
		if ctxWindow < 0 {
			ctxWindow = 0
		}
		rates, warnings := discoveryPrices(a.Type(), up, raw.Data[i])
		out = append(out, DiscoveredModel{
			Name: m.ID, Modalities: guessModalities(m.ID), ContextWindow: ctxWindow, Rates: rates, MetadataWarnings: warnings,
		})
	}
	return out, nil
}

// Prepare rewrites the target URL and injects credentials. Bodies pass through
// byte-for-byte apart from opting streaming responses into usage reporting.
func (a *OpenAICompatible) Prepare(up Upstream, req *Request) (*Prepared, error) {
	header := cloneHeader(req.Header)
	if up.APIKey != "" {
		header.Set("Authorization", "Bearer "+up.APIKey)
	} else {
		header.Del("Authorization")
	}
	body := req.Body
	if req.Streaming && isJSON(header) {
		body = ensureStreamUsage(body)
	}
	if a.name == "openai_compatible" && isJSON(header) && strings.HasPrefix(req.Path, "/v1/chat/completions") {
		body = defaultGPT5ReasoningEffort(body, req.Model)
	}
	return &Prepared{URL: joinURL(up.BaseURL, req.Path), Header: header, Body: body}, nil
}

// ExtractUsage reads the `usage` object from a JSON response.
func (a *OpenAICompatible) ExtractUsage(body []byte) (Usage, bool) { return parseOpenAIUsage(body) }

// NewStreamCollector returns a collector for OpenAI-style SSE frames.
func (a *OpenAICompatible) NewStreamCollector() StreamCollector { return &openAIStreamCollector{} }

// TransformResponse leaves OpenAI-shaped bodies alone apart from normalising
// the vendor-specific reasoning field (see normalizeReasoningFields).
func (a *OpenAICompatible) TransformResponse(req *Request, body []byte) ([]byte, error) {
	if req == nil || !strings.HasPrefix(req.Path, "/v1/chat/completions") {
		return body, nil
	}
	return normalizeReasoningFields(body), nil
}

// NewStreamTransformer applies the same reasoning-field normalisation to
// each streamed chunk.
func (a *OpenAICompatible) NewStreamTransformer(req *Request, _ http.Header) StreamTransformer {
	if req == nil || !strings.HasPrefix(req.Path, "/v1/chat/completions") {
		return PassthroughStream{}
	}
	return reasoningNormalizingStream{}
}

type reasoningNormalizingStream struct{}

func (reasoningNormalizingStream) TransformStreamLine(line []byte) ([]byte, error) {
	payload, ok := sseData(line)
	if !ok || !bytes.Contains(payload, []byte(`"reasoning`)) {
		return line, nil
	}
	normalized := normalizeReasoningFields(payload)
	if bytes.Equal(normalized, payload) {
		return line, nil
	}
	return append(append([]byte("data: "), normalized...), '\n', '\n'), nil
}

// gpt5Model matches the GPT-5 family (gpt-5, gpt-5.1, gpt-5-mini,
// gpt-5.6-sol, …) wherever it appears in a model id, but not, say,
// "gpt-50" or "gpt-5x".
var gpt5Model = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])gpt-5(?:[.\-]|$)`)

// defaultGPT5ReasoningEffort absorbs an OpenAI server-side quirk: on GPT-5.x
// models a chat.completions request that carries tools but no explicit
// reasoning_effort is rejected ("Function tools with reasoning_effort are not
// supported … set reasoning_effort to 'none'"), because the server applies
// its own default effort and then refuses the combination. Every
// OpenAI-compatible client that offers tools trips over this, so the gateway
// supplies reasoning_effort:"none" when — and only when — the model is GPT-5.x,
// the request declares tools, and the caller did not choose an effort itself.
// A caller-provided value (including an explicit non-"none" effort) is never
// overridden; that is the caller's decision to make and the upstream's to
// reject.
func defaultGPT5ReasoningEffort(body []byte, model string) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"tools"`)) {
		return body
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if model == "" {
		_ = json.Unmarshal(payload["model"], &model)
	}
	if !gpt5Model.MatchString(model) {
		return body
	}
	if _, set := payload["reasoning_effort"]; set {
		return body
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(payload["tools"], &tools); err != nil || len(tools) == 0 {
		return body
	}
	payload["reasoning_effort"] = json.RawMessage(`"none"`)
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// normalizeReasoningFields canonicalises the reasoning text that
// OpenAI-compatible vendors attach to chat messages and deltas under
// differing, non-standard names — xAI, DeepSeek and vLLM use
// `reasoning_content`; OpenRouter-style gateways use `reasoning`. Downstream
// clients only look for one of them, so a Janus response always carries the
// text as `reasoning_content` (the name Open WebUI and the DeepSeek/vLLM
// ecosystem render), never under a second alias as well. The same field is
// what the Anthropic adapter emits for thinking blocks, so reasoning looks
// identical to the caller whichever provider served the request. Bodies that
// need no change are returned byte-for-byte.
func normalizeReasoningFields(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"reasoning`)) {
		return body
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(payload["choices"], &choices); err != nil || len(choices) == 0 {
		return body
	}
	changed := false
	for i, choice := range choices {
		for _, key := range []string{"message", "delta"} {
			raw, ok := choice[key]
			if !ok {
				continue
			}
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(raw, &msg); err != nil || msg == nil {
				continue
			}
			if !canonicalizeReasoning(msg) {
				continue
			}
			encoded, err := json.Marshal(msg)
			if err != nil {
				return body
			}
			choice[key] = encoded
			changed = true
		}
		choices[i] = choice
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(choices)
	if err != nil {
		return body
	}
	payload["choices"] = encoded
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// canonicalizeReasoning folds the alias spellings into reasoning_content in
// place, reporting whether anything changed. Only string-valued aliases are
// folded: a structured `reasoning` object (some vendors nest summaries there)
// is left where it is rather than guessed at.
func canonicalizeReasoning(msg map[string]json.RawMessage) bool {
	changed := false
	for _, alias := range []string{"reasoning", "reasoning_text"} {
		raw, ok := msg[alias]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			continue
		}
		if existing, has := msg["reasoning_content"]; !has || bytes.Equal(existing, []byte("null")) || bytes.Equal(existing, []byte(`""`)) {
			msg["reasoning_content"] = raw
		}
		delete(msg, alias)
		changed = true
	}
	return changed
}

// openAIUsageEnvelope is the superset of every usage shape seen on the OpenAI
// wire format and the providers that extend it:
//
//   - chat/completions and embeddings: usage.prompt_tokens / completion_tokens
//     (+ prompt_tokens_details.cached_tokens).
//   - responses API, images (gpt-image-1) and TTS streaming
//     (speech.audio.done): usage.input_tokens / output_tokens
//     (+ input_tokens_details.cached_tokens).
//   - X.ai: usage.cost_in_usd_ticks — the exact cost of the request, and on
//     image generation the ONLY billing signal, since no token counts exist.
//     Some responses carry it at the top level instead; both are accepted.
//   - Groq: usage.prompt_time / completion_time (seconds) — measured
//     throughput.
//   - llama.cpp server: a top-level timings block with prompt_per_second /
//     predicted_per_second.
//   - Ollama's OpenAI shim: prompt_eval_count/eval_count with
//     *_duration in nanoseconds (the native fields survive the shim).
type openAIUsageEnvelope struct {
	Type  string `json:"type"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		TotalTokens         int64 `json:"total_tokens"`
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		// X.ai: request cost in USD ticks (see xaiTicksPerNanoUSD).
		CostInUSDTicks *int64 `json:"cost_in_usd_ticks"`
		// Groq: wall-clock seconds spent on the prompt and the completion.
		PromptTime     float64 `json:"prompt_time"`
		CompletionTime float64 `json:"completion_time"`
	} `json:"usage"`
	// X.ai occasionally reports the cost beside, not inside, usage.
	CostInUSDTicks *int64 `json:"cost_in_usd_ticks"`
	// llama.cpp server timings.
	Timings struct {
		PromptN            int64   `json:"prompt_n"`
		PromptPerSecond    float64 `json:"prompt_per_second"`
		PredictedN         int64   `json:"predicted_n"`
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"`
	// Ollama native timing fields (nanoseconds), also present through its
	// OpenAI-compatible shim.
	PromptEvalCount    int64 `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int64 `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
	Choices            []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Responses API streaming: the terminal response.completed event nests
	// the whole response (and its usage) under "response".
	Response *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"response"`
}

// xaiTicksPerNanoUSD converts X.ai's cost_in_usd_ticks to nano-USD. X.ai
// documents one tick as 1e-10 USD (10^10 ticks per dollar), so ten ticks make
// one nano-USD. The division rounds to nearest so a sub-nano remainder never
// systematically under-bills.
const xaiTicksPerNanoUSD = 10

// parseOpenAIUsage reads token counts, a provider-reported cost, and any
// provider-measured throughput out of one JSON document (a whole buffered body
// or a single SSE frame). The boolean mirrors Usage.Reported || CostReported:
// true when the document carried a billing signal the proxy can trust.
func parseOpenAIUsage(body []byte) (Usage, bool) {
	var env openAIUsageEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, false
	}
	if env.Response != nil && len(env.Response.Usage) > 0 && env.Usage.TotalTokens == 0 &&
		env.Usage.InputTokens == 0 && env.Usage.OutputTokens == 0 {
		// Lift a nested Responses-API usage block to the top level and
		// parse it with the same rules; the outer document has no other
		// billing signal.
		nested := append(append([]byte(`{"usage":`), env.Response.Usage...), '}')
		return parseOpenAIUsage(nested)
	}
	u := Usage{
		TokensIn:     max64(env.Usage.PromptTokens, env.Usage.InputTokens),
		TokensOut:    max64(env.Usage.CompletionTokens, env.Usage.OutputTokens),
		TokensCached: max64(env.Usage.PromptTokensDetails.CachedTokens, env.Usage.InputTokensDetails.CachedTokens),
	}
	if len(env.Choices) > 0 {
		u.FinishReason = env.Choices[0].FinishReason
	}
	u.Reported = u.TokensIn > 0 || u.TokensOut > 0

	ticks := env.Usage.CostInUSDTicks
	if ticks == nil {
		ticks = env.CostInUSDTicks
	}
	if ticks != nil && *ticks >= 0 {
		u.CostNano = (*ticks + xaiTicksPerNanoUSD/2) / xaiTicksPerNanoUSD
		u.CostReported = true
	}

	switch {
	case env.Usage.PromptTime > 0 || env.Usage.CompletionTime > 0:
		// Groq: seconds per phase; derive tokens/s from the counts it sits
		// next to. A phase the provider timed at zero stays zero rather than
		// dividing by it.
		u.TokensInPerSecond = ratePerSecond(u.TokensIn, env.Usage.PromptTime)
		u.TokensOutPerSecond = ratePerSecond(u.TokensOut, env.Usage.CompletionTime)
		u.ThroughputReported = true
	case env.Timings.PromptPerSecond > 0 || env.Timings.PredictedPerSecond > 0:
		u.TokensInPerSecond = env.Timings.PromptPerSecond
		u.TokensOutPerSecond = env.Timings.PredictedPerSecond
		u.ThroughputReported = true
	case env.PromptEvalDuration > 0 || env.EvalDuration > 0:
		u.TokensInPerSecond = ratePerSecond(env.PromptEvalCount, float64(env.PromptEvalDuration)/1e9)
		u.TokensOutPerSecond = ratePerSecond(env.EvalCount, float64(env.EvalDuration)/1e9)
		u.ThroughputReported = true
	}
	return u, u.Reported || u.CostReported
}

// ratePerSecond is tokens / seconds, or 0 when either is non-positive.
func ratePerSecond(tokens int64, seconds float64) float64 {
	if tokens <= 0 || seconds <= 0 {
		return 0
	}
	return float64(tokens) / seconds
}

type openAIStreamCollector struct{ usage Usage }

// Feed folds one SSE frame into the running usage. Only the frames that carry
// a usage block change the counts: the final chat.completion.chunk when
// stream_options.include_usage is set, the response.completed event of the
// Responses API, or the speech.audio.done event of a streamed TTS request
// (which is why a streamed TTS call is metered exactly while the binary form
// of the same call carries no usage at all).
func (c *openAIStreamCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	frame, _ := parseOpenAIUsage(payload)
	c.usage.MergeStream(frame)
}

func (c *openAIStreamCollector) Usage() Usage { return c.usage }

// ensureStreamUsage opts a streaming chat request into upstream usage reporting
// so token counts are available on the final frame. It is the one body edit the
// gateway makes, and it is additive and idempotent.
func ensureStreamUsage(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if _, exists := payload["stream_options"]; exists {
		return body
	}
	if stream, _ := payload["stream"].(bool); !stream {
		return body
	}
	payload["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func sseData(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// Allow base URLs that already carry the version segment.
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		return base + strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

func cloneHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, vals := range h {
		switch strings.ToLower(k) {
		case "host", "content-length", "connection", "authorization",
			"x-janus-csrf", "cookie", "transfer-encoding",
			// Never forward the client's Accept-Encoding: setting it explicitly
			// disables Go's transparent gzip negotiation, so the gateway would
			// relay compressed bytes it cannot parse (metering) and — because
			// Content-Encoding is not forwarded back — the client could not
			// decode them either. The transport negotiates gzip itself and
			// hands every layer a decoded body.
			"accept-encoding":
			continue
		}
		for _, v := range vals {
			out.Add(k, v)
		}
	}
	return out
}

func isJSON(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "json")
}

func doDiscovery(client *http.Client, req *http.Request) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned HTTP %d during model discovery", resp.StatusCode)
	}
	return body, nil
}

func guessModalities(name string) []string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "embed"):
		return []string{ModalityEmbedding}
	case strings.Contains(n, "whisper"):
		return []string{ModalitySTT}
	case strings.Contains(n, "tts"):
		return []string{ModalityTTS}
	// Video generation before image: "grok-imagine-video" must not fall
	// through to the image branch (and "imagine" alone is an image model).
	// sora (OpenAI), veo (Google) and any "-video" id are video generators.
	case strings.Contains(n, "video"), strings.Contains(n, "sora"), hasNameToken(n, "veo"):
		return []string{ModalityVideo}
	case strings.Contains(n, "dall-e"), strings.Contains(n, "image"), strings.Contains(n, "imagine"),
		strings.Contains(n, "flux"), strings.Contains(n, "diffusion"):
		return []string{ModalityImage}
	case strings.Contains(n, "moderation"):
		return []string{ModalityModeration}
	default:
		return []string{ModalityChat}
	}
}

// hasNameToken reports whether token appears in a lower-cased model id as a
// whole dash/dot/underscore-delimited segment (or a prefix of one, so "veo"
// matches "veo-3.1" and "veo3" but not "verbose").
func hasNameToken(name, token string) bool {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == '/' || r == ':' }) {
		if part == token {
			return true
		}
		if strings.HasPrefix(part, token) && len(part) > len(token) && part[len(token)] >= '0' && part[len(token)] <= '9' {
			return true
		}
	}
	return false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
