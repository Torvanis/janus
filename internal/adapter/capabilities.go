package adapter

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// Capabilities is what one provider said about what one model can take in,
// give back and be asked, under the keys below.
//
// Every provider answers these questions differently, and most answer some
// of them for free in their model list. The keys are shared with
// internal/capability, which layers what the provider said under what Janus
// measured for itself (a probe) and what an administrator set.
//
// A key that is absent means "the provider did not say". It never means no.
type Capabilities map[string]any

// Capability keys. The value type is fixed per key (see CapabilityKinds).
const (
	CapInText  = "input.text"
	CapInImage = "input.image"
	CapInAudio = "input.audio"
	CapInVideo = "input.video"
	CapInFile  = "input.file" // PDFs and other documents

	CapOutText      = "output.text"
	CapOutImage     = "output.image"
	CapOutAudio     = "output.audio"
	CapOutVideo     = "output.video"
	CapOutEmbedding = "output.embedding"
	CapOutRerank    = "output.rerank"

	// CapThinking: the model can reason before it answers.
	CapThinking = "thinking.supported"
	// CapThinkingDefault: it reasons when the request says nothing.
	CapThinkingDefault = "thinking.default_on"
	// CapThinkingOff: how reasoning is turned off (a ThinkingOff* value);
	// ThinkingOffNever when it cannot be.
	CapThinkingOff = "thinking.off"
	// CapThinkingLevels: every reasoning-effort value the model accepts.
	// An empty list on a reasoning model means it takes no effort setting.
	CapThinkingLevels = "thinking.levels"
	// CapThinkingDefaultLevel: the effort used when none is sent.
	CapThinkingDefaultLevel = "thinking.default_level"

	// CapOutputCap: the parameter that limits output length (an OutputCap*
	// value).
	CapOutputCap = "request.output_cap"
	// CapRefusedParams: request parameters the model refuses outright
	// (temperature, top_p, ...). They are left out of what is sent.
	CapRefusedParams = "request.refused_params"
	// CapTools: function tools are accepted.
	CapTools = "request.tools"
	// CapToolsEndpoint: where function tools work (a ToolsEndpoint* value).
	CapToolsEndpoint = "request.tools_endpoint"
	// CapStructured: a JSON schema response format is accepted.
	CapStructured = "request.structured_output"

	// CapMaxOutput: the most output tokens one request can ask for.
	CapMaxOutput = "limits.max_output_tokens"
)

// Values of CapThinkingOff.
const (
	ThinkingOffEffortNone     = "reasoning_effort_none"   // reasoning_effort: "none"
	ThinkingOffEnableThinking = "enable_thinking_false"   // chat_template_kwargs.enable_thinking: false
	ThinkingOffThink          = "think_false"             // think: false (Ollama)
	ThinkingOffDisabled       = "thinking_disabled"       // thinking: {type: "disabled"}
	ThinkingOffBudgetZero     = "thinking_budget_zero"    // Gemini 2.5 thinkingBudget: 0
	ThinkingOffReasoningFalse = "reasoning_enabled_false" // reasoning: {enabled: false}
	ThinkingOffNever          = "never"                   // cannot be turned off
)

// CarriesThinkingOff reports whether this adapter is known to carry a
// thinking-off switch to the provider as the provider's own API expects
// it, so a switch named only by the provider's model list can be used:
//   - anthropic: the Messages adapter passes a caller's `thinking` object
//     through untouched, so thinking {type: "disabled"} reaches Claude.
//   - gemini: the native adapter turns reasoning_effort "none" into
//     thinkingConfig {thinkingBudget: 0}.
//
// Any other pairing has not been shown to reach the model and must be
// measured first.
func CarriesThinkingOff(adapterType, off string) bool {
	switch adapterType {
	case "anthropic":
		return off == ThinkingOffDisabled
	case "gemini":
		return off == ThinkingOffBudgetZero
	}
	return false
}

// CarriesSoundAndVideo reports whether an adapter can put attached sound
// (an input_audio part) and video (a video_url part) into a chat request
// at all. Where it cannot, nothing about input.audio / input.video can be
// learned through it:
//   - anthropic and the ChatGPT-plan (codex) adapter refuse both parts
//     before sending;
//   - bedrock's Converse mapping flattens every part to text, so the model
//     would get the base64 as words.
//
// Every other adapter passes the parts through (gemini turns them into
// native inlineData), and the provider's answer is the evidence.
func CarriesSoundAndVideo(adapterType string) bool {
	switch adapterType {
	case "anthropic", CodexAdapterType, "bedrock", "tei":
		return false
	}
	return true
}

