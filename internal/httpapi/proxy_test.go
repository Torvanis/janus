package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/usage"
)

// harness is a fully wired gateway backed by an embedded database and a fake
// upstream, so the proxy path is exercised end to end without a network.
type harness struct {
	t        *testing.T
	server   *Server
	handler  http.Handler
	store    *store.Store
	upstream *httptest.Server

	user  *store.User
	token string
	model *store.Model

	upstreamRequests []recordedRequest

	// upstreamReply, when set, replaces the default non-streaming JSON
	// reply; streamFrames, when set, replaces the default SSE frames. Both
	// exist so egress tests can make the fake model emit specific text.
	upstreamReply string
	streamFrames  []string
}

type recordedRequest struct {
	Path   string
	Body   []byte
	Header http.Header
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t}

	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.upstreamRequests = append(h.upstreamRequests, recordedRequest{Path: r.URL.Path, Body: body, Header: r.Header.Clone()})

		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"},{"id":"text-embedding-3-small"}]}`))
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			frames := h.streamFrames
			if frames == nil {
				frames = []string{
					`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
					`data: {"choices":[{"delta":{"content":"lo"}}]}`,
					`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
					`data: [DONE]`,
				}
			}
			for _, frame := range frames {
				_, _ = w.Write([]byte(frame + "\n\n"))
				flusher.Flush()
			}
		default:
			payload := []byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":20}}}`)
			if h.upstreamReply != "" {
				payload = []byte(h.upstreamReply)
			}
			w.Header().Set("Content-Type", "application/json")
			// Behave like a real engine: honour gzip content negotiation. The
			// gateway must still deliver a decodable body and meter real token
			// counts (regression: the client's Accept-Encoding was forwarded
			// verbatim, which disabled Go's transparent decompression).
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
				gz := gzip.NewWriter(w)
				_, _ = gz.Write(payload)
				_ = gz.Close()
				return
			}
			_, _ = w.Write(payload)
		}
	}))
	t.Cleanup(h.upstream.Close)

	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h.store = db

	key := bytes.Repeat([]byte("k"), 32)
	cipher, err := crypto.New(key)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	metrics := telemetry.New("test", "test")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config.Config{
		PublicURL: "http://janus.test", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour,
		UpstreamTotalTimeout: 10 * time.Second, MaxResponseBytes: 10 << 20,
		UsageRetentionDays: 365, AuditRetentionDays: 730, DevAuthEnabled: true,
		BuildVersion: "test", DiscoveryInterval: time.Hour,
	}
	alerts := alerting.New(db, alerting.SMTPConfig{}, h.upstream.Client(), metrics, logger)
	quotaEngine := quota.NewEngine(db)

	h.server = &Server{
		Config: cfg, Store: db, Sessions: auth.NewMemorySessionStore(time.Hour, time.Hour),
		Cipher: cipher, Quota: quotaEngine, Metrics: metrics, Alerts: alerts,
		Discovery: discovery.New(db, cipher, h.upstream.Client(), metrics, alerts, logger, time.Hour),
		Logger:    logger, StartedAt: time.Now(),
	}
	h.handler = h.server.Handler()

	// Seed: an upstream, an enabled and priced model, a user, a grant, a token.
	up, err := db.CreateUpstream(ctx, "test-upstream", "openai_compatible", h.upstream.URL, "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := db.UpsertDiscoveredModel(ctx, up.ID, "test-model", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := db.ListModels(ctx, store.ModelFilter{})
	if err != nil || len(models) == 0 {
		t.Fatalf("list models: %v", err)
	}
	h.model = models[0]
	if err := db.SetModelRates(ctx, h.model.ID, store.RateCard{
		RateInNano: 10 * usage.NanoPerUSD, RateOutNano: 30 * usage.NanoPerUSD, RateCachedNano: 1 * usage.NanoPerUSD,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("set rates: %v", err)
	}
	if err := db.SetModelStatus(ctx, h.model.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	user, _, err := db.UpsertUserFromIdentity(ctx, "sub-test", "tester@example.com", "Tester", true, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	h.user = user
	if _, err := db.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	_, plaintext, err := db.CreateToken(ctx, user.ID, "test token")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	h.token = plaintext
	return h
}

func (h *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// waitForUsageEvents polls until the asynchronous metering writer has landed.
func (h *harness) waitForUsageEvents(want int) []*store.UsageEvent {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// IncludeInternal: the gateway's own classifier calls are hidden from
		// the request log by default, but tests assert on them (they are how
		// classifier cost is metered), so the harness sees everything.
		events, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 100, IncludeInternal: true})
		if err != nil {
			h.t.Fatalf("list usage events: %v", err)
		}
		if len(events) >= want {
			return events
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d usage events", want)
	return nil
}

// TestDrainWaitsForAsyncUsageWriters is the documented shutdown guarantee:
// usage events and quota-ledger updates are written by post-response
// goroutines, and Server.Drain must not return until they have landed — so a
// SIGTERM arriving right after the last response cannot lose its metering.
// No polling here on purpose: if Drain returns early, the assertions race the
// writer and fail.
func TestDrainWaitsForAsyncUsageWriters(t *testing.T) {
	h := newHarness(t)

	// A successful completion → recordEvent's async writer.
	rec := h.do("POST", "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat completion = %d, want 200", rec.Code)
	}
	// A rejected request (unknown model) → rejectProxyEvent's async writer.
	rec = h.do("POST", "/v1/chat/completions", map[string]any{"model": "no-such-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code == http.StatusOK {
		t.Fatalf("unknown model unexpectedly succeeded")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Immediately after Drain both events must be durable — no waiting allowed.
	events, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("usage events after Drain = %d, want 2 (success + rejection)", len(events))
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestProxyForwardsAndMeters(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Janus-Request-ID") == "" {
		t.Error("every response must carry a correlation id")
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if last.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", last.Path)
	}
	if !bytes.Contains(last.Body, []byte(`"messages"`)) {
		t.Error("the request body was not forwarded verbatim")
	}

	events := h.waitForUsageEvents(1)
	event := events[0]
	if event.TokensIn != 100 || event.TokensOut != 40 || event.TokensCached != 20 {
		t.Errorf("token counts = (%d,%d,%d), want (100,40,20) from the upstream usage block",
			event.TokensIn, event.TokensOut, event.TokensCached)
	}
	if event.Modality != "chat" {
		t.Errorf("modality = %q, want chat", event.Modality)
	}
	if event.AccountingMode != usage.AccountingUpstream {
		t.Errorf("accounting mode = %q, want %q", event.AccountingMode, usage.AccountingUpstream)
	}
	// 80 fresh @ $10/M + 20 cached @ $1/M + 40 out @ $30/M
	wantCost := int64(80*10*usage.NanoPerUSD+20*1*usage.NanoPerUSD+40*30*usage.NanoPerUSD) / int64(usage.TokensPerRateUnit)
	if event.CostNano != wantCost {
		t.Errorf("cost = %d nano-USD, want %d", event.CostNano, wantCost)
	}
	if event.HTTPStatus != http.StatusOK {
		t.Errorf("recorded status = %d, want 200", event.HTTPStatus)
	}
}

// TestProxyGzipRequestingClientGetsUsableResponse proves a client that asks for
// gzip (the OpenAI SDK / httpx default) receives a decodable body and is metered
// from the upstream usage block (regression: the client's Accept-Encoding was
// forwarded upstream, disabling Go's transparent decompression, while
// Content-Encoding was dropped from the response — clients received raw gzip
// bytes with no encoding header and metering fell back to byte estimation).
func TestProxyGzipRequestingClientGetsUsableResponse(t *testing.T) {
	h := newHarness(t)

	encoded, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("response carries Content-Encoding %q but the gateway serves identity bodies", ce)
	}
	payload := decodeBody(t, rec) // fails loudly if the body is still gzip bytes
	if payload["id"] != "cmpl-1" {
		t.Errorf("decoded body id = %v, want cmpl-1", payload["id"])
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if got := last.Header.Get("Accept-Encoding"); got == "gzip, deflate" {
		t.Errorf("the client's Accept-Encoding %q was forwarded verbatim; the transport must negotiate encoding itself", got)
	}

	events := h.waitForUsageEvents(1)
	event := events[0]
	if event.TokensIn != 100 || event.TokensOut != 40 || event.TokensCached != 20 {
		t.Errorf("token counts = (%d,%d,%d), want (100,40,20) parsed from the decompressed usage block",
			event.TokensIn, event.TokensOut, event.TokensCached)
	}
	if event.AccountingMode != usage.AccountingUpstream {
		t.Errorf("accounting mode = %q, want %q (byte fallback means the body was not parseable)",
			event.AccountingMode, usage.AccountingUpstream)
	}
}

// TestProxyCapturesRequestMetadata locks the contract: every allowlisted metadata
// field is captured on the usage event, and only metadata — bodies are never
// persisted.
func TestProxyCapturesRequestMetadata(t *testing.T) {
	h := newHarness(t)

	encoded, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("User-Agent", "openai-python/1.51.0")
	req.Header.Set("Referer", "https://internal.example.com/tools")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	event := h.waitForUsageEvents(1)[0]
	if event.UserAgent != "openai-python/1.51.0" {
		t.Errorf("user agent = %q, want the client's", event.UserAgent)
	}
	if event.Referer != "https://internal.example.com/tools" {
		t.Errorf("referer = %q, want the client's", event.Referer)
	}
	if event.EndpointPath != "/v1/chat/completions" || event.HTTPMethod != http.MethodPost {
		t.Errorf("endpoint/method = %q %q, want /v1/chat/completions POST", event.EndpointPath, event.HTTPMethod)
	}
	if event.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop from the upstream response", event.FinishReason)
	}
	if event.RequestBytes != int64(len(encoded)) {
		t.Errorf("request bytes = %d, want %d", event.RequestBytes, len(encoded))
	}
	if event.ResponseBytes <= 0 {
		t.Error("response bytes were not recorded")
	}
	if event.HTTPStatus != http.StatusOK {
		t.Errorf("status = %d, want 200", event.HTTPStatus)
	}
	if event.ClientIP == "" {
		t.Error("client IP was not recorded")
	}
	if event.UserID != h.user.ID {
		t.Errorf("user id = %q, want %q", event.UserID, h.user.ID)
	}
	if event.CostNano <= 0 {
		t.Error("cost was not computed at record time")
	}
}

// TestProxyRecordsModalityPerEndpoint locks the contract: modality is classified at
// request time and stored explicitly on the event, not re-derived from the
// path at query time.
func TestProxyRecordsModalityPerEndpoint(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/embeddings", map[string]any{
		"model": "test-model", "input": "hello world",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	event := h.waitForUsageEvents(1)[0]
	if event.Modality != "embedding" {
		t.Errorf("modality = %q, want embedding (captured explicitly)", event.Modality)
	}
}

// TestProxyBillingSuccessfulRequestNoUsageBlock proves a 2xx response whose
// upstream reports NO usage block is still metered via the byte-count
// fallback — i.e. the failed-request guard does not turn billing off
// wholesale.
func TestProxyBillingSuccessfulRequestNoUsageBlock(t *testing.T) {
	h := newHarness(t)
	noUsage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-nousage","choices":[{"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(noUsage.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, noUsage.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	event := h.waitForUsageEvents(1)[0]
	if event.AccountingMode != usage.AccountingBytes {
		t.Errorf("accounting mode = %q, want %q (successful responses without a usage block are byte-metered)",
			event.AccountingMode, usage.AccountingBytes)
	}
	if event.TokensIn <= 0 || event.TokensOut <= 0 {
		t.Errorf("token counts = (%d,%d), want both > 0 from the byte-count fallback",
			event.TokensIn, event.TokensOut)
	}
	if event.CostNano <= 0 {
		t.Errorf("cost = %d nano-USD, want > 0 (a successful request must still be billed)", event.CostNano)
	}
}

// TestProxyBillingFailedRequestNonStreaming locks the P1 billing fix: an
// upstream ERROR response ran no inference, so its error body must never be
// byte-metered into tokens and cost (regression: handleProxy called
// applyUsage unconditionally, so 4xx/5xx error text was billed as if it were
// model output). The event is still recorded for observability — status,
// latency, bytes — just marked not_billable.
func TestProxyBillingFailedRequestNonStreaming(t *testing.T) {
	h := newHarness(t)
	errorBody := []byte(`{"error":{"message":"Internal Server Error: chat template raised","type":"server_error"}}`)
	// Non-vacuity check: the old unguarded code would have byte-estimated
	// this body into a non-zero token count — so this test fails on it.
	if usage.EstimateTokensFromBytes(int64(len(errorBody))) <= 0 {
		t.Fatalf("error body of %d bytes estimates to zero tokens; the test cannot prove the fix", len(errorBody))
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(errorBody)
	}))
	t.Cleanup(failing.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, failing.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("proxy returned %d, want 500 relayed from the upstream", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), errorBody) {
		t.Errorf("client body = %q, want the upstream error passed through unchanged", rec.Body.String())
	}

	event := h.waitForUsageEvents(1)[0]
	if event.TokensIn != 0 || event.TokensOut != 0 {
		t.Errorf("token counts = (%d,%d), want (0,0): the error body must not be metered as tokens",
			event.TokensIn, event.TokensOut)
	}
	if event.CostNano != 0 {
		t.Errorf("cost = %d nano-USD, want 0: no inference ran", event.CostNano)
	}
	if event.AccountingMode != usage.AccountingNone {
		t.Errorf("accounting mode = %q, want %q (non-billed must be distinguishable from billed-at-zero)",
			event.AccountingMode, usage.AccountingNone)
	}
	if event.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500 (failures must still be recorded)", event.HTTPStatus)
	}
	if event.ResponseBytes != int64(len(errorBody)) {
		t.Errorf("response bytes = %d, want %d (observability fields must survive the guard)",
			event.ResponseBytes, len(errorBody))
	}
}

// TestProxyBillingFailedRequestStreaming covers the streaming relay path of
// the same P1 fix: relayBuffered self-guards, but a stream=true request that
// hits an upstream error went through the unguarded trailing applyUsage call.
func TestProxyBillingFailedRequestStreaming(t *testing.T) {
	h := newHarness(t)
	errorBody := []byte(`{"error":{"message":"model is loading","type":"server_error"}}`)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(errorBody)
	}))
	t.Cleanup(failing.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, failing.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream": true,
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("proxy returned %d, want 500 relayed from the upstream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("client body = %q, want the upstream error envelope relayed", rec.Body.String())
	}

	event := h.waitForUsageEvents(1)[0]
	if event.TokensIn != 0 || event.TokensOut != 0 || event.CostNano != 0 {
		t.Errorf("tokens/cost = (%d,%d,%d), want all zero for a failed stream",
			event.TokensIn, event.TokensOut, event.CostNano)
	}
	if event.AccountingMode != usage.AccountingNone {
		t.Errorf("accounting mode = %q, want %q", event.AccountingMode, usage.AccountingNone)
	}
	if event.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500", event.HTTPStatus)
	}
}

