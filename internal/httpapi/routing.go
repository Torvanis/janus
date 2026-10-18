package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/torvanis/janus/internal/balancer"
	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/routing"
	"github.com/torvanis/janus/internal/secgw"
	secgwclassify "github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Auto-routing on the proxy path.
//
// A router resolves like a managed alias whose target is its default, so
// grants, the router-scoped security bindings and quotas all apply to it
// before anything is decided. The decision then happens in one of three
// places, cheapest first:
//
//  1. Sticky: a later turn of a conversation the router has already placed
//     reuses that route without asking the classifier at all.
//  2. Riding the guard call: when a Security Gateway check is already
//     calling the same decision classifier about the same text, the
//     routing questions are added to that call (no extra round trip).
//  3. Its own call, started alongside the guards (or after them when a
//     policy redacts, so the classifier only ever sees the redacted text),
//     bounded by the router's time budget and a per-replica concurrency cap.
//
// Anything that goes wrong sends the request to the default. Shadow routers
// never wait: they serve the default and decide on a background queue.

const (
	headerRoute       = "X-Janus-Route"
	headerRouteReason = "X-Janus-Route-Reason"
	headerRouteModel  = "X-Janus-Route-Model"
)

// routingFeature labels routing questions in classifier traces.
const routingFeature = "auto_routing"

// routingState is the cached, request-independent routing configuration.
type routingState struct {
	settings store.RoutingSettings
	cats     []routing.Category
	minConf  map[string]float64
	// classifier is the decision classifier (nil when none is configured
	// or it cannot be built; problem says why).
	classifier routingClassifier
	problem    string
}

type routingClassifier interface {
	Decide(ctx context.Context, text string, questions []classify.Question) (classify.Result, error)
	Key() string
	Style() string
	IsDecision() bool
}

// routingRuntime is per-Server mutable state: the decision semaphore, the
// sticky LRU, the shadow queue and the evaluation queue.
type routingRuntime struct {
	inflight atomic.Int64
	sticky   *lru.Cache[string, stickyEntry]
	shadow   chan func()
	eval     *evalRunner
	// proxyInFlight counts proxied requests in progress on this replica;
	// evaluations pause while it is above the configured threshold.
	proxyInFlight atomic.Int64
	// reasoning is what this replica learned about a catalog model's
	// reasoning values from refusals of a level auto-routing forced
	// (modelID -> learnedReasoning). Facts expire, so an upgraded model
	// is probed again instead of being down-fitted forever.
	reasoning sync.Map
}

// learnedReasoningTTL is how long a learned refusal fact is trusted.
const learnedReasoningTTL = 24 * time.Hour

type learnedReasoning struct {
	facts   subscription.Reasoning
	learned time.Time
}

type stickyEntry struct {
	routeID    string
	category   string
	difficulty float64
	expires    time.Time
}

var routingRuntimes sync.Map // *Server -> *routingRuntime

func (s *Server) routingRT() *routingRuntime {
	if v, ok := routingRuntimes.Load(s); ok {
		return v.(*routingRuntime)
	}
	cache, _ := lru.New[string, stickyEntry](50000)
	rt := &routingRuntime{sticky: cache, shadow: make(chan func(), 2000)}
	actual, loaded := routingRuntimes.LoadOrStore(s, rt)
	if !loaded {
		for i := 0; i < 4; i++ {
			go func() {
				for job := range rt.shadow {
					job()
				}
			}()
		}
		rt.eval = newEvalRunner(s)
	}
	return actual.(*routingRuntime)
}

// routingState loads (and caches) the routing configuration.
func (s *Server) routingState(ctx context.Context) (*routingState, error) {
	return cachedRead(&s.configCache, "routing_state", func() (*routingState, error) {
		settings, err := s.Store.RoutingSettings(ctx)
		if err != nil {
			return nil, err
		}
		st := &routingState{settings: settings, minConf: map[string]float64{}}
		cats, err := s.Store.ListRoutingCategories(ctx)
		if err != nil {
			return nil, err
		}
		insights := map[string][2]string{}
		if ins, err := s.Store.ListClassificationCategories(ctx); err == nil {
			for _, c := range ins {
				insights[c.Slug] = [2]string{c.Name, c.Description}
			}
		}
		st.cats = routing.CategoriesFrom(cats, insights)
		for _, c := range st.cats {
			st.minConf[c.Slug] = c.MinConfidence
		}
		if settings.ClassifierModelID == "" {
			st.problem = "No routing classifier is chosen."
			return st, nil
		}
		cl, err := s.secgwClassifiers().For(ctx, settings.ClassifierModelID)
		if err != nil {
			st.problem = "The routing classifier cannot be used: " + err.Error()
			return st, nil
		}
		d, ok := cl.(routingClassifier)
		if !ok || !d.IsDecision() {
			st.problem = "The routing classifier is not a decision model."
			return st, nil
		}
		st.classifier = d
		return st, nil
	})
}

