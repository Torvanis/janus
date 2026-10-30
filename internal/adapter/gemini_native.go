package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Gemini built-in tools through the native API.
//
// Google's OpenAI-compatible endpoint refuses every built-in tool (live:
// "Unknown name google_search at tools[0]", the same for extra_body and
// web_search_options). A chat completion that asks for one of them —
// {"type":"web_search"} → google_search, {"type":"web_fetch"} → url_context,
// {"type":"code_execution"} → code_execution — is therefore sent to the
// native :streamGenerateContent / :generateContent endpoint, translated
// both ways here. Requests without built-in tools keep the compatible
// endpoint unchanged.
//
// The reply comes back as an OpenAI chat completion with the same Janus
// extensions the other providers use: janus_activity (searches, page reads,
// code runs) and url_citation annotations from groundingMetadata.

var geminiBuiltins = map[string]string{
	"web_search":     "google_search",
	"web_fetch":      "url_context",
	"code_execution": "code_execution",
}

// GeminiHostedTools are the generic hosted tools Gemini runs.
func GeminiHostedTools() map[string]bool {
	return map[string]bool{"web_search": true, "web_fetch": true, "code_execution": true}
}

// Google bills grounded search per query on Gemini 3+: $14 per 1,000 after a
// monthly free allowance Janus cannot see, so the list price is metered.
const geminiSearchFeeNano = 14_000_000

// geminiNativeRequest reports a chat completion that must use the native
// API (it asks for a built-in tool).
func geminiNativeRequest(req *Request) bool {
	if isChatCompletions(req) && geminiMusicModel(req.Model) {
		// Lyria answers with audio, which Google's OpenAI-compatible
		// endpoint cannot return (400 "Unhandled generated data mime type:
		// audio/mpeg"); the native API returns it as inlineData.
		return true
	}
	if isChatCompletions(req) && (bytes.Contains(req.Body, []byte(`"video_url"`)) || bytes.Contains(req.Body, []byte(`"input_audio"`))) &&
		geminiMediaParts(req.Body) {
		// Sound and video parts: the OpenAI-compatible endpoint has no
		// video part, so both go to the native API as inlineData.
		return true
	}
	if !isChatCompletions(req) || !bytes.Contains(req.Body, []byte(`"tools"`)) {
		return false
	}
	for _, t := range HostedTools(req.Body) {
		if _, ok := geminiBuiltins[t]; ok {
			return true
		}
	}
	return false
}

// geminiNativeURL is base's host + /v1beta/models/<model>:<method>.
// geminiMusicModel reports Google's Lyria music models (not the realtime one).
func geminiMusicModel(model string) bool {
	n := strings.ToLower(model)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.HasPrefix(n, "lyria") && !strings.Contains(n, "realtime")
}

func geminiNativeURL(base, model string, stream bool) string {
	u, err := url.Parse(geminiBase(base))
	if err != nil || u.Host == "" {
		return ""
	}
	method := "generateContent"
	q := ""
	if stream {
		method, q = "streamGenerateContent", "?alt=sse"
	}
	return u.Scheme + "://" + u.Host + "/v1beta/models/" + url.PathEscape(model) + ":" + method + q
}

