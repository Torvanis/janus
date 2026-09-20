package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/secgw/stream"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Security Gateway wiring for the proxy path. The engine is built from a
// policy snapshot and cached on the same cadence as every other hot-path
// configuration read; the common case (no bindings at all) is a single
// cached read that returns an empty engine and costs nothing further.

const (
	// CodeSecurityBlocked marks a request or response refused by a Security
	// Gateway policy. The offending check kind rides in error.param; the
	// rule id and matched span never reach the caller.
	CodeSecurityBlocked = "policy.security_blocked"
	// headerRedactions tells the caller how many spans the gateway
	// rewrote in their request before the upstream saw it. A rewritten
	// prompt produces an answer to different text than the user sent; that
	// must never be silent.
	headerRedactions = "X-Janus-Redactions"
	// headerSecgwAction summarises the outcome (observed / redacted) on
	// responses that were allowed through.
	headerSecgwAction = "X-Janus-Security-Action"
)

// ErrSecurityBlocked reports a Security Gateway refusal.
func ErrSecurityBlocked(kind store.SecgwCheckKind, classifierFailed bool) *APIError {
	msg := "This request was declined by your organisation's security policy. Contact a Janus administrator if you believe this is a mistake."
	if classifierFailed {
		msg = "This request could not be evaluated by the security classifier and the policy is configured to fail closed. Retry shortly, or contact a Janus administrator."
	}
	return newError(http.StatusForbidden, CodeSecurityBlocked, "permission_error", msg).WithParam(string(kind))
}

// secgwState is the cached engine plus the snapshot it was built from.
type secgwState struct {
	engine *secgw.Engine
	err    error
}

// secgwEngine returns the engine for the current policy snapshot.
func (s *Server) secgwEngine(ctx context.Context) (*secgw.Engine, error) {
	st, err := cachedRead(&s.configCache, "secgw_engine", func() (*secgwState, error) {
		snap, err := s.Store.LoadSecgwSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		if !snap.Empty() {
			lists, err := s.Store.LoadSecgwTermLists(ctx, s.Cipher)
			if err != nil {
				return nil, err
			}
			snap.TermLists = lists
		}
		eng, err := secgw.NewEngine(snap, s.secgwClassifiers(), s.Logger)
		// A snapshot that fails to compile (bad admin regex) is cached as
		// an error state so the proxy fails closed loudly rather than
		// re-compiling on every request.
		return &secgwState{engine: eng, err: err}, nil
	})
	if err != nil {
		return nil, err
	}
	return st.engine, st.err
}

// secgwClassifiers builds the resolver lazily; it holds breakers per model.
func (s *Server) secgwClassifiers() classify.Resolver {
	s.secgwOnce.Do(func() {
		s.secgwResolver = &classifierResolver{s: s, breakers: map[string]*classify.Breaker{}, roles: map[string]string{}}
	})
	return s.secgwResolver
}

// classifierResolver turns a model id into a breaker-wrapped classifier. It
// refuses models without a classifier role: a servable model must never be
// used as a judge, and a judge must never be servable.
type classifierResolver struct {
	s        *Server
	mu       sync.Mutex
	breakers map[string]*classify.Breaker
	// roles records the classifier_role each cached breaker was built for.
	// An admin can change a model's role (Prompt Guard -> Llama Guard) while
	// the process lives; the cached client speaks the OLD protocol and its
	// failure is confusing ("not a conversation guard" / 404 on /classify),
	// so a role change must rebuild rather than reuse. Keyed separately from
	// breakers so the breaker's open/closed state is dropped with it.
	roles map[string]string
}

