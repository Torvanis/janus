// Package telemetry owns Prometheus instrumentation.
//
// Cardinality contract: no metric may carry a user_id, token_id, or client_ip
// label. Per-user analytics are served from the usage-event table instead. This
// keeps the active series count bounded by models × upstreams × modalities.
package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ForbiddenLabels are rejected by the cardinality guard test.
var ForbiddenLabels = []string{"user_id", "token_id", "client_ip", "request_id", "session_id"}

// Metrics is the registered instrument set.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal      *prometheus.CounterVec
	ModelRequestsTotal *prometheus.CounterVec
	GatewayLatency     *prometheus.HistogramVec
	TTFB               *prometheus.HistogramVec
	UpstreamLatency    *prometheus.HistogramVec
	TokensIn           *prometheus.CounterVec
	TokensOut          *prometheus.CounterVec
	TokensCached       *prometheus.CounterVec
	// Prompt-cache write tokens, split by the TTL bucket the provider billed
	// (Anthropic reports 5-minute and 1-hour cache_creation buckets).
	TokensCacheWrite5m *prometheus.CounterVec
	TokensCacheWrite1h *prometheus.CounterVec
	CostTotal          *prometheus.CounterVec
	QuotaBreaches      *prometheus.CounterVec
	QuotaCheckLatency  prometheus.Histogram
	PolicyRejections   *prometheus.CounterVec
	// SecgwViolations counts Security Gateway matches by check kind, the
	// action taken and the direction, so a dashboard can show observe-mode
	// volume before a policy is switched to block.
	SecgwViolations  *prometheus.CounterVec
	UpstreamErrors   *prometheus.CounterVec
	TokenCacheHits   prometheus.Counter
	TokenCacheMisses prometheus.Counter
	// StreamQuotaCheckErrors counts mid-stream hard-kill quota re-checks that
	// failed (storage errors). The stream deliberately continues (fail-open:
	// it was admitted by a fail-closed pre-flight check), so this counter is
	// the operator's only signal that hard-kill enforcement is degraded.
	StreamQuotaCheckErrors prometheus.Counter
	// UnmeteredRequests counts successful media (audio/image/video)
	// responses that carried no usage the adapter could read, so they were
	// recorded with zero tokens and zero cost (usage.AccountingUnmetered).
	// Every increment is a configuration gap on the labelled upstream/model
	// — the provider's native cost signal is not being extracted — and the
	// counter is the operator's signal to fix it before the gap compounds.
	UnmeteredRequests *prometheus.CounterVec
	// ManagedModelFallbacks counts requests a managed alias sent to its
	// fallback model, by alias and the failure mode that triggered it.
	ManagedModelFallbacks *prometheus.CounterVec
	ActiveUsers           prometheus.Gauge
	ConcurrentRequests    prometheus.Gauge
	StreamsActive         prometheus.Gauge
	DiscoveryRuns         *prometheus.CounterVec
	PurgeDeletedRows      *prometheus.CounterVec
	AlertDispatch         *prometheus.CounterVec
	BuildInfo             *prometheus.GaugeVec
}

