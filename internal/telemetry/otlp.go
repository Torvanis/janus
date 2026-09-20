// OTLP export: when OTEL_EXPORTER_OTLP_ENDPOINT is set, Janus exports
// traces and metrics to an OpenTelemetry collector over OTLP/HTTP using the
// protobuf-JSON encoding defined by the OTLP specification.
//
// This is a deliberate, dependency-free implementation rather than the OTel Go
// SDK: the gateway needs exactly one root span per proxied request plus a few
// child spans, and a periodic snapshot of the Prometheus registry — both of
// which the stdlib expresses directly. Export is strictly non-blocking: span
// recording drops on a full buffer, an unreachable collector produces one
// rate-limited warning log, and the request path never waits on telemetry.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// OTLP span kinds (the numeric values are fixed by the OTLP protobuf schema).
const (
	SpanKindInternal = 1
	SpanKindServer   = 2
	SpanKindClient   = 3
)

// Attr is one string span attribute. Bodies, Authorization headers, and API
// keys must never be placed in attributes (integrations/telemetry).
type Attr struct {
	Key   string
	Value string
}

// String builds a span attribute.
func String(key, value string) Attr { return Attr{Key: key, Value: value} }

// OTLPConfig configures the exporter.
type OTLPConfig struct {
	// Endpoint is the collector base URL, e.g. http://collector:4318. The
	// signal paths /v1/traces and /v1/metrics are appended per the OTLP/HTTP
	// specification.
	Endpoint string
	// Interval is the metric export period (documented default: 30s).
	Interval time.Duration
	// ServiceName and ServiceVersion populate the OTLP resource.
	ServiceName    string
	ServiceVersion string
	// TLSConfig, when non-nil, is used for HTTPS collector endpoints. It
	// carries the JANUS_CA_BUNDLE trust store so collectors signed by a
	// private corporate CA verify; nil means default verification.
	TLSConfig *tls.Config
}

// span is one finished span awaiting export.
type span struct {
	traceID  string
	spanID   string
	parentID string
	name     string
	kind     int
	start    time.Time
	end      time.Time
	status   int // 0 unset, 1 ok, 2 error
	attrs    []Attr
}

// OTLP exports traces and metrics to a collector. A nil *OTLP is a valid
// no-op exporter: every method checks the receiver, so callers never need to
// guard for the disabled (no endpoint) case.
type OTLP struct {
	cfg        OTLPConfig
	metrics    *Metrics
	logger     *slog.Logger
	client     *http.Client
	spans      chan span
	tracesURL  string
	metricsURL string

	// enabled consults the otel_enabled feature flag, one of the
	// runtime flags in store.DefaultFeatureFlags. Nil means always enabled.
	enabled func(context.Context) bool

	mu       sync.Mutex
	lastWarn time.Time
	dropped  int64
}

// NewOTLP builds the exporter, or returns nil when no endpoint is configured
// (documented: not set → no OTLP export; Prometheus remains primary).
func NewOTLP(cfg OTLPConfig, metrics *Metrics, logger *slog.Logger) *OTLP {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "janus"
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	client := &http.Client{Timeout: 5 * time.Second}
	if cfg.TLSConfig != nil {
		client.Transport = &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: cfg.TLSConfig,
		}
	}
	return &OTLP{
		cfg:        cfg,
		metrics:    metrics,
		logger:     logger,
		client:     client,
		spans:      make(chan span, 2048),
		tracesURL:  base + "/v1/traces",
		metricsURL: base + "/v1/metrics",
	}
}

// SetEnabledFunc installs the runtime toggle (the otel_enabled feature flag).
// The function is consulted per export batch, off the request path.
func (o *OTLP) SetEnabledFunc(fn func(context.Context) bool) {
	if o == nil {
		return
	}
	o.enabled = fn
}

// Start launches the span-batching and metric-export loops.
func (o *OTLP) Start(ctx context.Context, wg *sync.WaitGroup) {
	if o == nil {
		return
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		o.spanLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		o.metricLoop(ctx)
	}()
}

// TraceContext carries one request's trace identifiers through the proxy path.
// A nil *TraceContext is a valid no-op, so the hot path never branches on
// whether tracing is active.
type TraceContext struct {
	o       *OTLP
	traceID string
	rootID  string
}

