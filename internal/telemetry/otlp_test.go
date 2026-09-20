package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// collector is a minimal in-process OTLP receiver.
type collector struct {
	mu     sync.Mutex
	bodies map[string][]string // path -> raw JSON bodies
}

func newCollector() (*collector, *httptest.Server) {
	c := &collector{bodies: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return c, srv
}

func (c *collector) received(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies[path]...)
}

func TestOTLPDisabledWithoutEndpoint(t *testing.T) {
	o := NewOTLP(OTLPConfig{Endpoint: ""}, New("test", "sha"), discardLogger())
	if o != nil {
		t.Fatal("no endpoint must mean no exporter (documented: disabled by default)")
	}
	// Every call on the nil exporter and nil trace context must be a no-op.
	var wg sync.WaitGroup
	o.Start(context.Background(), &wg)
	o.SetEnabledFunc(nil)
	tr := o.StartTrace()
	if tr != nil {
		t.Fatal("nil exporter must return a nil trace context")
	}
	tr.Child("noop", SpanKindInternal, time.Now(), time.Now())
	tr.End("noop", time.Now(), 200)
	wg.Wait()
}

func TestOTLPExportsSpans(t *testing.T) {
	c, srv := newCollector()
	defer srv.Close()

	o := NewOTLP(OTLPConfig{Endpoint: srv.URL, ServiceName: "janus", ServiceVersion: "test"}, New("test", "sha"), discardLogger())
	if o == nil {
		t.Fatal("exporter must be active when an endpoint is set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	o.Start(ctx, &wg)

	tr := o.StartTrace()
	if tr == nil {
		t.Fatal("active exporter must mint a trace context")
	}
	start := time.Now().Add(-50 * time.Millisecond)
	tr.Child("quota.check", SpanKindInternal, start, start.Add(2*time.Millisecond))
	tr.Child("upstream.call", SpanKindClient, start, start.Add(40*time.Millisecond), String("upstream", "mock"))
	tr.End("POST /v1/chat/completions", start, 200,
		String("model", "gpt-test"), String("request_id", "req-1"))

	cancel() // shutdown drains and flushes the batch
	wg.Wait()

	bodies := c.received("/v1/traces")
	if len(bodies) == 0 {
		t.Fatal("no trace export reached the collector")
	}
	var payload struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Name         string `json:"name"`
					Kind         int    `json:"kind"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("trace payload is not valid JSON: %v", err)
	}
	spans := payload.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	rootTrace := spans[0].TraceID
	if len(rootTrace) != 32 {
		t.Fatalf("traceId must be 32 hex chars, got %q", rootTrace)
	}
	var rootID string
	for _, sp := range spans {
		if sp.TraceID != rootTrace {
			t.Fatal("all spans must share the request trace id")
		}
		if len(sp.SpanID) != 16 {
			t.Fatalf("spanId must be 16 hex chars, got %q", sp.SpanID)
		}
		if sp.Name == "POST /v1/chat/completions" {
			rootID = sp.SpanID
			if sp.Kind != SpanKindServer {
				t.Fatalf("root span kind = %d, want server (%d)", sp.Kind, SpanKindServer)
			}
		}
	}
	for _, sp := range spans {
		if sp.Name != "POST /v1/chat/completions" && sp.ParentSpanID != rootID {
			t.Fatalf("child span %q does not parent to the root span", sp.Name)
		}
	}
	if !strings.Contains(bodies[0], `"service.name"`) {
		t.Fatal("resource must carry service.name")
	}
}

// TestOTLPExportsToPrivateCACollector locks the OTLPConfig.TLSConfig contract
// (regression: the exporter built a bare http.Client, so a collector behind a
// private corporate CA — the same CAs JANUS_CA_BUNDLE exists for — rejected
// every export with x509 unknown-authority errors).
func TestOTLPExportsToPrivateCACollector(t *testing.T) {
	c := &collector{bodies: map[string][]string{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	o := NewOTLP(OTLPConfig{
		Endpoint: srv.URL, ServiceName: "janus", ServiceVersion: "test",
		TLSConfig: &tls.Config{RootCAs: pool},
	}, New("test", "sha"), discardLogger())
	if o == nil {
		t.Fatal("exporter must be active when an endpoint is set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	o.Start(ctx, &wg)

	tr := o.StartTrace()
	start := time.Now().Add(-10 * time.Millisecond)
	tr.End("POST /v1/chat/completions", start, 200)

	cancel() // shutdown drains and flushes the batch
	wg.Wait()

	if len(c.received("/v1/traces")) == 0 {
		t.Fatal("no trace export reached the private-CA TLS collector; the bundle trust store is not wired into the exporter client")
	}
}

func TestOTLPExportsMetricsSnapshot(t *testing.T) {
	c, srv := newCollector()
	defer srv.Close()

	metrics := New("test", "sha")
	metrics.ObserveGateway("/v1/*", "chat", "mock", "POST", 200, 120*time.Millisecond)

	o := NewOTLP(OTLPConfig{Endpoint: srv.URL, ServiceVersion: "test"}, metrics, discardLogger())
	o.exportMetrics()

	bodies := c.received("/v1/metrics")
	if len(bodies) != 1 {
		t.Fatalf("got %d metric exports, want 1", len(bodies))
	}
	body := bodies[0]
	for _, want := range []string{`"resourceMetrics"`, "janus_requests_total", "janus_gateway_latency_seconds", `"explicitBounds"`, `"isMonotonic":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric payload is missing %s", want)
		}
	}
	// The cardinality contract holds on the OTLP surface too.
	for _, forbidden := range ForbiddenLabels {
		if strings.Contains(body, `"key":"`+forbidden+`"`) {
			t.Fatalf("metric payload carries forbidden label %s", forbidden)
		}
	}
}

func TestOTLPUnreachableCollectorIsNonBlocking(t *testing.T) {
	// Port 1 refuses connections immediately on any sane host.
	o := NewOTLP(OTLPConfig{Endpoint: "http://127.0.0.1:1"}, New("test", "sha"), discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr := o.StartTrace()
		// Overfill the buffer: recording must drop, never block.
		for i := 0; i < 5000; i++ {
			tr.Child("filler", SpanKindInternal, time.Now(), time.Now())
		}
		o.postTraces([]span{{traceID: "00", spanID: "00", name: "x", start: time.Now(), end: time.Now()}})
		o.exportMetrics()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("export against an unreachable collector blocked the caller")
	}
	o.mu.Lock()
	dropped := o.dropped
	o.mu.Unlock()
	if dropped == 0 {
		t.Fatal("overfilling the span buffer must drop spans rather than block")
	}
}

func TestOTLPFeatureFlagGatesExport(t *testing.T) {
	c, srv := newCollector()
	defer srv.Close()

	o := NewOTLP(OTLPConfig{Endpoint: srv.URL}, New("test", "sha"), discardLogger())
	o.SetEnabledFunc(func(context.Context) bool { return false })

	o.postTraces([]span{{traceID: "00", spanID: "00", name: "x", start: time.Now(), end: time.Now()}})
	if got := len(c.received("/v1/traces")); got != 0 {
		t.Fatalf("otel_enabled=false must suppress trace export, got %d posts", got)
	}

	o.SetEnabledFunc(func(context.Context) bool { return true })
	o.postTraces([]span{{traceID: "00", spanID: "00", name: "x", start: time.Now(), end: time.Now()}})
	if got := len(c.received("/v1/traces")); got != 1 {
		t.Fatalf("otel_enabled=true must resume trace export, got %d posts", got)
	}
}
