package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// OpenAIResponses serves a chat completion through OpenAI's Responses API.
//
// GPT-5 and GPT-6 refuse function tools on /v1/chat/completions whenever a
// reasoning effort applies ("Function tools with reasoning_effort are not
// supported … use /v1/responses or set reasoning_effort to 'none'"), and
// some (gpt-6.1-sol, gpt-6-astra) refuse "none" too, so on chat completions
// they cannot call tools at all. The proxy switches such a request to this
// adapter after the refusal (and goes straight to it for that model from
// then on): the chat request is translated to a Responses request, and the
// Responses event stream back into chat completion chunks (or one
// chat.completion for a caller that did not stream), with the same
// translator the ChatGPT-plan surface uses.
//
// It is not registered as an upstream type: it is chosen per request, for
// an "openai_compatible" upstream.
type OpenAIResponses struct{ codex Codex }

// ResponsesFallback returns the adapter.
func ResponsesFallback() *OpenAIResponses { return &OpenAIResponses{} }

// Type implements Adapter.
func (a *OpenAIResponses) Type() string { return "openai_responses" }

// Discover implements Adapter: never used for discovery.
func (a *OpenAIResponses) Discover(context.Context, Upstream, *http.Client) ([]DiscoveredModel, error) {
	return nil, errors.New("the Responses fallback is chosen per request, not discovered")
}

// BuffersEventStream: the request always streams upstream, so a caller
// that did not stream gets the stream folded into one body.
func (a *OpenAIResponses) BuffersEventStream(req *Request) bool { return req != nil && !req.Streaming }

// Prepare translates a chat completion into a Responses request.
func (a *OpenAIResponses) Prepare(up Upstream, req *Request) (*Prepared, error) {
	if !isChatCompletions(req) {
		return nil, errors.New("the Responses fallback serves chat completions only")
	}
	header := cloneHeader(req.Header)
	if up.APIKey != "" {
		header.Set("Authorization", "Bearer "+up.APIKey)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	body, err := chatToPublicResponses(req.Body, req.Model)
	if err != nil {
		return nil, err
	}
	return &Prepared{URL: joinURL(up.BaseURL, "/v1/responses"), Header: header, Body: body}, nil
}

// chatToPublicResponses is chatToResponses for the public API: the
// ChatGPT-plan defaults (a placeholder instructions string, no output cap,
// a reasoning summary) do not apply, and the caller's output cap and
// sampling travel as Responses fields.
func chatToPublicResponses(body []byte, model string) ([]byte, error) {
	out, _, err := chatToResponses(body, model)
	if err != nil {
		return nil, err
	}
	var in struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		MaxCompletionTokens *int     `json:"max_completion_tokens"`
		MaxTokens           *int     `json:"max_tokens"`
		Temperature         *float64 `json:"temperature"`
		TopP                *float64 `json:"top_p"`
		ServiceTier         string   `json:"service_tier"`
	}
	_ = json.Unmarshal(body, &in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	hasSystem := false
	for _, msg := range in.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			hasSystem = true
		}
	}
	if !hasSystem {
		delete(m, "instructions")
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		// A summary needs a verified organisation on the public API.
		delete(r, "summary")
	}
	switch {
	case in.MaxCompletionTokens != nil:
		m["max_output_tokens"] = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		m["max_output_tokens"] = *in.MaxTokens
	}
	if in.Temperature != nil {
		m["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		m["top_p"] = *in.TopP
	}
	if s := strings.TrimSpace(in.ServiceTier); s != "" {
		m["service_tier"] = s
	}
	return json.Marshal(m)
}

// ExtractUsage implements Adapter.
func (a *OpenAIResponses) ExtractUsage(body []byte) (Usage, bool) { return a.codex.ExtractUsage(body) }

// NewStreamCollector implements Adapter.
func (a *OpenAIResponses) NewStreamCollector() StreamCollector { return a.codex.NewStreamCollector() }

// TransformResponse implements Adapter.
func (a *OpenAIResponses) TransformResponse(req *Request, body []byte) ([]byte, error) {
	return a.codex.TransformResponse(req, body)
}

// NewStreamTransformer implements Adapter.
func (a *OpenAIResponses) NewStreamTransformer(req *Request, h http.Header) StreamTransformer {
	return a.codex.NewStreamTransformer(req, h)
}