// chatToGeminiNative translates a chat completion body into a native
// generateContent body.
func chatToGeminiNative(body []byte) ([]byte, error) {
	var in struct {
		Messages []struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
			ToolCallID string            `json:"tool_call_id"`
			Name       string            `json:"name"`
		} `json:"messages"`
		Tools           []json.RawMessage `json:"tools"`
		Temperature     *float64          `json:"temperature"`
		TopP            *float64          `json:"top_p"`
		MaxTokens       *int64            `json:"max_tokens"`
		MaxCompletion   *int64            `json:"max_completion_tokens"`
		Stop            json.RawMessage   `json:"stop"`
		ReasoningEffort string            `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat request: %w", err)
	}
	var system []string
	var contents []map[string]any
	callNames := map[string]string{}
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			if t := chatText(m.Content); t != "" {
				system = append(system, t)
			}
		case "user", "assistant":
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			parts := geminiParts(m.Content)
			for _, raw := range m.ToolCalls {
				var tc struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if json.Unmarshal(raw, &tc) != nil {
					continue
				}
				callNames[tc.ID] = tc.Function.Name
				var args any = map[string]any{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": tc.Function.Name, "args": args},
					"thoughtSignature": geminiSignatureFor(tc.ID)})
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": role, "parts": parts})
			}
		case "tool":
			name := callNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{
				"functionResponse": map[string]any{"name": name, "response": map[string]any{"result": chatText(m.Content)}}}}})
		}
	}
	out := map[string]any{"contents": contents}
	if len(system) > 0 {
		out["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": strings.Join(system, "\n\n")}}}
	}
	var tools []any
	var funcs []any
	// Google refuses sound and video next to code execution (400 "The
	// mime type: audio/mp3 is not supported for code execution"), so a
	// request carrying either goes without that one tool.
	media := geminiMediaParts(body)
	for _, raw := range in.Tools {
		var t struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if native, ok := geminiBuiltins[genericHostedType(t.Type)]; ok {
			if media && native == "code_execution" {
				continue
			}
			tools = append(tools, map[string]any{native: map[string]any{}})
			continue
		}
		if t.Type == "function" && t.Function.Name != "" {
			f := map[string]any{"name": t.Function.Name, "description": t.Function.Description}
			if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
				f["parametersJsonSchema"] = t.Function.Parameters
			}
			funcs = append(funcs, f)
		}
	}
	if len(funcs) > 0 {
		tools = append(tools, map[string]any{"functionDeclarations": funcs})
		if len(tools) > 1 {
			// Built-in tools alongside functions: Google requires this flag
			// (400 without it).
			out["toolConfig"] = map[string]any{"includeServerSideToolInvocations": true}
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	gen := map[string]any{}
	if in.Temperature != nil {
		gen["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		gen["topP"] = *in.TopP
	}
	if in.MaxCompletion != nil {
		gen["maxOutputTokens"] = *in.MaxCompletion
	} else if in.MaxTokens != nil {
		gen["maxOutputTokens"] = *in.MaxTokens
	}
	if stops := stopSequences(in.Stop); len(stops) > 0 {
		gen["stopSequences"] = stops
	}
	switch in.ReasoningEffort {
	case "minimal", "low", "medium", "high":
		gen["thinkingConfig"] = map[string]any{"thinkingLevel": in.ReasoningEffort, "includeThoughts": true}
	default:
		gen["thinkingConfig"] = map[string]any{"includeThoughts": true}
	}
	out["generationConfig"] = gen
	return json.Marshal(out)
}

// chatText is a message content's text (string or text parts).
func chatText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// geminiParts converts chat content (text, data-URL images) to parts.
func geminiParts(raw json.RawMessage) []any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"text": s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
		VideoURL struct {
			URL string `json:"url"`
		} `json:"video_url"`
		InputAudio struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, map[string]any{"text": p.Text})
			}
		case "image_url", "video_url":
			u := p.ImageURL.URL
			if p.Type == "video_url" {
				u = p.VideoURL.URL
			}
			if mime, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ";base64,"); ok && strings.HasPrefix(u, "data:") {
				out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}})
			}
		case "input_audio":
			if p.InputAudio.Data != "" {
				out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": geminiAudioMIME(p.InputAudio.Format), "data": p.InputAudio.Data}})
			}
		}
	}
	return out
}

// geminiAudioMIME is the MIME type of an input_audio format.
func geminiAudioMIME(format string) string {
	switch strings.ToLower(format) {
	case "mp3":
		return "audio/mp3"
	case "wav":
		return "audio/wav"
	case "m4a", "mp4", "aac":
		return "audio/aac"
	case "ogg", "opus":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	}
	return "audio/" + format
}

// geminiMediaParts reports a chat request whose messages carry a sound or
// video part (not just the words in some text).
func geminiMediaParts(body []byte) bool {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "video_url" || p.Type == "input_audio" {
				return true
			}
		}
	}
	return false
}

func geminiSignatureFor(id string) string {
	if sig, ok := geminiSignatures.Get(id); ok {
		return sig
	}
	return geminiSignatureSkip
}

// --- response ---------------------------------------------------------

type geminiNativeResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
				FunctionCall     *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
				ExecutableCode *struct {
					Language string `json:"language"`
					Code     string `json:"code"`
				} `json:"executableCode"`
				CodeExecutionResult *struct {
					Outcome string `json:"outcome"`
					Output  string `json:"output"`
				} `json:"codeExecutionResult"`
				InlineData *struct {
					MIMEType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason       string           `json:"finishReason"`
		GroundingMetadata  *geminiGrounding `json:"groundingMetadata"`
		URLContextMetadata *struct {
			URLMetadata []struct {
				RetrievedURL string `json:"retrievedUrl"`
				Status       string `json:"urlRetrievalStatus"`
			} `json:"urlMetadata"`
		} `json:"urlContextMetadata"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string          `json:"modelVersion"`
	ResponseID   string          `json:"responseId"`
	Error        json.RawMessage `json:"error"`
}