// routerConfig loads a router's mode and configuration through the cache.
func (s *Server) routerConfig(ctx context.Context, id string) (string, store.RouterConfig, error) {
	type rc struct {
		mode string
		cfg  store.RouterConfig
	}
	v, err := cachedModelRead(&s.configCache, s.Store.ModelRevision(), "router|"+id, func() (rc, error) {
		mode, cfg, err := s.Store.RouterConfigFor(ctx, id)
		return rc{mode, cfg}, err
	})
	return v.mode, v.cfg, err
}

// routeCandidates resolves each route's target for selection.
func (s *Server) routeCandidates(ctx context.Context, cfg store.RouterConfig) []routing.Candidate {
	out := make([]routing.Candidate, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		c := routing.Candidate{Route: r}
		if res, err := s.resolveRouteTarget(ctx, r.ModelID); err == nil && res.Model != nil {
			c.Servable = true
			c.ContextWindow = res.Model.ContextWindow
			if res.Managed != nil && res.Managed.ContextWindow > 0 {
				c.ContextWindow = res.Managed.ContextWindow
			}
		}
		out = append(out, c)
	}
	return out
}

// resolveRouteTarget resolves a route's model (catalog model or managed
// alias/pool) the same way a request naming it would be resolved.
func (s *Server) resolveRouteTarget(ctx context.Context, id string) (store.ResolvedModel, error) {
	if mm, err := cachedModelRead(&s.configCache, s.Store.ModelRevision(), "managed-by-id|"+id, func() (*store.ManagedModel, error) {
		return s.Store.ManagedModelByID(ctx, id)
	}); err == nil {
		if mm.Kind == store.ManagedKindRouter || mm.Status != store.ManagedModelEnabled {
			return store.ResolvedModel{}, store.ErrManagedModelBroken
		}
		return s.cachedResolveModel(ctx, mm.Name)
	}
	m, err := s.cachedModelByID(ctx, id)
	if err != nil {
		return store.ResolvedModel{}, err
	}
	if m.Status != store.ModelEnabled || m.ClassifierRole != "" {
		return store.ResolvedModel{}, store.ErrNotFound
	}
	return store.ResolvedModel{Model: m}, nil
}

// pendingRoute is a router request between resolution and forwarding.
type pendingRoute struct {
	router *store.ManagedModel
	mode   string
	cfg    store.RouterConfig
	st     *routingState
	cands  []routing.Candidate
	text   string
	// stickyKey identifies the conversation (empty when stickiness is off
	// or the request carries nothing stable to key on).
	stickyKey string
	// decided is set once a decision exists (sticky hit, ride-along, own
	// call, or a default for a stated reason).
	decided  bool
	decision routing.Decision
	answer   routing.Answer
	// extra rides on the guard call when one is made to the same classifier.
	extra *secgw.Extra
	// own is the in-flight own classifier call (nil when none).
	own     chan routingCall
	started time.Time
	calls   int
	status  string
	errText string
	waited  time.Duration
	// thinkingSent is the reasoning effort auto-routing put on the body
	// ("" when the route passes the caller's through).
	thinkingSent string
	// callerBody is the request before the route rewrote it, so an
	// evaluation replays what the default would really have been sent.
	callerBody []byte
	// instructions records what router instructions did to the forwarded
	// body: router, route, router+route, not_applied, or "" (none set).
	instructions string
}

// routedReasoning returns what this replica learned about a catalog
// model's reasoning values (nil when nothing, or the fact has expired).
func (s *Server) routedReasoning(modelID string) *subscription.Reasoning {
	rt := s.routingRT()
	v, ok := rt.reasoning.Load(modelID)
	if !ok {
		return nil
	}
	l := v.(learnedReasoning)
	if time.Since(l.learned) > learnedReasoningTTL {
		rt.reasoning.Delete(modelID)
		return nil
	}
	r := l.facts
	return &r
}

func (s *Server) rememberRoutedReasoning(modelID string, r subscription.Reasoning) {
	s.routingRT().reasoning.Store(modelID, learnedReasoning{facts: r, learned: time.Now()})
}

type routingCall struct {
	res     classify.Result
	err     error
	elapsed time.Duration
}

