// Package classtrace carries an optional per-request recorder for classifier
// calls through a context, so troubleshooting mode can capture what every
// classifier was asked and what it answered without each classifier package
// knowing about troubleshooting. The proxy installs a Sink only while a
// troubleshooting session could capture the request; otherwise Record is a
// no-op and nothing is buffered.
package classtrace

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Call is one classifier call as troubleshooting sees it.
type Call struct {
	// Feature names what asked: usage_insights, prompt_injection,
	// content_safety, or a combination ("prompt_injection+usage_insights").
	Feature    string    `json:"feature"`
	Classifier string    `json:"classifier"`
	Protocol   string    `json:"protocol"`
	URL        string    `json:"url,omitempty"`
	Mode       string    `json:"mode"` // inline | async
	StartedAt  time.Time `json:"started_at"`
	LatencyMs  int64     `json:"latency_ms"`
	Status     string    `json:"status"` // ok | error | timeout
	Error      string    `json:"error,omitempty"`
	Questions  []string  `json:"questions,omitempty"`
	// Request and Response are the wire bodies (request with credentials
	// never included; they travel in headers). Omitted when the session
	// does not capture bodies.
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// Sink receives calls. Implementations must be safe for concurrent use:
// per-question calls run in parallel.
type Sink interface {
	Record(Call)
}

type ctxKey struct{}

// With returns ctx carrying sink.
func With(ctx context.Context, sink Sink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, sink)
}

// From returns the sink in ctx, or nil.
func From(ctx context.Context) Sink {
	s, _ := ctx.Value(ctxKey{}).(Sink)
	return s
}

// Enabled reports whether ctx carries a sink, so callers can skip building
// a Call (and copying bodies) when nobody is listening.
func Enabled(ctx context.Context) bool { return From(ctx) != nil }

// Record hands c to the sink in ctx, if any.
func Record(ctx context.Context, c Call) {
	if s := From(ctx); s != nil {
		s.Record(c)
	}
}

// Buffer is a Sink that keeps calls in memory. Late calls (an async
// classification finishing after the request's capture was written) are
// passed to Late once it is set, so they are not lost.
type Buffer struct {
	mu    sync.Mutex
	calls []Call
	late  func(Call)
	// Bodies controls whether request/response bodies are kept.
	Bodies bool
}

// Record implements Sink.
func (b *Buffer) Record(c Call) {
	if !b.Bodies {
		c.Request, c.Response = nil, nil
	}
	b.mu.Lock()
	late := b.late
	if late == nil {
		b.calls = append(b.calls, c)
	}
	b.mu.Unlock()
	if late != nil {
		late(c)
	}
}

// Drain returns the buffered calls and routes every later call to late.
func (b *Buffer) Drain(late func(Call)) []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.calls
	b.calls = nil
	b.late = late
	return out
}
