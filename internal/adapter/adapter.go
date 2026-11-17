// Package adapter is the stable extension point for upstream providers.
//
// A provider is added by implementing Adapter in its own package and calling
// Register from that package's init. No file in this package, in internal/proxy,
// or anywhere else in the gateway needs to change.
//
//	type Adapter interface {
//	    Type() string                                   // stable enum value stored on upstream rows
//	    Discover(ctx, Upstream) ([]DiscoveredModel, error)
//	    Prepare(Upstream, *Request) (*Prepared, error)  // URL, auth, request body translation
//	    ExtractUsage([]byte) (Usage, bool)              // non-streaming token counts
//	    NewStreamCollector() StreamCollector            // streaming token counts
//	    TransformResponse(*Request, []byte) ([]byte, error)         // response body translation
//	    NewStreamTransformer(*Request, http.Header) StreamTransformer // SSE translation
//	}
//
// Translation is bidirectional. Prepare turns the OpenAI-shaped request into
// the provider's native request; TransformResponse / NewStreamTransformer
// turn the provider's native response back into the OpenAI shape the caller
// expects. Providers that already speak the OpenAI wire format implement the
// response side as a pass-through (PassthroughResponse / PassthroughStream).
package adapter

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Upstream is the provider configuration handed to an adapter. The APIKey is
// already decrypted; adapters must never log or echo it.
type Upstream struct {
	ID          string
	Name        string
	BaseURL     string
	APIKey      string
	AdapterType string
}

// Request is a downstream call awaiting normalization.
type Request struct {
	Method    string
	Path      string // gateway-relative, always starting /v1/
	Header    http.Header
	Body      []byte
	Model     string
	Streaming bool

	// Context, when non-nil, is the proxied request's context. Adapters that
	// must make side-channel calls while preparing (e.g. Vertex OAuth token
	// minting) derive from it so client disconnects cancel those calls too.
	// Nil is treated as context.Background().
	Context context.Context
	// Client, when non-nil, is the gateway's shared upstream HTTP client —
	// it carries the JANUS_CA_BUNDLE trust store and HTTPS_PROXY settings.
	// Adapters use it for side-channel calls so a TLS-inspecting egress
	// proxy or private CA applies to those exactly as to the proxied call.
	Client *http.Client
}

// Prepared is the upstream-ready form of a Request.
type Prepared struct {
	URL    string
	Header http.Header
	Body   []byte
	// OnRedirect, when set, adjusts the headers of a redirected request
	// before it is sent (Gemini's video download redirects to an endpoint
	// that takes only its own key header). nil = Go's default behavior.
	OnRedirect func(h http.Header)
}