func (r *classifierResolver) For(ctx context.Context, modelID string) (classify.Classifier, error) {
	model, err := r.s.Store.ModelByID(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("classifier model %s: %w", modelID, err)
	}
	if model.ClassifierRole == "" {
		return nil, fmt.Errorf("model %s has no classifier role", model.PublicName())
	}

	r.mu.Lock()
	if b, ok := r.breakers[modelID]; ok && r.roles[modelID] == model.ClassifierRole {
		r.mu.Unlock()
		return b, nil
	}
	r.mu.Unlock()

	upstream, err := r.s.Store.UpstreamByID(ctx, model.UpstreamID)
	if err != nil {
		return nil, fmt.Errorf("classifier upstream: %w", err)
	}
	apiKey := ""
	if upstream.HasAPIKey {
		if apiKey, err = r.s.Cipher.Decrypt(upstream.EncryptedKey()); err != nil {
			return nil, fmt.Errorf("decrypt classifier credential: %w", err)
		}
	}
	inner, err := classify.New(model.ClassifierRole, classify.Upstream{
		ModelID: model.ID, ModelName: model.Name, BaseURL: upstream.BaseURL, APIKey: apiKey,
		Client: r.s.upstreamHTTPClient(ctx),
		Meter:  r.s.meterClassifierCall(model, upstream),
	})
	if err != nil {
		return nil, err
	}
	b := &classify.Breaker{Inner: inner, ModelID: model.ID, Threshold: 3, Cooldown: 30 * time.Second,
		OnOpen: func(id string, cause error) {
			r.s.Logger.Error("secgw: classifier circuit breaker opened", "model", model.PublicName(), "error", cause.Error())
			if r.s.Alerts != nil {
				r.s.Alerts.Dispatch(context.Background(), alerting.Event{
					Trigger: alerting.TriggerSystem, Severity: "critical", NotifyAdmins: true,
					Title: "Security classifier unavailable: " + model.PublicName(),
					Body: "The classifier backing prompt-injection checks failed repeatedly (" + cause.Error() +
						"). Policies with fail=closed are refusing requests until it recovers.",
					Data: map[string]any{"model_id": model.ID, "reason": "secgw_classifier_breaker_open"},
				})
			}
		}}
	r.mu.Lock()
	r.breakers[modelID] = b
	r.roles[modelID] = model.ClassifierRole
	r.mu.Unlock()
	return b, nil
}

// meterClassifierCall writes a usage event for every classifier call on the
// platform-overhead subject: no user, no service token. Finance sees what
// the security feature costs; the caller's quota does not move.
func (s *Server) meterClassifierCall(model *store.Model, upstream *store.Upstream) func(context.Context, time.Duration, int, error) {
	return func(ctx context.Context, elapsed time.Duration, inputChars int, callErr error) {
		event := &store.UsageEvent{
			ModelID: model.ID, ModelName: model.PublicName(), UpstreamID: upstream.ID,
			EndpointPath: "/internal/secgw/classify", HTTPMethod: http.MethodPost, Modality: "classification",
			RequestBytes: int64(inputChars), LatencyMs: int(elapsed.Milliseconds()), UpstreamMs: int(elapsed.Milliseconds()),
			ClientApp: "janus-security-gateway", RequestID: RequestIDFrom(ctx), HTTPStatus: http.StatusOK,
			AccountingMode: usage.AccountingNone,
		}
		if callErr != nil {
			event.HTTPStatus = http.StatusBadGateway
			event.ErrorCode = CodeUpstreamDown
		}
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		s.pending.Add(1)
		go func() {
			defer s.pending.Done()
			defer cancel()
			if err := s.Store.InsertUsageEvent(bg, event); err != nil {
				s.Logger.ErrorContext(bg, "secgw: write classifier usage event", "error", err.Error())
			}
		}()
	}
}

// secgwSubject builds the resolver subject from the already-resolved
// principal and model. No new lookups.
func secgwSubject(event *store.UsageEvent, groupIDs []string, resolved store.ResolvedModel) secgw.Subject {
	sub := secgw.Subject{UserID: event.UserID, ServiceTokenID: event.ServiceTokenID, GroupIDs: groupIDs}
	if resolved.Model != nil {
		sub.ModelID = resolved.Model.ID
		sub.UpstreamID = resolved.Model.UpstreamID
	}
	if resolved.Managed != nil {
		sub.ManagedModelID = resolved.Managed.ID
	}
	return sub
}

// recordSecgwViolations persists violation rows after the usage event has
// been written (so usage_event_id is known). Failures are logged and never
// affect the traffic they describe.
func (s *Server) recordSecgwViolations(ctx context.Context, event *store.UsageEvent, policy *store.SecgwPolicy, vs []secgw.Violation) {
	if len(vs) == 0 {
		return
	}
	var capture store.SecgwCaptureConfig
	if policy != nil {
		capture = policy.Capture
	}
	encrypt := func(text string) (string, error) { return s.Cipher.Encrypt(text) }
	rows := make([]*store.SecgwViolation, 0, len(vs))
	for _, v := range vs {
		// Replayed history is enforced but not re-recorded (see
		// Violation.Record); the metrics below count enforcement, so they
		// still see every hit.
		if !v.Record {
			continue
		}
		row := v.ToStore(capture, encrypt)
		row.RequestID, row.UsageEventID = event.RequestID, event.ID
		row.UserID, row.ServiceTokenID, row.ModelName = event.UserID, event.ServiceTokenID, event.ModelName
		rows = append(rows, row)
	}
	if len(rows) > 0 {
		if err := s.Store.InsertSecgwViolations(ctx, rows); err != nil {
			s.Logger.ErrorContext(ctx, "secgw: persist violations", "error", err.Error(), "request_id", event.RequestID)
		}
	}
	if s.Metrics != nil {
		for _, v := range vs {
			s.Metrics.SecgwViolations.WithLabelValues(string(v.Kind), v.Action, v.Direction).Inc()
		}
	}
}

