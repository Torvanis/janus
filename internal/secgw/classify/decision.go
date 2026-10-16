package classify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/decisions"
)

// ProtocolDecision is the role/protocol of System One decision models.
// They do not score segments or judge conversations the way the encoder
// and generative guards do; they answer typed questions, and the engine
// asks them through DecisionBackend so their questions can share a call
// with other features' questions.
const ProtocolDecision = "decision"

// DecisionBackend is a classifier that answers typed questions.
type DecisionBackend interface {
	Classifier
	// Decide asks questions about text.
	Decide(ctx context.Context, text string, questions []dec.Question) (dec.Result, error)
	// Key identifies the classifier (provider + model) so callers can
	// group questions bound for the same one.
	Key() string
	// Style is how this classifier takes several questions
	// (dec.StyleCombined or dec.StyleSeparate).
	Style() string
}

// ErrNotDecisionModel is returned when a decision call reaches a classifier
// of another kind.
var ErrNotDecisionModel = errors.New("bound classifier is not a decision model")

func init() {
	Register(ProtocolDecision, newDecision)
}

// DecisionKey is the grouping key of a decision classifier: questions for
// the same key can share a call. Usage insights computes the same key from
// its own settings, so its questions can join a guard's call.
func DecisionKey(upstreamID, wireModel string) string { return upstreamID + "/" + wireModel }

func newDecision(up Upstream) (Classifier, error) {
	if strings.TrimSpace(up.BaseURL) == "" {
		return nil, fmt.Errorf("classifier %s has no base URL", up.ModelName)
	}
	if up.Client == nil {
		up.Client = http.DefaultClient
	}
	wire := up.WireModel
	if wire == "" {
		wire = defaultWireModel(up.ModelName, up.BaseURL)
	}
	dialect := up.Dialect
	if dialect == "" {
		dialect = decisions.DefaultDialect(up.BaseURL)
	}
	style := up.Style
	if style == "" {
		style = dec.DefaultStyle(up.AdapterType, up.BaseURL)
	}
	maxOpts := up.MaxOptions
	if maxOpts == 0 {
		maxOpts = dec.DefaultMaxOptions(up.AdapterType, up.BaseURL)
	}
	return &DecisionClassifier{
		Backend: &dec.SystemOne{URL: decisions.Endpoint(up.BaseURL, dialect), APIKey: up.APIKey, Model: wire, Client: up.Client,
			Style: style, MaxOptions: maxOpts, Label: up.ModelName, Dialect: dialect},
		ID: DecisionKey(up.UpstreamID, wire), meter: up.Meter, decisionMeter: up.DecisionMeter,
	}, nil
}

// DecisionClassifier adapts a System One backend to the guard registry.
type DecisionClassifier struct {
	Backend *dec.SystemOne
	// ID is the grouping key (DecisionKey).
	ID            string
	meter         func(ctx context.Context, elapsed time.Duration, inputChars int, err error)
	decisionMeter func(ctx context.Context, elapsed time.Duration, inputChars int, res dec.Result, err error)
}

// Protocol implements Classifier.
func (d *DecisionClassifier) Protocol() string { return ProtocolDecision }

// Classify implements Classifier; decision models are asked through Decide.
func (d *DecisionClassifier) Classify(context.Context, []Segment) ([]Score, error) {
	return nil, ErrNotDecisionModel
}

// Decide implements DecisionBackend. Each call is metered like every other
// guard call (one usage event on the platform-overhead subject).
func (d *DecisionClassifier) Decide(ctx context.Context, text string, questions []dec.Question) (dec.Result, error) {
	started := time.Now()
	r, err := d.Backend.Classify(ctx, text, questions)
	if d.decisionMeter != nil {
		d.decisionMeter(ctx, time.Since(started), len(text), r, err)
	} else if d.meter != nil {
		d.meter(ctx, time.Since(started), len(text), err)
	}
	return r, err
}

// Key implements DecisionBackend.
func (d *DecisionClassifier) Key() string { return d.ID }

// Style implements DecisionBackend.
func (d *DecisionClassifier) Style() string { return d.Backend.Style }

// Decide delegates to a DecisionBackend inner. Only Security Gateway calls
// (ctx marked WithGuardCall) count towards the breaker: auto-routing,
// admin tests and other features share the model, and their failures must
// not switch the guard off (or make fail-closed guards refuse everything).
// An open breaker still refuses every caller: the model is down for all.
func (b *Breaker) Decide(ctx context.Context, text string, questions []dec.Question) (dec.Result, error) {
	d, ok := b.Inner.(DecisionBackend)
	if !ok {
		return dec.Result{}, ErrNotDecisionModel
	}
	if b.Open() {
		return dec.Result{}, ErrBreakerOpen
	}
	r, err := d.Decide(ctx, text, questions)
	b.record(ctx, err, true)
	return r, err
}

// Key delegates.
func (b *Breaker) Key() string {
	if d, ok := b.Inner.(DecisionBackend); ok {
		return d.Key()
	}
	return b.ModelID
}

// Style delegates.
func (b *Breaker) Style() string {
	if d, ok := b.Inner.(DecisionBackend); ok {
		return d.Style()
	}
	return ""
}

// IsDecision reports whether the breaker wraps a decision model.
func (b *Breaker) IsDecision() bool {
	_, ok := b.Inner.(DecisionBackend)
	return ok
}

// defaultWireModel is the decision API's name for a catalog model when the
// admin has not set one. OpenRouter lists Jev as typesafe/jev-router, but its
// decisions endpoint only answers to ~typesafe/jev-latest: without this an
// admin who marks the listed model as a decision model gets "model does not
// exist" on every call.
func defaultWireModel(name, baseURL string) string {
	return decisions.WireModel(name, baseURL)
}