type geminiGrounding struct {
	WebSearchQueries []string `json:"webSearchQueries"`
	GroundingChunks  []struct {
		Web *struct {
			URI   string `json:"uri"`
			Title string `json:"title"`
		} `json:"web"`
	} `json:"groundingChunks"`
}

// geminiFinish maps native finish reasons.
func geminiFinish(r string) string {
	switch r {
	case "":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	}
	return "stop"
}

// geminiNativeDeltas renders one native response (or stream chunk) as chat
// deltas: text, reasoning, tool calls, activity, annotations.
func geminiNativeDeltas(r *geminiNativeResponse, toolIndex *int) (deltas []map[string]any, finish string) {
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				id := fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), *toolIndex)
				if p.ThoughtSignature != "" {
					geminiSignatures.Add(id, p.ThoughtSignature)
				}
				args := "{}"
				if len(p.FunctionCall.Args) > 0 {
					args = string(p.FunctionCall.Args)
				}
				deltas = append(deltas, map[string]any{"tool_calls": []any{map[string]any{"index": *toolIndex, "id": id, "type": "function",
					"function": map[string]any{"name": p.FunctionCall.Name, "arguments": args}}}})
				*toolIndex++
			case p.ExecutableCode != nil:
				d := activityDelta("code_execution", "searching", nil)
				d["janus_activity"].(map[string]any)["code"] = clip(p.ExecutableCode.Code)
				deltas = append(deltas, d)
			case p.CodeExecutionResult != nil:
				d := activityDelta("code_execution", "done", nil)
				if o := strings.TrimSpace(p.CodeExecutionResult.Output); o != "" {
					d["janus_activity"].(map[string]any)["output"] = clip(o)
				}
				deltas = append(deltas, d)
			case p.InlineData != nil && strings.HasPrefix(p.InlineData.MIMEType, "audio/"):
				// Generated audio (Lyria): OpenAI's message.audio shape.
				format := "mp3"
				if strings.Contains(p.InlineData.MIMEType, "wav") {
					format = "wav"
				}
				deltas = append(deltas, map[string]any{"audio": map[string]any{"data": p.InlineData.Data, "format": format}})
			case p.Thought && p.Text != "":
				deltas = append(deltas, map[string]any{"reasoning_content": p.Text})
			case p.Text != "":
				deltas = append(deltas, map[string]any{"content": p.Text})
			}
		}
		if g := c.GroundingMetadata; g != nil {
			if len(g.WebSearchQueries) > 0 {
				a := &searchAction{Queries: g.WebSearchQueries}
				for _, ch := range g.GroundingChunks {
					if ch.Web != nil && ch.Web.URI != "" {
						a.Sources = append(a.Sources, struct {
							URL string `json:"url"`
						}{ch.Web.URI})
					}
				}
				deltas = append(deltas, activityDelta("web_search", "done", a))
			}
			var anns []any
			for _, ch := range g.GroundingChunks {
				if ch.Web != nil && ch.Web.URI != "" {
					anns = append(anns, map[string]any{"type": "url_citation", "url_citation": map[string]any{
						"url": ch.Web.URI, "title": ch.Web.Title, "start_index": 0, "end_index": 0}})
				}
			}
			if len(anns) > 0 {
				deltas = append(deltas, map[string]any{"annotations": anns})
			}
		}
		if u := c.URLContextMetadata; u != nil {
			for _, m := range u.URLMetadata {
				if m.RetrievedURL != "" {
					a := &searchAction{Query: m.RetrievedURL}
					deltas = append(deltas, activityDelta("web_fetch", "done", a))
				}
			}
		}
		if f := geminiFinish(c.FinishReason); f != "" {
			finish = f
		}
	}
	return deltas, finish
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