// prepareRoute is called for every request whose model resolved to a
// router. It never fails: a router with nothing to decide, or no usable
// classifier, simply serves its default. granted and subject describe the
// caller, so a remembered (sticky) route is held to the same per-request
// rules as a fresh decision.
func (s *Server) prepareRoute(ctx context.Context, r *http.Request, body []byte, event *store.UsageEvent, router *store.ManagedModel,
	granted map[string]string, subject quota.Subject) *pendingRoute {
	mode, cfg, err := s.routerConfig(ctx, router.ID)
	if err != nil {
		s.Logger.WarnContext(ctx, "routing: router config unavailable", "router", router.Name, "error", err.Error())
		return nil
	}
	pr := &pendingRoute{router: router, mode: mode, cfg: cfg, started: time.Now()}
	pr.cands = s.routeCandidates(ctx, cfg)
	if !routing.NeedsDecision(cfg) {
		pr.settle(routing.Default(cfg, 0, store.RouteReasonDefault, nil))
		return pr
	}
	st, err := s.routingState(ctx)
	if err != nil || st.classifier == nil {
		pr.settle(routing.Default(cfg, 0, store.RouteReasonNoClassifier, nil))
		return pr
	}
	pr.st = st
	if !classificationEligible(event.EndpointPath, false) {
		pr.settle(routing.Default(cfg, 0, store.RouteReasonNoText, nil))
		return pr
	}
	// The new turn, exactly as the guards read it, so the questions can
	// share their call. An own call trims it to the routing limit.
	pr.text = secgw.NewTurnText(body)
	if strings.TrimSpace(pr.text) == "" {
		pr.text = classify.TrimText(classify.ExtractText(body, classify.ScopeLastUser, false), st.settings.MaxChars)
	}
	if strings.TrimSpace(pr.text) == "" {
		pr.settle(routing.Default(cfg, 0, store.RouteReasonNoText, nil))
		return pr
	}
	if s.refusedWhateverServes(ctx, subject, router.ID) {
		// A quota that applies whichever route serves (on the router, or
		// on no model) already refuses this caller: the request will be
		// refused, so it must not cost a classifier call first.
		pr.settle(routing.Default(cfg, 0, store.RouteReasonOverQuota, nil))
		return pr
	}
	if cfg.Sticky && mode == store.RouterModeLive {
		caller := event.UserID
		if event.ServiceTokenID != "" {
			caller = "svc:" + event.ServiceTokenID
		}
		if key, source := balancer.SessionKey(r.Header, body, caller); key != "" {
			pr.stickyKey = router.ID + "|" + key
			// A derived key hashes the conversation's opening; a request
			// with no assistant turn yet is that opening, so only another
			// replica's record of a retry could match: skip the DB read.
			first := source == "derived" && !hasAssistantTurn(body)
			if hit, ok := s.lookupSticky(ctx, pr.stickyKey, !first); ok {
				// The remembered route must still serve THIS request: it
				// fits the context window, the caller still holds the
				// grant a restricted route needs, and is not over a quota
				// the route skips on. Otherwise decide afresh.
				rc := s.routingContext(ctx, body, granted, subject, router.ID)
				if d, ok := routing.ByRouteID(cfg, pr.cands, hit.routeID, hit.difficulty, rc); ok {
					pr.answer = routing.Answer{Category: hit.category, Difficulty: hit.difficulty}
					pr.settle(d)
					return pr
				}
			}
		}
	}
	questions := routing.Questions(cfg, st.cats, st.settings.DifficultyStyle == store.RoutingDifficultyCategory)
	pr.extra = &secgw.Extra{ClassifierKey: st.classifier.Key(), Text: pr.text, Questions: questions, Feature: routingFeature,
		AnyStyle: mode == store.RouterModeLive, Timeout: time.Duration(cfg.BudgetMs) * time.Millisecond}
	return pr
}

func (pr *pendingRoute) settle(d routing.Decision) {
	pr.decided, pr.decision = true, d
}

// fallToDefault replaces a route that turned out unable to serve (every
// pool member out of service) with the router's default.
func (pr *pendingRoute) fallToDefault() {
	pr.decision = routing.Default(pr.cfg, pr.answer.Difficulty, store.RouteReasonUnavailable, pr.decision.Trace)
	pr.thinkingSent = ""
}

// refusedWhateverServes reports whether a quota that applies to every
// route (scoped to the router, or to no model) already refuses the caller.
// A storage error answers false: the real check after routing decides.
func (s *Server) refusedWhateverServes(ctx context.Context, subject quota.Subject, routerID string) bool {
	if s.Quota == nil || (subject.UserID == "" && subject.ServiceTokenID == "") {
		return false
	}
	// The router id as the "model" narrows the rules to unscoped ones and
	// those scoped to the router itself.
	dec, err := s.Quota.Check(ctx, subject, routerID)
	return err == nil && !dec.Allowed
}