// Values of CapOutputCap.
const (
	OutputCapMaxTokens           = "max_tokens"
	OutputCapMaxCompletionTokens = "max_completion_tokens"
)

// Values of CapToolsEndpoint.
const (
	ToolsEndpointChat      = "chat_completions"
	ToolsEndpointResponses = "responses"
)

// Kinds of capability value.
const (
	CapKindBool   = "bool"
	CapKindString = "string"
	CapKindList   = "list"
	CapKindInt    = "int"
)

// CapabilityKey describes one key: its value type and, for strings, the
// values it may hold.
type CapabilityKey struct {
	Key  string   `json:"key"`
	Kind string   `json:"kind"`
	Enum []string `json:"enum,omitempty"`
}

// CapabilityKeys lists every key, in the order a person reads them.
func CapabilityKeys() []CapabilityKey {
	b := func(k string) CapabilityKey { return CapabilityKey{Key: k, Kind: CapKindBool} }
	return []CapabilityKey{
		b(CapInText), b(CapInImage), b(CapInAudio), b(CapInVideo), b(CapInFile),
		b(CapOutText), b(CapOutImage), b(CapOutAudio), b(CapOutVideo), b(CapOutEmbedding), b(CapOutRerank),
		b(CapThinking), b(CapThinkingDefault),
		{Key: CapThinkingOff, Kind: CapKindString, Enum: []string{ThinkingOffEffortNone, ThinkingOffEnableThinking, ThinkingOffThink, ThinkingOffDisabled, ThinkingOffBudgetZero, ThinkingOffReasoningFalse, ThinkingOffNever}},
		{Key: CapThinkingLevels, Kind: CapKindList},
		{Key: CapThinkingDefaultLevel, Kind: CapKindString},
		{Key: CapOutputCap, Kind: CapKindString, Enum: []string{OutputCapMaxTokens, OutputCapMaxCompletionTokens}},
		{Key: CapRefusedParams, Kind: CapKindList},
		b(CapTools),
		{Key: CapToolsEndpoint, Kind: CapKindString, Enum: []string{ToolsEndpointChat, ToolsEndpointResponses}},
		b(CapStructured),
		{Key: CapMaxOutput, Kind: CapKindInt},
	}
}

// CapabilityProvider names the provider behind an endpoint, for reading its
// model list. It is MetadataProvider plus the providers whose lists carry
// capabilities but no prices Janus reads. Identity comes from the configured
// host, never from a model name.
func CapabilityProvider(adapterType, baseURL string) string {
	if p := MetadataProvider(adapterType, baseURL); p != "" {
		return p
	}
	u, err := url.Parse(baseURL)
	if err != nil || adapterType != "openai_compatible" {
		return ""
	}
	switch host := strings.ToLower(u.Hostname()); {
	case host == "api.mistral.ai":
		return "mistral"
	case host == "api.deepseek.com":
		return "deepseek"
	case host == "api.moonshot.ai", host == "api.moonshot.cn":
		return "moonshot"
	case host == "api.cerebras.ai":
		return "cerebras"
	case host == "api.groq.com":
		return "groq"
	case host == "api.together.ai", host == "api.together.xyz":
		return "together"
	case host == "api.fireworks.ai":
		return "fireworks"
	}
	return ""
}

// RowCapabilities reads what one entry of a provider's model list says,
// for callers outside this package that fetch the list themselves (the
// personal-plan catalogs in internal/subscription). provider is the
// CapabilityProvider name the row's field names follow.
func RowCapabilities(provider string, row map[string]json.RawMessage) Capabilities {
	return rowCapabilities(provider, row)
}

