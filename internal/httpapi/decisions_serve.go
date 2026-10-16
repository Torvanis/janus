package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/authz"
	"github.com/torvanis/janus/internal/decisions"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/secgw/detect"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/usage"
)

// Decision endpoints served to callers. Each takes one dialect and can reach
// any decision model: same-dialect requests are forwarded as sent (model name
// rewritten only), others are translated both ways.
//
//	POST /v1/decisions          OpenAI Decisions (official OpenAI SDKs)
//	POST /v1/systemone          System One (TypeSafe SDK)
//	POST /api/alpha/decisions   System One (OpenRouter SDK pointed at Janus)
//
// The request runs the same pipeline as a chat completion: policy rules, rate
// limits, grants, Security Gateway (on the evidence text), quotas, metering
// (modality "decision", priced by the model's cost source), the request log,
// troubleshooting capture and metrics.
func (s *Server) mountDecisionRoutes(r interface {
	Post(string, http.HandlerFunc)
}) {
	r.Post("/v1/decisions", s.decisionHandler(decisions.DialectOpenAI))
	r.Post("/v1/systemone", s.decisionHandler(decisions.DialectSystemOne))
	r.Post("/api/alpha/decisions", s.decisionHandler(decisions.DialectSystemOne))
}

// Response headers on served decisions.
const (
	headerDecisionDialect    = "X-Janus-Decision-Dialect"
	headerDecisionTranslated = "X-Janus-Decision-Translated"
)

