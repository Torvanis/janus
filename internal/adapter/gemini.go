package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func init() { Register(&Gemini{openai: &OpenAICompatible{name: "gemini"}}) }

// GeminiAPIBase is the Gemini API's OpenAI-compatible endpoint, the base URL
// Google documents for OpenAI clients (no /v1 segment: Janus's /v1/... request
// paths are appended without it, see joinURL).
const GeminiAPIBase = "https://generativelanguage.googleapis.com/v1beta/openai"

// Gemini talks to the Gemini API (Google AI Studio keys) through Google's
// OpenAI-compatible endpoint, so requests, streaming and responses are the
// OpenAI wire format end to end. What differs, and is handled here:
//
//   - The base URL carries its own version path (/v1beta/openai). A bare
//     https://generativelanguage.googleapis.com is completed to it.
//   - The OpenAI-style model list names models "models/gemini-2.5-flash" and
//     says nothing about context windows or which methods a model serves. The
//     native list (same key, x-goog-api-key header) has inputTokenLimit and
//     supportedGenerationMethods, so discovery reads both and drops models the
//     OpenAI endpoint cannot call (Live/bidi-only models, the AQA endpoint).
//   - completion_tokens leaves out thinking tokens while total_tokens includes
//     them, and Google bills thinking as output, so output is total − prompt.
//
// Google Vertex AI (service-account keys, regional endpoints) is the separate
// "vertex" adapter.
type Gemini struct{ openai *OpenAICompatible }

// Type returns the stored adapter enum value.
func (a *Gemini) Type() string { return "gemini" }

// geminiBase completes a bare Gemini API host to the OpenAI-compatible base.
// Any other URL (a proxy, a regional or test endpoint) is used as entered.
func geminiBase(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	if strings.EqualFold(u.Hostname(), "generativelanguage.googleapis.com") && strings.Trim(u.Path, "/") == "" {
		return GeminiAPIBase
	}
	return strings.TrimRight(raw, "/")
}

// geminiNativeModelsURL is the native model list on the same host as base:
// the scheme and host of base plus /v1beta/models.
func geminiNativeModelsURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/v1beta/models?pageSize=1000"
}

// geminiCallable reports whether a model serves at least one method the
// OpenAI-compatible endpoint maps onto (chat/images/TTS via generateContent,
// embeddings via embedContent, video via predictLongRunning).
func geminiCallable(methods []string) bool {
	for _, m := range methods {
		switch m {
		case "generateContent", "embedContent", "predictLongRunning":
			return true
		}
	}
	return false
}

// Discover lists models from the OpenAI-compatible endpoint and enriches them
// from the native list. A failing native list is not fatal: the models are
// still returned, without context windows, and a warning says why.
func (a *Gemini) Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error) {
	up.BaseURL = geminiBase(up.BaseURL)
	models, err := a.openai.Discover(ctx, up, client)
	if err != nil {
		return nil, err
	}
	type nativeModel struct {
		Name             string   `json:"name"`
		InputTokenLimit  int64    `json:"inputTokenLimit"`
		SupportedMethods []string `json:"supportedGenerationMethods"`
	}
	native := map[string]nativeModel{}
	nativeErr := ""
	if endpoint := geminiNativeModelsURL(up.BaseURL); endpoint != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			req.Header.Set("x-goog-api-key", up.APIKey)
			var body []byte
			if body, err = doDiscovery(client, req); err == nil {
				var payload struct {
					Models []nativeModel `json:"models"`
				}
				if err = json.Unmarshal(body, &payload); err == nil {
					for _, m := range payload.Models {
						native[strings.TrimPrefix(m.Name, "models/")] = m
					}
				}
			}
		}
		if err != nil {
			nativeErr = fmt.Sprintf("Gemini model details unavailable: %v", err)
		}
	}
	out := make([]DiscoveredModel, 0, len(models))
	for _, m := range models {
		m.Name = strings.TrimPrefix(m.Name, "models/")
		if n, ok := native[m.Name]; ok {
			if !geminiCallable(n.SupportedMethods) {
				continue
			}
			if m.ContextWindow == 0 && n.InputTokenLimit > 0 {
				m.ContextWindow = n.InputTokenLimit
			}
			m.Modalities = geminiModalities(m.Name, n.SupportedMethods)
		} else {
			m.Modalities = geminiModalities(m.Name, nil)
		}
		if nativeErr != "" {
			m.MetadataWarnings = append(m.MetadataWarnings, nativeErr)
		}
		out = append(out, m)
	}
	return out, nil
}

// geminiModalities refines the name-based guess with the methods a model
// serves: an embedContent model is an embedding model, predictLongRunning is
// Veo video generation; Lyria music and transcription models are audio.
func geminiModalities(name string, methods []string) []string {
	for _, m := range methods {
		switch m {
		case "embedContent":
			return []string{ModalityEmbedding}
		case "predictLongRunning":
			return []string{ModalityVideo}
		}
	}
	n := strings.ToLower(name)
	if strings.HasPrefix(n, "lyria") {
		return []string{ModalityAudio}
	}
	return guessModalities(name)
}

// Prepare targets the OpenAI-compatible endpoint (bare host completed) and
// authenticates with the API key as a bearer token, as Google documents.
func (a *Gemini) Prepare(up Upstream, req *Request) (*Prepared, error) {
	up.BaseURL = geminiBase(up.BaseURL)
	return a.openai.Prepare(up, req)
}

// ExtractUsage reads OpenAI-style usage and adds thinking tokens to output.
func (a *Gemini) ExtractUsage(body []byte) (Usage, bool) {
	u, ok := parseOpenAIUsage(body)
	return withGeminiThinking(u, body), ok
}

// NewStreamCollector folds streamed usage, adding thinking tokens to output.
func (a *Gemini) NewStreamCollector() StreamCollector { return &geminiStreamCollector{} }

// TransformResponse passes OpenAI-shaped bodies through unchanged.
func (a *Gemini) TransformResponse(req *Request, body []byte) ([]byte, error) {
	return a.openai.TransformResponse(req, body)
}

// NewStreamTransformer passes OpenAI-shaped SSE through unchanged.
func (a *Gemini) NewStreamTransformer(req *Request, h http.Header) StreamTransformer {
	return a.openai.NewStreamTransformer(req, h)
}

// withGeminiThinking makes output tokens what Google bills as output.
// Gemini's completion_tokens excludes thinking ("thoughts") tokens, which are
// counted in total_tokens and billed at the output rate, so the billed output
// is total_tokens − prompt_tokens whenever that is larger.
func withGeminiThinking(u Usage, body []byte) Usage {
	var env struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &env) != nil {
		return u
	}
	if billed := env.Usage.TotalTokens - env.Usage.PromptTokens; billed > u.TokensOut && env.Usage.PromptTokens > 0 {
		u.TokensOut = billed
		u.Reported = true
		u.HiddenReasoning = true
	}
	return u
}

type geminiStreamCollector struct{ usage Usage }

func (c *geminiStreamCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	frame, _ := parseOpenAIUsage(payload)
	c.usage.MergeStream(withGeminiThinking(frame, payload))
}

func (c *geminiStreamCollector) Usage() Usage { return c.usage }