// Usage is the token accounting extracted from an upstream response.
//
// One convention for every provider, the way providers bill (owner ruling:
// "record the cached tokens the same way that they are billed"): TokensIn is
// the WHOLE prompt — fresh input, cache reads and cache writes together.
// TokensCached (cache reads) and TokensCacheWrite5m / TokensCacheWrite1h
// (prompt-cache writes with 5-minute and 1-hour TTLs) are PARTS of TokensIn
// that are billed at their own rates; the fresh part billed at the input rate
// is TokensIn − TokensCached − the writes.
//
// OpenAI, Gemini and every OpenAI-compatible server already report it this
// way (prompt_tokens includes prompt_tokens_details.cached_tokens). Anthropic
// and Bedrock report input_tokens / inputTokens as the fresh part only, with
// cache reads and writes counted beside it; their adapters add the parts back
// so TokensIn is the whole prompt there too. Nothing downstream (billing,
// quotas, metrics, exports, translation to the OpenAI shape) needs to know
// which provider a count came from.
//
// Reported is true when the upstream supplied token counts (the OpenAI
// `usage` object, Anthropic's `usage`, Bedrock's `amazon-bedrock-invocationMetrics`,
// …). It is the ONLY condition under which the proxy trusts TokensIn/Out; when
// it is false the proxy either falls back to byte estimation (text-shaped
// modalities) or records the request unmetered (media modalities). Adapters
// must never set Reported on a guess.
//
// Some providers report what a request COST rather than (or as well as) how
// many tokens it consumed — X.ai's `cost_in_usd_ticks` on image generation is
// the canonical example, and it is the only exact billing signal such a
// response carries. CostReported=true with CostNano set lets an adapter hand
// that figure straight to metering (recorded under
// usage.AccountingUpstreamCost), bypassing the per-token rate card which cannot
// express per-image pricing. A cost-only report may leave every token count at
// zero.
//
// TokensInPerSecond / TokensOutPerSecond carry upstream-REPORTED throughput
// when the provider measures it itself (Groq's prompt_time/completion_time,
// llama.cpp's timings block, Ollama's *_duration fields).
// ThroughputReported distinguishes "the provider said 0" from "the provider
// said nothing": when it is false the proxy derives throughput from its own
// clock and labels the headers as calculated, never as reported.
type Usage struct {
	TokensIn           int64
	TokensOut          int64
	TokensCached       int64
	TokensCacheWrite5m int64
	TokensCacheWrite1h int64
	FinishReason       string
	Reported           bool

	// CostNano is a provider-reported request cost in nano-USD; valid only
	// when CostReported is true.
	CostNano     int64
	CostReported bool

	// ToolFeeNano is what the provider charges per hosted tool call (web
	// searches, fetches) on top of tokens, in nano-USD, when the provider
	// reports the calls but not the cost (Anthropic, OpenAI, Gemini). It
	// is added to the token cost; a reported CostNano already includes it.
	ToolFeeNano int64
	// ToolCalls counts hosted tool calls by kind ("web_search", …).
	ToolCalls map[string]int64

	// Upstream-reported throughput in tokens per second; valid only when
	// ThroughputReported is true.
	TokensInPerSecond  float64
	TokensOutPerSecond float64
	ThroughputReported bool

	// DurationBilled is set when TokensIn counts seconds of audio (Whisper's
	// {"type":"duration"} usage), not tokens.
	DurationBilled bool

	// HiddenReasoning is set when TokensOut includes reasoning the provider
	// generated but did not stream (Gemini's thinking tokens). They are
	// produced before the first visible byte, so time-to-first-byte is not
	// prompt processing and the generation window is the whole call.
	HiddenReasoning bool
}

// withCacheInPrompt returns u with TokensIn raised from the fresh input a
// provider like Anthropic or Bedrock reports to the whole prompt: fresh input
// plus cache reads plus cache writes (see Usage). Only those adapters call
// it; OpenAI-style usage already counts its cached tokens inside TokensIn.
func (u Usage) withCacheInPrompt() Usage {
	u.TokensIn += u.TokensCached + u.TokensCacheWrite5m + u.TokensCacheWrite1h
	return u
}

// MergeStream folds the usage parsed from one streamed frame into the running
// total a StreamCollector keeps. Counts take the maximum seen (providers
// report cumulative totals, and the final frame is authoritative); a reported
// cost or throughput replaces any earlier value; flags are sticky. It is
// shared by the adapters whose streams carry an OpenAI-shaped usage block so
// they cannot drift in what they keep from a trailing frame.
func (u *Usage) MergeStream(frame Usage) {
	if frame.Reported {
		u.TokensIn = max64(u.TokensIn, frame.TokensIn)
		u.TokensOut = max64(u.TokensOut, frame.TokensOut)
		u.TokensCached = max64(u.TokensCached, frame.TokensCached)
		u.TokensCacheWrite5m = max64(u.TokensCacheWrite5m, frame.TokensCacheWrite5m)
		u.TokensCacheWrite1h = max64(u.TokensCacheWrite1h, frame.TokensCacheWrite1h)
		u.Reported = true
	}
	if frame.CostReported {
		u.CostNano = frame.CostNano
		u.CostReported = true
	}
	if frame.ToolFeeNano > u.ToolFeeNano {
		u.ToolFeeNano = frame.ToolFeeNano
	}
	for k, n := range frame.ToolCalls {
		if u.ToolCalls == nil {
			u.ToolCalls = map[string]int64{}
		}
		if n > u.ToolCalls[k] {
			u.ToolCalls[k] = n
		}
	}
	if frame.ThroughputReported {
		u.TokensInPerSecond = frame.TokensInPerSecond
		u.TokensOutPerSecond = frame.TokensOutPerSecond
		u.ThroughputReported = true
	}
	if frame.FinishReason != "" {
		u.FinishReason = frame.FinishReason
	}
	if frame.HiddenReasoning {
		u.HiddenReasoning = true
	}
}

