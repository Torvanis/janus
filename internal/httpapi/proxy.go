package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/authz"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/secgw/stream"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/usage"
)

// maxInMemoryBody bounds how much of a request body the gateway will hold to
// extract the model name. Larger bodies are forwarded unbuffered: the buffered
// prefix is replayed and the remainder streams straight from the client. It is
// a variable only so tests can exercise the overflow path cheaply.
var maxInMemoryBody int64 = 32 << 20

// handleProxy is the OpenAI-compatible surface. It authenticates, applies
// policy, checks quotas, forwards the request untouched, and meters the result.
//
// Request and response bodies are held only as long as forwarding requires and
// are never written to disk, the database, or the logs.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s.Metrics.ConcurrentRequests.Inc()
	defer s.Metrics.ConcurrentRequests.Dec()

	// /v1/models is a gateway-owned listing, not a passthrough: it must reflect
	// this caller's grants rather than the union of every upstream.
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/v1/models/") {
		s.handleProxyModels(w, r)
		return
	}

	user := UserFrom(r.Context())
	token := TokenFrom(r.Context())
	serviceToken := ServiceTokenFrom(r.Context())
	// Exactly one principal kind must be present. A service token carries no
	// user; a user carries no service token.
	if serviceToken == nil && (user == nil || token == nil) {
		s.rejectProxy(w, r, ErrTokenInvalid("Provide your Janus API token as `Authorization: Bearer <token>`."), nil, started)
		return
	}

	// Buffer at most maxInMemoryBody+1 bytes: the extra byte tells us whether
	// the body overflows the buffer without consuming the remainder, which is
	// forwarded unbuffered further down.
	bodyRemainder := &countingReader{r: r.Body}
	body, err := io.ReadAll(io.LimitReader(bodyRemainder, maxInMemoryBody+1))
	if err != nil {
		event := &store.UsageEvent{EndpointPath: r.URL.Path, HTTPMethod: r.Method, Modality: usage.ModalityForPath(r.URL.Path), RequestID: RequestIDFrom(r.Context()), RequestBytes: bodyRemainder.total}
		if serviceToken != nil {
			event.ServiceTokenID = serviceToken.ID
		} else {
			event.UserID = user.ID
			event.TokenID = token.ID
			event.TeamIDs = token.TeamID
		}
		s.rejectProxyEvent(w, r, ErrInvalidRequest("The request body could not be read."), event, started)
		return
	}
	bodyOverflows := int64(len(body)) > maxInMemoryBody
	modelName := extractModelName(body, r)

	event := &store.UsageEvent{
		ModelName:    modelName,
		EndpointPath: r.URL.Path, HTTPMethod: r.Method,
		Modality:     usage.ModalityForPath(r.URL.Path),
		RequestBytes: int64(len(body)),
		UserAgent:    truncate(r.Header.Get("User-Agent"), 1024),
		ClientIP:     ClientIP(r, s.Config.TrustedProxies),
		Referer:      truncate(r.Header.Get("Referer"), 512),
		ClientApp:    truncate(clientAppName(r), 120),
		RequestID:    RequestIDFrom(r.Context()),
		Streaming:    isStreamingRequest(body),
	}
	stampReportingAdmission(event, r, s.Config.LocalOnly)
	// Principal attribution. Service-token events deliberately leave user_id
	// empty: that emptiness is what keeps this traffic out of every
	// people-oriented report while leaving it fully counted org-wide.
	if serviceToken != nil {
		event.ServiceTokenID = serviceToken.ID
	} else {
		event.UserID = user.ID
		event.TokenID = token.ID
		event.TeamIDs = token.TeamID // retain the attempted context even when membership was revoked
	}
	if s.trustsForwardedFor(r) {
		event.XForwardedFor = truncate(r.Header.Get("X-Forwarded-For"), 512)
	}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		event.AttachmentCount = countMultipartSections(body)
	} else if strings.Contains(strings.ToLower(ct), "json") && !bodyOverflows {
		// Vision requests arrive as JSON content parts on the chat endpoint;
		// count image parts so multimodal traffic is visible in usage data.
		// The body is already in memory here — no extra buffering — and only
		// the count is retained, never the image bytes.
		event.AttachmentCount = countJSONImageParts(body)
	}

	// one root span per proxied request. StartTrace returns nil when
	// no OTLP endpoint is configured, and every call on a nil trace context is
	// a no-op, so the hot path pays nothing while export is off. Recording is
	// buffered and never blocks. Attributes follow the trace taxonomy: request
	// metadata only, never bodies or credentials.
	trace := s.Trace.StartTrace()
	defer func() {
		trace.End(r.Method+" "+event.EndpointPath, started, event.HTTPStatus,
			telemetry.String("request_id", event.RequestID),
			telemetry.String("user_id", event.UserID),
			telemetry.String("model", event.ModelName),
			telemetry.String("upstream_id", event.UpstreamID),
			telemetry.String("modality", event.Modality),
			telemetry.String("http.status_code", strconv.Itoa(event.HTTPStatus)),
			telemetry.String("error_code", event.ErrorCode),
		)
	}()

	ctx := r.Context()
	// Principal context. A service token belongs to no user, so it has no
	// groups and no teams: its access comes solely from its own grants, and
	// team-inherited quotas cannot apply to it.
	var groupIDs, teamIDs []string
	if serviceToken == nil {
		groupIDs, teamIDs, err = s.principalContext(ctx, user)
		if err != nil {
			apiErr := ErrInternal()
			var supplied *APIError
			if errors.As(err, &supplied) {
				apiErr = supplied
			}
			s.rejectProxyEvent(w, r, apiErr, event, started)
			return
		}
		event.TeamIDs = strings.Join(teamIDs, ",")
		event.GroupIDs = append([]string{}, groupIDs...)
	}

	// Troubleshooting mode: while an admin has an active session whose
	// up-front criteria this request could satisfy, the response writer is
	// wrapped so a bounded copy of what the client receives is available
	// after the fact, and the request material rides on the context. With
	// no session (the default) this is a single cached read and returns w
	// and r untouched. Bodies are only ever persisted by finishCapture,
	// after the usage event is written and the full filter has matched.
	// Decided once the principal is fully known, so a group criterion can
	// take part in the up-front check; nothing above writes to w. The
	// request is replaced, so ctx is re-read: the pending capture rides on
	// the new request's context and finishCapture must find it there.
	w, r = s.beginCapture(w, r, event, body, bodyOverflows)
	ctx = r.Context()

	// quotaSubjectID is the identity quota rules are evaluated against. For a
	// service token it is the token id (paired with the service_token subject
	// type in the store), so an unattended integration can be bounded exactly
	// like a person.
	quotaSubjectID := ""
	if serviceToken != nil {
		quotaSubjectID = serviceToken.ID
	} else {
		quotaSubjectID = user.ID
	}

	// 1. Policy blocking rules run first: they are the cheapest check and are
	//    evaluated before any grant or quota work.
	rules, err := s.cachedBlockingRules(ctx)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	tokenPrefix := ""
	if serviceToken != nil {
		tokenPrefix = serviceToken.Prefix
	} else {
		tokenPrefix = token.Prefix
	}
	signals := authz.SignalsFromRequest(r, event.ClientIP, tokenPrefix, modelName)
	if rule, clause := authz.Match(rules, signals); rule != nil {
		event.BlockingRuleID = rule.ID
		if err := s.Store.RecordRuleHit(ctx, rule.ID); err != nil {
			s.Logger.WarnContext(ctx, "record rule hit", "error", err.Error())
		}
		reason := rule.Reason
		if reason == "" {
			reason = "Matched policy rule " + rule.Name + " (" + clause + ")."
		}
		s.rejectProxyEvent(w, r, ErrEndpointBlocked().WithReason(reason), event, started)
		return
	}

	// 2. Requests-per-minute rate limits, enforced only while the
	//    per_user_rate_limits_enabled feature flag is on (flags are read via
	//    the config cache; see store.DefaultFeatureFlags for the full set).
	//    Buckets live in this process; each replica enforces the configured
	//    rate independently. Service tokens are bucketed under their own id,
	//    so a rule targeting a service token throttles that integration only.
	if refused, resetAt := s.rateLimitExceeded(ctx, quotaSubjectID, r.URL.Path); refused {
		s.rejectProxyEvent(w, r, ErrRateLimited(
			"You have exceeded the request rate configured for this endpoint. Wait for the limit to refill and retry.").WithReset(resetAt), event, started)
		return
	}

	// 3. Model resolution and grant check.
	if modelName == "" {
		s.rejectProxyEvent(w, r, ErrInvalidRequest("Specify a model in the request body, for example {\"model\": \"gpt-4o-mini\"}.").WithParam("model"), event, started)
		return
	}
	// Resolution covers both real models and managed-model aliases. When an
	// alias is used, `resolved.Model` is the underlying model that will serve
	// and be billed; `resolved.Managed` describes the indirection.
	resolved, err := s.cachedResolveModel(ctx, modelName)
	if errors.Is(err, store.ErrManagedModelFallbackExhausted) {
		// The alias's target is unavailable AND the fallback the admin
		// configured for exactly this case cannot take over. Name both
		// faults so the admin knows there are two things to fix.
		primary, fallback := "its underlying model is unavailable", "the fallback model is unavailable"
		if resolved.Managed != nil {
			if resolved.Managed.BrokenReason != "" {
				primary = resolved.Managed.BrokenReason
			}
			if resolved.Managed.FallbackBrokenReason != "" {
				fallback = resolved.Managed.FallbackBrokenReason
			}
			event.RequestedModelName = resolved.Managed.Name
		}
		s.rejectProxyEvent(w, r, ErrManagedModelFallbackExhausted(modelName, primary, fallback), event, started)
		return
	}
	if errors.Is(err, store.ErrManagedModelBroken) {
		// The alias exists and the caller addressed it correctly — the
		// gateway's own configuration is at fault, so say so plainly rather
		// than implying the caller mistyped a model name.
		reason := "its underlying model is unavailable"
		if resolved.Managed != nil && resolved.Managed.BrokenReason != "" {
			reason = resolved.Managed.BrokenReason
		}
		if resolved.Managed != nil {
			event.RequestedModelName = resolved.Managed.Name
		}
		s.rejectProxyEvent(w, r, ErrManagedModelUnavailable(modelName, reason), event, started)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.rejectProxyEvent(w, r, ErrModelNotGranted(modelName), event, started)
		return
	}
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	// Runtime failure modes (unreachable upstream, model down/degraded on
	// recent traffic) are evaluated on every request, outside the resolution
	// cache, so an alias switches to its fallback as soon as the signal
	// appears and back as soon as it clears.
	resolved = s.applyManagedFallback(ctx, resolved)
	model := resolved.Model

	// Grants are resolved against whichever principal kind is calling. A
	// managed alias is granted in its own right: holding a grant on the
	// underlying model does NOT imply access to an alias pointing at it, and
	// vice versa, so an admin's alias grants are an independent control.
	granted, err := s.cachedGrantedIDs(ctx, store.GrantSubject{
		UserID: event.UserID, GroupIDs: groupIDs, ServiceTokenID: event.ServiceTokenID, TeamID: event.TeamIDs,
	})
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	accessID := model.ID
	if resolved.ViaManagedModel() {
		accessID = resolved.Managed.ID
	}
	if _, ok := granted[accessID]; !ok {
		s.rejectProxyEvent(w, r, ErrModelNotGranted(modelName), event, started)
		return
	}
	if resolved.FellBack {
		// The caller can see the substitution; the request log records the
		// model that actually served, exactly as for any alias. Set only
		// once the caller is authorised, so a rejection reveals nothing
		// about the alias's configuration.
		w.Header().Set(headerFallbackReason, resolved.FallbackReason)
		event.FallbackReason = resolved.FallbackReason
	}
	event.ModelID = model.ID
	event.UpstreamID = model.UpstreamID
	// Usage is recorded under the caller-facing catalog name — the admin-set
	// display name when one exists — so the request log and dashboards match
	// what users see in /v1/models. model_id stays the stable native key that
	// grants, quotas, and rate cards join on.
	//
	// For a managed alias this deliberately records the UNDERLYING model:
	// model reporting must always reflect what actually ran. The alias the
	// caller asked for is preserved separately in requested_model_name so
	// alias adoption stays visible without contaminating model reports.
	event.ModelName = model.PublicName()
	if classification, classificationErr := s.Store.GetReportModelClassification(ctx, model.ID); classificationErr == nil {
		event.ModelFamily = classification.Family
		event.ModelProvider = classification.Provider
		event.ModelHosting = classification.Hosting
	} else if !errors.Is(classificationErr, store.ErrNotFound) {
		// Reporting must not make a healthy inference path unavailable. Missing
		// metadata is explicit in coverage, and an operator gets the real error.
		s.Logger.ErrorContext(ctx, "reporting classification unavailable", "model_id", model.ID, "error", classificationErr)
	}
	if resolved.ViaManagedModel() {
		event.RequestedModelName = resolved.Managed.Name
	}

	// The upstream only understands the native model name. When the caller
	// addressed a renamed model by its display name, the buffered JSON body's
	// model field is rewritten to the native name before the adapter sees it.
	// Requests already carrying the native name skip this branch entirely and
	// are forwarded byte-for-byte, exactly as before renames existed.
	if modelName != model.Name {
		if bodyOverflows {
			s.rejectProxyEvent(w, r, errAliasRewriteImpossible(modelName, model.Name,
				"the request body is too large for the gateway to buffer and rewrite"), event, started)
			return
		}
		rewritten, ok := rewriteJSONModelField(body, model.Name)
		if !ok {
			s.rejectProxyEvent(w, r, errAliasRewriteImpossible(modelName, model.Name,
				"the request body is not a JSON object with a \"model\" field the gateway can rewrite"), event, started)
			return
		}
		body = rewritten
	}

	// 3b. Security Gateway, ingress. Runs on the OpenAI-shaped body before
	//     the adapter translates it, so one implementation covers every
	//     provider. The common case (no bindings) is one cached read that
	//     returns an empty effective set. A policy that failed to compile
	//     fails closed: the admin's intent was to filter, and silently
	//     forwarding unfiltered traffic would be the worst outcome.
	secgwEng, err := s.secgwEngine(ctx)
	if err != nil {
		s.Logger.ErrorContext(ctx, "secgw: engine unavailable, failing closed", "error", err.Error(), "request_id", event.RequestID)
		s.rejectProxyEvent(w, r, ErrSecurityBlocked("", true), event, started)
		return
	}
	secgwEff := secgwEng.Resolve(secgwSubject(event, groupIDs, resolved))
	var secgwViolations []secgw.Violation
	if !secgwEff.Empty() {
		if bodyOverflows {
			// The body cannot be inspected in full; a policy applies, so
			// refuse rather than forward what was not scanned.
			s.rejectProxyEvent(w, r, ErrPayloadTooLarge(maxInMemoryBody), event, started)
			return
		}
		secgwStart := time.Now()
		dec, derr := secgwEng.Ingress(ctx, secgwEff, secgw.Request{Body: body, IsServiceToken: serviceToken != nil})
		trace.Child("secgw.ingress", telemetry.SpanKindInternal, secgwStart, time.Now())
		if derr != nil {
			s.Logger.ErrorContext(ctx, "secgw: ingress", "error", derr.Error(), "request_id", event.RequestID)
			s.rejectProxyEvent(w, r, ErrInternal(), event, started)
			return
		}
		secgwViolations = dec.Violations
		event.SecgwAction = dec.Action
		event.SecgwViolations = secgw.Recorded(dec.Violations)
		if dec.Action == store.SecgwActionBlocked {
			s.attachSecgwViolations(event, secgwEff.Policy, secgwViolations)
			s.writeSecgwBlock(w, r, event, secgwEff.Policy, dec, started)
			return
		}
		if dec.Action == store.SecgwActionRedacted {
			body = dec.Body
		}
		secgwActionHeader(w, dec.Action, dec.Redactions)
	}

	// 4. Quota enforcement, ahead of any upstream contact so a breached
	//    principal can never incur provider spend. quotaSubject carries
	//    whichever principal kind is calling, so a service token is bounded
	//    by its own rules exactly as a user is bounded by theirs.
	quotaSubject := quota.UserSubject(event.UserID, teamIDs)
	if serviceToken != nil {
		quotaSubject = quota.ServiceTokenSubject(serviceToken.ID)
	}
	quotaStart := time.Now()
	decision, err := s.Quota.Check(ctx, quotaSubject, model.ID)
	s.Metrics.QuotaCheckLatency.Observe(time.Since(quotaStart).Seconds())
	trace.Child("quota.check", telemetry.SpanKindInternal, quotaStart, time.Now())
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	if !decision.Allowed {
		event.QuotaViolated = true
		breached := decision.Breached
		s.Metrics.QuotaBreaches.WithLabelValues(breached.Quota.Metric, breached.Quota.SubjectType).Inc()
		s.rejectProxyEvent(w, r, ErrQuotaExceeded(quotaBreachMessage("Your", breached)).WithReset(breached.ResetAt), event, started)
		return
	}

	// 5. Resolve the upstream and its adapter.
	upstream, err := s.cachedUpstreamByID(ctx, model.UpstreamID)
	if err != nil || !upstream.Enabled {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(model.UpstreamName), event, started)
		return
	}
	providerAdapter, err := adapter.Get(upstream.AdapterType)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	apiKey, err := s.Cipher.Decrypt(upstream.EncryptedKey())
	if err != nil {
		s.Logger.ErrorContext(ctx, "decrypt upstream credential", "upstream", upstream.Name, "error", err.Error())
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	// adapterReq is handed to Prepare AND to the response-side hooks
	// (TransformResponse / NewStreamTransformer), so an adapter that
	// translated the request knows to translate the response back.
	adapterReq := &adapter.Request{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(),
		// Model is always the native upstream name: adapters that embed the
		// model in the URL (Bedrock, Vertex) must never see a display name.
		Body: body, Model: model.Name, Streaming: event.Streaming,
		// Side-channel calls (e.g. Vertex token minting) must follow the
		// caller's cancellation and the gateway's CA/proxy configuration.
		Context: ctx, Client: s.upstreamHTTPClient(ctx),
	}
	prepared, err := providerAdapter.Prepare(adapter.Upstream{
		ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType,
	}, adapterReq)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name).WithReason(err.Error()), event, started)
		return
	}
	if r.URL.RawQuery != "" {
		prepared.URL += "?" + r.URL.RawQuery
	}

	// 6. Forward. Generative calls are never retried: a retry is a second bill.
	//
	// Bodies that fit the in-memory buffer are sent from the (possibly
	// adapter-rewritten) buffer. Larger bodies are relayed unbuffered: the
	// buffered prefix is replayed and the rest streams from the client, so
	// uploads of any size reach the provider byte-for-byte. That is only
	// possible when the adapter forwards bodies verbatim; an adapter that must
	// rewrite the body (chat translation) cannot act on a partial buffer, so
	// that combination is refused with an explicit 413 rather than forwarding
	// a corrupted payload.
	var outboundBody io.Reader = bytes.NewReader(prepared.Body)
	outboundLength := int64(len(prepared.Body))
	if bodyOverflows {
		if !bytes.Equal(prepared.Body, body) {
			s.rejectProxyEvent(w, r, ErrPayloadTooLarge(maxInMemoryBody), event, started)
			return
		}
		outboundBody = io.MultiReader(bytes.NewReader(body), bodyRemainder)
		outboundLength = r.ContentLength // -1 (chunked) when the client did not declare one
		if outboundLength == 0 {
			outboundLength = -1
		}
	}
	upstreamStart := time.Now()
	outbound, err := http.NewRequestWithContext(ctx, r.Method, prepared.URL, outboundBody)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	outbound.Header = prepared.Header
	outbound.ContentLength = outboundLength

	// The upstream may bill an attempt even if the connection fails.
	if !s.Config.LocalOnly {
		event.CostStatus = "unknown"
	}
	resp, err := s.upstreamHTTPClient(ctx).Do(outbound)
	if err != nil {
		s.Metrics.UpstreamErrors.WithLabelValues(upstream.Name, "unreachable").Inc()
		trace.Child("upstream.call", telemetry.SpanKindClient, upstreamStart, time.Now(),
			telemetry.String("upstream", upstream.Name), telemetry.String("error", "unreachable"))
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	event.TTFBMs = int(time.Since(upstreamStart).Milliseconds())
	event.HTTPStatus = resp.StatusCode
	if resp.StatusCode == http.StatusTooManyRequests {
		s.Metrics.UpstreamErrors.WithLabelValues(upstream.Name, "rate_limit").Inc()
		event.ErrorCode = CodeUpstreamLimit
	} else if resp.StatusCode >= 500 {
		s.Metrics.UpstreamErrors.WithLabelValues(upstream.Name, "5xx").Inc()
	}

	rates := usage.Rates{
		InNano: model.RateInNano, OutNano: model.RateOutNano, CachedNano: model.RateCachedNano,
		CacheWrite5mNano: model.RateCacheWrite5mNano, CacheWrite1hNano: model.RateCacheWrite1hNano,
	}
	card := s.cachedRateCard(ctx, model.ID)
	if card.OK {
		rates = usage.Rates{
			InNano: card.In, OutNano: card.Out, CachedNano: card.Cached,
			CacheWrite5mNano: card.CacheWrite5m, CacheWrite1hNano: card.CacheWrite1h,
		}
	}

	stampReportingCost(event, rates, card.OK, s.Config.LocalOnly)
	streaming := isEventStream(resp.Header) || event.Streaming
	// A request can stream without saying `stream: true` (TTS with
	// stream_format: sse); the recorded event reflects how the response was
	// actually relayed, which is what the throughput derivation keys on.
	event.Streaming = streaming

	var extracted adapter.Usage
	if streaming {
		// The transformer is created before headers are copied: an adapter
		// whose native stream is not SSE (Bedrock's binary event-stream)
		// corrects Content-Type to text/event-stream for the translated output.
		transformer := providerAdapter.NewStreamTransformer(adapterReq, resp.Header)
		copyResponseHeaders(w, resp.Header)
		hardKill := decision.HardKill || s.Quota.HardKillActive(ctx, quotaSubject, model.ID)
		var hold *stream.Holdback
		if hasEgressChecks(secgwEff) && resp.StatusCode < 300 {
			hold = stream.New(secgwHoldBytes(secgwEff), secgwEng.MaxSpan(), secgwStreamScanner(secgwEng, secgwEff))
		}
		extracted = s.relayStream(w, r, resp, providerAdapter, transformer, event, hardKill, quotaSubject, rates, hold, &secgwViolations)
	} else {
		var egress *secgwEgress
		if hasEgressChecks(secgwEff) && resp.StatusCode < 300 {
			egress = &secgwEgress{eng: secgwEng, eff: secgwEff, sink: &secgwViolations, reqBody: adapterReq.Body}
		}
		copyResponseHeaders(w, resp.Header)
		extracted = s.relayBuffered(w, resp, providerAdapter, adapterReq, event, rates, upstreamStart, egress)
	}
	if len(secgwViolations) > 0 {
		event.SecgwViolations = secgw.Recorded(secgwViolations)
		if event.SecgwAction == "" {
			event.SecgwAction = store.SecgwActionObserved
		}
	} else if secgwEff != nil && !secgwEff.Empty() && event.SecgwAction == "" {
		// A policy covered this request and found nothing. Recorded so the
		// log can distinguish "inspected, clean" from "never inspected".
		event.SecgwAction = store.SecgwActionChecked
	}

	event.LatencyMs = int(time.Since(started).Milliseconds())
	event.UpstreamMs = int(time.Since(upstreamStart).Milliseconds())
	trace.Child("upstream.call", telemetry.SpanKindClient, upstreamStart, time.Now(),
		telemetry.String("upstream", upstream.Name),
		telemetry.String("http.status_code", strconv.Itoa(resp.StatusCode)))
	if bodyOverflows {
		// The remainder has been consumed by the upstream write; only now is
		// the true request size known.
		event.RequestBytes = bodyRemainder.total
	}
	event.FinishReason = extracted.FinishReason
	// Failed requests ran no inference, so they must never be costed: without
	// this guard the byte-count fallback meters the upstream ERROR body as if
	// it were model output. The event is still recorded (status, latency,
	// bytes, error_code) — it is just marked non-billable, so a non-billed
	// event stays distinguishable from one billed at zero.
	if resp.StatusCode < 300 {
		if event.AccountingMode == "" {
			// The buffered path already ran applyUsage (it needs the cost
			// before flushing headers); running it twice would double the
			// unmetered-gap log line and counter.
			s.applyUsage(event, extracted, rates)
		}
		if event.Streaming {
			// A stream's headers were flushed long ago; throughput is only
			// knowable at the end, so it travels as HTTP trailers (the
			// TrailerPrefix form needs no advance declaration). The buffered
			// path computed and set its headers inside relayBuffered, before
			// WriteHeader, so the recorded and returned figures match.
			applyThroughput(event, extracted, time.Since(upstreamStart), time.Duration(event.TTFBMs)*time.Millisecond)
			for k, v := range throughputHeaders(event) {
				w.Header().Set(http.TrailerPrefix+k, v)
			}
		}
	} else {
		event.AccountingMode = usage.AccountingNone
	}

	s.attachSecgwViolations(event, secgwEff.Policy, secgwViolations)
	s.recordEvent(ctx, event, quotaSubject, model, trace)
	s.observeProxyMetrics(event, upstream.Name, started)
}