// writeSecgwBlock answers a blocked request in one of exactly two shapes:
// a 403 with an OpenAI error object (default), or — when the policy asks
// for it — a 200 with a well-formed completion whose finish_reason is
// content_filter, for clients that render errors badly. Streaming callers
// on the synthetic path get the same as one chunk + [DONE].
func (s *Server) writeSecgwBlock(w http.ResponseWriter, r *http.Request, event *store.UsageEvent, policy *store.SecgwPolicy, dec secgw.Decision, started time.Time) {
	apiErr := ErrSecurityBlocked(dec.BlockKind, dec.ClassifierFailed)
	if policy == nil || !policy.SyntheticRefusal {
		s.rejectProxyEvent(w, r, apiErr, event, started)
		return
	}
	event.HTTPStatus = http.StatusOK
	event.ErrorCode = CodeSecurityBlocked
	event.LatencyMs = int(time.Since(started).Milliseconds())
	event.AccountingMode = usage.AccountingNone
	event.FinishReason = "content_filter"
	s.Metrics.PolicyRejections.WithLabelValues(CodeSecurityBlocked).Inc()
	id := "chatcmpl-" + event.RequestID
	created := time.Now().Unix()
	if event.Streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": event.ModelName,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": policy.RefusalText}, "finish_reason": "content_filter"}}}
		payload, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	} else {
		body := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": event.ModelName,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": policy.RefusalText}, "finish_reason": "content_filter"}},
			"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
		WriteJSON(w, http.StatusOK, body)
	}
	s.Metrics.ObserveGateway("/v1/*", event.Modality, "", r.Method, http.StatusOK, time.Since(started))
	bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		defer cancel()
		if err := s.Store.InsertUsageEvent(bg, event); err != nil {
			s.Logger.ErrorContext(bg, "write blocked usage event", "error", err.Error())
		}
		event.RunAfterInsert(bg)
		s.finishCapture(bg, event)
	}()
}

