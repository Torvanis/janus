package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Route evaluation: is a routed answer as good as what the default would
// have said?
//
// Out of band, after the caller has their answer: a sampled routed request
// is sent again to the router's default model, then a judge model compares
// the two answers twice, blind and with the order swapped. Position bias is
// the judge's best-known failure, so a verdict that flips with the order is
// recorded as a tie. Only verdicts, scores, cost and timing are stored; the
// prompt and both answers live in memory for the duration of the job.
//
// Evaluations never compete with users: a bounded queue (full = sample
// dropped), at most eval_max_concurrent running at once per replica, a
// pause while the replica is busy, and per-route daily count and spend
// limits reserved atomically before anything is spent. All evaluation calls
// are metered as internal platform overhead on the caller's request id,
// never against the caller's quota.
//
// The two out-of-band calls go through the Security Gateway policies that
// would apply to the caller sending that text to that model (the router,
// the model, its upstream, the caller and their groups): a policy that
// would block or redact skips the sample, so evaluation never sends a
// prompt somewhere the caller's own request could not have gone.

const evalQueueSize = 64

type evalJob struct {
	requestID      string
	routerID       string
	routeID        string
	routedModelID  string
	defaultModelID string
	// defaultThinking is the reasoning effort the router's default would
	// have been given for this request ("" = the caller's own setting).
	defaultThinking string
	cfg             store.RouteEval
	// body is the caller's request as it stood before the route rewrote it
	// (after any router-level redaction).
	body          []byte
	routedAnswer  string
	routedCost    int64
	routedLatency time.Duration
	// The caller, for the Security Gateway subject of the out-of-band calls.
	userID         string
	serviceTokenID string
	groupIDs       []string
}

type evalRunner struct {
	s       *Server
	queue   chan *evalJob
	pending atomic.Int64
	// running counts evaluations in progress; freed wakes the dispatcher.
	running atomic.Int64
	freed   chan struct{}
}

func newEvalRunner(s *Server) *evalRunner {
	r := &evalRunner{s: s, queue: make(chan *evalJob, evalQueueSize), freed: make(chan struct{}, 1)}
	go r.dispatch()
	return r
}

// admit reserves a queue slot before the answer is captured, so a full
// queue costs nothing.
func (r *evalRunner) admit() bool {
	if r.pending.Add(1) > evalQueueSize {
		r.pending.Add(-1)
		return false
	}
	return true
}

func (r *evalRunner) release() { r.pending.Add(-1) }

func (r *evalRunner) enqueue(j *evalJob) {
	select {
	case r.queue <- j:
	default:
		r.release()
	}
}

// Pending is the evaluation backlog on this replica.
func (r *evalRunner) Pending() int64 { return r.pending.Load() }

// Running is how many evaluations are in progress on this replica.
func (r *evalRunner) Running() int64 { return r.running.Load() }

// dispatch starts queued evaluations, never more than the configured
// eval_max_concurrent at once (read per job, so a change applies to the
// next one).
func (r *evalRunner) dispatch() {
	for j := range r.queue {
		for r.running.Load() >= int64(r.maxConcurrent()) {
			select {
			case <-r.freed:
			case <-time.After(time.Second):
			}
		}
		r.running.Add(1)
		go func(j *evalJob) {
			defer func() {
				r.running.Add(-1)
				select {
				case r.freed <- struct{}{}:
				default:
				}
				r.release()
			}()
			r.run(j)
		}(j)
	}
}

func (r *evalRunner) maxConcurrent() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := r.s.Store.RoutingSettings(ctx); err == nil && st.EvalMaxConcurrent > 0 {
		return st.EvalMaxConcurrent
	}
	return store.DefaultRoutingSettings().EvalMaxConcurrent
}

