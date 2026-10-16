// Package classify is the pluggable model-backed judgement layer of the
// Security Gateway. v1 ships one protocol, text_classification, which is how
// encoder guard models such as Llama Prompt Guard 2 are served (HF TEI /
// transformers pipeline wire shape). Generative guards (Llama Guard) are a
// later protocol behind the same interface; adding one is a registry
// entry, not a pipeline change.
//
// Every backend honours the same operational contract: a per-call timeout
// from the check's options, a per-model circuit breaker that opens after
// consecutive failures so a wedged classifier is loud rather than a silent
// 100%-fail-closed outage, and metering of every call to a platform
// overhead subject (never the caller's quota).
package classify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
)

// Segment is one chunk of text to score.
type Segment struct {
	Index int
	Text  string
}

// Score is the verdict for one segment. Malicious is the backend's own
// mapping of its labels onto the binary the engine needs; Label carries the
// raw label for the audit row.
type Score struct {
	Index     int
	Label     string
	Score     float64
	Malicious bool
}

// Classifier scores segments.
type Classifier interface {
	Protocol() string
	Classify(ctx context.Context, in []Segment) ([]Score, error)
}

// Upstream is what a backend needs to reach its model.
type Upstream struct {
	ModelID   string
	ModelName string
	BaseURL   string
	APIKey    string
	Client    *http.Client
	// Meter is invoked after every call with the wall time and the number of
	// input characters, so the gateway can write a usage event on the
	// overhead subject. Optional.
	Meter func(ctx context.Context, elapsed time.Duration, inputChars int, err error)
	// DecisionMeter, when set, replaces Meter for decision models: it also
	// receives what the call consumed, so the call can be priced.
	DecisionMeter func(ctx context.Context, elapsed time.Duration, inputChars int, res dec.Result, err error)
	// Dialect is the decision wire format ("" = detected from BaseURL).
	Dialect string

	// Decision models only.
	UpstreamID  string
	AdapterType string
	// WireModel is the name sent to the decision API when it differs from
	// the catalog name (OpenRouter lists Jev as typesafe/jev-router but
	// answers decisions as ~typesafe/jev-latest). Empty = ModelName.
	WireModel string
	// Style and MaxOptions come from the classifier's profile; empty/0
	// means the default for the server (dec.DefaultStyle/MaxOptions).
	Style      string
	MaxOptions int
}

// Constructor builds a classifier for an upstream.
type Constructor func(Upstream) (Classifier, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Constructor{}
)

// Register adds a protocol.
func Register(protocol string, ctor Constructor) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[protocol] = ctor
}

// New builds a classifier for a protocol.
func New(protocol string, up Upstream) (Classifier, error) {
	regMu.RLock()
	ctor, ok := registry[protocol]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown classifier protocol %q", protocol)
	}
	return ctor(up)
}

// Protocols lists registered protocols.
func Protocols() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for p := range registry {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Resolver hands the engine a classifier for a model id. The gateway's
// implementation looks the model up, refuses one without a classifier
// role, decrypts the upstream credential and wraps the result in a
// breaker. Tests supply a fake.
type Resolver interface {
	For(ctx context.Context, modelID string) (Classifier, error)
}

// InjectionOptions are the tunables for the prompt-injection check.
type InjectionOptions struct {
	Threshold    float64
	ChunkChars   int
	OverlapChars int
	Timeout      time.Duration
}

// Chunk splits text into overlapping windows of at most size characters.
// Guard encoders have a fixed context (Prompt Guard: 512 tokens); a message
// longer than that must be scanned in pieces, with overlap so an attack
// straddling a boundary is seen whole by at least one window.
func Chunk(text string, size, overlap int) []string {
	if size <= 0 {
		return []string{text}
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 8
	}
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}
	var out []string
	step := size - overlap
	for start := 0; start < len(runes); start += step {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}

// ErrBreakerOpen is returned while a classifier's breaker is open.
var ErrBreakerOpen = errors.New("classifier circuit breaker is open")

// Breaker wraps a classifier with a consecutive-failure circuit breaker.
type Breaker struct {
	Inner     Classifier
	Threshold int           // consecutive failures that open the breaker
	Cooldown  time.Duration // how long it stays open
	OnOpen    func(modelID string, err error)
	ModelID   string

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// Protocol delegates.
func (b *Breaker) Protocol() string { return b.Inner.Protocol() }

// Open reports whether the breaker is currently refusing calls.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().Before(b.openUntil)
}

// Classify delegates unless the breaker is open.
func (b *Breaker) Classify(ctx context.Context, in []Segment) ([]Score, error) {
	if b.Open() {
		return nil, ErrBreakerOpen
	}
	out, err := b.Inner.Classify(ctx, in)
	b.record(ctx, err, false)
	return out, err
}

// Judge delegates to a ConversationGuard inner under the same breaker
// accounting as Classify. It returns ErrNotConversationGuard when the
// inner backend is an encoder classifier, so the engine can tell
// "wrong protocol bound" from "guard is down".
func (b *Breaker) Judge(ctx context.Context, turns []Turn) (Verdict, error) {
	cg, ok := b.Inner.(ConversationGuard)
	if !ok {
		return Verdict{}, ErrNotConversationGuard
	}
	if b.Open() {
		return Verdict{}, ErrBreakerOpen
	}
	v, err := cg.Judge(ctx, turns)
	b.record(ctx, err, false)
	return v, err
}

type guardCallKey struct{}

// WithGuardCall marks ctx as a Security Gateway check's own call. Only
// such calls move a decision model's breaker: the same model also answers
// auto-routing, admin tests and other features, and their failures (a
// router's budget running out, a rejected routing question, an admin's
// batch test) say nothing about whether the guard can judge. Without the
// separation three routing timeouts opened the breaker and every
// fail-closed guard on that model refused all traffic for the cooldown.
func WithGuardCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, guardCallKey{}, true)
}

// IsGuardCall reports whether ctx was marked by WithGuardCall.
func IsGuardCall(ctx context.Context) bool {
	v, _ := ctx.Value(guardCallKey{}).(bool)
	return v
}

type trialCallKey struct{}

// WithTrialCall marks ctx as an administrator's test (policy dry run): its
// classifier calls never move a breaker, whichever guard they exercise.
func WithTrialCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, trialCallKey{}, true)
}

func isTrialCall(ctx context.Context) bool {
	v, _ := ctx.Value(trialCallKey{}).(bool)
	return v
}

// record applies one call's outcome to the failure counter and opens the
// breaker at the threshold. guardOnly limits the accounting to calls
// marked WithGuardCall (decision models, which other features share).
// Administrators' trial calls (WithTrialCall) never count. A call the
// caller itself cancelled (client gone, request finished) is
// never a classifier failure.
func (b *Breaker) record(ctx context.Context, err error, guardOnly bool) {
	if isTrialCall(ctx) || (guardOnly && !IsGuardCall(ctx)) {
		return
	}
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.failures = 0
		return
	}
	b.failures++
	threshold := b.Threshold
	if threshold <= 0 {
		threshold = 3
	}
	if b.failures >= threshold {
		cooldown := b.Cooldown
		if cooldown <= 0 {
			cooldown = 30 * time.Second
		}
		b.openUntil = time.Now().Add(cooldown)
		b.failures = 0
		if b.OnOpen != nil {
			go b.OnOpen(b.ModelID, err)
		}
	}
}

// ErrNotConversationGuard is returned by Judge when the bound classifier
// is an encoder (text_classification) rather than a generative guard.
var ErrNotConversationGuard = errors.New("bound classifier is not a conversation guard; content_safety needs a generative_guard model")