// writeStreamSecurityError emits the in-band SSE error frame when egress
// filtering cuts a live stream — the fourth call site of the pattern the
// hard-kill, cap and translate paths already use. The pending hold-back
// queue has been dropped by the caller; nothing held is ever written.
func (s *Server) writeStreamSecurityError(w http.ResponseWriter, flusher http.Flusher, kind store.SecgwCheckKind, requestID string) {
	apiErr := ErrSecurityBlocked(kind, false)
	apiErr.Message = "This stream was stopped: the response matched your organisation's security policy."
	apiErr.RequestID = requestID
	payload, err := json.Marshal(map[string]any{"error": apiErr})
	if err != nil {
		return
	}
	if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
		return
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// secgwStreamScanner adapts the engine's deterministic egress scan to the
// hold-back buffer's Scanner contract, applying each check's mode.
func secgwStreamScanner(eng *secgw.Engine, eff *secgw.Effective) stream.Scanner {
	return func(channel string, full string, from int) []stream.Match {
		vs := eng.ScanEgressText(eff, full, from)
		if len(vs) == 0 {
			return nil
		}
		out := make([]stream.Match, 0, len(vs))
		for _, v := range vs {
			c := eff.Checks[v.Kind]
			m := stream.Match{Offset: v.Offset, Length: v.Length, Replacement: v.Replacement}
			switch c.Mode {
			case store.SecgwModeBlock:
				m.Block = true
				v.Action = store.SecgwActionStreamCut
			case store.SecgwModeRedact:
				v.Action = store.SecgwActionRedacted
			default:
				// Observe: report but neither cut nor rewrite. It still goes
				// through the Holdback so overlap-window duplicates are
				// dropped there like every other match.
				m.Observe = true
				v.Action = store.SecgwActionObserved
			}
			m.Tag = v
			out = append(out, m)
		}
		return out
	}
}

// secgwCollectStreamViolations pulls the violations the Holdback accepted
// (deduplicated, in order) out of a feed result.
func secgwCollectStreamViolations(res stream.Result, sink *[]secgw.Violation) {
	for _, m := range res.Matches {
		if v, ok := m.Tag.(secgw.Violation); ok {
			*sink = append(*sink, v)
		}
	}
}

// secgwHoldBytes picks the effective hold window: the largest explicit
// HoldBytes any egress check asks for, else the default. An explicit
// value below the default is honoured — the resolver's mandatory floor
// already prevents a more specific binding from lowering an org value.
func secgwHoldBytes(eff *secgw.Effective) int {
	hold := 0
	for _, c := range eff.Checks {
		if c.HoldBytes > hold {
			hold = c.HoldBytes
		}
	}
	if hold == 0 {
		return store.SecgwDefaultHoldBytes
	}
	return hold
}

// hasEgressChecks reports whether any deterministic egress check applies.
func hasEgressChecks(eff *secgw.Effective) bool {
	for _, k := range []store.SecgwCheckKind{store.SecgwCheckSecrets, store.SecgwCheckPII, store.SecgwCheckTerms, store.SecgwCheckContentSafety} {
		if _, ok := eff.Has(k, store.SecgwDirectionEgress); ok {
			return true
		}
	}
	return false
}

// secgwEgress carries what relayBuffered needs to scan a response.
type secgwEgress struct {
	eng  *secgw.Engine
	eff  *secgw.Effective
	sink *[]secgw.Violation
	// reqBody is the ingress request (post-redaction) so a content guard
	// can judge the response in the context of the prompt it answers.
	reqBody []byte
}

// streamCutKind reports the kind of the first stream_cut violation in the
// sink, for the in-band error frame's param.
func streamCutKind(vs []secgw.Violation) store.SecgwCheckKind {
	for _, v := range vs {
		if v.Action == store.SecgwActionStreamCut {
			return v.Kind
		}
	}
	return ""
}

// attachSecgwViolations registers a hook that runs on the same goroutine that inserted the usage
// event, after the insert, so violation rows can carry usage_event_id
// without any cross-goroutine read of the event. Registered on the event
// before recordEvent / rejectProxyEvent / writeSecgwBlock hand it off.
func (s *Server) attachSecgwViolations(event *store.UsageEvent, policy *store.SecgwPolicy, vs []secgw.Violation) {
	if len(vs) == 0 {
		return
	}
	event.AfterInsert = func(ctx context.Context) { s.recordSecgwViolations(ctx, event, policy, vs) }
}

func secgwActionHeader(w http.ResponseWriter, action string, redactions int) {
	if action != "" {
		w.Header().Set(headerSecgwAction, action)
	}
	if redactions > 0 {
		w.Header().Set(headerRedactions, strconv.Itoa(redactions))
	}
}

// handleRequestSecurity serves GET /api/v1/admin/requests/{id}/security:
// what the security gateway did on one request. {id} is a usage event ID;
// the runs are looked up by that event's request_id, so a chat request
// reports the classifier calls the gateway made while inspecting it.
//
// This exists because classifier calls are usage events of their own (to
// keep their cost attributable) and would otherwise read as separate
// requests in the log. Here they read as what they are: checks that ran on
// a request, with their verdicts.
func (s *Server) handleRequestSecurity(w http.ResponseWriter, r *http.Request) {
	event, err := s.Store.UsageEventByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That request"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	runs, err := s.Store.SecgwClassifierRunsForRequest(r.Context(), event.RequestID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Non-model findings are a bounded metadata preview of the newest request
	// violations, not a complete inventory. Probe one extra row before filtering
	// out classifier findings so the completeness flag describes that exact scope.
	const violationLimit = 200
	all := []*store.SecgwViolation{}
	if event.RequestID != "" {
		all, err = s.Store.ListSecgwViolations(r.Context(), store.SecgwViolationFilter{RequestID: event.RequestID, Limit: violationLimit + 1})
		if err != nil {
			WriteError(w, r, err)
			return
		}
	}
	hasMore := len(all) > violationLimit
	if hasMore {
		all = all[:violationLimit]
	}
	other := []*store.SecgwViolation{}
	for _, v := range all {
		if v.ClassifierModel == "" {
			other = append(other, v)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"request_id":                 event.RequestID,
		"classifier_runs":            runs,
		"other_violations":           other,
		"violations_has_more":        hasMore,
		"violations_limit":           violationLimit,
		"violations_scanned":         len(all),
		"violations_scope_available": event.RequestID != "",
		"action":                     event.SecgwAction,
	})
}