func (r *evalRunner) run(j *evalJob) {
	s := r.s
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	settings, err := s.Store.RoutingSettings(ctx)
	if err != nil {
		return
	}
	// Pause while this replica is busy serving users.
	for settings.EvalPauseAboveInFlight > 0 && s.routingRT().proxyInFlight.Load() > int64(settings.EvalPauseAboveInFlight) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	judgeID := j.cfg.JudgeModelID
	if judgeID == "" {
		judgeID = settings.EvalJudgeModelID
	}
	res := &store.RoutingEvalResult{RequestID: j.requestID, RouterID: j.routerID, RouteID: j.routeID,
		RoutedModelID: j.routedModelID, DefaultModelID: j.defaultModelID, JudgeModelID: judgeID,
		RoutedCostNano: j.routedCost, RoutedLatencyMs: int(j.routedLatency.Milliseconds())}
	prompt := evalPromptText(j.body)
	// Resolve both models the way a request naming them would be served
	// (an alias or pool serves from one of its members). A disabled model,
	// or one that has since become a classifier, is not called.
	def, defErr := s.evalTarget(ctx, j.defaultModelID)
	var replay []byte
	if defErr == nil {
		replay = s.evalDefaultBody(ctx, j, def, settings.EvalMaxOutputTokens)
	}
	var judge store.ResolvedModel
	judgeErr := errors.New("no judge model is configured")
	if judgeID != "" {
		judge, judgeErr = s.evalTarget(ctx, judgeID)
	}
	// Security Gateway first, before anything is reserved or spent: the
	// default gets the replayed request, the judge gets the prompt and
	// the routed answer (and later the default's answer, checked again
	// below).
	if defErr == nil {
		if reason := s.evalPolicyStops(ctx, j, def, replay, "the default model"); reason != "" {
			s.recordEvalSkip(ctx, j, res, reason)
			return
		}
	}
	if judgeErr == nil {
		if reason := s.evalPolicyStops(ctx, j, judge, judgeRequestBody(prompt, j.routedAnswer, ""), "the judge"); reason != "" {
			s.recordEvalSkip(ctx, j, res, reason)
			return
		}
	}
	// Reserve against today's count and budget atomically, at a
	// conservative estimate, so concurrent jobs (on any replica) cannot all
	// pass the check and all spend.
	estimate := s.evalEstimate(ctx, def.Model, judge.Model, replay, prompt, j.routedAnswer, settings.EvalMaxOutputTokens)
	ok, err := s.Store.ReserveRouteEval(ctx, res, j.cfg.MaxPerDay, usage.NanoFromUSD(j.cfg.DailyBudgetUSD), estimate, time.Now().UTC())
	if err != nil {
		s.Logger.Warn("routing eval: reserve", "error", err.Error())
		return
	}
	if !ok {
		return
	}
	res.EvalCostNano = 0
	defer func() {
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer wcancel()
		if err := s.Store.FinishRoutingEval(wctx, res); err != nil {
			s.Logger.Warn("routing eval: write result", "error", err.Error())
		}
	}()
	if judgeErr != nil {
		res.Status, res.Error = "error", "judge: "+judgeErr.Error()
		return
	}
	if defErr != nil {
		res.Status, res.Error = "error", "default model: "+defErr.Error()
		return
	}
	// 1. The default model's answer to the same request.
	started := time.Now()
	defAnswer, defCost, err := s.internalChatTo(ctx, def, replay, j.requestID)
	res.DefaultLatencyMs = int(time.Since(started).Milliseconds())
	res.DefaultCostNano = defCost
	res.EvalCostNano += defCost
	if err != nil {
		res.Status, res.Error = "error", "default model: "+truncate(err.Error(), 200)
		return
	}
	// The judge also reads the default's answer: check that copy too.
	if reason := s.evalPolicyStops(ctx, j, judge, judgeRequestBody(prompt, j.routedAnswer, defAnswer), "the judge"); reason != "" {
		res.Status, res.Error = store.EvalStatusSkipped, reason
		return
	}
	// 2. Judge twice, order swapped.
	v1, c1, err := s.judge(ctx, judge, prompt, j.routedAnswer, defAnswer, j.requestID)
	res.EvalCostNano += c1
	if err != nil {
		res.Status, res.Error = "error", "judge: "+truncate(err.Error(), 200)
		return
	}
	v2, c2, err := s.judge(ctx, judge, prompt, defAnswer, j.routedAnswer, j.requestID)
	res.EvalCostNano += c2
	if err != nil {
		res.Status, res.Error = "error", "judge: "+truncate(err.Error(), 200)
		return
	}
	// v1: A = routed; v2: A = default. Convert both to the routed answer's
	// point of view.
	first := v1.winnerAs("A")
	second := v2.winnerAs("B")
	res.RoutedScore = (v1.ScoreA + v2.ScoreB) / 2
	res.DefaultScore = (v1.ScoreB + v2.ScoreA) / 2
	res.Consistent = first == second
	switch {
	case first == second:
		res.Verdict = first
	default:
		res.Verdict = store.EvalVerdictTie
	}
	res.Rationale = nameAnswers(v1.Reason, s.modelLabel(ctx, j.routedModelID), evalTargetLabel(def))
	res.Status = "ok"
}