// hasAssistantTurn reports whether a chat body already carries an
// assistant message (a later turn of a conversation).
func hasAssistantTurn(body []byte) bool {
	var m struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	for _, msg := range m.Messages {
		if msg.Role == "assistant" {
			return true
		}
	}
	return false
}

// guardWillCall reports whether the effective policy makes a model-backed
// ingress call to the routing classifier about the same text, so the
// routing questions can ride on it.
func (s *Server) guardWillCall(ctx context.Context, eff *secgw.Effective, pr *pendingRoute) bool {
	if eff.Empty() || pr.st == nil {
		return false
	}
	for _, kind := range []store.SecgwCheckKind{store.SecgwCheckPromptInjection, store.SecgwCheckContentSafety} {
		c, ok := eff.Has(kind, store.SecgwDirectionIngress)
		if !ok || c.ClassifierModelID == "" {
			continue
		}
		cl, err := s.secgwClassifiers().For(ctx, c.ClassifierModelID)
		if err != nil {
			continue
		}
		if d, ok := cl.(routingClassifier); ok && d.IsDecision() && d.Key() == pr.st.classifier.Key() && secgw.CanCarry(c, pr.extra.Timeout) {
			if pr.mode == store.RouterModeLive || d.Style() == classify.StyleCombined {
				return true
			}
		}
	}
	return false
}

// policyRedacts reports whether any ingress check may rewrite the body: the
// classifier must then only ever see the redacted text.
func policyRedacts(eff *secgw.Effective) bool {
	if eff.Empty() {
		return false
	}
	for _, c := range eff.Checks {
		if c.Enabled && c.Mode == store.SecgwModeRedact && c.Direction != store.SecgwDirectionEgress {
			return true
		}
	}
	return false
}

// startOwnCall starts the routing classifier call on its own, bounded by
// the router's budget and the per-replica concurrency cap. Live routers
// only; shadow routers decide in the background.
func (s *Server) startOwnCall(ctx context.Context, pr *pendingRoute) {
	if pr == nil || pr.decided || pr.extra == nil || pr.own != nil || pr.mode != store.RouterModeLive {
		return
	}
	rt := s.routingRT()
	if rt.inflight.Add(1) > int64(pr.st.settings.MaxConcurrentDecisions) {
		rt.inflight.Add(-1)
		pr.settle(routing.Default(pr.cfg, 0, store.RouteReasonBusy, nil))
		return
	}
	pr.own = make(chan routingCall, 1)
	text, questions := classify.TrimText(pr.extra.Text, pr.st.settings.MaxChars), pr.extra.Questions
	budget := time.Duration(pr.cfg.BudgetMs) * time.Millisecond
	cctx := classify.WithFeature(context.WithoutCancel(ctx), routingFeature, "inline")
	go func() {
		defer rt.inflight.Add(-1)
		cctx, cancel := context.WithTimeout(cctx, budget)
		defer cancel()
		started := time.Now()
		res, err := pr.st.classifier.Decide(cctx, text, questions)
		pr.own <- routingCall{res: res, err: err, elapsed: time.Since(started)}
	}()
}

// finishRoute settles the decision after the Security Gateway ran: from the
// ride-along answer, the own call, or the default. It waits at most what is
// left of the router's budget.
func (s *Server) finishRoute(ctx context.Context, pr *pendingRoute, rc routing.Context) {
	if pr == nil || pr.decided {
		return
	}
	if pr.mode != store.RouterModeLive {
		pr.settle(routing.Default(pr.cfg, 0, store.RouteReasonShadow, nil))
		return
	}
	var res classify.Result
	var err error
	switch {
	case pr.extra != nil && pr.extra.Ran:
		res, err = pr.extra.Result, pr.extra.Err
		pr.waited = 0 // counted in the guards' wait
	case pr.own != nil:
		remaining := time.Duration(pr.cfg.BudgetMs)*time.Millisecond - time.Since(pr.started)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case c := <-pr.own:
			res, err = c.res, c.err
			pr.calls = max(1, c.res.Calls)
		case <-time.After(remaining):
			err = context.DeadlineExceeded
			pr.calls = 1
		}
		pr.waited = time.Since(pr.started)
	default:
		pr.settle(routing.Default(pr.cfg, 0, store.RouteReasonError, nil))
		return
	}
	if err != nil {
		reason := store.RouteReasonError
		if errors.Is(err, context.DeadlineExceeded) {
			reason = store.RouteReasonTimeout
		}
		pr.status, pr.errText = "error", truncate(err.Error(), 300)
		pr.settle(routing.Default(pr.cfg, 0, reason, nil))
		return
	}
	pr.status = "ok"
	pr.answer = routing.Interpret(res)
	pr.settle(routing.Choose(pr.cfg, pr.cands, pr.answer, pr.st.minConf, rc))
}