// TestProxyAttachmentCountJSONImageParts locks the P2 telemetry fix: a vision
// request sent as OpenAI-style JSON content parts is countable in usage data
// (regression: attachment_count was only populated for multipart uploads, so
// JSON image_url parts logged as attachment_count=0 and multimodal traffic
// was indistinguishable from plain text). Modality deliberately stays "chat":
// it means endpoint class, and "image" means image GENERATION.
func TestProxyAttachmentCountJSONImageParts(t *testing.T) {
	h := newHarness(t)

	// Subtests share one harness, so events are matched by request id rather
	// than by list position (created_at ties within a second are unordered).
	eventFor := func(t *testing.T, rec *httptest.ResponseRecorder, want int) *store.UsageEvent {
		t.Helper()
		requestID := rec.Header().Get("X-Janus-Request-ID")
		if requestID == "" {
			t.Fatal("response carries no X-Janus-Request-ID")
		}
		for _, event := range h.waitForUsageEvents(want) {
			if event.RequestID == requestID {
				return event
			}
		}
		t.Fatalf("no usage event recorded for request %s", requestID)
		return nil
	}

	t.Run("two image_url parts count as two attachments", func(t *testing.T) {
		rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "test-model",
			"messages": []map[string]any{{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "What's in these images?"},
					{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
					{"type": "image_url", "image_url": map[string]any{"url": "https://files.example.com/b.png"}},
				},
			}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("vision request returned %d: %s", rec.Code, rec.Body.String())
		}
		event := eventFor(t, rec, 1)
		if event.AttachmentCount != 2 {
			t.Errorf("attachment count = %d, want 2 (one per image_url part)", event.AttachmentCount)
		}
		if event.Modality != "chat" {
			t.Errorf("modality = %q, want chat (modality means endpoint class, not input type)", event.Modality)
		}
	})

	t.Run("plain text chat counts zero attachments", func(t *testing.T) {
		rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("chat request returned %d: %s", rec.Code, rec.Body.String())
		}
		event := eventFor(t, rec, 2)
		if event.AttachmentCount != 0 {
			t.Errorf("attachment count = %d, want 0 for text-only chat", event.AttachmentCount)
		}
	})

	t.Run("multipart uploads still count form sections", func(t *testing.T) {
		boundary := "janusboundary"
		var buf bytes.Buffer
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n")
		buf.WriteString("Content-Type: audio/wav\r\n\r\naudio-bytes\r\n")
		buf.WriteString("--" + boundary + "--\r\n")

		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(buf.Bytes()))
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("multipart request returned %d: %s", rec.Code, rec.Body.String())
		}
		event := eventFor(t, rec, 3)
		if event.AttachmentCount != 2 {
			t.Errorf("attachment count = %d, want 2 form-data sections (multipart path unchanged)", event.AttachmentCount)
		}
	})
}