// evalTarget resolves a default or judge model for an out-of-band call. It
// refuses what a caller could not be served from either: a disabled model
// or alias, or a model that is a classifier.
func (s *Server) evalTarget(ctx context.Context, id string) (store.ResolvedModel, error) {
	resolved, err := s.resolveRouteTarget(ctx, id)
	if err == nil && resolved.Model != nil {
		return resolved, nil
	}
	if mm, merr := s.Store.ManagedModelByID(ctx, id); merr == nil {
		return store.ResolvedModel{}, fmt.Errorf("%s is not enabled or has nothing to serve from", mm.Name)
	}
	m, merr := s.Store.ModelByID(ctx, id)
	if merr != nil {
		return store.ResolvedModel{}, fmt.Errorf("model %s no longer exists", id)
	}
	if m.ClassifierRole != "" {
		return store.ResolvedModel{}, fmt.Errorf("%s is a classifier and cannot answer or judge", m.PublicName())
	}
	return store.ResolvedModel{}, fmt.Errorf("%s is not enabled", m.PublicName())
}

func evalTargetLabel(r store.ResolvedModel) string {
	if r.Managed != nil {
		return r.Managed.Name
	}
	if r.Model != nil {
		return r.Model.PublicName()
	}
	return "the other answer"
}

// evalDefaultBody is the request as the router's default would really have
// served it: the caller's own request (never the route's rewrites), with
// the default's thinking setting applied, never streamed, output capped.
func (s *Server) evalDefaultBody(ctx context.Context, j *evalJob, def store.ResolvedModel, maxTokens int) []byte {
	body := evalReplayBody(j.body, maxTokens)
	if j.defaultThinking == "" || def.Model == nil {
		return body
	}
	adapterType := ""
	if up, err := s.cachedUpstreamByID(ctx, def.Model.UpstreamID); err == nil {
		adapterType = up.AdapterType
	}
	effort := j.defaultThinking
	if facts := s.routedReasoning(def.Model.ID); facts != nil && effort != "none" {
		if fitted, changed := facts.Fit(effort); changed && fitted != "" {
			effort = fitted
		}
	}
	if out, _, ok := applyThinking(body, effort, adapterType); ok {
		body = out
	}
	return body
}

// evalPolicyStops runs the Security Gateway checks that would apply to the
// caller sending body to target through this router. It returns why the
// call must not be made (a check would block or redact it, or the gateway
// could not check), or "" when it may go.
func (s *Server) evalPolicyStops(ctx context.Context, j *evalJob, target store.ResolvedModel, body []byte, who string) string {
	eng, err := s.secgwEngine(ctx)
	if err != nil {
		return "not evaluated: the Security Gateway is unavailable, so nothing was sent to " + who
	}
	ev := &store.UsageEvent{UserID: j.userID, ServiceTokenID: j.serviceTokenID}
	eff := eng.Resolve(secgwSubjectFor(ev, j.groupIDs, &store.ManagedModel{ID: j.routerID}, target))
	if eff.Empty() {
		return ""
	}
	dec, err := eng.Ingress(ctx, eff, secgw.Request{Body: body, IsServiceToken: j.serviceTokenID != ""})
	if err != nil {
		return "not evaluated: the Security Gateway could not check the copy for " + who
	}
	switch dec.Action {
	case store.SecgwActionBlocked:
		return fmt.Sprintf("not evaluated: a Security Gateway %s check blocks sending this request to %s", checkLabel(dec.BlockKind), who)
	case store.SecgwActionRedacted:
		return "not evaluated: a Security Gateway policy would redact this request before it reached " + who
	}
	return ""
}

func checkLabel(k store.SecgwCheckKind) string {
	if k == "" {
		return "policy"
	}
	return strings.ReplaceAll(string(k), "_", " ")
}

// recordEvalSkip records a sample the Security Gateway kept from being
// evaluated, so the Evaluations tab can say why a route has fewer
// comparisons than expected. Bounded by the route's daily count.
func (s *Server) recordEvalSkip(ctx context.Context, j *evalJob, res *store.RoutingEvalResult, reason string) {
	n, err := s.Store.RouteEvalSkipsToday(ctx, j.routerID, j.routeID, time.Now().UTC())
	if err != nil || n >= j.cfg.MaxPerDay {
		return
	}
	res.Status, res.Error, res.EvalCostNano = store.EvalStatusSkipped, reason, 0
	if err := s.Store.InsertRoutingEval(ctx, res); err != nil {
		s.Logger.Warn("routing eval: write skip", "error", err.Error())
	}
}