// effectiveUpstreamTimeouts resolves the timeout set in force right now: the
// environment defaults (JANUS_UPSTREAM_*_TIMEOUT_SECONDS) with any
// administrator override from the database layered on top. Overrides are
// read through the config cache, so the hot path costs one map lookup per
// request and a change made on another replica is picked up within the cache
// TTL. A storage failure falls back to the environment defaults — the proxy
// must keep serving during a database blip — and is logged so it is not silent.
func (s *Server) effectiveUpstreamTimeouts(ctx context.Context) config.UpstreamTimeouts {
	defaults := s.Config.UpstreamTimeoutDefaults()
	if s.Store == nil {
		return defaults
	}
	overrides, err := s.cachedUpstreamTimeoutOverrides(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "read upstream timeout overrides; using environment defaults", "error", err.Error())
		return defaults
	}
	return defaults.WithOverrides(overrides.ConnectSeconds, overrides.TTFBSeconds, overrides.TotalSeconds)
}

// upstreamHTTPClient returns the shared client used for every upstream call,
// built for the timeouts currently in effect. Connections are pooled across
// requests: the client is only rebuilt when the effective timeout set changes
// (an administrator saved new values, or the environment defaults were the
// only thing in force and an override just landed). The previous client's
// idle connections are released; requests already in flight on it finish
// undisturbed under the timeouts they started with.
//
// All three timeout knobs are wired into the transport:
//   - the dialer timeout bounds connection establishment,
//   - ResponseHeaderTimeout bounds time-to-first-byte after the request is sent,
//   - the client timeout bounds the whole exchange.
//
// TLSClientConfig carries the JANUS_CA_BUNDLE trust store so internally-signed
// upstreams (corporate vLLM/Ollama behind a private CA) verify; it is nil —
// default verification — when no bundle is configured.
func (s *Server) upstreamHTTPClient(ctx context.Context) *http.Client {
	timeouts := s.effectiveUpstreamTimeouts(ctx)
	if current := s.upstreamClient.Load(); current != nil && current.timeouts == timeouts {
		return current.client
	}
	s.upstreamClientMu.Lock()
	defer s.upstreamClientMu.Unlock()
	current := s.upstreamClient.Load()
	if current != nil && current.timeouts == timeouts {
		return current.client // another request rebuilt it while we waited
	}
	next := &upstreamClientState{timeouts: timeouts, client: s.buildUpstreamClient(timeouts)}
	s.upstreamClient.Store(next)
	if current != nil {
		current.client.CloseIdleConnections()
		s.Logger.InfoContext(ctx, "upstream timeouts changed; rebuilt upstream HTTP client",
			"connect_seconds", int(timeouts.Connect.Seconds()),
			"ttfb_seconds", int(timeouts.TTFB.Seconds()),
			"total_seconds", int(timeouts.Total.Seconds()))
	}
	return next.client
}