// StreamCollector accumulates token counts across a streamed response. Feed is
// called once per raw SSE line as it is relayed to the client; the proxy never
// buffers the stream.
type StreamCollector interface {
	Feed(line []byte)
	Usage() Usage
}

// DiscoveredModel is one model reported by a provider.
type DiscoveredModel struct {
	Name       string
	Modalities []string
	// ContextWindow is the served token limit; zero means unreported.
	ContextWindow int64
	// Rates contains only reported, supported prices in nano-USD per million tokens.
	Rates            map[string]int64
	MetadataWarnings []string
	// Capabilities is what the provider's model list says the model can
	// take, give and be asked (see capabilities.go). Nil or missing keys
	// mean the provider did not say.
	Capabilities Capabilities
	// ClassifierRole is a hint from providers that can tell, at discovery
	// time, that a model is a guard classifier and which wire protocol the
	// security gateway must speak to it (TEI reports a classifier's label set
	// on /info). Empty for every ordinary model. Discovery applies the hint
	// only to a model that has no role yet — an admin's explicit choice, or
	// their explicit clearing of it, is never overridden by a poll.
	ClassifierRole string
}

// Classifier role hints a provider may set on a DiscoveredModel. These are
// the security gateway's protocol names, duplicated here because adapter
// must stay free of internal imports; the store validates them on write.
const (
	ClassifierRoleTextClassification = "text_classification"
	// ClassifierRoleDecision marks a decision model (typed answers).
	ClassifierRoleDecision = "decision"
)

// StreamTransformer rewrites one provider's streamed response into the
// OpenAI chat.completion.chunk SSE format, one relayed line at a time. A new
// transformer is created per stream because translation needs per-stream
// state: the message id and model that every chunk must repeat, the mapping
// from provider content-block indexes to OpenAI tool_call indexes, and the
// typed `event:` lines that must be swallowed rather than forwarded.
//
// TransformStreamLine receives each raw upstream line (including its
// trailing newline) and returns the bytes to relay to the client — possibly
// empty (line swallowed), possibly a complete `data: …\n\n` frame, possibly
// several frames. A non-nil error means the stream cannot be translated; the
// proxy signals that in-band and cuts the stream rather than relaying a
// payload the client cannot read.
type StreamTransformer interface {
	TransformStreamLine(line []byte) ([]byte, error)
}

// StreamFinisher is an optional StreamTransformer method: what to emit when
// the upstream closes the stream cleanly (for providers whose SSE has no
// [DONE] sentinel, so the translation's closing frames come from here).
type StreamFinisher interface {
	FinishStream() []byte
}