// TestCountJSONImageParts pins the helper's edge cases: only part TYPES are
// inspected, and every non-vision shape counts zero.
func TestCountJSONImageParts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"plain string content", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 0},
		{"one image part", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`, 1},
		{"mixed parts across messages", `{"messages":[{"content":[{"type":"text","text":"a"},{"type":"image_url"}]},{"content":[{"type":"image_url"}]}]}`, 2},
		{"no messages key", `{"input":"hello"}`, 0},
		{"malformed json", `{"messages":[`, 0},
		{"empty body", ``, 0},
		{"non-object body", `[1,2,3]`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countJSONImageParts([]byte(tc.body)); got != tc.want {
				t.Errorf("countJSONImageParts(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestTrustedProxyGatingIPv6 proves an IPv6 peer that is NOT a configured
// trusted proxy has its X-Forwarded-For header ignored (regression: stripPort
// used LastIndex(":") and left the brackets on "[::1]:port", so the comparison
// against ClientIP always differed and every IPv6 peer was treated as trusted).
func TestTrustedProxyGatingIPv6(t *testing.T) {
	if got := stripPort("[::1]:52011"); got != "::1" {
		t.Fatalf("stripPort(\"[::1]:52011\") = %q, want \"::1\"", got)
	}
	if got := stripPort("10.0.0.5:1234"); got != "10.0.0.5" {
		t.Fatalf("stripPort(\"10.0.0.5:1234\") = %q, want \"10.0.0.5\"", got)
	}
	if got := stripPort("::1"); got != "::1" {
		t.Fatalf("stripPort(\"::1\") = %q, want \"::1\" (portless addresses pass through)", got)
	}

	h := newHarness(t)
	_, trustedNet, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse trusted CIDR: %v", err)
	}
	h.server.Config.TrustedProxies = []*net.IPNet{trustedNet}

	encoded, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "[2001:db8::7]:40000" // IPv6 peer outside the trusted range
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	events := h.waitForUsageEvents(1)
	if events[0].XForwardedFor != "" {
		t.Errorf("X-Forwarded-For %q was recorded from an untrusted IPv6 peer; trusted-proxy gating is required",
			events[0].XForwardedFor)
	}
	if events[0].ClientIP != "2001:db8::7" {
		t.Errorf("client IP = %q, want the direct peer 2001:db8::7", events[0].ClientIP)
	}
}

// TestProxyCostHeaderReflectsMeteredCost proves the X-Janus-Cost-USD header on
// buffered responses carries the same cost the metering pipeline records
// (regression: the header was hardcoded to 0).
func TestProxyCostHeaderReflectsMeteredCost(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	header := rec.Header().Get("X-Janus-Cost-USD")
	if header == "" {
		t.Fatal("buffered proxy responses must carry X-Janus-Cost-USD")
	}
	got, err := strconv.ParseFloat(header, 64)
	if err != nil {
		t.Fatalf("X-Janus-Cost-USD %q is not a number: %v", header, err)
	}
	if got <= 0 {
		t.Fatalf("X-Janus-Cost-USD = %v, want the real request cost, not zero", got)
	}
	// 80 fresh @ $10/M + 20 cached @ $1/M + 40 out @ $30/M — the same figure
	// the usage event records.
	wantCost := int64(80*10*usage.NanoPerUSD+20*1*usage.NanoPerUSD+40*30*usage.NanoPerUSD) / int64(usage.TokensPerRateUnit)
	if want := usage.USD(wantCost); got != want {
		t.Errorf("X-Janus-Cost-USD = %v, want %v", got, want)
	}
	events := h.waitForUsageEvents(1)
	if usage.USD(events[0].CostNano) != got {
		t.Errorf("header cost %v differs from metered cost %v", got, usage.USD(events[0].CostNano))
	}
}

// TestProxyForwardsOversizedBodiesUnbuffered proves that a request body larger
// than the in-memory buffer reaches the upstream byte-for-byte instead of being
// silently truncated (regression: bodies >maxInMemoryBody were forwarded
// corrupted with a wrong Content-Length).
func TestProxyForwardsOversizedBodiesUnbuffered(t *testing.T) {
	restore := maxInMemoryBody
	maxInMemoryBody = 64 << 10
	t.Cleanup(func() { maxInMemoryBody = restore })

	h := newHarness(t)

	// A multipart payload ~4x the buffer, with the model field in the prologue.
	boundary := "janusboundary"
	var buf bytes.Buffer
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n")
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n")
	buf.WriteString("Content-Type: audio/wav\r\n\r\n")
	buf.Write(bytes.Repeat([]byte("abcdefgh"), (256<<10)/8))
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	payload := buf.Bytes()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("oversized upload returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	got := h.upstreamRequests[len(h.upstreamRequests)-1].Body
	if len(got) != len(payload) {
		t.Fatalf("upstream received %d bytes, want %d (body was truncated)", len(got), len(payload))
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("upstream body differs from what the client sent")
	}

	events := h.waitForUsageEvents(1)
	if events[0].RequestBytes != int64(len(payload)) {
		t.Errorf("metered request bytes = %d, want %d", events[0].RequestBytes, len(payload))
	}
}

// TestProxyRelaysOversizedBufferedResponse proves a non-streaming response
// larger than MaxResponseBytes reaches the client complete and byte-for-byte
// (regression: relayBuffered read io.LimitReader(body, cap) and forwarded the
// truncated prefix under the upstream's 200, with Content-Length stripped —
// clients received silently corrupted JSON with no error signal).
func TestProxyRelaysOversizedBufferedResponse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A response ~8x the cap, shaped like a base64 image-generation payload.
	oversized := []byte(`{"created":1,"data":[{"b64_json":"` + strings.Repeat("A", 8<<10) + `"}]}`)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(big.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, big.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}
	h.server.Config.MaxResponseBytes = 1 << 10

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("oversized response returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, oversized) {
		t.Fatalf("client received %d bytes, want the full %d-byte upstream body (truncated or corrupted)",
			len(got), len(oversized))
	}
	// The truncated prefix is unparseable, so metering must fall back to byte
	// counting over the FULL relayed body, not the buffered prefix.
	event := h.waitForUsageEvents(1)[0]
	if event.ResponseBytes != int64(len(oversized)) {
		t.Errorf("metered response bytes = %d, want %d", event.ResponseBytes, len(oversized))
	}
	if event.AccountingMode != usage.AccountingBytes {
		t.Errorf("accounting mode = %q, want %q for an unparsed oversized body", event.AccountingMode, usage.AccountingBytes)
	}
}

// TestStreamQuotaCheckFailsOpenWithTelemetry locks the documented posture for
// mid-stream hard-kill quota checks during a storage outage: the stream
// continues (fail-open — it was admitted by the fail-closed pre-flight check)
// but never silently (regression: streamExceededLimit returned nil on
// CheckWithPending errors with no log and no metric, so operators could not
// tell hard-kill enforcement was degraded).
func TestStreamQuotaCheckFailsOpenWithTelemetry(t *testing.T) {
	h := newHarness(t)
	providerAdapter, err := adapter.Get("openai_compatible")
	if err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	event := &store.UsageEvent{RequestID: "req-test", ModelID: h.model.ID, RequestBytes: 64}

	// Sever the storage the quota engine reads from.
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	before := testutil.ToFloat64(h.server.Metrics.StreamQuotaCheckErrors)
	breached := h.server.streamExceededLimit(context.Background(), event, providerAdapter.NewStreamCollector(), 128, quota.UserSubject(h.user.ID, nil), usage.Rates{})
	if breached != nil {
		t.Fatalf("streamExceededLimit during a storage outage = %+v, want nil (fail-open for admitted streams)", breached)
	}
	if got := testutil.ToFloat64(h.server.Metrics.StreamQuotaCheckErrors); got != before+1 {
		t.Fatalf("janus_stream_quota_check_errors_total = %v after a failed check, want %v (silent fail-open)", got, before+1)
	}
}

// TestProxyStreamCapsNewlineFreeFlood proves MaxResponseBytes holds even when
// the upstream never emits a newline (regression: relayStream's
// ReadBytes('\n') accumulated the whole flood in memory and relayed it to the
// client because the cap check only ran between complete lines — a
// resource-exhaustion vector for a misbehaving or compromised upstream).
func TestProxyStreamCapsNewlineFreeFlood(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const cap = int64(1 << 10)
	floodSize := 64 << 10 // 64x the cap, not one newline in it
	flood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 1<<10)
		flusher := w.(http.Flusher)
		for written := 0; written < floodSize; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return // gateway hung up: exactly what the cap should cause
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(flood.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, flood.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}
	h.server.Config.MaxResponseBytes = cap

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream": true,
	})
	// The client may receive at most cap+1 flood bytes plus one bounded
	// in-band error frame announcing the cut (documented: a cap cut must
	// never look like a network failure).
	const errorFrameAllowance = 512
	if got := int64(rec.Body.Len()); got > cap+1+errorFrameAllowance {
		t.Fatalf("client received %d bytes of a newline-free flood, want at most cap+1+frame = %d (cap bypassed)", got, cap+1+errorFrameAllowance)
	}
	if body := rec.Body.String(); !strings.Contains(body, "policy.response_too_large") {
		t.Errorf("capped stream carries no in-band error frame; tail: %q", body[max(0, len(body)-200):])
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ResponseBytes > cap+1 {
		t.Errorf("metered response bytes = %d, want at most cap+1 = %d", event.ResponseBytes, cap+1)
	}
	if event.ErrorCode != "policy.response_too_large" {
		t.Errorf("event.error_code = %q, want policy.response_too_large (the cut must be attributable in the request log)", event.ErrorCode)
	}
}

func TestProxyStreamsServerSentEvents(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: {\"choices\"") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream was not relayed verbatim: %q", body)
	}
	events := h.waitForUsageEvents(1)
	if events[0].TokensIn != 11 || events[0].TokensOut != 7 {
		t.Errorf("streamed token counts = (%d,%d), want (11,7) from the final frame",
			events[0].TokensIn, events[0].TokensOut)
	}
	if !events[0].Streaming {
		t.Error("the event should be flagged as streaming")
	}
}

// TestProxyHardKillCutsStreamMidFlight proves a hard-kill quota severs an
// in-flight SSE stream once recorded consumption crosses the limit, without
// waiting for the upstream to finish. It also exercises the throttled
// re-evaluation path (quota checks are floored at hardKillRecheckInterval so
// long streams no longer issue several DB queries per relayed line).
func TestProxyHardKillCutsStreamMidFlight(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A slow upstream: usage is reported on the very first frame (as Anthropic
	// does), then frames drip for ~3s before the [DONE] marker.
	const totalFrames = 150
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < totalFrames; i++ {
			frame := `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`
			if _, err := w.Write([]byte(frame + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(slow.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, slow.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	// A hard-kill quota that is NOT yet breached, so the request is admitted.
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricTokensIn,
		Limit: 1000, Window: store.WindowDaily, BreachBehavior: store.BreachHardKill,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	// Mid-stream, consumption recorded elsewhere (e.g. a parallel request
	// finishing) blows the limit; the stream must be cut shortly after.
	go func() {
		time.Sleep(400 * time.Millisecond)
		if err := h.server.Quota.Record(ctx, quota.UserSubject(h.user.ID, nil), h.model.ID, quota.Delta{TokensIn: 5000, Requests: 1}); err != nil {
			t.Errorf("record breaching consumption: %v", err)
		}
	}()

	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("streaming proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatal("stream ran to completion; the hard-kill quota never cut it")
	}
	frames := strings.Count(body, "data: ")
	if frames == 0 {
		t.Fatal("no frames were relayed before the cut")
	}
	if frames >= totalFrames {
		t.Fatalf("all %d frames were relayed; the stream was not cut", frames)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("stream ran %v before the cut; hard-kill should sever within ~one recheck interval of the breach", elapsed)
	}
	assertHardKillCutIsAttributable(t, h, body)
}

// assertHardKillCutIsAttributable locks the documented contract for a deliberate
// stream cut: the client receives a final OpenAI-style SSE error frame naming
// policy.quota_exceeded with a reset_at, and the recorded usage event carries
// quota_violated + error_code so the request log can distinguish a hard-kill
// from a network failure (as the troubleshooting docs instruct users to check).
func assertHardKillCutIsAttributable(t *testing.T, h *harness, body string) {
	t.Helper()
	if !strings.Contains(body, `"code":"`+CodeQuotaExceeded+`"`) {
		t.Errorf("the cut stream carries no SSE error frame with code %s; a hard-kill must be explained in-band: %q",
			CodeQuotaExceeded, body)
	}
	if !strings.Contains(body, `"reset_at":"`) {
		t.Errorf("the SSE error frame must tell the caller when access resumes (reset_at): %q", body)
	}
	// The error frame must be the last data frame, after the relayed content.
	if idx := strings.LastIndex(body, "data: "); !strings.Contains(body[idx:], CodeQuotaExceeded) {
		t.Errorf("the error frame is not the final frame of the stream: %q", body[idx:])
	}
	event := h.waitForUsageEvents(1)[0]
	if !event.QuotaViolated {
		t.Error("a hard-killed stream must record quota_violated=true on its usage event")
	}
	if event.ErrorCode != CodeQuotaExceeded {
		t.Errorf("recorded error_code = %q, want %s", event.ErrorCode, CodeQuotaExceeded)
	}
}

// TestQuotaBreachMessageRollingVsCalendar locks the documented copy
// contract: calendar windows reset at one instant ("Access resumes at"),
// while rolling windows recover gradually, so their reset must be framed as
// an upper bound rather than a promise that can overstate the wait by up to
// the full window width.
func TestQuotaBreachMessageRollingVsCalendar(t *testing.T) {
	reset := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

	calendar := &quota.Status{
		Quota: &store.Quota{Window: store.WindowDaily, Metric: store.MetricRequests},
		Limit: 100, ResetAt: reset,
	}
	msg := quotaBreachMessage("Your", calendar)
	if !strings.Contains(msg, "Access resumes at 2026-01-02T15:00:00Z") {
		t.Errorf("calendar breach message = %q, want a fixed reset instant", msg)
	}

	rolling := &quota.Status{
		Quota: &store.Quota{Window: store.WindowRolling24, Metric: store.MetricRequests},
		Limit: 100, ResetAt: reset,
	}
	msg = quotaBreachMessage("Your", rolling)
	if strings.Contains(msg, "Access resumes at") {
		t.Errorf("rolling breach message promises a fixed reset it cannot keep: %q", msg)
	}
	if !strings.Contains(msg, "at the latest by 2026-01-02T15:00:00Z") {
		t.Errorf("rolling breach message = %q, want the reset framed as an upper bound", msg)
	}
}

// TestProxyHardKillSelfCutsStream proves a hard-kill quota is breached by the
// stream's OWN in-flight consumption, with no parallel request recording
// anything (regression: streamExceededLimit consulted only *recorded*
// consumption, which for the running stream is written post-flight, so a lone
// large stream was never cut by what it was itself consuming).
func TestProxyHardKillSelfCutsStream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// An upstream whose frames carry cumulative usage that grows well past the
	// quota limit while the stream is still in flight.
	const totalFrames = 150
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 1; i <= totalFrames; i++ {
			frame := `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":5,"completion_tokens":` + strconv.Itoa(i*10) + `}}`
			if _, err := w.Write([]byte(frame + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(slow.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, slow.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	// A hard-kill quota on output tokens. Nothing is recorded before or during
	// the request: only the stream's own usage can breach it. The upstream
	// reports ~500 output tokens within the first second of the stream.
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricTokensOut,
		Limit: 400, Window: store.WindowDaily, BreachBehavior: store.BreachHardKill,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("streaming proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatal("stream ran to completion; the hard-kill quota never counted the stream's own usage")
	}
	frames := strings.Count(body, "data: ")
	if frames == 0 {
		t.Fatal("no frames were relayed before the cut")
	}
	if frames >= totalFrames {
		t.Fatalf("all %d frames were relayed; the stream was not cut", frames)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("stream ran %v before the cut; the breach should sever within ~one recheck interval", elapsed)
	}
	assertHardKillCutIsAttributable(t, h, body)
}

// TestProxyHardKillCutsStreamWithoutMidflightUsage locks the streaming contract for upstreams
// that speak the OpenAI wire format, where the usage block arrives ONLY on the
// final SSE frame (stream_options.include_usage). Until that frame, the
// collector has nothing reported, so the hard-kill check must fall back to
// byte-count estimation of the in-flight stream — otherwise a hard-kill quota
// can never sever an openai_compatible/vlm/llama_cpp stream mid-flight
// (regression: streamExceededLimit returned nil whenever usage was unreported,
// which for these adapters is the entire life of the stream).
func TestProxyHardKillCutsStreamWithoutMidflightUsage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A slow OpenAI-shaped upstream: delta frames carry NO usage block; usage
	// arrives only on the final frame, which a cut stream never reaches.
	const totalFrames = 150
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < totalFrames; i++ {
			frame := `data: {"choices":[{"delta":{"content":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}]}`
			if _, err := w.Write([]byte(frame + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":900}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(slow.Close)
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, slow.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	// A hard-kill quota on output tokens. Each frame is ~90 bytes (~22
	// estimated tokens), so the byte-count estimate crosses the limit within
	// the first handful of frames, long before the final usage frame.
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricTokensOut,
		Limit: 100, Window: store.WindowDaily, BreachBehavior: store.BreachHardKill,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("streaming proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatal("stream ran to completion; the hard-kill quota never cut a usage-silent stream")
	}
	frames := strings.Count(body, "data: ")
	if frames == 0 {
		t.Fatal("no frames were relayed before the cut")
	}
	if frames >= totalFrames {
		t.Fatalf("all %d frames were relayed; the stream was not cut", frames)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("stream ran %v before the cut; byte-estimated hard-kill should sever within ~one recheck interval", elapsed)
	}
	assertHardKillCutIsAttributable(t, h, body)
}

// TestProxyUpstreamTTFBTimeoutIsEnforced proves the JANUS_UPSTREAM_TTFB_TIMEOUT
// knob actually bounds the upstream hop (regression: config loaded and the docs
// described the connect/TTFB timeouts, but the proxy used the default transport
// so only the total timeout had any effect).
func TestProxyUpstreamTTFBTimeoutIsEnforced(t *testing.T) {
	h := newHarness(t)

	// An upstream that stalls before writing response headers for far longer
	// than the configured TTFB timeout, but well within the total timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, slow.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}
	h.server.Config.UpstreamConnTimeout = time.Second
	h.server.Config.UpstreamTTFBTimeout = 150 * time.Millisecond
	// Harness total timeout is 10s; the request must fail long before that.

	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled upstream returned %d, want 503: %s", rec.Code, rec.Body.String())
	}
	apiErr := decodeBody(t, rec)["error"].(map[string]any)
	if apiErr["code"] != CodeUpstreamDown {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeUpstreamDown)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("request took %v; the 150ms TTFB timeout was not enforced", elapsed)
	}
}

// TestProxyTrustsPrivateCAUpstream locks the JANUS_CA_BUNDLE contract on the
// proxy hot path (regression: the variable was documented and loaded but never
// wired into the upstream transport, so every TLS upstream signed by a private
// corporate CA failed with x509 unknown-authority errors and a 503).
func TestProxyTrustsPrivateCAUpstream(t *testing.T) {
	newTLSUpstream := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmpl-tls","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":4}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	request := func(h *harness) *httptest.ResponseRecorder {
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
	}

	t.Run("without a bundle the private-CA upstream is refused", func(t *testing.T) {
		h := newHarness(t)
		tlsUpstream := newTLSUpstream(t)
		if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, tlsUpstream.URL, true, "", ""); err != nil {
			t.Fatalf("repoint upstream: %v", err)
		}
		rec := request(h)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("untrusted TLS upstream returned %d, want 503", rec.Code)
		}
		apiErr := decodeBody(t, rec)["error"].(map[string]any)
		if apiErr["code"] != CodeUpstreamDown {
			t.Errorf("error code = %v, want %s", apiErr["code"], CodeUpstreamDown)
		}
	})

	t.Run("with the CA bundle the upstream verifies and is metered", func(t *testing.T) {
		h := newHarness(t)
		tlsUpstream := newTLSUpstream(t)
		if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, tlsUpstream.URL, true, "", ""); err != nil {
			t.Fatalf("repoint upstream: %v", err)
		}
		// Trust exactly what JANUS_CA_BUNDLE would have loaded: a pool
		// containing only the private CA (here, the test server's cert).
		pool := x509.NewCertPool()
		pool.AddCert(tlsUpstream.Certificate())
		h.server.Config.CABundle = pool

		rec := request(h)
		if rec.Code != http.StatusOK {
			t.Fatalf("bundle-trusted TLS upstream returned %d: %s", rec.Code, rec.Body.String())
		}
		payload := decodeBody(t, rec)
		if payload["id"] != "cmpl-tls" {
			t.Errorf("response id = %v, want cmpl-tls from the TLS upstream", payload["id"])
		}
		events := h.waitForUsageEvents(1)
		if events[0].TokensIn != 10 || events[0].TokensOut != 4 {
			t.Errorf("token counts = (%d,%d), want (10,4) from the TLS upstream usage block",
				events[0].TokensIn, events[0].TokensOut)
		}
	})
}

func TestProxyRejectsUngrantedModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up, err := h.store.UpstreamByID(ctx, h.model.UpstreamID)
	if err != nil {
		t.Fatalf("load upstream: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "secret-model", []string{"chat"}); err != nil {
		t.Fatalf("seed second model: %v", err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{})
	for _, m := range models {
		if m.Name == "secret-model" {
			_ = h.store.SetModelRates(ctx, m.ID, store.RateCard{
				RateInNano: usage.NanoPerUSD, RateOutNano: usage.NanoPerUSD,
				EffectiveFrom: time.Now().Add(-time.Hour),
			})
			_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
		}
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "secret-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted model returned %d, want 403", rec.Code)
	}
	payload := decodeBody(t, rec)
	apiErr := payload["error"].(map[string]any)
	if apiErr["code"] != CodeModelNotGranted {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeModelNotGranted)
	}
	if !strings.Contains(apiErr["message"].(string), "secret-model") {
		t.Errorf("error message should name the model: %v", apiErr["message"])
	}
}

func TestProxyRejectsRevokedToken(t *testing.T) {
	h := newHarness(t)
	tokens, err := h.store.ListTokens(context.Background(), h.user.ID)
	if err != nil || len(tokens) == 0 {
		t.Fatalf("list tokens: %v", err)
	}
	if err := h.store.RevokeToken(context.Background(), tokens[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	h.server.InvalidateTokenCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token returned %d, want 401", rec.Code)
	}
	apiErr := decodeBody(t, rec)["error"].(map[string]any)
	if apiErr["code"] != CodeTokenInvalid {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeTokenInvalid)
	}
}

func TestProxyEnforcesQuotaWithResetTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A limit of 50 input tokens is exceeded by the first 100-token response.
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricTokensIn,
		Limit: 50, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}

	first := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first request returned %d, want 200 (the limit is not yet consumed)", first.Code)
	}
	h.waitForUsageEvents(1)

	// The in-flight request finished; the next one must be refused.
	var second *httptest.ResponseRecorder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		second = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if second.Code == http.StatusTooManyRequests {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("request after breach returned %d, want 429", second.Code)
	}
	apiErr := decodeBody(t, second)["error"].(map[string]any)
	if apiErr["code"] != CodeQuotaExceeded {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeQuotaExceeded)
	}
	if apiErr["reset_at"] == nil || apiErr["reset_at"] == "" {
		t.Error("a quota error must tell the caller when access resumes")
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("a 429 should carry a Retry-After header")
	}
}

func TestProxyEnforcesRateLimitsWhenFlagIsOn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.CreateRateLimitRule(ctx, &store.RateLimitRule{
		Endpoint: "/v1/chat/completions", RequestsPerMinute: 2,
	}); err != nil {
		t.Fatalf("create rate limit rule: %v", err)
	}

	call := func() *httptest.ResponseRecorder {
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
	}

	// Flag off: rules configured but never enforced.
	for i := 0; i < 4; i++ {
		if rec := call(); rec.Code != http.StatusOK {
			t.Fatalf("with the flag off request %d returned %d, want 200", i+1, rec.Code)
		}
	}

	if err := h.store.SetFeatureFlag(ctx, "per_user_rate_limits_enabled", true); err != nil {
		t.Fatalf("enable flag: %v", err)
	}
	// The flag was flipped directly in the store, bypassing the admin handler
	// that clears the hot-path config cache; do what the handler would do.
	// (Cross-replica, the change would take effect within configCacheTTL.)
	h.server.InvalidateConfigCache()

	// Flag on: the third request within the minute is refused with 429.
	statuses := []int{}
	var refused *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		rec := call()
		statuses = append(statuses, rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			refused = rec
		}
	}
	if refused == nil {
		t.Fatalf("no request was rate limited; statuses = %v", statuses)
	}
	apiErr := decodeBody(t, refused)["error"].(map[string]any)
	if apiErr["code"] != CodeRateLimit {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeRateLimit)
	}
	if apiErr["reset_at"] == nil || apiErr["reset_at"] == "" {
		t.Error("a rate limit refusal must tell the caller when the limit refills")
	}
	if refused.Header().Get("Retry-After") == "" {
		t.Error("a 429 should carry a Retry-After header")
	}

	// A different endpoint is not covered by the rule.
	if rec := h.do(http.MethodPost, "/v1/embeddings", map[string]any{
		"model": "test-model", "input": "hi",
	}); rec.Code == http.StatusTooManyRequests {
		t.Error("the rule is scoped to /v1/chat/completions but /v1/embeddings was refused")
	}
}

func TestRateLimitAdminCRUD(t *testing.T) {
	h := newHarness(t)

	created := h.do(http.MethodPost, "/api/v1/admin/rate-limits", map[string]any{
		"endpoint": "/v1/chat/completions", "requests_per_minute": 60,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create rate limit returned %d: %s", created.Code, created.Body.String())
	}
	rule := decodeBody(t, created)["rule"].(map[string]any)
	id, _ := rule["id"].(string)
	if id == "" {
		t.Fatal("created rule carries no id")
	}

	bad := h.do(http.MethodPost, "/api/v1/admin/rate-limits", map[string]any{
		"endpoint": "/v1/chat/completions", "requests_per_minute": 0,
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("zero rpm returned %d, want 400", bad.Code)
	}
	badPath := h.do(http.MethodPost, "/api/v1/admin/rate-limits", map[string]any{
		"endpoint": "/etc/passwd", "requests_per_minute": 10,
	})
	if badPath.Code != http.StatusBadRequest {
		t.Fatalf("non-proxy endpoint returned %d, want 400", badPath.Code)
	}

	list := h.do(http.MethodGet, "/api/v1/admin/rate-limits", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list rate limits returned %d", list.Code)
	}
	payload := decodeBody(t, list)
	if enabled, _ := payload["enforcement_enabled"].(bool); enabled {
		t.Error("enforcement should be reported off while the flag is off")
	}
	if rules := payload["rules"].([]any); len(rules) != 1 {
		t.Fatalf("listed %d rules, want 1", len(rules))
	}

	if del := h.do(http.MethodDelete, "/api/v1/admin/rate-limits/"+id, nil); del.Code != http.StatusOK {
		t.Fatalf("delete rate limit returned %d", del.Code)
	}
	after := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/rate-limits", nil))
	if rules := after["rules"].([]any); len(rules) != 0 {
		t.Fatalf("listed %d rules after delete, want 0", len(rules))
	}
}

func TestProxyBlockedByPolicyRule(t *testing.T) {
	h := newHarness(t)
	if err := h.store.CreateBlockingRule(context.Background(), &store.BlockingRule{
		Name: "no-openclaw", Combinator: "and", Enabled: true, Reason: "openclaw is not approved for corporate use",
		Clauses: []store.RuleClause{{Type: store.ClauseUserAgent, Pattern: "openclaw"}},
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("User-Agent", "openclaw/0.9")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked user agent returned %d, want 403", rec.Code)
	}
	apiErr := decodeBody(t, rec)["error"].(map[string]any)
	if apiErr["code"] != CodeEndpointBlocked {
		t.Errorf("error code = %v, want %s", apiErr["code"], CodeEndpointBlocked)
	}
	if !strings.Contains(apiErr["reason"].(string), "openclaw is not approved") {
		t.Errorf("the operator's reason should reach the caller: %v", apiErr["reason"])
	}

	// A permitted client is unaffected by the same rule.
	if ok := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); ok.Code != http.StatusOK {
		t.Fatalf("permitted client returned %d, want 200", ok.Code)
	}
}

func TestModelsEndpointIsGrantFiltered(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models returned %d", rec.Code)
	}
	payload := decodeBody(t, rec)
	if payload["object"] != "list" {
		t.Errorf("response object = %v, want list (OpenAI shape)", payload["object"])
	}
	data := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("model list has %d entries, want the single granted model", len(data))
	}
	if data[0].(map[string]any)["id"] != "test-model" {
		t.Errorf("listed model = %v, want test-model", data[0])
	}
}

func TestUnauthenticatedProxyIsRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous proxy call returned %d, want 401", rec.Code)
	}
	apiErr := decodeBody(t, rec)["error"].(map[string]any)
	if apiErr["type"] != "authentication_error" {
		t.Errorf("error type = %v, want authentication_error", apiErr["type"])
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d, want 200: %s", path, rec.Code, rec.Body.String())
		}
	}

	// Generate traffic so the counters are populated.
	h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	h.waitForUsageEvents(1)

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	body := rec.Body.String()
	names := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE janus_") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				names[parts[2]] = true
			}
		}
	}
	if len(names) < 15 {
		t.Errorf("/metrics exposes %d janus metrics, want at least 15", len(names))
	}
	// Cardinality guard: per-user labels would explode the series count.
	for _, forbidden := range telemetry.ForbiddenLabels {
		if strings.Contains(body, forbidden+"=") {
			t.Errorf("metric output contains the forbidden label %q", forbidden)
		}
	}
}

func TestTokenLifecycleOverAPI(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/api/v1/tokens", map[string]any{"description": "ci runner"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token returned %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeBody(t, rec)
	value, _ := created["value"].(string)
	if !strings.HasPrefix(value, store.TokenPrefix) {
		t.Fatalf("issued token %q lacks the janus prefix", value)
	}

	list := h.do(http.MethodGet, "/api/v1/tokens", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list tokens returned %d", list.Code)
	}
	if strings.Contains(list.Body.String(), value) {
		t.Fatal("the token value must never be returned again after creation")
	}

	// Validation: an empty description is refused with a field-specific error.
	bad := h.do(http.MethodPost, "/api/v1/tokens", map[string]any{"description": "  "})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("blank description returned %d, want 400", bad.Code)
	}
	if decodeBody(t, bad)["error"].(map[string]any)["param"] != "description" {
		t.Error("validation errors should name the offending field")
	}
}

func TestAdminSurfaceRequiresAdminRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true); err != nil {
		t.Fatalf("demote user: %v", err)
	}
	h.server.InvalidateTokenCache()

	rec := h.do(http.MethodGet, "/api/v1/admin/upstreams", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin admin call returned %d, want 403", rec.Code)
	}

	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatalf("promote user: %v", err)
	}
	h.server.InvalidateTokenCache()
	if ok := h.do(http.MethodGet, "/api/v1/admin/upstreams", nil); ok.Code != http.StatusOK {
		t.Fatalf("admin call returned %d, want 200: %s", ok.Code, ok.Body.String())
	}
}

func TestModelCannotBeEnabledWithoutRates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up, _ := h.store.UpstreamByID(ctx, h.model.UpstreamID)
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "unpriced-model", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{Search: "unpriced"})
	if len(models) != 1 {
		t.Fatalf("expected the unpriced model to exist")
	}

	rec := h.do(http.MethodPatch, "/api/v1/admin/models/"+models[0].ID, map[string]any{"status": "enabled"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enabling an unpriced model returned %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate card") {
		t.Errorf("the error should explain the missing rate card: %s", rec.Body.String())
	}
}

// --- Model rename (display name) proxy contract -------------------------------

// rename sets a display name on the harness model and clears the hot-path
// caches, mirroring what the admin PATCH handler does after a rename.
func (h *harness) rename(display string) {
	h.t.Helper()
	if err := h.store.SetModelDisplayName(context.Background(), h.model.ID, display); err != nil {
		h.t.Fatalf("set display name: %v", err)
	}
	h.server.InvalidateConfigCache()
}

// doRaw sends an exact byte payload, bypassing JSON re-encoding, so tests can
// pin byte-for-byte passthrough.
func (h *harness) doRaw(method, path, contentType string, raw []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestProxyNoRenamePassthroughByteIdentical pins pass-through behavior for models without a
// display name: the body the client sent is the body the upstream receives,
// byte for byte — no re-marshalling, no key reordering, no whitespace changes.
func TestProxyNoRenamePassthroughByteIdentical(t *testing.T) {
	h := newHarness(t)

	// Deliberately odd formatting: extra whitespace, unusual key order, a tab,
	// and a unicode escape — any re-encoding would normalise at least one.
	raw := []byte("{\n  \"messages\": [ {\"role\":\"user\",\"content\":\"caf\\u00e9\"} ] ,	\"model\":\"test-model\"  }")
	rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	got := h.upstreamRequests[len(h.upstreamRequests)-1].Body
	if !bytes.Equal(got, raw) {
		t.Fatalf("upstream body differs from what the client sent:\n got: %q\nwant: %q", got, raw)
	}
}

// TestProxyDisplayNameRewritesBodyToNativeName is the rename acceptance path:
// a client calls the model by its admin-set display name, the upstream
// receives the native name, every other body field survives untouched, and the
// usage event records the display name against the stable model_id.
func TestProxyDisplayNameRewritesBodyToNativeName(t *testing.T) {
	h := newHarness(t)
	h.rename("fable-5")

	raw := []byte(`{"model":"fable-5","messages":[{"role":"user","content":"hi"}],"temperature":0.25}`)
	rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("display-name call returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	forwarded := h.upstreamRequests[len(h.upstreamRequests)-1].Body
	if bytes.Contains(forwarded, []byte("fable-5")) {
		t.Fatalf("the display name leaked upstream: %s", forwarded)
	}
	var body struct {
		Model       string              `json:"model"`
		Temperature float64             `json:"temperature"`
		Messages    []map[string]string `json:"messages"`
	}
	if err := json.Unmarshal(forwarded, &body); err != nil {
		t.Fatalf("upstream body is not valid JSON after the rewrite: %v", err)
	}
	if body.Model != "test-model" {
		t.Errorf("upstream model = %q, want the native name test-model", body.Model)
	}
	if body.Temperature != 0.25 {
		t.Errorf("temperature = %v, want 0.25 (other fields must survive the rewrite)", body.Temperature)
	}
	if len(body.Messages) != 1 || body.Messages[0]["content"] != "hi" {
		t.Errorf("messages did not survive the rewrite: %v", body.Messages)
	}

	event := h.waitForUsageEvents(1)[0]
	if event.ModelName != "fable-5" {
		t.Errorf("usage event model name = %q, want the display name fable-5", event.ModelName)
	}
	if event.ModelID != h.model.ID {
		t.Errorf("usage event model_id = %q, want the stable id %q", event.ModelID, h.model.ID)
	}
	if event.TokensIn != 100 || event.TokensOut != 40 {
		t.Errorf("token counts = (%d,%d), want (100,40) — metering must work on rewritten requests",
			event.TokensIn, event.TokensOut)
	}
}

// TestProxyNativeNameStillRoutesAfterRename locks backward compatibility:
// clients configured with the original upstream name keep working after an
// admin sets a display name, with zero body mutation — and their usage is
// still recorded under the display name so dashboards stay consistent.
func TestProxyNativeNameStillRoutesAfterRename(t *testing.T) {
	h := newHarness(t)
	h.rename("fable-5")

	raw := []byte(`{"model":"test-model",  "messages":[{"role":"user","content":"hi"}]}`)
	rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("native-name call after rename returned %d: %s", rec.Code, rec.Body.String())
	}
	got := h.upstreamRequests[len(h.upstreamRequests)-1].Body
	if !bytes.Equal(got, raw) {
		t.Fatalf("native-name request was mutated:\n got: %q\nwant: %q", got, raw)
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ModelName != "fable-5" {
		t.Errorf("usage event model name = %q, want the display name fable-5 (dashboards show the catalog name)", event.ModelName)
	}
	if event.ModelID != h.model.ID {
		t.Errorf("usage event model_id = %q, want %q", event.ModelID, h.model.ID)
	}
}

// TestProxyAliasWithUnrewritableBodyIsRefused pins the refusal contract: when
// the caller uses a display name but the gateway cannot rewrite the body (it
// overflowed the in-memory buffer, or is not a JSON object), the request is
// refused with a 400 invalid_request_error that names the native model name —
// never forwarded with a model the upstream would not recognise.
func TestProxyAliasWithUnrewritableBodyIsRefused(t *testing.T) {
	restore := maxInMemoryBody
	maxInMemoryBody = 64 << 10
	t.Cleanup(func() { maxInMemoryBody = restore })

	h := newHarness(t)
	h.rename("fable-5")

	buildMultipart := func(fillerBytes int) []byte {
		boundary := "janusboundary"
		var buf bytes.Buffer
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\nfable-5\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n")
		buf.WriteString("Content-Type: audio/wav\r\n\r\n")
		buf.Write(bytes.Repeat([]byte("x"), fillerBytes))
		buf.WriteString("\r\n--" + boundary + "--\r\n")
		return buf.Bytes()
	}

	assertRefused := func(t *testing.T, rec *httptest.ResponseRecorder, upstreamCallsBefore int) {
		t.Helper()
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unrewritable alias request returned %d, want 400: %s", rec.Code, rec.Body.String())
		}
		apiErr := decodeBody(t, rec)["error"].(map[string]any)
		if apiErr["code"] != CodeInvalidRequest {
			t.Errorf("error code = %v, want %s", apiErr["code"], CodeInvalidRequest)
		}
		message, _ := apiErr["message"].(string)
		if !strings.Contains(message, "test-model") {
			t.Errorf("the refusal must name the native model name to use: %q", message)
		}
		if !strings.Contains(message, "fable-5") {
			t.Errorf("the refusal should name the alias the caller sent: %q", message)
		}
		if len(h.upstreamRequests) != upstreamCallsBefore {
			t.Error("a refused request must never reach the upstream")
		}
	}

	t.Run("overflowed body", func(t *testing.T) {
		before := len(h.upstreamRequests)
		payload := buildMultipart(256 << 10) // ~4x the buffer
		rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions",
			"multipart/form-data; boundary=janusboundary", payload)
		assertRefused(t, rec, before)
	})

	t.Run("non-JSON buffered body", func(t *testing.T) {
		before := len(h.upstreamRequests)
		payload := buildMultipart(128) // small enough to buffer, still not JSON
		rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions",
			"multipart/form-data; boundary=janusboundary", payload)
		assertRefused(t, rec, before)
	})
}