// applyRoute turns the decision into the model that serves: it resolves
// the chosen target (alias, pool or catalog model), rewrites the body's
// model and reasoning effort, and stamps the event and headers. It returns
// the new resolution and the body to forward.
func (s *Server) applyRoute(ctx context.Context, w http.ResponseWriter, pr *pendingRoute, event *store.UsageEvent, body []byte, fallback store.ResolvedModel) (store.ResolvedModel, []byte) {
	pr.callerBody = body
	d := pr.decision
	resolved, err := s.resolveRouteTarget(ctx, d.ModelID)
	if err != nil && !d.IsDefault() {
		// The chosen route became unservable between selection and now.
		d = routing.Default(pr.cfg, pr.answer.Difficulty, store.RouteReasonUnavailable, d.Trace)
		pr.decision = d
		resolved, err = s.resolveRouteTarget(ctx, d.ModelID)
	}
	if err != nil || resolved.Model == nil {
		resolved = fallback
	}
	event.ReasoningAdjustment = ""
	reason := d.Reason
	if d.Reason == store.RouteReasonMatched && pr.answer.Category != "" {
		reason += ":" + pr.answer.Category
	}
	event.RouteReason = reason
	if pr.cfg.Disclose {
		w.Header().Set(headerRoute, routeLabel(pr.cfg, d))
		w.Header().Set(headerRouteReason, reason)
		if resolved.Model != nil {
			w.Header().Set(headerRouteModel, resolved.Model.PublicName())
		}
	}
	if d.Thinking != "" && resolved.Model != nil {
		adapterType := ""
		if up, err := s.cachedUpstreamByID(ctx, resolved.Model.UpstreamID); err == nil {
			adapterType = up.AdapterType
		}
		effort := d.Thinking
		if facts := s.routedReasoning(resolved.Model.ID); facts != nil && effort != "none" {
			// A level this model is known to refuse: send the nearest it takes.
			if fitted, changed := facts.Fit(effort); changed && fitted != "" {
				effort = fitted
			}
		}
		if out, from, ok := applyThinking(body, effort, adapterType); ok {
			body = out
			pr.thinkingSent = effort
			if from == "" {
				from = "unset"
			}
			event.ReasoningAdjustment = from + "->" + effort + " (auto-routing)"
		}
	}
	return resolved, body
}

func routeLabel(cfg store.RouterConfig, d routing.Decision) string {
	if d.IsDefault() {
		return "default"
	}
	if d.Index >= 0 && d.Index < len(cfg.Routes) && cfg.Routes[d.Index].Name != "" {
		return cfg.Routes[d.Index].Name
	}
	return "route " + strconv.Itoa(d.Index+1)
}