// Adapter normalizes one provider's protocol.
type Adapter interface {
	Type() string
	Discover(ctx context.Context, up Upstream, client *http.Client) ([]DiscoveredModel, error)
	Prepare(up Upstream, req *Request) (*Prepared, error)
	ExtractUsage(body []byte) (Usage, bool)
	NewStreamCollector() StreamCollector
	// TransformResponse rewrites a buffered (non-streaming) upstream response
	// body into the OpenAI-compatible shape for the given request. It runs
	// after ExtractUsage — metering always reads the provider's native
	// response — and before the body is written to the client. req is the
	// same Request that was handed to Prepare, so the adapter can tell a
	// translated /v1/chat/completions call from a native pass-through path.
	// Bodies the adapter does not translate are returned unchanged.
	TransformResponse(req *Request, body []byte) ([]byte, error)
	// NewStreamTransformer returns the per-stream rewriter for a streamed
	// response. respHeader is the upstream response header set, handed over
	// BEFORE it is copied to the client so an adapter whose native stream is
	// not SSE (e.g. AWS event-stream framing) can correct Content-Type to
	// text/event-stream for the translated output. Adapters that relay the
	// stream verbatim return PassthroughStream.
	NewStreamTransformer(req *Request, respHeader http.Header) StreamTransformer
}

// EventStreamBufferer is implemented by adapters whose upstream answers a
// non-streaming request with an event stream anyway (the ChatGPT plan
// surface streams unconditionally). When BuffersEventStream reports true
// the proxy relays the response through the buffered path, so
// TransformResponse can fold the whole stream into one JSON body for a
// caller that did not ask to stream.
type EventStreamBufferer interface {
	BuffersEventStream(req *Request) bool
}

// PassthroughResponse is the TransformResponse implementation for providers
// whose native response already IS the OpenAI shape.
func PassthroughResponse(_ *Request, body []byte) ([]byte, error) { return body, nil }

// PassthroughStream relays every SSE line unchanged.
type PassthroughStream struct{}

// TransformStreamLine returns the line as-is.
func (PassthroughStream) TransformStreamLine(line []byte) ([]byte, error) { return line, nil }

var (
	registryMu sync.RWMutex
	registry   = map[string]Adapter{}
)

// Register adds an adapter to the registry. It panics on a duplicate type
// because that is a programming error discovered at process start.
func Register(a Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[a.Type()]; exists {
		panic(fmt.Sprintf("adapter %q registered twice", a.Type()))
	}
	registry[a.Type()] = a
}

// Get resolves an adapter by its stored type.
func Get(adapterType string) (Adapter, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[adapterType]
	if !ok {
		return nil, fmt.Errorf("no adapter registered for provider type %q", adapterType)
	}
	return a, nil
}

// PersonalOnly is implemented by adapters that exist only for personal
// subscriptions (a consumer plan's inference surface, reached with the
// user's own sign-in). They are never offered as an upstream type.
type PersonalOnly interface{ PersonalOnly() bool }

// Types lists every adapter an administrator can configure as an upstream,
// sorted for stable UI ordering. Personal-only adapters are excluded.
func Types() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k, a := range registry {
		if p, ok := a.(PersonalOnly); ok && p.PersonalOnly() {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Modality names (14 values). Modality is captured explicitly on every usage
// event rather than inferred at query time.
const (
	ModalityChat       = "chat"
	ModalityEmbedding  = "embedding"
	ModalityImage      = "image"
	ModalityAudio      = "audio"
	ModalityVideo      = "video"
	ModalityTTS        = "tts"
	ModalitySTT        = "stt"
	ModalityFunction   = "function"
	ModalityResponse   = "response"
	ModalityAssistant  = "assistant"
	ModalityThread     = "thread"
	ModalityFile       = "file"
	ModalityModeration = "moderation"
	// ModalityDecision is a decision-API call (typed answers, no text).
	ModalityDecision = "decision"
	ModalityFineTune = "fine_tune"
)

// AllModalities is the canonical enum, used for validation and UI filters.
var AllModalities = []string{
	ModalityChat, ModalityEmbedding, ModalityImage, ModalityAudio, ModalityVideo,
	ModalityTTS, ModalitySTT, ModalityFunction, ModalityResponse, ModalityAssistant,
	ModalityThread, ModalityFile, ModalityModeration, ModalityFineTune,
}