// TestListModelsShowsDisplayNames covers both catalog surfaces after a rename:
// the OpenAI-shaped /v1/models lists the display name as the model id (with
// the native name kept under janus.native_name for provenance), and the app
// catalog /api/v1/models returns display_name alongside the native name.
func TestListModelsShowsDisplayNames(t *testing.T) {
	h := newHarness(t)
	h.rename("fable-5")

	t.Run("proxy /v1/models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/v1/models returned %d", rec.Code)
		}
		data := decodeBody(t, rec)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("model list has %d entries, want 1", len(data))
		}
		entry := data[0].(map[string]any)
		if entry["id"] != "fable-5" {
			t.Errorf("listed id = %v, want the display name fable-5", entry["id"])
		}
		janus := entry["janus"].(map[string]any)
		if janus["native_name"] != "test-model" {
			t.Errorf("janus.native_name = %v, want test-model (provenance)", janus["native_name"])
		}
	})

	t.Run("app catalog /api/v1/models", func(t *testing.T) {
		rec := h.do(http.MethodGet, "/api/v1/models", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/v1/models returned %d: %s", rec.Code, rec.Body.String())
		}
		models := decodeBody(t, rec)["models"].([]any)
		if len(models) != 1 {
			t.Fatalf("catalog has %d entries, want 1", len(models))
		}
		entry := models[0].(map[string]any)
		if entry["name"] != "test-model" {
			t.Errorf("catalog name = %v, want the native test-model", entry["name"])
		}
		if entry["display_name"] != "fable-5" {
			t.Errorf("catalog display_name = %v, want fable-5", entry["display_name"])
		}
	})
}