// applyThinking forces a reasoning effort on a chat body. OpenAI-wire
// upstreams, including Vertex (its OpenAI-compatible Gemini surface), take
// reasoning_effort. So does Anthropic: its adapter translates the effort
// into the thinking form each Claude model takes (adaptive thinking with
// an effort on 4.6 and later, a token budget before that) and adjusts the
// output cap and sampling to match. A caller's own Anthropic `thinking`
// object is replaced: the route decides. Bedrock's Converse translation
// carries no thinking field, so a forced level could only inflate
// max_tokens and drop the caller's temperature: the body is left as it is
// and nothing is recorded. It returns the previous effort for the record.
func applyThinking(body []byte, effort, adapterType string) ([]byte, string, bool) {
	if adapterType == "bedrock" {
		return body, "", false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, "", false
	}
	prev := subscription.RequestedEffort(m)
	switch adapterType {
	case "anthropic":
		delete(m, "thinking")
		delete(m, "reasoning")
		m["reasoning_effort"], _ = json.Marshal(effort)
	default:
		if _, ok := m["reasoning"]; ok {
			subscription.SetEffort(m, effort)
		} else {
			m["reasoning_effort"], _ = json.Marshal(effort)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, "", false
	}
	return out, prev, true
}

// routingContext builds the selection context for one request.
func (s *Server) routingContext(ctx context.Context, body []byte, granted map[string]string, subject quota.Subject, routerID string) routing.Context {
	return routing.Context{
		EstimatedTokens: int64(len(body) / 4),
		Granted: func(modelID string) bool {
			_, ok := granted[modelID]
			return ok
		},
		OverQuota: func(modelID string) bool {
			res, err := s.resolveRouteTarget(ctx, modelID)
			if err != nil || res.Model == nil {
				return false
			}
			also := []string{routerID}
			if res.Managed != nil {
				also = append(also, res.Managed.ID)
			}
			dec, err := s.Quota.Check(ctx, subject, res.Model.ID, also...)
			return err == nil && !dec.Allowed
		},
	}
}

// --- stickiness ---------------------------------------------------------------

func (s *Server) lookupSticky(ctx context.Context, key string, readDB bool) (stickyEntry, bool) {
	rt := s.routingRT()
	now := time.Now()
	if e, ok := rt.sticky.Get(key); ok && e.expires.After(now) {
		return e, true
	}
	if !readDB {
		return stickyEntry{}, false
	}
	// Another replica may have placed this conversation: one indexed read.
	rctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	row, err := s.Store.LookupStickyRoute(rctx, key, now)
	if err != nil {
		return stickyEntry{}, false
	}
	e := stickyEntry{routeID: row.RouteID, category: row.Category, difficulty: row.Difficulty, expires: row.ExpiresAt}
	rt.sticky.Add(key, e)
	return e, true
}

func (s *Server) rememberSticky(pr *pendingRoute) {
	if pr.stickyKey == "" || !pr.cfg.Sticky || pr.mode != store.RouterModeLive {
		return
	}
	switch pr.decision.Reason {
	case store.RouteReasonMatched, store.RouteReasonNoMatch, store.RouteReasonLowConfidence, store.RouteReasonSticky:
	default:
		// A timeout or failure is not a decision about the conversation.
		return
	}
	exp := time.Now().Add(time.Duration(pr.cfg.StickyMinutes) * time.Minute)
	e := stickyEntry{routeID: pr.decision.RouteID, category: pr.answer.Category, difficulty: pr.answer.Difficulty, expires: exp}
	s.routingRT().sticky.Add(pr.stickyKey, e)
	key, routerID := pr.stickyKey, pr.router.ID
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Store.PutStickyRoute(ctx, store.StickyRoute{Key: key, RouterID: routerID, RouteID: e.routeID, Category: e.category,
			Difficulty: e.difficulty, ExpiresAt: exp}); err != nil {
			s.Logger.Warn("routing: remember conversation", "error", err.Error())
		}
	}()
}

// --- recording -----------------------------------------------------------------

// recordRouteDecision persists the decision (labels and timing only).
func (s *Server) recordRouteDecision(pr *pendingRoute, event *store.UsageEvent, served *store.Model) {
	if pr == nil {
		return
	}
	d := &store.RoutingDecision{RequestID: event.RequestID, RouterID: pr.router.ID, Mode: pr.mode, RouteID: pr.decision.RouteID,
		RouteName: routeLabel(pr.cfg, pr.decision), ModelID: pr.decision.ModelID, Category: pr.answer.Category,
		Confidence: pr.answer.Confidence, Difficulty: pr.answer.Difficulty, Thinking: pr.decision.Thinking, Reason: pr.decision.Reason,
		Status: pr.status, Error: pr.errText, DecisionMs: int(pr.waited.Milliseconds()), Calls: pr.calls,
		UserID: event.UserID, ServiceTokenID: event.ServiceTokenID}
	if served != nil {
		d.ModelName = served.PublicName()
	}
	if pr.extra != nil && pr.extra.Ran {
		d.Calls = 0 // shared with the guards
	}
	s.persistRouteDecision(d)
	s.rememberSticky(pr)
}

func (s *Server) persistRouteDecision(d *store.RoutingDecision) {
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Store.InsertRoutingDecision(ctx, d); err != nil {
			s.Logger.Warn("routing: write decision", "error", err.Error(), "request_id", d.RequestID)
		}
	}()
}