// evalEstimate is a deliberately high estimate of one evaluation's cost,
// reserved against the daily budget before anything is spent: the replay
// at a full output cap, and two judge calls reading both answers.
func (s *Server) evalEstimate(ctx context.Context, def, judge *store.Model, replay []byte, prompt, routedAnswer string, maxOut int) int64 {
	if s.Config != nil && s.Config.LocalOnly {
		return 0
	}
	cost := func(m *store.Model, in, out int64) int64 {
		if m == nil {
			return 0
		}
		rates := usage.Rates{InNano: m.RateInNano, OutNano: m.RateOutNano, CachedNano: m.RateCachedNano}
		if card := s.cachedRateCard(ctx, m.ID); card.OK {
			rates = usage.Rates{InNano: card.In, OutNano: card.Out, CachedNano: card.Cached}
		}
		return usage.ComputeCostAll(usage.TokenCounts{In: in, Out: out}, rates)
	}
	// About 3 bytes per token errs high for prose and code.
	tokens := func(n int) int64 { return int64(n/3 + 1) }
	est := cost(def, tokens(len(replay)), int64(maxOut))
	judgeIn := tokens(len(judgeRequestBody(prompt, routedAnswer, ""))) + int64(maxOut)
	est += 2 * cost(judge, judgeIn, judgeMaxTokens)
	return est
}

// judgeVerdict is the judge's parsed answer.
type judgeVerdict struct {
	Winner string  `json:"winner"` // A, B or tie
	ScoreA float64 `json:"score_a"`
	ScoreB float64 `json:"score_b"`
	Reason string  `json:"reason"`
}

// winnerAs returns better/worse/tie from the point of view of the answer in
// position pos.
func (v judgeVerdict) winnerAs(pos string) string {
	w := strings.ToUpper(strings.TrimSpace(v.Winner))
	switch {
	case w == pos:
		return store.EvalVerdictBetter
	case w == "A" || w == "B":
		return store.EvalVerdictWorse
	}
	return store.EvalVerdictTie
}

const judgeSystem = `You are an impartial expert judge comparing two AI assistant answers to the same request.
Judge on: correctness, completeness, following the user's instructions, and clarity without padding. Length is not quality.
Do not favour either position. You do not know which systems wrote the answers.
Reply with ONLY a JSON object: {"winner":"A"|"B"|"tie","score_a":1-5,"score_b":1-5,"reason":"one short sentence"}`

// judgeMaxTokens caps each judge reply.
const judgeMaxTokens = 1024

// judgeRequestBody is the chat request the judge receives.
func judgeRequestBody(prompt, a, b string) []byte {
	const maxAnswer = 12000
	user := "REQUEST:\n" + trimMiddle(prompt, 8000) + "\n\nANSWER A:\n" + trimMiddle(a, maxAnswer) + "\n\nANSWER B:\n" + trimMiddle(b, maxAnswer)
	body, _ := json.Marshal(map[string]any{
		"messages":    []map[string]string{{"role": "system", "content": judgeSystem}, {"role": "user", "content": user}},
		"temperature": 0, "max_tokens": judgeMaxTokens,
	})
	return body
}

func (s *Server) judge(ctx context.Context, judge store.ResolvedModel, prompt, a, b, requestID string) (judgeVerdict, int64, error) {
	text, cost, err := s.internalChatTo(ctx, judge, judgeRequestBody(prompt, a, b), requestID)
	if err != nil {
		return judgeVerdict{}, cost, err
	}
	var v judgeVerdict
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start || json.Unmarshal([]byte(text[start:end+1]), &v) != nil {
		return judgeVerdict{}, cost, errors.New("the judge's answer was not the requested JSON")
	}
	switch strings.ToUpper(strings.TrimSpace(v.Winner)) {
	case "A", "B", "TIE":
	default:
		return judgeVerdict{}, cost, fmt.Errorf("the judge named no winner (%q)", truncate(v.Winner, 40))
	}
	return v, cost, nil
}

func trimMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max/2]) + "\n…\n" + string(r[len(r)-max/2:])
}