// rowCapabilities reads what one entry of a model list says about the
// model. It knows the field names each provider uses; a field it does not
// find adds nothing, so one reader serves every OpenAI-shaped list,
// including self-hosted servers (llama.cpp reports modalities the same way
// OpenRouter does).
func rowCapabilities(provider string, row map[string]json.RawMessage) Capabilities {
	c := Capabilities{}
	obj := func(raw json.RawMessage) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		return m
	}
	list := func(raw json.RawMessage) ([]string, bool) {
		var l []string
		// null is "not said" (OpenRouter uses it for "all of them").
		if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &l) != nil {
			return nil, false
		}
		for i := range l {
			l[i] = strings.ToLower(strings.TrimSpace(l[i]))
		}
		return l, true
	}
	boolean := func(raw json.RawMessage) (bool, bool) {
		var v bool
		if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
			return false, false
		}
		return v, true
	}
	integer := func(raw json.RawMessage) int64 {
		var v int64
		_ = json.Unmarshal(raw, &v)
		return v
	}
	text := func(raw json.RawMessage) string {
		var v string
		_ = json.Unmarshal(raw, &v)
		return strings.ToLower(strings.TrimSpace(v))
	}
	setBool := func(key string, raw json.RawMessage) {
		if v, ok := boolean(raw); ok {
			c[key] = v
		}
	}
	// Modalities: a list names everything the model takes (or gives), so
	// what is not on it is a no.
	modalities := func(in, out json.RawMessage) {
		if l, ok := list(in); ok && len(l) > 0 {
			has := func(names ...string) bool {
				for _, n := range names {
					for _, v := range l {
						if v == n {
							return true
						}
					}
				}
				return false
			}
			c[CapInText] = has("text")
			c[CapInImage] = has("image")
			c[CapInAudio] = has("audio")
			c[CapInVideo] = has("video")
			c[CapInFile] = has("file", "pdf", "document")
		}
		if l, ok := list(out); ok && len(l) > 0 {
			for _, v := range l {
				switch v {
				case "text":
					c[CapOutText] = true
				case "image":
					c[CapOutImage] = true
				case "audio", "speech":
					c[CapOutAudio] = true
				case "video":
					c[CapOutVideo] = true
				case "embeddings", "embedding":
					c[CapOutEmbedding] = true
				case "rerank":
					c[CapOutRerank] = true
				}
			}
		}
	}
	levels := func(l []string) []string {
		out := []string{}
		for _, v := range l {
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	}

	// xAI, DeepSeek, Cerebras (OpenRouter format): top-level lists.
	modalities(row["input_modalities"], row["output_modalities"])
	// OpenRouter and llama.cpp: under architecture.
	if arch := obj(row["architecture"]); arch != nil {
		modalities(arch["input_modalities"], arch["output_modalities"])
	}
	// Bedrock ListFoundationModels.
	modalities(row["inputModalities"], row["outputModalities"])

	// Output ceilings, by the names providers use.
	for _, raw := range []json.RawMessage{row["max_output_tokens"], row["max_completion_tokens"], row["outputTokenLimit"], obj(row["limits"])["max_completion_tokens"], obj(row["top_provider"])["max_completion_tokens"]} {
		if n := integer(raw); n > 0 {
			c[CapMaxOutput] = n
			break
		}
	}
	// Anthropic's max_tokens on a model row is the ceiling for the parameter.
	if provider == "anthropic" {
		if n := integer(row["max_tokens"]); n > 0 {
			c[CapMaxOutput] = n
		}
	}

	// Moonshot and Fireworks: flat booleans.
	setBool(CapInImage, row["supports_image_in"])
	setBool(CapInVideo, row["supports_video_in"])
	setBool(CapThinking, row["supports_reasoning"])
	setBool(CapInImage, row["supportsImageInput"])
	setBool(CapTools, row["supportsTools"])
	// Gemini native: "thinking" is a plain boolean on the model.
	setBool(CapThinking, row["thinking"])

	// DeepSeek: effort {supported_levels, default_level}. Thinking is on by
	// default there, and "none" turns it off though the list omits it.
	if effort := obj(row["effort"]); effort != nil {
		if l, ok := list(effort["supported_levels"]); ok {
			c[CapThinking] = len(l) > 0
			c[CapThinkingLevels] = levels(l)
		}
		if d := text(effort["default_level"]); d != "" {
			c[CapThinkingDefaultLevel] = d
		}
	}

	// OpenRouter: reasoning {supported_efforts, default_effort,
	// default_enabled, mandatory}.
	if r := obj(row["reasoning"]); r != nil {
		c[CapThinking] = true
		if l, ok := list(r["supported_efforts"]); ok {
			c[CapThinkingLevels] = levels(l)
		}
		if d := text(r["default_effort"]); d != "" {
			c[CapThinkingDefaultLevel] = d
		}
		setBool(CapThinkingDefault, r["default_enabled"])
		if m, ok := boolean(r["mandatory"]); ok {
			if m {
				c[CapThinkingOff] = ThinkingOffNever
			} else {
				c[CapThinkingOff] = ThinkingOffEffortNone
			}
		}
	}

	// supported_parameters: a list on OpenRouter (the union across its
	// providers), an object of booleans on Cerebras.
	if l, ok := list(row["supported_parameters"]); ok && len(l) > 0 {
		has := func(n string) bool {
			for _, v := range l {
				if v == n {
					return true
				}
			}
			return false
		}
		c[CapTools] = has("tools")
		c[CapStructured] = has("structured_outputs")
		if _, said := c[CapThinking]; !said {
			c[CapThinking] = has("reasoning") || has("reasoning_effort")
		}
		switch {
		case has("max_completion_tokens") && !has("max_tokens"):
			c[CapOutputCap] = OutputCapMaxCompletionTokens
		case has("max_tokens"):
			c[CapOutputCap] = OutputCapMaxTokens
		}
		var refused []string
		for _, p := range []string{"temperature", "top_p"} {
			if !has(p) {
				refused = append(refused, p)
			}
		}
		if refused != nil {
			c[CapRefusedParams] = refused
		}
	} else if params := obj(row["supported_parameters"]); params != nil {
		var refused []string
		for name, raw := range params {
			if v, ok := boolean(raw); ok && !v && (name == "temperature" || name == "top_p" || name == "presence_penalty" || name == "frequency_penalty") {
				refused = append(refused, name)
			}
		}
		if refused != nil {
			sort.Strings(refused)
			c[CapRefusedParams] = refused
		}
	}

	caps := obj(row["capabilities"])
	if caps == nil {
		return c
	}
	if provider == "anthropic" {
		supported := func(raw json.RawMessage) (bool, bool) { return boolean(obj(raw)["supported"]) }
		if v, ok := supported(caps["image_input"]); ok {
			c[CapInImage] = v
		}
		if v, ok := supported(caps["pdf_input"]); ok {
			c[CapInFile] = v
		}
		if v, ok := supported(caps["structured_outputs"]); ok {
			c[CapStructured] = v
		}
		if effort := obj(caps["effort"]); effort != nil {
			var l []string
			for _, name := range []string{"low", "medium", "high", "xhigh", "max"} {
				if v, ok := supported(effort[name]); ok && v {
					l = append(l, name)
				}
			}
			c[CapThinkingLevels] = levels(l)
		}
		if types := obj(obj(caps["thinking"])["types"]); types != nil {
			thinks := false
			for name, raw := range types {
				if v, ok := supported(raw); ok && v && name != "disabled" {
					thinks = true
				}
			}
			c[CapThinking] = thinks
			if v, ok := supported(types["disabled"]); ok && thinks {
				if v {
					c[CapThinkingOff] = ThinkingOffDisabled
				} else {
					c[CapThinkingOff] = ThinkingOffNever
				}
			}
		}
		// Every model on the Messages API takes text and gives text.
		c[CapInText], c[CapOutText] = true, true
		c[CapOutputCap] = OutputCapMaxTokens
		return c
	}
	// Mistral and Cerebras: booleans. xAI: reasoning_effort is the list of
	// accepted values.
	setBool(CapInImage, caps["vision"])
	setBool(CapInAudio, caps["audio"])
	setBool(CapThinking, caps["reasoning"])
	setBool(CapStructured, caps["structured_outputs"])
	for _, name := range []string{"function_calling", "tools"} {
		setBool(CapTools, caps[name])
	}
	if l, ok := list(caps["reasoning_effort"]); ok {
		c[CapThinking] = len(l) > 0
		c[CapThinkingLevels] = levels(l)
		if len(l) > 0 {
			off := ThinkingOffNever
			for _, v := range l {
				if v == "none" {
					off = ThinkingOffEffortNone
				}
			}
			c[CapThinkingOff] = off
		}
	}
	if d := text(caps["default_reasoning_effort"]); d != "" {
		c[CapThinkingDefaultLevel] = d
		c[CapThinkingDefault] = d != "none"
	}
	return c
}

// mergeCapabilities adds what a second source said; the first source wins
// where both speak.
func mergeCapabilities(base, extra Capabilities) Capabilities {
	if base == nil {
		base = Capabilities{}
	}
	for k, v := range extra {
		if _, ok := base[k]; !ok {
			base[k] = v
		}
	}
	return base
}

// modelRows indexes a model list by id. It accepts the shapes providers
// use: {"data": [...]}, {"models": [...]} or a bare array, keyed by "id".
func modelRows(body []byte) map[string]map[string]json.RawMessage {
	var rows []map[string]json.RawMessage
	var wrapped struct {
		Data   []map[string]json.RawMessage `json:"data"`
		Models []map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(body, &wrapped) == nil && (wrapped.Data != nil || wrapped.Models != nil) {
		rows = append(wrapped.Data, wrapped.Models...)
	} else {
		_ = json.Unmarshal(body, &rows)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, row := range rows {
		var id string
		if json.Unmarshal(row["id"], &id) == nil && id != "" {
			out[id] = row
		}
	}
	return out
}