// geminiNativeUsage is usage from a native response: thinking billed as
// output, plus Google's per-query search fee.
func geminiNativeUsage(r *geminiNativeResponse) (Usage, bool) {
	if r.UsageMetadata == nil {
		return Usage{}, false
	}
	m := r.UsageMetadata
	u := Usage{
		TokensIn:     m.PromptTokenCount + m.ToolUsePromptTokenCount,
		TokensOut:    m.CandidatesTokenCount + m.ThoughtsTokenCount,
		TokensCached: m.CachedContentTokenCount,
		Reported:     m.PromptTokenCount > 0 || m.CandidatesTokenCount > 0,
	}
	if m.ThoughtsTokenCount > 0 {
		u.HiddenReasoning = true
	}
	var queries int64
	for _, c := range r.Candidates {
		if c.GroundingMetadata != nil {
			for _, q := range c.GroundingMetadata.WebSearchQueries {
				if strings.TrimSpace(q) != "" {
					queries++
				}
			}
		}
		if c.FinishReason != "" {
			u.FinishReason = c.FinishReason
		}
	}
	if queries > 0 {
		u.ToolCalls = map[string]int64{"web_search": queries}
		u.ToolFeeNano = queries * geminiSearchFeeNano
	}
	return u, u.Reported
}

// geminiNativeCompletion renders a whole native response as a chat
// completion.
func geminiNativeCompletion(body []byte, model string) ([]byte, error) {
	var r geminiNativeResponse
	if err := json.Unmarshal(body, &r); err != nil || len(r.Error) > 0 {
		return body, nil
	}
	idx := 0
	deltas, finish := geminiNativeDeltas(&r, &idx)
	msg := map[string]any{"role": "assistant"}
	var text, reasoning strings.Builder
	var calls, anns, acts []any
	for _, d := range deltas {
		if v, ok := d["content"].(string); ok {
			text.WriteString(v)
		}
		if v, ok := d["reasoning_content"].(string); ok {
			reasoning.WriteString(v)
		}
		if v, ok := d["tool_calls"].([]any); ok {
			for _, c := range v {
				cm := c.(map[string]any)
				delete(cm, "index")
				calls = append(calls, cm)
			}
		}
		if v, ok := d["annotations"].([]any); ok {
			anns = append(anns, v...)
		}
		if v, ok := d["janus_activity"]; ok {
			acts = append(acts, v)
		}
		if v, ok := d["audio"]; ok {
			msg["audio"] = v
		}
	}
	msg["content"] = text.String()
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
		finish = "tool_calls"
	}
	if len(anns) > 0 {
		msg["annotations"] = anns
	}
	if len(acts) > 0 {
		msg["janus_activities"] = acts
	}
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{"id": firstNonEmptyStr(r.ResponseID, "chatcmpl-gemini"), "object": "chat.completion", "created": time.Now().Unix(),
		"model": firstNonEmptyStr(r.ModelVersion, model), "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}}
	if u, ok := geminiNativeUsage(&r); ok {
		out["usage"] = map[string]any{"prompt_tokens": u.TokensIn, "completion_tokens": u.TokensOut, "total_tokens": u.TokensIn + u.TokensOut}
	}
	return json.Marshal(out)
}

// geminiNativeStream translates native SSE chunks into chat chunks.
type geminiNativeStream struct {
	model   string
	id      string
	created int64
	tools   int
	finish  string
	usage   *Usage
	started bool
	done    atomic.Bool
	// raw: the upstream answered with a non-SSE (error) body.
	raw bool
}