// evalPromptText is the conversation as the judge reads it.
func evalPromptText(body []byte) string {
	var m struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range m.Messages {
		var text string
		if json.Unmarshal(msg.Content, &text) != nil {
			var parts []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(msg.Content, &parts)
			for _, p := range parts {
				text += p.Text
			}
		}
		b.WriteString("[" + msg.Role + "] " + text + "\n")
	}
	return b.String()
}

// evalReplayBody is the request as sent to the default model: same
// messages and sampling, never streamed, output capped.
func evalReplayBody(body []byte, maxTokens int) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	delete(m, "stream")
	delete(m, "stream_options")
	cap := maxTokens
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		var v int
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &v) == nil && v > 0 && v < cap {
			cap = v
		}
		delete(m, k)
	}
	m["max_tokens"], _ = json.Marshal(cap)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// internalChatTo sends a non-streaming chat completion to a resolved
// catalog or managed model through its upstream adapter and meters it as
// internal overhead. It returns the assistant text and the call's cost.
func (s *Server) internalChatTo(ctx context.Context, resolved store.ResolvedModel, body []byte, requestID string) (string, int64, error) {
	model := resolved.Model
	if model == nil {
		return "", 0, errors.New("no model to call")
	}
	upstream, err := s.cachedUpstreamByID(ctx, model.UpstreamID)
	if err != nil || !upstream.Enabled {
		return "", 0, fmt.Errorf("upstream for %s is unavailable", model.PublicName())
	}
	provider, err := adapter.Get(upstream.AdapterType)
	if err != nil {
		return "", 0, err
	}
	apiKey, err := s.Cipher.Decrypt(upstream.EncryptedKey())
	if err != nil {
		return "", 0, errors.New("upstream credential cannot be decrypted")
	}
	if rewritten, ok := rewriteJSONModelField(ensureModelField(body), model.Name); ok {
		body = rewritten
	}
	req := &adapter.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: http.Header{"Content-Type": {"application/json"}},
		Body: body, Model: model.Name, Context: ctx, Client: s.upstreamHTTPClient(ctx)}
	prepared, err := provider.Prepare(adapter.Upstream{ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType}, req)
	if err != nil {
		return "", 0, err
	}
	send := func(p *adapter.Prepared) (*http.Response, error) {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(p.Body))
		if err != nil {
			return nil, err
		}
		hreq.Header = p.Header
		return s.upstreamHTTPClient(ctx).Do(hreq)
	}
	started := time.Now()
	resp, err := send(prepared)
	// Reasoning models refuse max_tokens and a non-default temperature, and
	// say which parameter: fix that one and resend (at most twice). A 400 is
	// not billed.
	for attempt := 0; err == nil && resp.StatusCode == http.StatusBadRequest && attempt < 2; attempt++ {
		refusal, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		fixed, ok := adjustRefusedParam(req.Body, refusal)
		if !ok {
			resp.Body = io.NopCloser(bytes.NewReader(refusal))
			break
		}
		req.Body = fixed
		again, perr := provider.Prepare(adapter.Upstream{ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType}, req)
		if perr != nil {
			return "", 0, perr
		}
		resp, err = send(again)
	}
	event := &store.UsageEvent{ModelID: model.ID, ModelName: model.PublicName(), UpstreamID: upstream.ID, EndpointPath: "/internal/routing/eval",
		HTTPMethod: http.MethodPost, Modality: "chat", RequestBytes: int64(len(body)), ClientApp: store.InternalRoutingClientApp, RequestID: requestID}
	defer func() {
		event.LatencyMs = int(time.Since(started).Milliseconds())
		event.UpstreamMs = event.LatencyMs
		s.writeUsage(ctx, event, "routing eval: write usage event", nil)
	}()
	if err != nil {
		event.HTTPStatus, event.ErrorCode, event.AccountingMode = http.StatusBadGateway, CodeUpstreamDown, usage.AccountingNone
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	event.HTTPStatus, event.ResponseBytes = resp.StatusCode, int64(len(raw))
	if err != nil {
		event.AccountingMode = usage.AccountingNone
		return "", 0, err
	}
	if resp.StatusCode/100 != 2 {
		event.AccountingMode = usage.AccountingNone
		if msg := upstreamErrorText(raw); msg != "" {
			return "", 0, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, upstream.Name, msg)
		}
		return "", 0, fmt.Errorf("HTTP %d from %s", resp.StatusCode, upstream.Name)
	}
	u, _ := provider.ExtractUsage(raw)
	rates := usage.Rates{InNano: model.RateInNano, OutNano: model.RateOutNano, CachedNano: model.RateCachedNano}
	if card := s.cachedRateCard(ctx, model.ID); card.OK {
		rates = usage.Rates{InNano: card.In, OutNano: card.Out, CachedNano: card.Cached}
	}
	s.applyUsage(event, u, rates, -1)
	if s.Config != nil && s.Config.LocalOnly {
		event.CostNano = 0
	}
	out, err := provider.TransformResponse(req, raw)
	if err != nil {
		return "", event.CostNano, err
	}
	return answerText(out), event.CostNano, nil
}

