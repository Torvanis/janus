package classify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/classtrace"
)

// traceCall reports one guard call to troubleshooting, when a session is
// listening on this request. Bodies are passed as is; the request never
// carries credentials (those are headers).
func traceCall(ctx context.Context, up Upstream, protocol, url string, started time.Time, reqBody, respBody []byte, err error) {
	if !classtrace.Enabled(ctx) {
		return
	}
	c := classtrace.Call{Feature: dec.FeatureFrom(ctx), Classifier: up.ModelName, Protocol: protocol, URL: url, Mode: "inline",
		StartedAt: started.UTC(), LatencyMs: time.Since(started).Milliseconds(), Status: "ok", Request: json.RawMessage(reqBody)}
	if json.Valid(respBody) {
		c.Response = json.RawMessage(respBody)
	}
	if err != nil {
		c.Status, c.Error = "error", strings.Join(strings.Fields(err.Error()), " ")
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.Status = "timeout"
		}
	}
	classtrace.Record(ctx, c)
}