// shadowRoute decides a shadow router's request in the background: what
// would have served, recorded without touching the request.
func (s *Server) shadowRoute(pr *pendingRoute, event *store.UsageEvent, body []byte) {
	if pr == nil || pr.mode == store.RouterModeLive || pr.st == nil || pr.extra == nil {
		return
	}
	requestID, userID, svcID := event.RequestID, event.UserID, event.ServiceTokenID
	rc := routing.Context{EstimatedTokens: int64(len(body) / 4)}
	record := func(res classify.Result, err error, elapsed time.Duration, calls int) {
		d := &store.RoutingDecision{RequestID: requestID, RouterID: pr.router.ID, Mode: store.RouterModeShadow,
			DecisionMs: int(elapsed.Milliseconds()), Calls: calls, UserID: userID, ServiceTokenID: svcID}
		var dec routing.Decision
		if err != nil {
			reason := store.RouteReasonError
			if errors.Is(err, context.DeadlineExceeded) {
				reason = store.RouteReasonTimeout
			}
			dec = routing.Default(pr.cfg, 0, reason, nil)
			d.Status, d.Error = "error", truncate(err.Error(), 300)
		} else {
			ans := routing.Interpret(res)
			dec = routing.Choose(pr.cfg, pr.cands, ans, pr.st.minConf, rc)
			d.Status, d.Category, d.Confidence, d.Difficulty = "ok", ans.Category, ans.Confidence, ans.Difficulty
		}
		d.RouteID, d.RouteName, d.ModelID, d.Thinking, d.Reason = dec.RouteID, routeLabel(pr.cfg, dec), dec.ModelID, dec.Thinking, dec.Reason
		if res, err := s.resolveRouteTarget(context.Background(), dec.ModelID); err == nil && res.Model != nil {
			d.ModelName = res.Model.PublicName()
		}
		s.persistRouteDecision(d)
	}
	if pr.extra.Ran {
		record(pr.extra.Result, pr.extra.Err, 0, 0)
		return
	}
	text, questions := classify.TrimText(pr.extra.Text, pr.st.settings.MaxChars), pr.extra.Questions
	budget := time.Duration(pr.cfg.BudgetMs) * time.Millisecond
	select {
	case s.routingRT().shadow <- func() {
		ctx, cancel := context.WithTimeout(classify.WithFeature(context.Background(), routingFeature, "async"), budget*4)
		defer cancel()
		started := time.Now()
		res, err := pr.st.classifier.Decide(ctx, text, questions)
		record(res, err, time.Since(started), max(1, res.Calls))
	}:
	default:
		// Queue full: shadow sampling is best effort.
	}
}

// --- evaluation sampling -------------------------------------------------------

// evalSample decides whether this routed request is compared against the
// default, and if so wraps the writer to keep a bounded copy of the answer.
// The prompt and answer stay in memory only and are dropped after judging.
func (s *Server) evalSample(w http.ResponseWriter, pr *pendingRoute, event *store.UsageEvent, body []byte) (http.ResponseWriter, *evalCapture) {
	if pr == nil || pr.mode != store.RouterModeLive || pr.decision.IsDefault() || pr.decision.Index < 0 || pr.decision.Index >= len(pr.cfg.Routes) {
		return w, nil
	}
	route := pr.cfg.Routes[pr.decision.Index]
	ev := route.Eval
	if !ev.Enabled || (!ev.Until.IsZero() && time.Now().After(ev.Until)) || ev.SamplePct <= 0 {
		return w, nil
	}
	if strings.TrimSuffix(event.EndpointPath, "/") != "/v1/chat/completions" || len(body) > 64<<10 || hasToolsOrImages(body) {
		return w, nil
	}
	if rand.Float64()*100 >= ev.SamplePct {
		return w, nil
	}
	if !s.routingRT().eval.admit() {
		return w, nil
	}
	// The default's replay starts from the caller's request, not the
	// route's rewrite of it (forced thinking, raised max_tokens, dropped
	// temperature), with the default's own thinking setting.
	callerBody := body
	if pr.callerBody != nil {
		callerBody = pr.callerBody
	}
	// Results are tallied per configured pairing (route model, default,
	// judge), so the ids recorded are the configured ones.
	// The routed answer already followed the route's instructions (they
	// are in the served body); the default's replay gets the router-wide
	// and default instructions, so the judge compares like with like.
	cw := &evalCapture{ResponseWriter: w, limit: 256 << 10, router: pr.router.ID, routeID: route.ID, routedModelID: route.ModelID,
		defaultModelID: pr.cfg.Default.ModelID, defaultThinking: routing.Thinking(pr.cfg.Default.Thinking, pr.answer.Difficulty),
		defaultInstructions: defaultInstructions(pr.cfg),
		eval:                ev, body: append([]byte(nil), callerBody...), started: time.Now()}
	return cw, cw
}

func hasToolsOrImages(body []byte) bool {
	var m struct {
		Tools    json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &m) != nil {
		return true
	}
	if len(m.Tools) > 0 && string(m.Tools) != "null" && string(m.Tools) != "[]" {
		return true
	}
	for _, msg := range m.Messages {
		if bytes.Contains(msg.Content, []byte(`"image_url"`)) || bytes.Contains(msg.Content, []byte(`"input_audio"`)) {
			return true
		}
	}
	return false
}

// evalCapture keeps a bounded copy of the routed answer.
type evalCapture struct {
	http.ResponseWriter
	buf             bytes.Buffer
	limit           int
	status          int
	router          string
	routeID         string
	routedModelID   string
	defaultModelID  string
	defaultThinking string
	// defaultInstructions is what the default adds to the system prompt.
	defaultInstructions string
	eval                store.RouteEval
	body                []byte
	started             time.Time
	// done is set once the slot has been handed on or released.
	done bool
	// overflowed reports that the answer was longer than the capture: a
	// cut-off answer would be judged unfairly, so it is not evaluated.
	overflowed bool
}