// buildUpstreamClient constructs a pooled client for one timeout set.
func (s *Server) buildUpstreamClient(timeouts config.UpstreamTimeouts) *http.Client {
	transport := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: s.Config.TLSClientConfig(),
		DialContext: (&net.Dialer{
			Timeout:   timeouts.Connect,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeouts.TTFB,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: timeouts.Total}
}

// applyUsage fills token counts and cost from the upstream's report, or falls
// back to byte estimation when nothing was reported AND the modality is text.
//
// Precedence:
//
//  1. A provider-reported COST (X.ai cost_in_usd_ticks) is the exact bill and
//     wins outright; token counts, if any, are kept for the record but the
//     rate card is not consulted. AccountingUpstreamCost.
//  2. Provider-reported token counts priced by the rate card.
//     AccountingUpstream.
//  3. Text-shaped modality with no usage: ~bytes/4. AccountingBytes.
//  4. Anything else: zero tokens, zero cost. AccountingUnmetered.
//
// The byte fallback is a ~4-bytes-per-token approximation for ENGLISH TEXT. It
// is meaningless for a binary or media response: a 30 KB MP3 becomes 7,680
// phantom "output tokens" against a real 48 (a measured 159x overcharge), and
// the same image billed as a URL vs base64 differs by three orders of magnitude
// purely because the response encoding changed. Worse, those invented tokens
// feed the quota engine, so a user can be hard-killed at a fraction of their
// real budget on fabricated numbers.
//
// So the fallback is applied only to text-shaped modalities. Anything else
// records zero tokens and zero cost under AccountingUnmetered — distinct from
// AccountingNone (error responses) and from AccountingBytes (legitimate text
// estimates) so the admin view can list exactly which models lack real usage
// extraction. The gap is logged and counted (janus_unmetered_requests_total)
// because it is a configuration problem, not a billing outcome.
func (s *Server) applyUsage(event *store.UsageEvent, extracted adapter.Usage, rates usage.Rates) {
	switch {
	case extracted.CostReported:
		event.TokensIn, event.TokensOut, event.TokensCached = extracted.TokensIn, extracted.TokensOut, extracted.TokensCached
		event.TokensCacheWrite5m, event.TokensCacheWrite1h = extracted.TokensCacheWrite5m, extracted.TokensCacheWrite1h
		event.AccountingMode = usage.AccountingUpstreamCost
		event.CostNano = extracted.CostNano
		if s.Config.LocalOnly {
			event.CostNano = 0
		}
		return
	case extracted.Reported:
		event.TokensIn, event.TokensOut, event.TokensCached = extracted.TokensIn, extracted.TokensOut, extracted.TokensCached
		event.TokensCacheWrite5m, event.TokensCacheWrite1h = extracted.TokensCacheWrite5m, extracted.TokensCacheWrite1h
		event.AccountingMode = usage.AccountingUpstream
	case usage.ByteEstimatable(event.Modality):
		event.TokensIn = usage.EstimateTokensFromBytes(event.RequestBytes)
		event.TokensOut = usage.EstimateTokensFromBytes(event.ResponseBytes)
		// Byte estimation cannot see cache writes; they stay zero so the
		// cost formula degrades to exactly the pre-cache-write arithmetic.
		event.TokensCacheWrite5m, event.TokensCacheWrite1h = 0, 0
		event.AccountingMode = usage.AccountingBytes
	default:
		event.TokensIn, event.TokensOut, event.TokensCached = 0, 0, 0
		event.TokensCacheWrite5m, event.TokensCacheWrite1h = 0, 0
		event.AccountingMode = usage.AccountingUnmetered
		event.CostNano = 0
		s.Logger.Warn("usage not metered: media response carried no usage the adapter could read; fix the upstream's usage extraction (configuration gap)",
			"modality", event.Modality,
			"model", event.ModelName,
			"upstream_id", event.UpstreamID,
			"endpoint", event.EndpointPath,
			"streaming", event.Streaming,
			"response_bytes", event.ResponseBytes,
			"request_id", event.RequestID)
		if s.Metrics != nil && s.Metrics.UnmeteredRequests != nil {
			s.Metrics.UnmeteredRequests.WithLabelValues(event.UpstreamID, event.ModelName, event.Modality).Inc()
		}
		return
	}
	if s.Config.LocalOnly {
		// Local-only mode (JANUS_LOCAL_ONLY): cost tracking is off, so the
		// event records zero cost while token counts stay fully metered.
		event.CostNano = 0
		return
	}
	event.CostNano = usage.ComputeCostAll(usage.TokenCounts{
		In: event.TokensIn, Out: event.TokensOut, Cached: event.TokensCached,
		CacheWrite5m: event.TokensCacheWrite5m, CacheWrite1h: event.TokensCacheWrite1h,
		CachedDisjoint: extracted.CachedDisjoint,
	}, rates)
}

// applyThroughput fills the event's tokens-per-second figures. Provider-measured
// throughput is forwarded as-is (ThroughputUpstream). Otherwise the gateway
// derives it from its own clock (ThroughputCalculated): for a stream, prompt
// processing is approximated by time-to-first-byte and generation by the
// remainder; for a buffered response only the total is known, so both phases
// share it. Calculated figures include network and proxy time and are
// therefore a lower bound on the provider's real speed — which is why the
// headers that carry them say "Calculated".
func applyThroughput(event *store.UsageEvent, extracted adapter.Usage, upstreamElapsed, ttfb time.Duration) {
	if extracted.ThroughputReported {
		event.TokensInPerSecond = extracted.TokensInPerSecond
		event.TokensOutPerSecond = extracted.TokensOutPerSecond
		event.ThroughputSource = usage.ThroughputUpstream
		return
	}
	if event.TokensIn == 0 && event.TokensOut == 0 {
		return
	}
	if upstreamElapsed <= 0 {
		return
	}
	inWindow, outWindow := upstreamElapsed, upstreamElapsed
	if event.Streaming && ttfb > 0 && ttfb < upstreamElapsed {
		inWindow, outWindow = ttfb, upstreamElapsed-ttfb
	}
	event.TokensInPerSecond = usage.TokensPerSecond(event.TokensIn, inWindow)
	event.TokensOutPerSecond = usage.TokensPerSecond(event.TokensOut, outWindow)
	event.ThroughputSource = usage.ThroughputCalculated
}

// Throughput response headers. Reported and calculated figures use DIFFERENT
// header names so a client can never mistake a gateway-clock estimate for a
// provider measurement; X-Janus-Throughput-Source names which pair is present.
const (
	headerThroughputSource       = "X-Janus-Throughput-Source"
	headerTokensInPerSecond      = "X-Janus-Tokens-In-Per-Second"
	headerTokensOutPerSecond     = "X-Janus-Tokens-Out-Per-Second"
	headerCalcTokensInPerSecond  = "X-Janus-Calculated-Tokens-In-Per-Second"
	headerCalcTokensOutPerSecond = "X-Janus-Calculated-Tokens-Out-Per-Second"
)

// throughputHeaders renders the event's throughput as response headers. Empty
// when no throughput was determined (unmetered response, zero tokens).
func throughputHeaders(event *store.UsageEvent) map[string]string {
	if event.ThroughputSource == "" {
		return nil
	}
	inKey, outKey := headerTokensInPerSecond, headerTokensOutPerSecond
	if event.ThroughputSource == usage.ThroughputCalculated {
		inKey, outKey = headerCalcTokensInPerSecond, headerCalcTokensOutPerSecond
	}
	return map[string]string{
		headerThroughputSource: event.ThroughputSource,
		inKey:                  strconv.FormatFloat(event.TokensInPerSecond, 'f', 2, 64),
		outKey:                 strconv.FormatFloat(event.TokensOutPerSecond, 'f', 2, 64),
	}
}

// relayBuffered forwards a non-streaming response and extracts token counts.
//
// Bodies are buffered up to MaxResponseBytes+1: the extra byte detects
// responses that overflow the cap (large base64 image-generation payloads)
// without consuming the remainder. An oversized body is relayed unbuffered —
// the buffered prefix is replayed and the rest streams straight from the
// upstream — so the client receives it byte-for-byte. Usage extraction is
// skipped for those (the prefix is unparseable JSON); metering falls back to
// byte counting. What must never happen is the pre-fix behaviour: a truncated
// body delivered under the upstream's success status with no error signal.
func (s *Server) relayBuffered(w http.ResponseWriter, resp *http.Response, providerAdapter adapter.Adapter, adapterReq *adapter.Request, event *store.UsageEvent, rates usage.Rates, upstreamStart time.Time, egress *secgwEgress) adapter.Usage {
	payload, err := io.ReadAll(io.LimitReader(resp.Body, s.Config.MaxResponseBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return adapter.Usage{}
	}
	if int64(len(payload)) > s.Config.MaxResponseBytes {
		w.WriteHeader(resp.StatusCode)
		total, err := io.Copy(w, io.MultiReader(bytes.NewReader(payload), resp.Body))
		if err != nil {
			s.Logger.Warn("relay oversized proxied response", "error", err.Error(), "request_id", event.RequestID)
		}
		event.ResponseBytes = total
		return adapter.Usage{}
	}
	event.ResponseBytes = int64(len(payload))
	extracted, _ := providerAdapter.ExtractUsage(payload)

	// Metering has read the provider's native body; now translate it into
	// the OpenAI shape the caller speaks (a no-op for OpenAI-wire providers).
	// A body that cannot be translated is never relayed as-is — the client
	// could not read it — so the failure is reported as a gateway error
	// instead (translate-or-fail-loudly).
	translated, err := providerAdapter.TransformResponse(adapterReq, payload)
	if err != nil {
		s.Logger.Warn("translate proxied response", "error", err.Error(), "adapter", providerAdapter.Type(), "request_id", event.RequestID)
		apiErr := newError(http.StatusBadGateway, CodeUpstreamDown, "server_error",
			"The upstream response could not be translated to the OpenAI format: "+err.Error())
		apiErr.RequestID = event.RequestID
		event.ErrorCode = CodeUpstreamDown
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": apiErr})
		return extracted
	}
	payload = translated

	// Security Gateway, egress. The body is OpenAI-shaped now, so one scan
	// covers every provider. A block replaces the body with the refusal
	// shape; a redaction rewrites the spans in place and says so in a
	// header. Tokens the upstream generated are metered regardless: the
	// spend happened, and hiding it would misstate what the filter costs.
	if egress != nil && resp.StatusCode < 300 {
		res := egress.eng.Egress(egress.eff, payload)
		*egress.sink = append(*egress.sink, res.Violations...)
		// Content safety on egress is observe-only (v1 ruling): judged and
		// recorded, never withheld. Streaming responses are not judged at
		// all — this is the buffered path only.
		if cs := egress.eng.EgressContentSafety(adapterReq.Context, egress.eff, egress.reqBody, payload); len(cs) > 0 {
			*egress.sink = append(*egress.sink, cs...)
			if res.Action == "" {
				res.Action = store.SecgwActionObserved
			}
		}
		switch res.Action {
		case store.SecgwActionBlocked:
			s.applyUsage(event, extracted, rates)
			event.SecgwAction = store.SecgwActionBlocked
			event.SecgwViolations = secgw.Recorded(*egress.sink)
			event.ErrorCode = CodeSecurityBlocked
			apiErr := ErrSecurityBlocked(res.BlockKind, false)
			apiErr.Message = "The model's response was withheld: it matched your organisation's security policy."
			apiErr.RequestID = event.RequestID
			event.HTTPStatus = apiErr.Status()
			s.Metrics.PolicyRejections.WithLabelValues(CodeSecurityBlocked).Inc()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(apiErr.Status())
			_ = json.NewEncoder(w).Encode(map[string]any{"error": apiErr})
			return extracted
		case store.SecgwActionRedacted:
			payload = res.Body
			event.SecgwAction = store.SecgwActionRedacted
			secgwActionHeader(w, res.Action, res.Redactions)
		case store.SecgwActionObserved:
			if event.SecgwAction == "" {
				event.SecgwAction = store.SecgwActionObserved
			}
		}
	}

	// Cost is only knowable after the body is parsed, so this header is
	// meaningful on buffered responses only (streams carry no trailer here).
	// It must carry the same value the metering pipeline records, so the
	// usage/cost computation runs here, before the header is flushed.
	if resp.StatusCode < 300 {
		s.applyUsage(event, extracted, rates)
		w.Header().Set("X-Janus-Cost-USD", strconv.FormatFloat(usage.USD(event.CostNano), 'f', -1, 64))
		w.Header().Set("X-Janus-Token-Accounting", event.AccountingMode)
		// Throughput for a buffered response: the whole upstream exchange is
		// the only window the gateway can measure, so a calculated figure
		// here covers prompt + generation together.
		applyThroughput(event, extracted, time.Since(upstreamStart), 0)
		for k, v := range throughputHeaders(event) {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(payload); err != nil {
		s.Logger.Warn("write proxied response", "error", err.Error())
	}
	return extracted
}

// hardKillRecheckInterval bounds how often a hard-kill stream re-evaluates its
// quotas. Without a floor, every relayed SSE line triggers a quota check —
// several DB queries each — once the collector has usage (Anthropic reports
// input tokens on the first frame), which is unbounded DB load on long
// streams. A breach is still enforced within one interval.
const hardKillRecheckInterval = 250 * time.Millisecond

// relayStream forwards Server-Sent Events line by line without buffering, so the
// client sees tokens as they are produced. userID and teamIDs are resolved once
// by the caller so mid-stream quota checks never re-query the principal.
//
// Each upstream line is fed to the adapter's usage collector in its native
// form (metering reads what the provider actually sent), then passed through
// the adapter's stream transformer, which rewrites provider-native events
// into OpenAI chat.completion.chunk frames — a pass-through for providers
// that already speak the OpenAI wire format. ResponseBytes counts upstream
// bytes, the quantity the byte-estimate metering fallback is calibrated on.
func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, providerAdapter adapter.Adapter, transformer adapter.StreamTransformer, event *store.UsageEvent, hardKill bool, subject quota.Subject, rates usage.Rates, hold *stream.Holdback, violations *[]secgw.Violation) adapter.Usage {
	s.Metrics.StreamsActive.Inc()
	defer s.Metrics.StreamsActive.Dec()

	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	collector := providerAdapter.NewStreamCollector()
	if transformer == nil {
		transformer = adapter.PassthroughStream{}
	}

	// write hands a transformed frame to the client, through the Security
	// Gateway hold-back when one applies. The hold-back keeps a reassembled
	// plaintext buffer per channel and releases frames only once their
	// text is older than the hold window and scanned clean; on a blocking
	// match it drops everything held (never written) and reports a cut.
	// Returns false when the stream must stop.
	write := func(out []byte) bool {
		if hold == nil {
			if _, err := w.Write(out); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}
		res := hold.Feed(out)
		if violations != nil {
			secgwCollectStreamViolations(res, violations)
		}
		if res.Cut {
			event.SecgwAction = store.SecgwActionStreamCut
			event.ErrorCode = CodeSecurityBlocked
			s.Logger.InfoContext(r.Context(), "stream cut by security policy", "request_id", event.RequestID)
			s.Metrics.PolicyRejections.WithLabelValues(CodeSecurityBlocked).Inc()
			var kind store.SecgwCheckKind
			if violations != nil {
				kind = streamCutKind(*violations)
			}
			s.writeStreamSecurityError(w, flusher, kind, event.RequestID)
			return false
		}
		if res.Redactions > 0 && event.SecgwAction == "" {
			event.SecgwAction = store.SecgwActionRedacted
		}
		for _, f := range res.Release {
			if _, err := w.Write(f); err != nil {
				return false
			}
		}
		if len(res.Release) > 0 && flusher != nil {
			flusher.Flush()
		}
		return true
	}
	cutByPolicy := false

	// The response cap must hold even when the upstream never emits a
	// newline: ReadBytes accumulates until it sees one, so without this
	// LimitReader a misbehaving upstream sending one endless "line" would
	// grow gateway memory without bound (the total check below only runs
	// between complete lines). With it, a single line can occupy at most
	// MaxResponseBytes+1 bytes before the loop observes the overrun and cuts
	// the stream.
	reader := bufio.NewReaderSize(io.LimitReader(resp.Body, s.Config.MaxResponseBytes+1), 32<<10)
	var total int64
	var nextQuotaCheck time.Time
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			total += int64(len(line))
			collector.Feed(line)
			out, transformErr := transformer.TransformStreamLine(line)
			if transformErr != nil {
				// The provider's stream can no longer be translated; relaying
				// its raw frames would hand the client something unreadable.
				// Cut the stream with an in-band error, exactly like the
				// hard-kill and cap paths, so the failure is attributable.
				s.Logger.WarnContext(r.Context(), "translate proxied stream", "error", transformErr.Error(), "adapter", providerAdapter.Type(), "request_id", event.RequestID)
				event.ErrorCode = CodeUpstreamDown
				s.writeStreamTranslateError(w, flusher, transformErr, event.RequestID)
				break
			}
			if len(out) > 0 {
				if !write(out) {
					cutByPolicy = event.SecgwAction == store.SecgwActionStreamCut
					break // client disconnected or policy cut; stop relaying but still meter
				}
			}
			if hardKill && !time.Now().Before(nextQuotaCheck) {
				nextQuotaCheck = time.Now().Add(hardKillRecheckInterval)
				if breached := s.streamExceededLimit(r.Context(), event, collector, total, subject, rates); breached != nil {
					// A hard-kill quota cuts the stream the moment the limit
					// falls. The cut must be attributable: the recorded event
					// carries quota_violated + error_code (the troubleshooting
					// docs tell users to check them), and the client receives
					// a final OpenAI-style SSE error frame so a deliberate
					// kill is distinguishable from a network failure.
					event.QuotaViolated = true
					event.ErrorCode = CodeQuotaExceeded
					s.Logger.InfoContext(r.Context(), "stream cut by hard-kill quota", "request_id", event.RequestID)
					s.writeStreamQuotaError(w, flusher, breached, event.RequestID)
					break
				}
			}
			if total > s.Config.MaxResponseBytes {
				// The cut must be signalled in-band, exactly like the
				// hard-kill path above: without a final error frame the
				// client cannot distinguish this deliberate cut from a
				// network failure.
				s.Logger.WarnContext(r.Context(), "stream exceeded response cap", "request_id", event.RequestID)
				event.ErrorCode = CodeResponseTooLarge
				s.writeStreamCapError(w, flusher, event.RequestID)
				break
			}
		}
		if err != nil {
			break
		}
	}
	if hold != nil && !cutByPolicy {
		// End of stream: release whatever the hold window still holds.
		for _, f := range hold.Flush() {
			if _, err := w.Write(f); err != nil {
				break
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	event.ResponseBytes = total
	return collector.Usage()
}

// rateLimitExceeded consults the configured requests-per-minute rules for this
// user and path. It is a no-op while the per_user_rate_limits_enabled feature
// flag is off, and fails open on storage errors: a broken flag table must not
// take the proxy down.
func (s *Server) rateLimitExceeded(ctx context.Context, userID, path string) (bool, time.Time) {
	flags, err := s.cachedFeatureFlags(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "read feature flags for rate limiting", "error", err.Error())
		return false, time.Time{}
	}
	if !flags["per_user_rate_limits_enabled"] {
		return false, time.Time{}
	}
	rules, err := s.cachedRateLimitRules(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "list rate limit rules", "error", err.Error())
		return false, time.Time{}
	}
	now := time.Now()
	for _, rule := range rules {
		if !quota.MatchRateLimitRule(rule, userID, path) {
			continue
		}
		if allowed, resetAt := s.RateLimits.Allow(rule.ID+"|"+userID, rule.RequestsPerMinute, now); !allowed {
			return true, resetAt
		}
	}
	return false, time.Time{}
}

// streamExceededLimit re-evaluates quotas mid-stream for hard-kill rules only.
// The principal context is resolved once by relayStream's caller. The stream's
// own in-flight consumption is recorded only after it finishes, so the check
// must add the collector's running counts (and their estimated cost) on top of
// recorded consumption — otherwise a single large stream that started
// under-quota would never be cut by what it is itself consuming.
// It returns the breached quota status (nil when the stream may continue) so
// the caller can tell the client which limit was hit and when it resets.
//
// Not every upstream reports usage mid-stream: the OpenAI wire format
// (openai_compatible/vlm/llama_cpp with stream_options.include_usage) carries
// the usage block only on the FINAL frame, so waiting for reported usage would
// mean a hard-kill quota could never sever such a stream in flight.
// When usage is not yet reported, the stream's in-flight consumption is
// estimated from the bytes relayed so far — the same byte-count fallback the
// post-flight accounting applies (usage.EstimateTokensFromBytes) — and under
// the same rule: ONLY for text-shaped modalities. A streamed TTS or image
// response is not estimated from its bytes mid-flight any more than it is
// afterwards; its in-flight delta is zero until the upstream reports usage
// (speech.audio.done carries exact counts), so the re-check still cuts the
// stream for consumption recorded by OTHER requests but never on invented
// tokens.
func (s *Server) streamExceededLimit(ctx context.Context, event *store.UsageEvent, collector adapter.StreamCollector, relayedBytes int64, subject quota.Subject, rates usage.Rates) *quota.Status {
	u := collector.Usage()
	if !u.Reported && !u.CostReported {
		u.TokensIn, u.TokensOut, u.TokensCached = 0, 0, 0
		if usage.ByteEstimatable(event.Modality) {
			u.TokensIn = usage.EstimateTokensFromBytes(event.RequestBytes)
			u.TokensOut = usage.EstimateTokensFromBytes(relayedBytes)
		}
	}
	// Local-only mode disables cost tracking, so the in-flight delta carries
	// zero cost — USD quotas are inert and never sever a stream.
	var pendingCost int64
	if !s.Config.LocalOnly {
		if u.CostReported {
			pendingCost = u.CostNano
		} else {
			pendingCost = usage.ComputeCostAll(usage.TokenCounts{
				In: u.TokensIn, Out: u.TokensOut, Cached: u.TokensCached,
				CacheWrite5m: u.TokensCacheWrite5m, CacheWrite1h: u.TokensCacheWrite1h,
				CachedDisjoint: u.CachedDisjoint,
			}, rates)
		}
	}
	pending := quota.Delta{
		TokensIn:  u.TokensIn,
		TokensOut: u.TokensOut,
		CostNano:  pendingCost,
		// Requests is deliberately zero: this request was already admitted by
		// the pre-flight check, and a stream consumes no further requests.
	}
	decision, err := s.Quota.CheckWithPending(ctx, subject, event.ModelID, pending)
	if err != nil {
		// Deliberate fail-open, documented posture: this stream was admitted
		// by the fail-closed pre-flight check, so a storage error here means
		// choosing between severing every in-flight stream during a database
		// blip or letting already-admitted streams run to completion
		// unmetered by hard-kill rules. We choose the latter — but never
		// silently: the operator sees a warn log per failed check and the
		// janus_stream_quota_check_errors_total counter (alertable) moving.
		s.Metrics.StreamQuotaCheckErrors.Inc()
		s.Logger.WarnContext(ctx, "mid-stream hard-kill quota check failed; stream continues (fail-open)",
			"error", err.Error(), "request_id", event.RequestID)
		return nil
	}
	if decision.Allowed {
		return nil
	}
	return decision.Breached
}

// writeStreamQuotaError emits a final OpenAI-style SSE error frame before a
// hard-kill cut, so the client sees an explicit policy.quota_exceeded error
// (with reset_at) instead of an unexplained truncation. The HTTP status is
// already committed at this point, so the error can only travel in-band.
func (s *Server) writeStreamQuotaError(w http.ResponseWriter, flusher http.Flusher, breached *quota.Status, requestID string) {
	message := "This stream was stopped by a hard-kill quota."
	if breached != nil && breached.Quota != nil {
		message = "This stream was stopped: " + quotaBreachMessage("your", breached)
	}
	apiErr := ErrQuotaExceeded(message)
	if breached != nil {
		apiErr = apiErr.WithReset(breached.ResetAt)
	}
	apiErr.RequestID = requestID
	payload, err := json.Marshal(map[string]any{"error": apiErr})
	if err != nil {
		s.Logger.Warn("encode stream quota error frame", "error", err.Error(), "request_id", requestID)
		return
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return // client already gone; nothing left to tell it
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// quotaBreachMessage renders the user-facing breach copy. Calendar windows
// reset at a fixed instant. Rolling windows recover gradually as the oldest
// in-window events age out, so promising "access resumes at now+width" can
// overstate the wait by up to the full width — the reset is
// framed as an upper bound instead. possessive is "Your" or "your" so the
// same copy serves both the pre-flight rejection and the mid-stream frame.
func quotaBreachMessage(possessive string, breached *quota.Status) string {
	base := fmt.Sprintf("%s %s quota of %s %s has been used up.",
		possessive,
		quota.WindowLabel(breached.Quota.Window),
		formatLimit(breached.Quota.Metric, breached.Limit),
		quota.MetricLabel(breached.Quota.Metric))
	if quota.IsRolling(breached.Quota.Window) {
		return fmt.Sprintf("%s Capacity frees up gradually as older usage leaves the rolling window — at the latest by %s.",
			base, breached.ResetAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s Access resumes at %s.", base, breached.ResetAt.Format(time.RFC3339))
}

// writeStreamCapError emits a final in-band SSE error frame when the response
// size cap cuts a stream, mirroring writeStreamQuotaError: truncation must
// never be silent. The HTTP status is already committed, so in-band is the
// only channel left.
func (s *Server) writeStreamCapError(w http.ResponseWriter, flusher http.Flusher, requestID string) {
	apiErr := newError(http.StatusInternalServerError, CodeResponseTooLarge, "server_error",
		fmt.Sprintf("This stream was stopped: the response exceeded the gateway's %d MB response size cap (JANUS_MAX_RESPONSE_BYTES).",
			s.Config.MaxResponseBytes>>20))
	apiErr.RequestID = requestID
	payload, err := json.Marshal(map[string]any{"error": apiErr})
	if err != nil {
		s.Logger.Warn("encode stream cap error frame", "error", err.Error(), "request_id", requestID)
		return
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return // client already gone; nothing left to tell it
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeStreamTranslateError tells the client, in-band, that the upstream
// stream could not be rewritten into the OpenAI format and was cut.
func (s *Server) writeStreamTranslateError(w http.ResponseWriter, flusher http.Flusher, cause error, requestID string) {
	apiErr := newError(http.StatusBadGateway, CodeUpstreamDown, "server_error",
		"This stream was stopped: the upstream response could not be translated to the OpenAI format: "+cause.Error())
	apiErr.RequestID = requestID
	payload, err := json.Marshal(map[string]any{"error": apiErr})
	if err != nil {
		s.Logger.Warn("encode stream translate error frame", "error", err.Error(), "request_id", requestID)
		return
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// recordEvent persists metering and applies quota consumption. It runs after the
// response has been delivered so it never adds to client-visible latency.
func (s *Server) recordEvent(ctx context.Context, event *store.UsageEvent, subject quota.Subject, model *store.Model, trace *telemetry.TraceContext) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		defer cancel()
		writeStart := time.Now()
		if err := s.Store.InsertUsageEvent(bg, event); err != nil {
			s.Logger.ErrorContext(bg, "write usage event", "error", err.Error(), "request_id", event.RequestID)
		}
		trace.Child("db.usage_event.insert", telemetry.SpanKindInternal, writeStart, time.Now())
		event.RunAfterInsert(bg)
		// Troubleshooting capture rides on the (uncancelled) context and is
		// a no-op unless beginCapture marked this request as a candidate.
		s.finishCapture(bg, event)
		if subject.UserID == "" && subject.ServiceTokenID == "" {
			return
		}
		modelID := ""
		if model != nil {
			modelID = model.ID
		}
		if err := s.Quota.Record(bg, subject, modelID, quota.Delta{
			TokensIn: event.TokensIn, TokensOut: event.TokensOut, CostNano: event.CostNano, Requests: 1,
		}); err != nil {
			s.Logger.ErrorContext(bg, "record quota consumption", "error", err.Error(), "request_id", event.RequestID)
		}
	}()
}

func (s *Server) observeProxyMetrics(event *store.UsageEvent, upstreamName string, started time.Time) {
	statusClass := telemetry.StatusClass(event.HTTPStatus)
	s.Metrics.ObserveGateway("/v1/*", event.Modality, upstreamName, event.HTTPMethod, event.HTTPStatus, time.Since(started))
	s.Metrics.ModelRequestsTotal.WithLabelValues(event.ModelName, event.Modality, upstreamName, statusClass).Inc()
	s.Metrics.TokensIn.WithLabelValues(event.ModelName, event.Modality, upstreamName).Add(float64(event.TokensIn))
	s.Metrics.TokensOut.WithLabelValues(event.ModelName, event.Modality, upstreamName).Add(float64(event.TokensOut))
	s.Metrics.TokensCached.WithLabelValues(event.ModelName, event.Modality, upstreamName).Add(float64(event.TokensCached))
	s.Metrics.TokensCacheWrite5m.WithLabelValues(event.ModelName, event.Modality, upstreamName).Add(float64(event.TokensCacheWrite5m))
	s.Metrics.TokensCacheWrite1h.WithLabelValues(event.ModelName, event.Modality, upstreamName).Add(float64(event.TokensCacheWrite1h))
	s.Metrics.CostTotal.WithLabelValues(event.ModelName, upstreamName).Add(usage.USD(event.CostNano))
	if event.UpstreamMs > 0 {
		s.Metrics.UpstreamLatency.WithLabelValues(upstreamName).Observe(float64(event.UpstreamMs) / 1000)
	}
	if event.TTFBMs > 0 {
		s.Metrics.TTFB.WithLabelValues(upstreamName).Observe(float64(event.TTFBMs) / 1000)
	}
}

// rejectProxy answers a refused request without a usage event (no principal yet).
func (s *Server) rejectProxy(w http.ResponseWriter, r *http.Request, apiErr *APIError, _ *store.User, started time.Time) {
	s.Metrics.PolicyRejections.WithLabelValues(apiErr.Code).Inc()
	s.Metrics.ObserveGateway("/v1/*", "", "", r.Method, apiErr.Status(), time.Since(started))
	WriteError(w, r, apiErr)
}

// rejectProxyEvent answers a refused request and still records the attempt, so
// blocked traffic is visible in analytics and audit.
func (s *Server) rejectProxyEvent(w http.ResponseWriter, r *http.Request, apiErr *APIError, event *store.UsageEvent, started time.Time) {
	event.HTTPStatus = apiErr.Status()
	event.ErrorCode = apiErr.Code
	event.LatencyMs = int(time.Since(started).Milliseconds())
	// Pre-proxy rejections have always been zero-cost; the label just makes
	// that honest in the data instead of claiming a byte-count estimate.
	event.AccountingMode = usage.AccountingNone

	s.Metrics.PolicyRejections.WithLabelValues(apiErr.Code).Inc()
	s.Metrics.ObserveGateway("/v1/*", event.Modality, "", r.Method, apiErr.Status(), time.Since(started))

	// The error envelope is written before the event is recorded so that a
	// troubleshooting capture (which runs after the insert) sees the
	// response the client actually received.
	WriteError(w, r, apiErr)

	bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		defer cancel()
		if err := s.Store.InsertUsageEvent(bg, event); err != nil {
			s.Logger.ErrorContext(bg, "write rejected usage event", "error", err.Error())
		}
		event.RunAfterInsert(bg)
		s.finishCapture(bg, event)
	}()
}

// handleProxyModels answers GET /v1/models in the OpenAI list shape, filtered
// to this caller's grants. Both principal kinds may call it: a service token
// needs to discover what it may use just as much as a person does.
//
// Managed models are listed alongside real ones and are deliberately
// transparent: each carries a janus.managed block naming the model it
// currently resolves to. Hiding the target would leave a caller unable to
// reason about context windows, capabilities, or provider behaviour — the
// point of the alias is to spare users the bookkeeping, not to keep a secret.
func (s *Server) handleProxyModels(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	serviceToken := ServiceTokenFrom(r.Context())
	if user == nil && serviceToken == nil {
		WriteError(w, r, ErrTokenInvalid(""))
		return
	}
	catalog, err := s.grantedCatalog(r.Context(), user, serviceToken)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]map[string]any, 0, len(catalog.Models)+len(catalog.Managed))
	for _, m := range catalog.Models {
		// The listed id is the caller-facing catalog name (the admin-set
		// display name when one exists); the native upstream name stays
		// available under janus.native_name so callers can correlate with
		// provider documentation. Both names route (documented backward compat).
		data = append(data, map[string]any{
			"id": m.PublicName(), "object": "model", "created": m.DiscoveredAt.Unix(), "owned_by": m.UpstreamName,
			"janus": map[string]any{
				"upstream": m.UpstreamName, "adapter_type": m.AdapterType, "modalities": m.Modalities,
				"usd_per_mtok_in": usage.USD(m.RateInNano), "usd_per_mtok_out": usage.USD(m.RateOutNano),
				"grant_source": m.GrantSource, "native_name": m.Name,
				"context_window": m.ContextWindow,
				"managed":        false,
			},
		})
	}
	for _, mm := range catalog.Managed {
		data = append(data, map[string]any{
			"id": mm.Name, "object": "model", "created": mm.CreatedAt.Unix(), "owned_by": mm.TargetUpstreamName,
			"janus": map[string]any{
				"upstream": mm.TargetUpstreamName, "modalities": mm.Modalities,
				"grant_source": mm.GrantSource, "context_window": mm.ContextWindow,
				"managed":     true,
				"description": mm.Description,
				// The alias is an indirection, not a disguise: say plainly
				// what it resolves to right now, and warn that it may change.
				"managed_model": map[string]any{
					"resolves_to":      mm.TargetPublicName,
					"native_name":      mm.TargetName,
					"upstream":         mm.TargetUpstreamName,
					"may_change":       true,
					"managed_by_admin": true,
				},
			},
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// --- helpers -----------------------------------------------------------------

// rewriteJSONModelField replaces the top-level "model" field of a JSON object
// body with the given native name, leaving every other field byte-for-byte
// intact (values are carried as json.RawMessage; only the model string and the
// top-level framing are re-encoded). ok is false when the body is not a JSON
// object carrying a string model field — the caller refuses such requests
// rather than guessing at a body it cannot parse.
func rewriteJSONModelField(body []byte, nativeName string) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false
	}
	raw, exists := fields["model"]
	if !exists {
		return nil, false
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(nativeName)
	if err != nil {
		return nil, false
	}
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return out, true
}

// errAliasRewriteImpossible refuses a request that addressed a renamed model by
// its display name through a body the gateway cannot rewrite (overflowed the
// in-memory buffer, or not a JSON object). The message names the native model
// name so the caller can retry immediately without administrator help.
func errAliasRewriteImpossible(alias, nativeName, why string) *APIError {
	return ErrInvalidRequest(fmt.Sprintf(
		"%q is the catalog display name for the upstream model %q, and %s. Retry using the native model name %q in your request.",
		alias, nativeName, why, nativeName)).WithParam("model")
}

// countingReader counts every byte read through it, so the gateway can meter
// request bodies it forwards without buffering them.
type countingReader struct {
	r     io.Reader
	total int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.total += int64(n)
	return n, err
}

// clientAppName identifies the calling application — the agent harness, IDE
// plugin, or app — as distinct from the SDK, which is all User-Agent can tell
// you. Every OpenAI-compatible SDK reports only itself ("OpenAI/Python
// 2.24.0"), so a fleet of different agents is indistinguishable from the user
// agent alone.
//
// There is no standard for this. OpenRouter's attribution headers are the
// de-facto convention and many clients already send them, so those are
// honoured; X-Janus-Agent is an explicit escape hatch for a client that wants
// to name itself unambiguously to this gateway.
//
// The value is caller-supplied and therefore untrusted: it is a reporting
// label, never an authorisation input, and it is length-capped by the caller.
// An empty result simply means the client did not identify itself.
func clientAppName(r *http.Request) string {
	for _, header := range []string{
		"X-Janus-Agent",      // explicit, Janus-specific
		"X-OpenRouter-Title", // current OpenRouter attribution header
		"X-Title",            // the widely adopted older spelling
	} {
		if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
			return v
		}
	}
	// HTTP-Referer is OpenRouter's companion attribution header. The standard
	// Referer is captured separately and is a page URL rather than an app
	// name, so only the OpenRouter spelling is treated as an identity.
	return strings.TrimSpace(r.Header.Get("HTTP-Referer"))
}

// extractModelName finds the target model without disturbing the forwarded body.
func extractModelName(body []byte, r *http.Request) string {
	if len(body) > 0 && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &probe); err == nil && probe.Model != "" {
			return probe.Model
		}
	}
	if v := r.URL.Query().Get("model"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Janus-Model"); v != "" {
		return v
	}
	// Multipart uploads (audio transcription, image edits) carry the model as a
	// form field; scan the raw prologue rather than parsing the whole payload.
	if idx := bytes.Index(body, []byte("name=\"model\"")); idx >= 0 {
		rest := body[idx:]
		if start := bytes.Index(rest, []byte("\r\n\r\n")); start >= 0 {
			rest = rest[start+4:]
			if end := bytes.Index(rest, []byte("\r\n")); end > 0 {
				return string(bytes.TrimSpace(rest[:end]))
			}
		}
	}
	return ""
}

func isStreamingRequest(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Stream
}

func isEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

func countMultipartSections(body []byte) int {
	return bytes.Count(body, []byte("Content-Disposition: form-data"))
}

// countJSONImageParts counts image_url content parts in an OpenAI-style JSON
// chat body, so vision requests are attributable in usage data. Only part
// TYPES are decoded — image payloads are discarded, never retained or logged.
// Plain-string content, non-chat shapes, and malformed JSON all count zero.
func countJSONImageParts(body []byte) int {
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	count := 0
	for _, msg := range doc.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err != nil {
			continue // plain-string content or an unknown shape: no image parts
		}
		for _, part := range parts {
			if part.Type == "image_url" {
				count++
			}
		}
	}
	return count
}

func copyResponseHeaders(w http.ResponseWriter, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "connection", "transfer-encoding", "content-encoding":
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
}

func (s *Server) trustsForwardedFor(r *http.Request) bool {
	if len(s.Config.TrustedProxies) == 0 {
		return false
	}
	return ClientIP(r, s.Config.TrustedProxies) != stripPort(r.RemoteAddr)
}

// stripPort returns the host part of a host:port address. It must produce the
// exact same string ClientIP derives from RemoteAddr — net.SplitHostPort strips
// IPv6 brackets ("[::1]:80" → "::1") where a naive LastIndex(":") would not,
// which previously made trustsForwardedFor treat every IPv6 peer as a trusted
// proxy.
func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

func formatLimit(metric string, limit int64) string {
	if metric == store.MetricCostUSD {
		return fmt.Sprintf("$%.2f", usage.USD(limit))
	}
	return strconv.FormatInt(limit, 10)
}