// StartTrace mints the identifiers for one request trace. It performs no I/O.
func (o *OTLP) StartTrace() *TraceContext {
	if o == nil {
		return nil
	}
	traceID, err := randomHex(16)
	if err != nil {
		return nil
	}
	rootID, err := randomHex(8)
	if err != nil {
		return nil
	}
	return &TraceContext{o: o, traceID: traceID, rootID: rootID}
}

// Child records a completed child span under the request's root span.
func (t *TraceContext) Child(name string, kind int, start, end time.Time, attrs ...Attr) {
	if t == nil {
		return
	}
	id, err := randomHex(8)
	if err != nil {
		return
	}
	t.o.record(span{
		traceID: t.traceID, spanID: id, parentID: t.rootID,
		name: name, kind: kind, start: start, end: end, attrs: attrs,
	})
}

// End records the request's root span. httpStatus >= 500 marks the span as an
// error per OTLP status conventions.
func (t *TraceContext) End(name string, start time.Time, httpStatus int, attrs ...Attr) {
	if t == nil {
		return
	}
	status := 0
	if httpStatus >= 500 {
		status = 2
	}
	t.o.record(span{
		traceID: t.traceID, spanID: t.rootID,
		name: name, kind: SpanKindServer, start: start, end: time.Now(),
		status: status, attrs: attrs,
	})
}

// record queues a span without ever blocking the caller.
func (o *OTLP) record(sp span) {
	select {
	case o.spans <- sp:
	default:
		o.mu.Lock()
		o.dropped++
		o.mu.Unlock()
	}
}

// spanLoop batches spans and flushes them every few seconds, on batch
// pressure, and once more at shutdown (draining anything still queued).
func (o *OTLP) spanLoop(ctx context.Context) {
	const maxBatch = 512
	batch := make([]span, 0, maxBatch)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		o.postTraces(batch)
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case sp := <-o.spans:
					batch = append(batch, sp)
					if len(batch) >= maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case sp := <-o.spans:
			batch = append(batch, sp)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (o *OTLP) metricLoop(ctx context.Context) {
	ticker := time.NewTicker(o.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !o.exportAllowed() {
				continue
			}
			o.exportMetrics()
		}
	}
}

// exportAllowed consults the otel_enabled feature flag off the request path.
func (o *OTLP) exportAllowed() bool {
	if o.enabled == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return o.enabled(ctx)
}

func (o *OTLP) postTraces(batch []span) {
	if !o.exportAllowed() {
		return
	}
	spans := make([]map[string]any, 0, len(batch))
	for _, sp := range batch {
		encoded := map[string]any{
			"traceId":           sp.traceID,
			"spanId":            sp.spanID,
			"name":              sp.name,
			"kind":              sp.kind,
			"startTimeUnixNano": strconv.FormatInt(sp.start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(sp.end.UnixNano(), 10),
			"attributes":        encodeAttrs(sp.attrs),
			"status":            map[string]any{"code": sp.status},
		}
		if sp.parentID != "" {
			encoded["parentSpanId"] = sp.parentID
		}
		spans = append(spans, encoded)
	}
	payload := map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": o.resource(),
			"scopeSpans": []map[string]any{{
				"scope": o.scope(),
				"spans": spans,
			}},
		}},
	}
	o.post(o.tracesURL, payload)
}

// exportMetrics snapshots the Prometheus registry and ships it as OTLP
// cumulative sums, gauges, and histograms. One source of truth for both
// telemetry surfaces keeps the two from ever disagreeing.
func (o *OTLP) exportMetrics() {
	families, err := o.metrics.Registry().Gather()
	if err != nil {
		o.warn("gather metrics for OTLP export", err)
		return
	}
	nowNano := strconv.FormatInt(time.Now().UnixNano(), 10)
	converted := make([]map[string]any, 0, len(families))
	for _, mf := range families {
		if m := convertFamily(mf, nowNano); m != nil {
			converted = append(converted, m)
		}
	}
	payload := map[string]any{
		"resourceMetrics": []map[string]any{{
			"resource": o.resource(),
			"scopeMetrics": []map[string]any{{
				"scope":   o.scope(),
				"metrics": converted,
			}},
		}},
	}
	o.post(o.metricsURL, payload)
}