// TestProxyBedrockAdapterReceivesNativeModelName proves URL-embedded adapters
// (Bedrock, Vertex) are handed the native upstream name after a rename: the
// signed Bedrock URL must carry the provider's model id, never the alias.
func TestProxyBedrockAdapterReceivesNativeModelName(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	encKey, err := h.server.Cipher.Encrypt("AKIDEXAMPLE:testsecretkey")
	if err != nil {
		t.Fatalf("encrypt test credential: %v", err)
	}
	up, err := h.store.CreateUpstream(ctx, "bedrock-upstream", "bedrock", h.upstream.URL, encKey, "AKID…")
	if err != nil {
		t.Fatalf("create bedrock upstream: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "anthropic.claude-3-haiku-v1", []string{"chat"}); err != nil {
		t.Fatalf("seed bedrock model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{Search: "claude"})
	if err != nil || len(models) != 1 {
		t.Fatalf("find bedrock model: %v (%d found)", err, len(models))
	}
	bm := models[0]
	if err := h.store.SetModelRates(ctx, bm.ID, store.RateCard{RateInNano: usage.NanoPerUSD, RateOutNano: usage.NanoPerUSD, EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatalf("set rates: %v", err)
	}
	if err := h.store.SetModelStatus(ctx, bm.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, bm.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant model: %v", err)
	}
	if err := h.store.SetModelDisplayName(ctx, bm.ID, "claude-haiku"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-haiku", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bedrock alias call returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.upstreamRequests) == 0 {
		t.Fatal("the request never reached the upstream")
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if want := "/model/anthropic.claude-3-haiku-v1/converse"; last.Path != want {
		t.Errorf("bedrock URL path = %q, want %q (the alias must never reach a URL-embedded adapter)", last.Path, want)
	}
	if strings.Contains(last.Path, "Claude") {
		t.Errorf("the display name leaked into the upstream URL: %q", last.Path)
	}
}

// TestProxyMetersAnthropicCacheWriteTokens locks the cache-write metering
// contract end to end: a proxied Anthropic request whose usage block reports
// prompt-cache writes produces a usage event carrying the 5m/1h counts and a
// cost that includes the write-rate terms — for buffered and streamed
// responses alike — while a request without cache writes is priced exactly as
// before the dimensions existed.
func TestProxyMetersAnthropicCacheWriteTokens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A fake Anthropic Messages endpoint. The gateway's anthropic adapter
	// translates /v1/chat/completions to /v1/messages, so the shape switch
	// keys off the translated request body.
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(`"stream":true`)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for _, frame := range []string{
				"event: message_start",
				`data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"output_tokens":0,` +
					`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200},"cache_read_input_tokens":50}}}`,
				`data: {"type":"content_block_delta","delta":{"text":"hi"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}`,
			} {
				_, _ = w.Write([]byte(frame + "\n\n"))
				flusher.Flush()
			}
		case bytes.Contains(body, []byte("nocache")):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":50}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stop_reason":"end_turn",` +
				`"usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200},"cache_read_input_tokens":50}}`))
		}
	}))
	t.Cleanup(anthropicUp.Close)

	up, err := h.store.CreateUpstream(ctx, "anthropic-test", "anthropic", anthropicUp.URL, "", "")
	if err != nil {
		t.Fatalf("create anthropic upstream: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "claude-cache-test", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{Search: "claude-cache-test"})
	if err != nil || len(models) != 1 {
		t.Fatalf("find model: %v (%d found)", err, len(models))
	}
	model := models[0]
	// Anthropic-style card: $10 in, $30 out, $1 cache-hit, $12.50 5m-write,
	// $20 1h-write per MTok.
	if err := h.store.SetModelRates(ctx, model.ID, store.RateCard{
		RateInNano: 10 * usage.NanoPerUSD, RateOutNano: 30 * usage.NanoPerUSD, RateCachedNano: 1 * usage.NanoPerUSD,
		RateCacheWrite5mNano: usage.NanoFromUSD(12.50), RateCacheWrite1hNano: 20 * usage.NanoPerUSD,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("set rates: %v", err)
	}
	if err := h.store.SetModelStatus(ctx, model.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, model.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant model: %v", err)
	}
	h.server.InvalidateConfigCache()

	// Anthropic reports cache reads and cache writes DISJOINT from
	// input_tokens, so ALL of them are billed on top of the full 1000 fresh
	// input tokens — nothing is subtracted. (This previously asserted
	// "950 fresh + 50 cached", i.e. the OpenAI subset convention applied to
	// an Anthropic body; that clamped the cached prefix away and undercharged
	// every cache hit. Note the same payload's cache_creation tokens were
	// already treated as additive here, so the old expectation was also
	// internally inconsistent.)
	//
	// 1000 fresh @ $10/M + 50 cache-read @ $1/M + 500 out @ $30/M
	//   + 100 5m-writes @ $12.50/M + 200 1h-writes @ $20/M.
	wantWithWrites := (1000*10*int64(usage.NanoPerUSD) +
		50*1*int64(usage.NanoPerUSD) +
		500*30*int64(usage.NanoPerUSD) +
		100*usage.NanoFromUSD(12.50) +
		200*20*int64(usage.NanoPerUSD)) / int64(usage.TokensPerRateUnit)

	findEvent := func(events []*store.UsageEvent, match func(*store.UsageEvent) bool) *store.UsageEvent {
		for _, e := range events {
			if match(e) {
				return e
			}
		}
		return nil
	}

	// 1. Buffered response with the cache_creation breakdown.
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-cache-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("buffered anthropic proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	events := h.waitForUsageEvents(1)
	event := findEvent(events, func(e *store.UsageEvent) bool { return !e.Streaming && e.TokensCacheWrite5m == 100 })
	if event == nil {
		t.Fatalf("no buffered event with cache-write tokens among %d events", len(events))
	}
	if event.TokensCacheWrite5m != 100 || event.TokensCacheWrite1h != 200 {
		t.Errorf("cache-write tokens = (%d, %d), want (100, 200)", event.TokensCacheWrite5m, event.TokensCacheWrite1h)
	}
	if event.TokensIn != 1000 || event.TokensOut != 500 || event.TokensCached != 50 {
		t.Errorf("token counts = (%d, %d, %d), want (1000, 500, 50)", event.TokensIn, event.TokensOut, event.TokensCached)
	}
	if event.CostNano != wantWithWrites {
		t.Errorf("cost = %d nano-USD, want %d (must include both write-rate terms)", event.CostNano, wantWithWrites)
	}
	// The cost header on buffered responses must carry the same figure.
	if got := rec.Header().Get("X-Janus-Cost-USD"); got != strconv.FormatFloat(usage.USD(wantWithWrites), 'f', -1, 64) {
		t.Errorf("X-Janus-Cost-USD = %q, want %q", got, strconv.FormatFloat(usage.USD(wantWithWrites), 'f', -1, 64))
	}
	// Prometheus must export the cache dimensions with model/modality/upstream
	// labels: cache reads on the existing counter, cache writes on the two new
	// TTL-bucketed counters (AC: cache-write visibility for operators).
	if got := testutil.ToFloat64(h.server.Metrics.TokensCached.WithLabelValues("claude-cache-test", "chat", "anthropic-test")); got != 50 {
		t.Errorf("janus_tokens_cached_total = %v, want 50", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.TokensCacheWrite5m.WithLabelValues("claude-cache-test", "chat", "anthropic-test")); got != 100 {
		t.Errorf("janus_tokens_cache_write_5m_total = %v, want 100", got)
	}
	if got := testutil.ToFloat64(h.server.Metrics.TokensCacheWrite1h.WithLabelValues("claude-cache-test", "chat", "anthropic-test")); got != 200 {
		t.Errorf("janus_tokens_cache_write_1h_total = %v, want 200", got)
	}

	// 2. Streamed response: counts arrive on message_start, output on
	// message_delta; the event and cost must match the buffered figures.
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-cache-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming anthropic proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	events = h.waitForUsageEvents(2)
	event = findEvent(events, func(e *store.UsageEvent) bool { return e.Streaming && e.TokensCacheWrite5m == 100 })
	if event == nil {
		t.Fatalf("no streamed event with cache-write tokens among %d events", len(events))
	}
	if event.TokensCacheWrite1h != 200 || event.TokensIn != 1000 || event.TokensOut != 500 || event.TokensCached != 50 {
		t.Errorf("streamed counts = (in %d, out %d, cached %d, cw5m %d, cw1h %d), want (1000, 500, 50, 100, 200)",
			event.TokensIn, event.TokensOut, event.TokensCached, event.TokensCacheWrite5m, event.TokensCacheWrite1h)
	}
	if event.CostNano != wantWithWrites {
		t.Errorf("streamed cost = %d nano-USD, want %d", event.CostNano, wantWithWrites)
	}

	// 3. No cache writes: zero-valued fields and a cost bit-for-bit equal to
	// the legacy three-dimension arithmetic, even though the card carries
	// non-zero write rates.
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-cache-test", "messages": []map[string]string{{"role": "user", "content": "nocache"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-cache anthropic proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	events = h.waitForUsageEvents(3)
	event = findEvent(events, func(e *store.UsageEvent) bool { return !e.Streaming && e.TokensCacheWrite5m == 0 && e.TokensIn == 1000 })
	if event == nil {
		t.Fatalf("no cache-write-free event among %d events", len(events))
	}
	if event.TokensCacheWrite5m != 0 || event.TokensCacheWrite1h != 0 {
		t.Errorf("cache-write tokens = (%d, %d), want (0, 0)", event.TokensCacheWrite5m, event.TokensCacheWrite1h)
	}
	rates := usage.Rates{
		InNano: 10 * usage.NanoPerUSD, OutNano: 30 * usage.NanoPerUSD, CachedNano: 1 * usage.NanoPerUSD,
		CacheWrite5mNano: usage.NanoFromUSD(12.50), CacheWrite1hNano: 20 * usage.NanoPerUSD,
	}
	// Still an Anthropic body, so the cache-read count stays additive; only
	// the cache-WRITE dimensions are absent from this response.
	want := usage.ComputeCostAll(usage.TokenCounts{
		In: 1000, Out: 500, Cached: 50, CachedDisjoint: true,
	}, rates)
	if event.CostNano != want {
		t.Errorf("cost without cache writes = %d nano-USD, want %d", event.CostNano, want)
	}
}

func TestPersonalDashboardReflectsUsage(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	h.waitForUsageEvents(1)

	rec := h.do(http.MethodGet, "/api/v1/dashboard/personal?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard returned %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	totals := payload["totals"].(map[string]any)
	if totals["request_count"].(float64) != 1 {
		t.Errorf("dashboard request count = %v, want 1", totals["request_count"])
	}
	if totals["tokens_in"].(float64) != 100 {
		t.Errorf("dashboard tokens_in = %v, want 100", totals["tokens_in"])
	}
	if len(payload["per_model"].([]any)) == 0 {
		t.Error("the per-model breakdown should not be empty after a request")
	}
}

// TestProxyTranslatesAnthropicResponsesToOpenAIShape covers the failure the
// monitoring screenshots showed: requests to a Claude model logged as
// "200 · stream" with tokens metered, yet the chat client rendered empty
// bubbles, because the Anthropic Messages response (JSON and typed SSE
// events alike) was relayed byte-for-byte to a client that speaks the OpenAI
// chat.completion shape. Both directions of the relay must now produce what
// an OpenAI SDK parses.
func TestProxyTranslatesAnthropicResponsesToOpenAIShape(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var gotUpstreamBody []byte
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpstreamBody, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		if bytes.Contains(gotUpstreamBody, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for _, frame := range []string{
				"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-translate-test","usage":{"input_tokens":7,"output_tokens":1}}}`,
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hey"}}`,
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Casey!"}}`,
				"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
				"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
				"event: message_stop\ndata: " + `{"type":"message_stop"}`,
			} {
				_, _ = w.Write([]byte(frame + "\n\n"))
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_b","type":"message","role":"assistant","model":"claude-translate-test",` +
			`"content":[{"type":"text","text":"Hey Casey! Welcome in."}],"stop_reason":"end_turn","stop_sequence":null,` +
			`"usage":{"input_tokens":7,"output_tokens":6}}`))
	}))
	t.Cleanup(anthropicUp.Close)

	up, err := h.store.CreateUpstream(ctx, "anthropic-translate", "anthropic", anthropicUp.URL, "", "")
	if err != nil {
		t.Fatalf("create anthropic upstream: %v", err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "claude-translate-test", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{Search: "claude-translate-test"})
	if err != nil || len(models) != 1 {
		t.Fatalf("find model: %v (%d found)", err, len(models))
	}
	if err := h.store.SetModelStatus(ctx, models[0].ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, models[0].ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant model: %v", err)
	}
	h.server.InvalidateConfigCache()

	// 1. Buffered: a tool-bearing request must reach Anthropic in Messages
	// shape and the answer must come back as chat.completion.
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-translate-test", "max_completion_tokens": 64,
		"messages": []map[string]any{{"role": "user", "content": "Hello!"}},
		"tools": []map[string]any{{"type": "function", "function": map[string]any{
			"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("buffered anthropic proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(gotUpstreamBody, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent["max_tokens"] != float64(64) {
		t.Errorf("max_completion_tokens not translated: %v", sent["max_tokens"])
	}
	if tools, _ := sent["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["input_schema"] == nil {
		t.Errorf("tools not translated: %v", sent["tools"])
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("client body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != "Hey Casey! Welcome in." || completion.Choices[0].FinishReason != "stop" ||
		completion.Usage.TotalTokens != 13 {
		t.Fatalf("client received a non-OpenAI body: %s", rec.Body.String())
	}
	// Metering still reads the native response.
	events := h.waitForUsageEvents(1)
	if events[0].TokensIn != 7 || events[0].TokensOut != 6 {
		t.Errorf("buffered metering = (%d, %d), want (7, 6)", events[0].TokensIn, events[0].TokensOut)
	}

	// 2. Streamed: typed Messages events must arrive as chat.completion.chunk
	// frames terminated by [DONE], with no Anthropic event names leaking.
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "claude-translate-test", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "Hello!"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming anthropic proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "event:") || strings.Contains(raw, "content_block_delta") || strings.Contains(raw, "message_start") {
		t.Fatalf("Anthropic SSE leaked to the client:\n%s", raw)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE]:\n%s", raw)
	}
	var text strings.Builder
	var finish string
	for _, frame := range strings.Split(raw, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" || frame == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &chunk); err != nil {
			t.Fatalf("frame %q is not a chunk: %v", frame, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("frame object = %q: %s", chunk.Object, frame)
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if text.String() != "Hey Casey!" || finish != "stop" {
		t.Fatalf("assembled stream = %q finish=%q:\n%s", text.String(), finish, raw)
	}
	events = h.waitForUsageEvents(2)
	var streamed *store.UsageEvent
	for _, e := range events {
		if e.Streaming {
			streamed = e
		}
	}
	if streamed == nil || streamed.TokensIn != 7 || streamed.TokensOut != 3 {
		t.Fatalf("streamed metering event = %+v", streamed)
	}
}