func (s *Server) decisionHandler(inbound string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleDecision(w, r, inbound) }
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request, inbound string) {
	started := time.Now()
	s.Metrics.ConcurrentRequests.Inc()
	defer s.Metrics.ConcurrentRequests.Dec()

	user, token, serviceToken := UserFrom(r.Context()), TokenFrom(r.Context()), ServiceTokenFrom(r.Context())
	if serviceToken == nil && (user == nil || token == nil) {
		s.rejectProxy(w, r, ErrTokenInvalid("Provide your Janus API token as `Authorization: Bearer <token>`."), nil, started)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInMemoryBody+1))
	event := &store.UsageEvent{
		EndpointPath: r.URL.Path, HTTPMethod: r.Method, Modality: adapter.ModalityDecision,
		RequestBytes: int64(len(body)), UserAgent: truncate(r.Header.Get("User-Agent"), 1024),
		ClientIP: ClientIP(r, s.Config.TrustedProxies), Referer: truncate(r.Header.Get("Referer"), 512),
		ClientApp: truncate(clientAppName(r), 120), RequestID: RequestIDFrom(r.Context()),
	}
	stampReportingAdmission(event, r, s.Config.LocalOnly)
	if serviceToken != nil {
		event.ServiceTokenID = serviceToken.ID
	} else {
		event.UserID, event.TokenID, event.TeamIDs = user.ID, token.ID, token.TeamID
	}
	if s.trustsForwardedFor(r) {
		event.XForwardedFor = truncate(r.Header.Get("X-Forwarded-For"), 512)
	}
	// One root span per request, as on the chat path (no-op without OTLP).
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
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInvalidRequest("The request body could not be read."), event, started)
		return
	}
	if int64(len(body)) > maxInMemoryBody {
		s.rejectProxyEvent(w, r, ErrPayloadTooLarge(maxInMemoryBody), event, started)
		return
	}
	var req decisions.Request
	if inbound == decisions.DialectOpenAI {
		req, err = decisions.ParseOpenAI(body)
	} else {
		req, err = decisions.ParseSystemOne(body)
	}
	event.ModelName = req.Model
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInvalidRequest("This is not a valid "+dialectName(inbound)+" request: "+err.Error()+"."), event, started)
		return
	}
	event.AttachmentCount = req.Images()
	ctx := r.Context()

	var groupIDs, teamIDs []string
	if serviceToken == nil {
		if groupIDs, teamIDs, err = s.principalContext(ctx, user); err != nil {
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
	w, r = s.beginCapture(w, r, event, body, false)
	ctx = r.Context()
	subjectID, tokenPrefix := "", ""
	if serviceToken != nil {
		subjectID, tokenPrefix = serviceToken.ID, serviceToken.Prefix
	} else {
		subjectID, tokenPrefix = user.ID, token.Prefix
	}

	// Policy blocking rules and rate limits, as for every proxied call.
	rules, err := s.cachedBlockingRules(ctx)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	// /api/alpha/decisions is outside /v1/ but serves the same models as
	// /v1/systemone: rules written for /v1/* (or for /v1/systemone) apply
	// to it too.
	for _, path := range decisionRulePaths(r.URL.Path) {
		sig := authz.SignalsFromRequest(r, event.ClientIP, tokenPrefix, req.Model)
		sig.Path = path
		if rule, clause := authz.Match(rules, sig); rule != nil {
			event.BlockingRuleID = rule.ID
			_ = s.Store.RecordRuleHit(ctx, rule.ID)
			reason := rule.Reason
			if reason == "" {
				reason = "Matched policy rule " + rule.Name + " (" + clause + ")."
			}
			s.rejectProxyEvent(w, r, ErrEndpointBlocked().WithReason(reason), event, started)
			return
		}
	}
	for _, path := range decisionRulePaths(r.URL.Path) {
		if refused, resetAt := s.rateLimitExceeded(ctx, subjectID, path); refused {
			s.rejectProxyEvent(w, r, ErrRateLimited("You have exceeded the request rate configured for this endpoint. Wait for the limit to refill and retry.").WithReset(resetAt), event, started)
			return
		}
	}

	// Model: a decision model, or a managed alias of one.
	if req.Model == "" {
		s.rejectProxyEvent(w, r, ErrInvalidRequest(`Specify a decision model, for example {"model": "~typesafe/jev-latest"}.`).WithParam("model"), event, started)
		return
	}
	resolved, apiErr := s.resolveDecisionModel(ctx, req.Model)
	if apiErr != nil {
		s.rejectProxyEvent(w, r, apiErr, event, started)
		return
	}
	// A pooled alias picks its member per request, as on the chat path.
	var route *poolRoute
	if isPool(resolved) {
		route = s.choosePoolMember(ctx, r, body, event, resolved.Managed)
		switch {
		case route != nil:
			resolved.Model = route.current()
		case resolved.Fallback != nil:
			resolved = resolved.WithFallback(store.FallbackTriggerPoolExhausted)
		default:
			event.RequestedModelName = resolved.Managed.Name
			s.rejectProxyEvent(w, r, ErrManagedModelUnavailable(req.Model, "every model in its pool is out of service"), event, started)
			return
		}
		if resolved.Model == nil || resolved.Model.ClassifierRole != store.ClassifierRoleDecision {
			s.rejectProxyEvent(w, r, ErrInvalidRequest(req.Model+" resolves to a chat model. Call /v1/chat/completions, or name a decision model.").WithParam("model"), event, started)
			return
		}
	}
	model := resolved.Model
	granted, err := s.cachedGrantedIDs(ctx, store.GrantSubject{UserID: event.UserID, GroupIDs: groupIDs, ServiceTokenID: event.ServiceTokenID, TeamID: event.TeamIDs})
	if err != nil {
		s.rejectProxyEvent(w, r, ErrInternal(), event, started)
		return
	}
	if !resolved.ViaManagedModel() {
		model = chooseGrantedCandidate(model, resolved.Alternatives, granted)
		resolved.Model = model
	}
	accessID := model.ID
	if resolved.ViaManagedModel() {
		accessID = resolved.Managed.ID
	}
	if _, ok := granted[accessID]; !ok {
		s.rejectProxyEvent(w, r, ErrModelNotGranted(req.Model), event, started)
		return
	}
	// A grant made before the model became a blocking guard does not let
	// callers probe the judge.
	// The admin-facing reason names the policies the model judges for;
	// the caller is told only that it is reserved.
	if ok, _ := s.decisionStillGrantable(ctx, model); !ok {
		s.rejectProxyEvent(w, r, ErrModelNotGranted(req.Model).WithReason(store.DecisionReservedReason), event, started)
		return
	}
	if resolved.FellBack {
		w.Header().Set(headerFallbackReason, resolved.FallbackReason)
		event.FallbackReason = resolved.FallbackReason
	}
	s.stampServedModel(ctx, event, model)
	if resolved.ViaManagedModel() {
		event.RequestedModelName, event.ManagedModelID = resolved.Managed.Name, resolved.Managed.ID
	}

	// Security Gateway, on everything the caller wrote: the question text
	// (instructions, boundaries, choices, levels, names, safety id and any
	// field forwarded as sent) as the first user turn, then the evidence,
	// one user turn per text part.
	secgwEng, err := s.secgwEngine(ctx)
	if err != nil {
		s.rejectProxyEvent(w, r, ErrSecurityBlocked("", true), event, started)
		return
	}
	secgwEff := secgwEng.Resolve(secgwSubject(event, groupIDs, resolved))
	var violations []secgw.Violation
	// redacted: the evidence was rewritten, so the request is re-encoded;
	// forward is the caller's body with any question-text redactions,
	// forwarded as sent (model name rewritten) when nothing else changed.
	redacted := false
	forward := body
	if !secgwEff.Empty() {
		fields, ferr := decisions.QuestionText(body, inbound)
		if ferr != nil {
			s.rejectProxyEvent(w, r, ErrInvalidRequest("This is not a valid "+dialectName(inbound)+" request: "+ferr.Error()+"."), event, started)
			return
		}
		qtext, starts := joinQuestionText(fields)
		chat := decisionChatBody(qtext, req)
		dec, derr := secgwEng.Ingress(ctx, secgwEff, secgw.Request{Body: chat, IsServiceToken: serviceToken != nil})
		event.ClassifierMs, event.ClassifierCalls = int(dec.Model.Elapsed.Milliseconds()), dec.Model.Calls
		if derr != nil {
			s.rejectProxyEvent(w, r, ErrInternal(), event, started)
			return
		}
		var newBody []byte
		if dec.Action == store.SecgwActionRedacted {
			// Redactions in the question text are written back into the
			// fields they hit; one that hits a name, a choice value or two
			// fields at once cannot be, and blocks instead.
			newBody, dec = redactQuestionText(body, inbound, fields, starts, dec)
		}
		violations = dec.Violations
		event.SecgwAction, event.SecgwViolations = dec.Action, secgw.Recorded(dec.Violations)
		if dec.Action == store.SecgwActionBlocked {
			s.attachSecgwViolations(event, secgwEff.Policy, violations)
			s.writeSecgwBlock(w, r, event, secgwEff.Policy, dec, started)
			return
		}
		if dec.Action == store.SecgwActionRedacted {
			if newBody != nil {
				forward = newBody
				var perr error
				if inbound == decisions.DialectOpenAI {
					req, perr = decisions.ParseOpenAI(newBody)
				} else {
					req, perr = decisions.ParseSystemOne(newBody)
				}
				if perr != nil {
					// Unreachable for redactable fields; never forward an
					// unredacted request because of it.
					s.rejectProxyEvent(w, r, ErrInternal(), event, started)
					return
				}
			}
			req, redacted = applyRedactedText(req, dec.Body)
		}
		secgwActionHeader(w, dec.Action, dec.Redactions)
	}

	// Quotas, before any provider spend.
	quotaSubject := quota.UserSubject(event.UserID, teamIDs)
	if serviceToken != nil {
		quotaSubject = quota.ServiceTokenSubject(serviceToken.ID)
	}
	quotaStart := time.Now()
	decision, err := s.Quota.Check(ctx, quotaSubject, model.ID, event.ManagedModelID)
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

	// Upstream call, in the dialect the model's provider speaks.
	upstream, err := s.cachedUpstreamByID(ctx, model.UpstreamID)
	if err != nil || !upstream.Enabled {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(model.UpstreamName), event, started)
		return
	}
	apiKey := ""
	if upstream.HasAPIKey {
		if apiKey, err = s.Cipher.Decrypt(upstream.EncryptedKey()); err != nil {
			s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
			return
		}
	}
	prof, _ := s.Store.ClassifierProfileFor(ctx, upstream.ID, model.Name)
	target := decisionDialect(prof, upstream.BaseURL)
	wire := prof.WireModel
	if wire == "" {
		wire = decisions.WireModel(model.Name, upstream.BaseURL)
	}
	translated := target != inbound
	var outBody []byte
	switch {
	case !translated && !redacted:
		var ok bool
		if outBody, ok = rewriteJSONModelField(forward, wire); !ok {
			outBody = forward
		}
	case target == decisions.DialectOpenAI:
		outBody, err = decisions.EncodeOpenAI(req, wire)
	default:
		outBody, err = decisions.EncodeSystemOne(req, wire)
	}
	if err != nil {
		msg := err.Error()
		if errors.Is(err, decisions.ErrUnsupported) {
			msg = model.PublicName() + " cannot answer this request: " + strings.TrimPrefix(msg, decisions.ErrUnsupported.Error()+": ") + "."
		}
		s.rejectProxyEvent(w, r, ErrInvalidRequest(msg), event, started)
		return
	}
	if !s.Config.LocalOnly {
		event.CostStatus = "unknown"
	}
	if route != nil {
		route.begin(s.poolRegistry())
		defer route.end()
		event.PoolReason, event.PoolAttempts = route.reason, route.attempts()
		w.Header().Set(headerPoolMember, model.PublicName()+"@"+model.UpstreamName)
		w.Header().Set(headerPoolReason, route.reason)
	}
	upstreamStart := time.Now()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, decisions.Endpoint(upstream.BaseURL, target), bytes.NewReader(outBody))
	if err != nil {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		out.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.upstreamHTTPClient(ctx).Do(out)
	if err != nil {
		s.Metrics.UpstreamErrors.WithLabelValues(upstream.Name, "unreachable").Inc()
		if route != nil {
			s.poolFailure(ctx, route, "unreachable: "+err.Error())
		}
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Time to the provider's response headers; the answer is read whole.
	event.TTFBMs = int(time.Since(upstreamStart).Milliseconds())
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	event.UpstreamMs = int(time.Since(upstreamStart).Milliseconds())
	if route != nil {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			s.poolFailure(ctx, route, "HTTP "+strconv.Itoa(resp.StatusCode))
		} else {
			s.poolSuccess(ctx, route)
		}
	}
	event.HTTPStatus = resp.StatusCode
	event.ResponseBytes = int64(len(raw))
	if err != nil {
		s.rejectProxyEvent(w, r, ErrUpstreamUnavailable(upstream.Name), event, started)
		return
	}
	w.Header().Set(headerDecisionDialect, target)
	if translated {
		w.Header().Set(headerDecisionTranslated, inbound+"->"+target)
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusTooManyRequests {
			event.ErrorCode = CodeUpstreamLimit
		} else if resp.StatusCode >= 500 {
			s.Metrics.UpstreamErrors.WithLabelValues(upstream.Name, "5xx").Inc()
		}
		event.AccountingMode = usage.AccountingNone
		s.relayDecisionError(w, resp, raw, upstream.Name)
		s.finishDecisionEvent(ctx, event, quotaSubject, model, upstream.Name, started, secgwEff, violations)
		return
	}
	var parsed decisions.Response
	if target == decisions.DialectOpenAI {
		parsed, err = decisions.ParseOpenAIResponse(raw, req.Questions)
	} else {
		parsed, err = decisions.ParseSystemOneResponse(raw, req.Questions)
	}
	if err != nil {
		event.HTTPStatus, event.ErrorCode, event.AccountingMode = http.StatusBadGateway, CodeUpstreamDown, usage.AccountingNone
		WriteError(w, r, ErrUpstreamUnavailable(upstream.Name).WithReason("The provider's answer could not be read: "+err.Error()))
		s.finishDecisionEvent(ctx, event, quotaSubject, model, upstream.Name, started, secgwEff, violations)
		return
	}
	event.TokensIn, event.TokensOut, event.TokensCached = parsed.Usage.InputTokens, parsed.Usage.OutputTokens, parsed.Usage.CachedTokens
	call := decisionCall{
		TokensIn: parsed.Usage.InputTokens, TokensOut: parsed.Usage.OutputTokens, TokensCached: parsed.Usage.CachedTokens,
		CostUSD: parsed.Usage.CostUSD, CostReported: parsed.Usage.CostReported,
	}
	event.CostNano, event.AccountingMode = s.decisionCost(ctx, model, call)
	event.CostStatus = s.decisionCostStatus(ctx, model, call, event.CostNano)
	reply := raw
	if translated {
		if inbound == decisions.DialectOpenAI {
			reply = decisions.EncodeOpenAIResponse(parsed)
		} else {
			reply = decisions.EncodeSystemOneResponse(parsed, req.Questions)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Janus-Cost-USD", strconv.FormatFloat(usage.USD(event.CostNano), 'f', 9, 64))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
	s.finishDecisionEvent(ctx, event, quotaSubject, model, upstream.Name, started, secgwEff, violations)
}

func (s *Server) finishDecisionEvent(ctx context.Context, event *store.UsageEvent, subject quota.Subject, model *store.Model, upstreamName string, started time.Time, eff *secgw.Effective, vs []secgw.Violation) {
	event.LatencyMs = int(time.Since(started).Milliseconds())
	if len(vs) == 0 && eff != nil && !eff.Empty() && event.SecgwAction == "" {
		event.SecgwAction = store.SecgwActionChecked
	}
	if eff != nil {
		s.attachSecgwViolations(event, eff.Policy, vs)
	}
	s.recordEvent(ctx, event, subject, model, nil)
	s.observeProxyMetrics(event, upstreamName, started)
}

// relayDecisionError passes the provider's error on: its status, and its
// body when it is a JSON error object (both dialects use {"error":{...}}).
func (s *Server) relayDecisionError(w http.ResponseWriter, resp *http.Response, raw []byte, upstreamName string) {
	if json.Valid(raw) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	apiErr := ErrUpstreamUnavailable(upstreamName).WithReason("HTTP " + strconv.Itoa(resp.StatusCode))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": apiErr.Message + " (" + apiErr.Reason + ")", "code": apiErr.Code}})
}

// resolveDecisionModel finds the decision model a caller named: a managed
// alias whose target is a decision model, or a decision catalog row.
func (s *Server) resolveDecisionModel(ctx context.Context, name string) (store.ResolvedModel, *APIError) {
	if mm, err := s.Store.ManagedModelByName(ctx, name); err == nil && mm.Status == store.ManagedModelEnabled {
		resolved, rerr := s.cachedResolveModel(ctx, name)
		if rerr != nil {
			return resolved, ErrManagedModelUnavailable(name, "its decision model is unavailable")
		}
		if isPool(resolved) {
			// The handler picks the member (and the pool's fallback).
			return resolved, nil
		}
		resolved = s.applyManagedFallback(ctx, resolved)
		if resolved.Model == nil || resolved.Model.ClassifierRole != store.ClassifierRoleDecision {
			return resolved, ErrInvalidRequest(name + " is a chat model. Call /v1/chat/completions, or name a decision model.").WithParam("model")
		}
		return resolved, nil
	}
	candidates, err := s.Store.DecisionModelsByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		if _, cerr := s.Store.ModelsByName(ctx, name); cerr == nil {
			return store.ResolvedModel{}, ErrInvalidRequest(name + " is a chat model, not a decision model. Call /v1/chat/completions, or name a decision model (GET /v1/models lists them with janus.kind \"decision\").").WithParam("model")
		}
		return store.ResolvedModel{}, ErrModelNotGranted(name)
	}
	if err != nil {
		return store.ResolvedModel{}, ErrInternal()
	}
	return store.ResolvedModel{Model: candidates[0], Alternatives: candidates[1:]}, nil
}

// decisionStillGrantable re-checks the guard rule at request time (cached
// with the config): a model that started backing a blocking check after it
// was granted is no longer callable unless the admin allowed it.
func (s *Server) decisionStillGrantable(ctx context.Context, m *store.Model) (bool, string) {
	type verdict struct {
		ok  bool
		why string
	}
	v, err := cachedRead(&s.configCache, "decisiongrantable|"+m.ID, func() (verdict, error) {
		ok, why, err := s.Store.DecisionGrantable(ctx, m)
		return verdict{ok, why}, err
	})
	if err != nil {
		return false, "the model's grant rule could not be checked"
	}
	return v.ok, v.why
}

// managedModelKind is the janus.kind /v1/models shows for an alias:
// "decision" when its target is a decision model, "chat" otherwise, and ""
// when the alias should not be listed (its decision target is reserved for
// security checks).
func (s *Server) managedModelKind(ctx context.Context, mm *store.ManagedModel) string {
	target, err := s.cachedModelByID(ctx, mm.TargetModelID)
	if err != nil || target.ClassifierRole != store.ClassifierRoleDecision {
		return "chat"
	}
	if ok, _ := s.decisionStillGrantable(ctx, target); !ok {
		return ""
	}
	return "decision"
}

// decisionChatBody is a decision request as a chat body for the Security
// Gateway: the question text as the first user message (always present,
// so the evidence starts at message 1), then one user message per text
// part of the evidence. The evidence comes last so decision-model guards,
// which judge the most recent text, see it as they did before the
// question text was inspected.
func decisionChatBody(questionText string, r decisions.Request) []byte {
	msgs := []map[string]string{{"role": "user", "content": questionText}}
	for _, p := range r.Parts {
		if p.Text != "" {
			msgs = append(msgs, map[string]string{"role": "user", "content": p.Text})
		}
	}
	b, _ := json.Marshal(map[string]any{"messages": msgs})
	return b
}

// questionTextSep separates fields in the joined question text.
const questionTextSep = "\n\n"

// joinQuestionText joins the fields into one text and returns where each
// field starts in it (byte offsets).
func joinQuestionText(fields []decisions.TextField) (string, []int) {
	var b strings.Builder
	starts := make([]int, len(fields))
	for i, f := range fields {
		if i > 0 {
			b.WriteString(questionTextSep)
		}
		starts[i] = b.Len()
		b.WriteString(f.Text)
	}
	return b.String(), starts
}

// redactQuestionText applies the redactions the Security Gateway found in
// the question text (message 0) to the request body, field by field. It
// returns the rewritten body (nil when the question text was untouched)
// and the decision, turned into a block when a redaction falls on a field
// that cannot be rewritten (a question name, a choice value, a key) or
// across two fields.
func redactQuestionText(body []byte, dialect string, fields []decisions.TextField, starts []int, dec secgw.Decision) ([]byte, secgw.Decision) {
	spans := map[int][]detect.Match{}
	blockAt := -1
	for i, v := range dec.Violations {
		if v.MessageIndex != 0 || v.Action != store.SecgwActionRedacted {
			continue
		}
		f := fieldAt(starts, v.Offset)
		if f < 0 || !fields[f].Redactable || v.Offset+v.Length > starts[f]+len(fields[f].Text) {
			blockAt = i
			break
		}
		spans[f] = append(spans[f], detect.Match{Offset: v.Offset - starts[f], Length: v.Length, RuleID: v.RuleID, Replacement: v.Replacement})
	}
	if blockAt >= 0 {
		v := dec.Violations[blockAt]
		dec.Violations[blockAt].Action = store.SecgwActionBlocked
		dec.Action, dec.BlockKind, dec.Body, dec.Redactions = store.SecgwActionBlocked, v.Kind, nil, 0
		return nil, dec
	}
	if len(spans) == 0 {
		return nil, dec
	}
	replaced := make(map[int]string, len(spans))
	for f, ms := range spans {
		replaced[f] = detect.Redact(fields[f].Text, ms)
	}
	out, err := decisions.RewriteQuestionText(body, dialect, replaced)
	if err != nil {
		// Never forward the unredacted text: block on the first finding.
		for i, v := range dec.Violations {
			if v.MessageIndex == 0 && v.Action == store.SecgwActionRedacted {
				dec.Violations[i].Action = store.SecgwActionBlocked
				dec.Action, dec.BlockKind, dec.Body, dec.Redactions = store.SecgwActionBlocked, v.Kind, nil, 0
				break
			}
		}
		return nil, dec
	}
	return out, dec
}

// fieldAt is the index of the field whose text contains offset, or -1 when
// offset falls in a separator.
func fieldAt(starts []int, offset int) int {
	return sort.SearchInts(starts, offset+1) - 1
}

// applyRedactedText puts the Security Gateway's redacted evidence back
// into the request's text parts (messages 1.. of decisionChatBody) and
// reports whether any changed. A JSON state whose text was redacted is
// sent on as text.
func applyRedactedText(r decisions.Request, chat []byte) (decisions.Request, bool) {
	var b struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(chat, &b) != nil || len(b.Messages) == 0 {
		return r, false
	}
	evidence := b.Messages[1:]
	changed := false
	i := 0
	for j := range r.Parts {
		if r.Parts[j].Text == "" {
			continue
		}
		if i < len(evidence) && r.Parts[j].Text != evidence[i].Content {
			r.Parts[j].Text = evidence[i].Content
			r.StateJSON = nil
			changed = true
		}
		i++
	}
	return r, changed
}

// decisionRulePaths are the paths blocking and rate-limit rules are matched
// against for a decision request: its own, and for /api/alpha/decisions
// (OpenRouter's path, outside /v1/) the /v1/systemone path it is equivalent
// to, so a rule an admin scoped to /v1/* cannot be stepped around.
func decisionRulePaths(path string) []string {
	if path == "/api/alpha/decisions" {
		return []string{path, "/v1/systemone"}
	}
	return []string{path}
}

func dialectName(d string) string {
	if d == decisions.DialectOpenAI {
		return "OpenAI Decisions"
	}
	return "System One"
}