func (t *geminiNativeStream) chunk(delta map[string]any, finish *string, usage map[string]any) []byte {
	ch := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		ch["finish_reason"] = *finish
	}
	out := map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model, "choices": []any{ch}}
	if usage != nil {
		out["choices"] = []any{}
		out["usage"] = usage
	}
	b, _ := json.Marshal(out)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func (t *geminiNativeStream) TransformStreamLine(line []byte) ([]byte, error) {
	payload, ok := sseData(line)
	if !ok {
		if len(bytes.TrimSpace(line)) == 0 {
			if t.raw {
				return line, nil
			}
			return nil, nil
		}
		// Not SSE: an error body answering the stream (Google pretty-
		// prints it over many lines). Relay it unchanged, and do not close
		// it as if it were a finished stream.
		t.raw = true
		return line, nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return t.end(), nil
	}
	var r geminiNativeResponse
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("parse Gemini stream chunk: %w", err)
	}
	if len(r.Error) > 0 {
		return append(append([]byte(`data: {"error":`), r.Error...), '}', '\n', '\n'), nil
	}
	var out []byte
	if !t.started {
		t.started = true
		t.id = firstNonEmptyStr(r.ResponseID, fmt.Sprintf("chatcmpl-gemini-%d", time.Now().UnixNano()))
		t.created = time.Now().Unix()
		if r.ModelVersion != "" {
			t.model = r.ModelVersion
		}
		out = append(out, t.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)...)
	}
	deltas, finish := geminiNativeDeltas(&r, &t.tools)
	for _, d := range deltas {
		out = append(out, t.chunk(d, nil, nil)...)
	}
	if finish != "" {
		t.finish = finish
	}
	if u, ok := geminiNativeUsage(&r); ok {
		t.usage = &u
	}
	return out, nil
}

// end emits the finish, usage and [DONE] once (Gemini's SSE has no [DONE]:
// the proxy calls Flush at end of stream).
func (t *geminiNativeStream) end() []byte {
	if t.done.Swap(true) || t.raw || !t.started {
		return nil
	}
	finish := t.finish
	if t.tools > 0 {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	out := t.chunk(map[string]any{}, &finish, nil)
	if t.usage != nil {
		out = append(out, t.chunk(nil, nil, map[string]any{"prompt_tokens": t.usage.TokensIn, "completion_tokens": t.usage.TokensOut,
			"total_tokens": t.usage.TokensIn + t.usage.TokensOut})...)
	}
	return append(out, []byte("data: [DONE]\n\n")...)
}

// FinishStream ends the stream when the upstream closes (no [DONE]).
func (t *geminiNativeStream) FinishStream() []byte { return t.end() }

// geminiNativeCollector meters native stream chunks: token counts from
// the latest usageMetadata; search queries summed over the chunks that
// carry grounding metadata (each query is billed once).
type geminiNativeCollector struct {
	usage   Usage
	queries map[string]bool
}

func (c *geminiNativeCollector) Feed(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	var r geminiNativeResponse
	if json.Unmarshal(payload, &r) != nil {
		return
	}
	if r.UsageMetadata != nil {
		if u, ok := geminiNativeUsage(&geminiNativeResponse{UsageMetadata: r.UsageMetadata}); ok {
			c.usage.MergeStream(u)
		}
	}
	for _, cand := range r.Candidates {
		if cand.FinishReason != "" {
			c.usage.FinishReason = cand.FinishReason
		}
		if g := cand.GroundingMetadata; g != nil {
			for _, q := range g.WebSearchQueries {
				if q = strings.TrimSpace(q); q != "" {
					if c.queries == nil {
						c.queries = map[string]bool{}
					}
					c.queries[q] = true
				}
			}
		}
	}
	if n := int64(len(c.queries)); n > 0 {
		c.usage.ToolCalls = map[string]int64{"web_search": n}
		c.usage.ToolFeeNano = n * geminiSearchFeeNano
	}
}

func (c *geminiNativeCollector) Usage() Usage { return c.usage }

// geminiNativeHeaders authenticates a native request.
func geminiNativeHeaders(src http.Header, key string) http.Header {
	h := cloneHeader(src)
	h.Del("Authorization")
	if key != "" {
		h.Set("x-goog-api-key", key)
	}
	h.Set("Content-Type", "application/json")
	return h
}