func ensureModelField(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["model"]; ok {
		return body
	}
	m["model"] = json.RawMessage(`""`)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// reasoningOutputFloor is the output cap used once a model shows it reasons
// (it refused max_tokens): reasoning tokens count against the cap, so a
// small cap would be spent before any answer is written.
const reasoningOutputFloor = 8192

// adjustRefusedParam reads an OpenAI-style 400 naming an unsupported
// parameter and returns the body with that parameter fixed: max_tokens is
// renamed to max_completion_tokens (raised to leave room for reasoning), and
// temperature / top_p are dropped so the model default applies.
func adjustRefusedParam(body, refusal []byte) ([]byte, bool) {
	var e struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(refusal, &e) != nil {
		return nil, false
	}
	param := e.Error.Param
	if param == "" {
		for _, p := range []string{"max_tokens", "temperature", "top_p"} {
			if strings.Contains(e.Error.Message, "'"+p+"'") {
				param = p
				break
			}
		}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	switch param {
	case "max_tokens":
		raw, ok := m["max_tokens"]
		if !ok {
			return nil, false
		}
		var v int
		_ = json.Unmarshal(raw, &v)
		delete(m, "max_tokens")
		m["max_completion_tokens"], _ = json.Marshal(max(v, reasoningOutputFloor))
	case "temperature", "top_p":
		if _, ok := m[param]; !ok {
			return nil, false
		}
		delete(m, param)
	default:
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// upstreamErrorText is a short, single-line form of an upstream error body
// for evaluation results (never the request or answers).
func upstreamErrorText(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &e) == nil {
		msg = e.Error.Message
		if msg == "" {
			msg = e.Message
		}
	}
	return truncate(strings.Join(strings.Fields(msg), " "), 200)
}

// reAnswerRef matches the judge's explicit references to the two answers:
// "Answer A", "answer B's", "(A)", "A's", and a bare A/B used as a subject
// ("A is", "B was", "B wins", "A and B"). A capital "A" used as the English
// article ("A more complete answer") is left alone.
var reAnswerRef = regexp.MustCompile(`\b[Aa]nswer\s+([AB])\b|\(([AB])\)|\b([AB])'s\b|\b([AB])\b(?:\s+(?:is|was|are|were|has|had|does|did|wins|won|gives|gave|handles|adds|uses|misses|fails|covers|includes|provides|answers|correctly|also|and|or|but)\b|\s*[,.;:!?)]|$)|\b(?:than|and|or|over|while|but|vs\.?|versus)\s+([AB])\b`)

// nameAnswers replaces the judge's A/B (A = routed in the first pass) with
// the model names, so the stored reason reads on its own.
func nameAnswers(reason, routed, def string) string {
	return reAnswerRef.ReplaceAllStringFunc(reason, func(m string) string {
		sub := reAnswerRef.FindStringSubmatchIndex(m)
		for g := 1; g*2+1 < len(sub); g++ {
			if sub[g*2] < 0 {
				continue
			}
			name := def
			if m[sub[g*2]:sub[g*2+1]] == "A" {
				name = routed
			}
			if g == 1 {
				// "Answer A" reads as the model's name alone.
				return name
			}
			return m[:sub[g*2]] + name + m[sub[g*2+1]:]
		}
		return m
	})
}

func (s *Server) modelLabel(ctx context.Context, id string) string {
	if r, err := s.resolveRouteTarget(ctx, id); err == nil && r.Model != nil {
		if r.Managed != nil {
			return r.Managed.Name
		}
		return r.Model.PublicName()
	}
	if m, err := s.Store.ModelByID(ctx, id); err == nil {
		return m.PublicName()
	}
	return "the other answer"
}