func (c *evalCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *evalCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	if len(p) > 0 && c.buf.Len() >= c.limit {
		c.overflowed = true
	}
	return c.ResponseWriter.Write(p)
}

func (c *evalCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *evalCapture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// answerText extracts the assistant's text from a captured chat response,
// buffered JSON or SSE.
func answerText(raw []byte) string {
	var full struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &full) == nil && len(full.Choices) > 0 {
		switch v := full.Choices[0].Message.Content.(type) {
		case string:
			return v
		case []any:
			var b strings.Builder
			for _, p := range v {
				if m, ok := p.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						b.WriteString(t)
					}
				}
			}
			return b.String()
		}
		return ""
	}
	var b strings.Builder
	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload, ok := sseDataPayload(line)
		if !ok {
			continue
		}
		var f struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &f) != nil {
			continue
		}
		for _, ch := range f.Choices {
			if ch.Delta.Content != nil {
				b.WriteString(*ch.Delta.Content)
			}
		}
	}
	return b.String()
}

// finishEvalSample hands a captured answer to the evaluation queue once the
// request has been served.
//
// It runs deferred, on every exit from the proxy handler: a request that
// ends early (refused, upstream unavailable, client gone) releases the
// queue slot evalSample reserved, or after enough of them evaluation would
// stop for good on the replica.
func (s *Server) finishEvalSample(c *evalCapture, event *store.UsageEvent) {
	if c == nil || c.done {
		return
	}
	c.done = true
	if c.status >= 300 || event.HTTPStatus >= 300 || c.overflowed {
		s.routingRT().eval.release()
		return
	}
	text := answerText(c.buf.Bytes())
	if strings.TrimSpace(text) == "" {
		s.routingRT().eval.release()
		return
	}
	s.routingRT().eval.enqueue(&evalJob{
		requestID: event.RequestID, routerID: c.router, routeID: c.routeID, routedModelID: c.routedModelID,
		defaultModelID: c.defaultModelID, defaultThinking: c.defaultThinking, defaultInstructions: c.defaultInstructions,
		cfg: c.eval, body: c.body, routedAnswer: text,
		routedCost: event.CostNano, routedLatency: time.Since(c.started),
		userID: event.UserID, serviceTokenID: event.ServiceTokenID, groupIDs: event.GroupIDs,
	})
}

// secgwSubjectFor builds the security subject for a served model reached
// through a router: the router's own bindings plus the served model's.
func secgwSubjectFor(event *store.UsageEvent, groupIDs []string, router *store.ManagedModel, resolved store.ResolvedModel) secgw.Subject {
	sub := secgwSubject(event, groupIDs, resolved)
	if resolved.Managed != nil && router != nil && resolved.Managed.ID == router.ID {
		sub.ManagedModelID = ""
	}
	if router != nil {
		sub.RouterID = router.ID
	}
	return sub
}

// routerSubject is the security subject before a route is chosen: the
// caller and the router only. Bindings on the eventual model or its
// upstream are added once the route is known (secgwSubjectFor).
func routerSubject(event *store.UsageEvent, groupIDs []string, router *store.ManagedModel) secgw.Subject {
	sub := secgwSubject(event, groupIDs, store.ResolvedModel{})
	sub.RouterID = router.ID
	return sub
}

// effectiveDiffers reports whether two resolutions apply different checks
// (another binding became relevant once the served model was known).
func effectiveDiffers(a, b *secgw.Effective) bool {
	if a.Empty() != b.Empty() {
		return true
	}
	if a.Empty() {
		return false
	}
	if len(a.Checks) != len(b.Checks) {
		return true
	}
	for k, ca := range a.Checks {
		cb, ok := b.Checks[k]
		if !ok || ca.BindingID != cb.BindingID || ca.PolicyID != cb.PolicyID {
			return true
		}
	}
	return false
}

// addedChecks is the part of b that a did not already run: checks b adds,
// or holds from a different binding or policy. The served model's second
// ingress pass runs only these.
func addedChecks(a, b *secgw.Effective) *secgw.Effective {
	if b.Empty() {
		return b
	}
	out := &secgw.Effective{Checks: map[store.SecgwCheckKind]secgw.ResolvedCheck{}, Policy: b.Policy}
	for k, cb := range b.Checks {
		if a != nil {
			if ca, ok := a.Checks[k]; ok && ca.BindingID == cb.BindingID && ca.PolicyID == cb.PolicyID {
				continue
			}
		}
		out.Checks[k] = cb
	}
	return out
}

// keep imported for the classifier interface assertion.
var _ routingClassifier = (*secgwclassify.Breaker)(nil)