// New registers every instrument on a private registry.
func New(version, sha string) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg}

	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(c)
		return c
	}
	histogram := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
		reg.MustRegister(h)
		return h
	}
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
		reg.MustRegister(g)
		return g
	}
	plainCounter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
		reg.MustRegister(c)
		return c
	}

	latencyBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

	m.RequestsTotal = counter("janus_requests_total", "Total HTTP requests handled by the gateway.", "method", "status", "endpoint", "modality", "upstream")
	m.ModelRequestsTotal = counter("janus_model_requests_total", "Proxied requests by model.", "model", "modality", "upstream", "status_class")
	m.GatewayLatency = histogram("janus_gateway_latency_seconds", "End-to-end gateway latency.", latencyBuckets, "endpoint", "modality")
	m.TTFB = histogram("janus_ttfb_seconds", "Time to first byte from the upstream.", latencyBuckets, "upstream")
	m.UpstreamLatency = histogram("janus_upstream_latency_seconds", "Upstream call latency.", latencyBuckets, "upstream")
	m.TokensIn = counter("janus_tokens_in_total", "Input tokens consumed.", "model", "modality", "upstream")
	m.TokensOut = counter("janus_tokens_out_total", "Output tokens generated.", "model", "modality", "upstream")
	m.TokensCached = counter("janus_tokens_cached_total", "Cached input tokens served by the upstream.", "model", "modality", "upstream")
	m.TokensCacheWrite5m = counter("janus_tokens_cache_write_5m_total", "Prompt-cache write tokens billed at the 5-minute TTL rate.", "model", "modality", "upstream")
	m.TokensCacheWrite1h = counter("janus_tokens_cache_write_1h_total", "Prompt-cache write tokens billed at the 1-hour TTL rate.", "model", "modality", "upstream")
	m.CostTotal = counter("janus_cost_usd_total", "Attributed spend in USD.", "model", "upstream")
	m.QuotaBreaches = counter("janus_quota_breaches_total", "Quota breaches by metric.", "metric", "subject_type")
	m.PolicyRejections = counter("janus_policy_rejections_total", "Requests refused by gateway policy.", "code")
	m.SecgwViolations = counter("janus_secgw_violations_total", "Security Gateway matches by check kind, action and direction.", "kind", "action", "direction")
	m.UpstreamErrors = counter("janus_upstream_errors_total", "Upstream failures.", "upstream", "error_code")
	m.ManagedModelFallbacks = counter("janus_managed_model_fallbacks_total", "Requests served by a managed model's fallback, by alias and trigger.", "alias", "trigger")
	m.UnmeteredRequests = counter("janus_unmetered_requests_total", "Media responses recorded with no usage because the upstream reported none (configuration gap).", "upstream", "model", "modality")
	m.DiscoveryRuns = counter("janus_discovery_runs_total", "Model discovery runs.", "upstream", "result")
	m.PurgeDeletedRows = counter("janus_purge_deleted_rows_total", "Rows removed by the retention purge job.", "table")
	m.AlertDispatch = counter("janus_alert_dispatch_total", "Alert deliveries attempted.", "channel", "result")
	m.BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "janus_build_info", Help: "Build metadata."}, []string{"version", "sha"})
	reg.MustRegister(m.BuildInfo)
	m.BuildInfo.WithLabelValues(version, sha).Set(1)

	m.QuotaCheckLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "janus_quota_check_latency_seconds", Help: "Quota evaluation latency.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1},
	})
	reg.MustRegister(m.QuotaCheckLatency)

	m.TokenCacheHits = plainCounter("janus_token_cache_hits_total", "Bearer-token cache hits.")
	m.TokenCacheMisses = plainCounter("janus_token_cache_misses_total", "Bearer-token cache misses.")
	m.StreamQuotaCheckErrors = plainCounter("janus_stream_quota_check_errors_total",
		"Mid-stream hard-kill quota checks that failed on storage errors; the stream continues (fail-open).")
	m.ActiveUsers = gauge("janus_active_users", "Distinct users seen in the last 15 minutes.")
	m.ConcurrentRequests = gauge("janus_concurrent_requests", "In-flight requests.")
	m.StreamsActive = gauge("janus_streams_active", "In-flight streaming responses.")

	m.initZeroSeries()
	return m
}

// initZeroSeries creates the label combinations that are known ahead of time.
//
// Prometheus only exports a labelled series once it has been observed, so
// without this a fresh gateway would report "no data" for quota breaches and
// policy rejections rather than an honest zero — and the exported metric set
// would change shape as traffic arrived.
func (m *Metrics) initZeroSeries() {
	policyCodes := []string{
		"policy.quota_exceeded", "policy.user_disabled", "policy.model_not_granted",
		"policy.endpoint_blocked", "policy.token_invalid", "policy.rate_limit",
		"upstream.unavailable", "upstream.rate_limit", "invalid_request_error", "server_error",
	}
	for _, code := range policyCodes {
		m.PolicyRejections.WithLabelValues(code)
	}
	for _, metric := range []string{"tokens_in", "tokens_out", "cost_usd", "requests"} {
		for _, subject := range []string{"user", "team"} {
			m.QuotaBreaches.WithLabelValues(metric, subject)
		}
	}
	for _, table := range []string{"usage_event", "audit_log", "docs_feedback", "web_session", "quota_alert_state", "oidc_state"} {
		m.PurgeDeletedRows.WithLabelValues(table)
	}
	for _, channel := range []string{"in_app", "email", "webhook"} {
		for _, result := range []string{"ok", "error"} {
			m.AlertDispatch.WithLabelValues(channel, result)
		}
	}
}

// Handler exposes the Prometheus scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveGateway records one completed HTTP request.
func (m *Metrics) ObserveGateway(endpoint, modality, upstream, method string, status int, d time.Duration) {
	m.RequestsTotal.WithLabelValues(method, strconv.Itoa(status), endpoint, modality, upstream).Inc()
	m.GatewayLatency.WithLabelValues(endpoint, modality).Observe(d.Seconds())
}

// StatusClass buckets a status code for low-cardinality labels.
func StatusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}