func (o *OTLP) resource() map[string]any {
	return map[string]any{"attributes": []map[string]any{
		{"key": "service.name", "value": map[string]any{"stringValue": o.cfg.ServiceName}},
		{"key": "service.version", "value": map[string]any{"stringValue": o.cfg.ServiceVersion}},
	}}
}

func (o *OTLP) scope() map[string]any {
	return map[string]any{"name": "github.com/torvanis/janus", "version": o.cfg.ServiceVersion}
}

// post ships one payload. Any failure is a rate-limited warning and nothing
// more: the gateway must keep operating when the collector is
// unreachable.
func (o *OTLP) post(url string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		o.warn("encode OTLP payload", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		o.warn("build OTLP request", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		o.warn("OTLP collector unreachable; export skipped, gateway continues", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		o.warn("OTLP collector rejected export", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url))
	}
}

// warn logs at most once per minute so a dead collector cannot flood the logs.
func (o *OTLP) warn(msg string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if time.Since(o.lastWarn) < time.Minute {
		return
	}
	o.lastWarn = time.Now()
	if o.logger != nil {
		o.logger.Warn(msg, "error", err.Error(), "otlp_endpoint", o.cfg.Endpoint)
	}
}

func encodeAttrs(attrs []Attr) []map[string]any {
	out := make([]map[string]any, 0, len(attrs))
	for _, a := range attrs {
		if a.Value == "" {
			continue
		}
		out = append(out, map[string]any{"key": a.Key, "value": map[string]any{"stringValue": a.Value}})
	}
	return out
}

// convertFamily maps one Prometheus metric family onto its OTLP shape.
// Prometheus histogram buckets are cumulative; OTLP bucketCounts are
// per-bucket, with a final overflow bucket, so the counts are differenced.
func convertFamily(mf *dto.MetricFamily, nowNano string) map[string]any {
	out := map[string]any{"name": mf.GetName(), "description": mf.GetHelp()}
	switch mf.GetType() {
	case dto.MetricType_COUNTER:
		points := make([]map[string]any, 0, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			points = append(points, map[string]any{
				"attributes":   labelAttrs(m),
				"timeUnixNano": nowNano,
				"asDouble":     m.GetCounter().GetValue(),
			})
		}
		out["sum"] = map[string]any{
			"dataPoints":             points,
			"aggregationTemporality": 2, // cumulative
			"isMonotonic":            true,
		}
	case dto.MetricType_GAUGE:
		points := make([]map[string]any, 0, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			points = append(points, map[string]any{
				"attributes":   labelAttrs(m),
				"timeUnixNano": nowNano,
				"asDouble":     m.GetGauge().GetValue(),
			})
		}
		out["gauge"] = map[string]any{"dataPoints": points}
	case dto.MetricType_HISTOGRAM:
		points := make([]map[string]any, 0, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			bounds := make([]float64, 0, len(h.GetBucket()))
			counts := make([]string, 0, len(h.GetBucket())+1)
			prev := uint64(0)
			for _, b := range h.GetBucket() {
				if math.IsInf(b.GetUpperBound(), +1) {
					continue
				}
				bounds = append(bounds, b.GetUpperBound())
				c := b.GetCumulativeCount()
				counts = append(counts, strconv.FormatUint(c-prev, 10))
				prev = c
			}
			counts = append(counts, strconv.FormatUint(h.GetSampleCount()-prev, 10))
			points = append(points, map[string]any{
				"attributes":     labelAttrs(m),
				"timeUnixNano":   nowNano,
				"count":          strconv.FormatUint(h.GetSampleCount(), 10),
				"sum":            h.GetSampleSum(),
				"bucketCounts":   counts,
				"explicitBounds": bounds,
			})
		}
		out["histogram"] = map[string]any{
			"dataPoints":             points,
			"aggregationTemporality": 2,
		}
	default:
		return nil
	}
	return out
}

func labelAttrs(m *dto.Metric) []map[string]any {
	out := make([]map[string]any, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		out = append(out, map[string]any{"key": lp.GetName(), "value": map[string]any{"stringValue": lp.GetValue()}})
	}
	return out
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
