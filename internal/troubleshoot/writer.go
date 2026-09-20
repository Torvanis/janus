package troubleshoot

import (
	"bytes"
	"context"
	"net/http"
)

// CapturingWriter wraps an http.ResponseWriter and keeps a bounded copy of
// everything written through it — status, headers and up to Limit bytes of
// body — while passing every call straight through. Streaming responses keep
// streaming: Flush is forwarded when the underlying writer supports it.
type CapturingWriter struct {
	http.ResponseWriter
	Limit     int64
	body      bytes.Buffer
	status    int
	truncated bool
}

// NewCapturingWriter wraps w with a body cap of limit bytes.
func NewCapturingWriter(w http.ResponseWriter, limit int64) *CapturingWriter {
	return &CapturingWriter{ResponseWriter: w, Limit: limit}
}

// WriteHeader records the status and forwards it.
func (c *CapturingWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
	c.ResponseWriter.WriteHeader(status)
}

// Write copies up to the remaining capture budget and forwards everything.
func (c *CapturingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if remaining := c.Limit - int64(c.body.Len()); remaining > 0 {
		if int64(len(p)) <= remaining {
			c.body.Write(p)
		} else {
			c.body.Write(p[:remaining])
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return c.ResponseWriter.Write(p)
}

// Flush forwards to the underlying writer when it streams.
func (c *CapturingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer for http.ResponseController.
func (c *CapturingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Status is the first status written (200 if a body was written without one).
func (c *CapturingWriter) Status() int { return c.status }

// Body is the captured prefix of the response body.
func (c *CapturingWriter) Body() []byte { return c.body.Bytes() }

// Truncated reports whether the body exceeded the capture budget.
func (c *CapturingWriter) Truncated() bool { return c.truncated }

// Pending is the per-request capture state the proxy threads through the
// request context: the wrapped writer plus the request-side material that
// is only available at the top of the handler.
type Pending struct {
	Writer         *CapturingWriter
	RequestHeaders http.Header
	RequestBody    []byte
	// RequestTruncated is set when the request body overflowed the proxy's
	// in-memory buffer; only the buffered prefix is captured.
	RequestTruncated bool
}

// Payload assembles the capture payload once the response is complete.
func (p *Pending) Payload() Payload {
	if p == nil {
		return Payload{}
	}
	out := Payload{RequestHeaders: p.RequestHeaders, RequestBody: p.RequestBody, Truncated: p.RequestTruncated}
	if p.Writer != nil {
		out.ResponseHeaders = p.Writer.Header().Clone()
		out.ResponseBody = p.Writer.Body()
		out.Truncated = out.Truncated || p.Writer.Truncated()
	}
	return out
}

type pendingKey struct{}

// WithPending stores the pending capture on a context.
func WithPending(ctx context.Context, p *Pending) context.Context {
	return context.WithValue(ctx, pendingKey{}, p)
}

// PendingFrom returns the pending capture stored on the context, or nil.
func PendingFrom(ctx context.Context) *Pending {
	p, _ := ctx.Value(pendingKey{}).(*Pending)
	return p
}
