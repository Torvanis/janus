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

func init() { Register(&Ollama{}) }

// Ollama exposes both a native API and an OpenAI-compatible shim. Discovery
// uses the native /api/tags endpoint because it carries richer metadata; proxy
// traffic uses the OpenAI shim so downstream clients need no changes.
type Ollama struct{}

// Type returns the stored adapter enum value.
func (a *Ollama) Type() string { return "ollama" }

// Discover lists locally pulled models.
func (a *Ollama) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(up.BaseURL, "/api/tags"), nil)
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
		Models []struct {
			Name    string `json:"name"`
			Model   string `json:"model"`
			Details struct {
				Family string `json:"family"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse Ollama tag list: %w", err)
	}
	out := make([]DiscoveredModel, 0, len(payload.Models))
	for _, m := range payload.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if name != "" {
			window, caps := a.show(ctx, up, client, name)
			out = append(out, DiscoveredModel{
				Name:          name,
				Modalities:    guessModalities(name),
				ContextWindow: window,
				Capabilities:  caps,
			})
		}
	}
	return out, nil
}

// show asks /api/show for one model's context length and capabilities.
// Ollama's tag list carries neither, so this is a second round trip per
// model, bounded by a short timeout and best-effort: any failure returns
// "unknown" rather than failing the whole discovery run.
//
// The context key is architecture-prefixed (e.g. "qwen35.context_length"),
// and the architecture is not knowable up front, so the first
// *.context_length entry wins. "capabilities" is a list such as
// ["completion", "vision", "tools", "thinking"]; servers too old to send it
// leave the capabilities unknown.
func (a *Ollama) show(ctx context.Context, up Upstream, client *http.Client, model string) (int64, Capabilities) {
	showCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(showCtx, http.MethodPost, joinURL(up.BaseURL, "/api/show"), bytes.NewReader(payload))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	body, err := doDiscovery(client, req)
	if err != nil {
		return 0, nil
	}
	var shown struct {
		ModelInfo    map[string]json.RawMessage `json:"model_info"`
		Capabilities []string                   `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &shown); err != nil {
		return 0, nil
	}
	var window int64
	for key, raw := range shown.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n int64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			window = n
			break
		}
	}
	return window, ollamaCapabilities(shown.Capabilities)
}

func ollamaCapabilities(list []string) Capabilities {
	if len(list) == 0 {
		return nil
	}
	has := func(name string) bool {
		for _, v := range list {
			if strings.EqualFold(v, name) {
				return true
			}
		}
		return false
	}
	c := Capabilities{CapInText: true, CapInImage: has("vision"), CapTools: has("tools"), CapThinking: has("thinking")}
	if has("embedding") {
		c[CapOutEmbedding] = true
	}
	if has("completion") {
		c[CapOutText] = true
	}
	// How its reasoning is turned off is not recorded here: /api/show does
	// not say, and think: false only works on Ollama's native API, not on the
	// OpenAI-compatible one Janus calls. The probe finds the switch that works.
	return c
}

// Prepare forwards through the OpenAI-compatible shim.
func (a *Ollama) Prepare(up Upstream, req *Request) (*Prepared, error) {
	header := cloneHeader(req.Header)
	if up.APIKey != "" {
		header.Set("Authorization", "Bearer "+up.APIKey)
	}
	body := req.Body
	if req.Streaming && isJSON(header) && streamUsagePath(req.Path) {
		body = ensureStreamUsage(body)
	}
	return &Prepared{URL: joinURL(up.BaseURL, req.Path), Header: header, Body: body}, nil
}

// ExtractUsage understands both the OpenAI shim shape and Ollama's native
// prompt_eval_count / eval_count fields.
func (a *Ollama) ExtractUsage(body []byte) (Usage, bool) {
	if u, ok := parseOpenAIUsage(body); ok {
		return u, true
	}
	var native struct {
		PromptEvalCount    int64  `json:"prompt_eval_count"`
		PromptEvalDuration int64  `json:"prompt_eval_duration"`
		EvalCount          int64  `json:"eval_count"`
		EvalDuration       int64  `json:"eval_duration"`
		DoneReason         string `json:"done_reason"`
	}
	if err := json.Unmarshal(body, &native); err != nil {
		return Usage{}, false
	}
	u := Usage{TokensIn: native.PromptEvalCount, TokensOut: native.EvalCount, FinishReason: native.DoneReason}
	u.Reported = u.TokensIn > 0 || u.TokensOut > 0
	// Ollama times each phase in nanoseconds, which is a measured throughput
	// the proxy can forward as reported rather than recomputing from its own
	// (network-inclusive) clock.
	if native.PromptEvalDuration > 0 || native.EvalDuration > 0 {
		u.TokensInPerSecond = ratePerSecond(native.PromptEvalCount, float64(native.PromptEvalDuration)/1e9)
		u.TokensOutPerSecond = ratePerSecond(native.EvalCount, float64(native.EvalDuration)/1e9)
		u.ThroughputReported = true
	}
	return u, u.Reported
}

// NewStreamCollector accumulates counts from either stream shape.
func (a *Ollama) NewStreamCollector() StreamCollector { return &ollamaStreamCollector{adapter: a} }

// TransformResponse is a pass-through: proxy traffic uses Ollama's OpenAI
// shim, so responses already arrive in the OpenAI shape.
func (a *Ollama) TransformResponse(req *Request, body []byte) ([]byte, error) {
	return PassthroughResponse(req, body)
}

// NewStreamTransformer is a pass-through for the same reason.
func (a *Ollama) NewStreamTransformer(*Request, http.Header) StreamTransformer {
	return PassthroughStream{}
}

type ollamaStreamCollector struct {
	adapter *Ollama
	usage   Usage
}

func (c *ollamaStreamCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	if u, found := c.adapter.ExtractUsage(payload); found {
		c.usage.MergeStream(u)
	}
}

func (c *ollamaStreamCollector) Usage() Usage { return c.usage }
