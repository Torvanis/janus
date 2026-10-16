package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/classtrace"
	"github.com/torvanis/janus/internal/decisions"
	"github.com/torvanis/janus/internal/secgw"
	secgwclassify "github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Prompt classification wiring. A sampled request's text is labelled with a
// category from the admin's taxonomy, either before it is forwarded (inline:
// the label rides on the response and is available to future routing) or on
// a background queue (async: zero added latency). Either way a classifier
// failure never affects the request, and the prompt text is never stored.

// headerCategory reports the inline label to the caller.
const headerCategory = "X-Janus-Category"

// classificationState is the cached plan plus the settings it came from.
type classificationState struct {
	settings store.ClassificationSettings
	plan     *classify.Plan
	// problem explains why classification is on but not running (no
	// model, unknown upstream), shown on the admin page.
	problem string
}

// classifyRunners holds one async runner per Server (tests build many).
var classifyRunners sync.Map // *Server -> *classify.Runner

// classifyRunner returns this server's async runner, starting it once.
func (s *Server) classifyRunner() *classify.Runner {
	if v, ok := classifyRunners.Load(s); ok {
		return v.(*classify.Runner)
	}
	r := classify.NewRunner()
	r.Observe = func(o classify.Outcome) {
		if s.Metrics == nil {
			return
		}
		mode := o.Mode
		if mode == "" {
			mode = store.ClassificationModeAsync
		}
		s.Metrics.ClassificationsTotal.WithLabelValues(o.Status, mode).Inc()
		if o.Status != store.ClassificationStatusDropped {
			s.Metrics.ClassificationLatency.WithLabelValues(mode, o.Protocol).Observe(o.Timing.Elapsed.Seconds())
		}
	}
	actual, loaded := classifyRunners.LoadOrStore(s, r)
	if loaded {
		r.Close()
	}
	return actual.(*classify.Runner)
}

// classificationState loads (and caches) the effective configuration.
func (s *Server) classificationState(ctx context.Context) (*classificationState, error) {
	return cachedRead(&s.configCache, "classification", func() (*classificationState, error) {
		settings, err := s.Store.ClassificationSettings(ctx)
		if err != nil {
			return nil, err
		}
		st := &classificationState{settings: settings}
		if !settings.Enabled {
			return st, nil
		}
		plan, problem, err := s.buildClassificationPlan(ctx, settings)
		if err != nil {
			return nil, err
		}
		st.plan, st.problem = plan, problem
		if plan != nil {
			s.classifyRunner().Configure(settings.Concurrency, settings.QueueSize)
		}
		return st, nil
	})
}

// buildClassificationPlan turns settings into a runnable plan. A problem
// string (not an error) means the configuration cannot run as saved; the
// gateway skips classification and the admin page says why.
func (s *Server) buildClassificationPlan(ctx context.Context, settings store.ClassificationSettings) (*classify.Plan, string, error) {
	cats, err := s.Store.ListClassificationCategories(ctx)
	if err != nil {
		return nil, "", err
	}
	tax := classify.Taxonomy{Children: map[string][]classify.Option{}}
	enabledRoots := map[string]bool{}
	for _, c := range cats {
		if c.Enabled && c.ParentSlug == "" {
			tax.Roots = append(tax.Roots, classify.Option{Slug: c.Slug, Name: c.Name, Description: c.Description})
			enabledRoots[c.Slug] = true
		}
	}
	for _, c := range cats {
		if c.Enabled && c.ParentSlug != "" && enabledRoots[c.ParentSlug] {
			tax.Children[c.ParentSlug] = append(tax.Children[c.ParentSlug], classify.Option{Slug: c.Slug, Name: c.Name, Description: c.Description})
		}
	}
	if len(tax.Roots) < 2 {
		return nil, "Enable at least two categories.", nil
	}
	upstream, err := s.Store.UpstreamByID(ctx, settings.UpstreamID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "The classifier's provider no longer exists. Choose another provider.", nil
	}
	if err != nil {
		return nil, "", err
	}
	if !upstream.Enabled {
		return nil, "The classifier's provider " + upstream.Name + " is disabled.", nil
	}
	apiKey := ""
	if upstream.HasAPIKey {
		if apiKey, err = s.Cipher.Decrypt(upstream.EncryptedKey()); err != nil {
			return nil, "The classifier's provider credential cannot be decrypted.", nil
		}
	}
	plan := &classify.Plan{
		Taxonomy: tax, Mode: settings.Mode, SampleRate: settings.SampleRate,
		Timeout:   time.Duration(settings.TimeoutMs) * time.Millisecond,
		TextScope: settings.TextScope, IncludeSystem: settings.IncludeSystemPrompt, MaxChars: settings.MaxChars,
		Subcategories: settings.Subcategories,
		Classifier:    upstream.Name + " / " + settings.Model,
	}
	switch settings.Protocol {
	case store.ClassificationProtocolSystemOne:
		// The catalog row this wire name belongs to (an OpenAI
		// "<model>:decisions" row, OpenRouter's typesafe/jev-router, ...)
		// carries the admin's call profile and the price. Profiles are
		// saved under the catalog name, so look them up by it.
		m, err := s.decisionModelForWire(ctx, upstream, settings.Model)
		if err != nil {
			return nil, "", err
		}
		var prof store.ClassifierProfile
		if m != nil {
			prof, err = s.Store.ClassifierProfileFor(ctx, upstream.ID, m.Name)
		} else {
			prof, err = s.Store.ClassifierProfileForWire(ctx, upstream.ID, settings.Model)
		}
		if err != nil {
			return nil, "", err
		}
		style := prof.CallStyle
		if style == "" {
			style = classify.DefaultStyle(upstream.AdapterType, upstream.BaseURL)
		}
		maxOpts := prof.MaxOptions
		if maxOpts == 0 {
			maxOpts = classify.DefaultMaxOptions(upstream.AdapterType, upstream.BaseURL)
		}
		dialect := decisionDialect(prof, upstream.BaseURL)
		plan.Backend = &classify.SystemOne{URL: decisions.Endpoint(upstream.BaseURL, dialect), APIKey: apiKey, Model: settings.Model,
			Client: s.upstreamHTTPClient(ctx), Style: style, MaxOptions: maxOpts, Label: plan.Classifier, Dialect: dialect}
		// Price calls through the catalog row of this classifier, when
		// there is one (its rate card, cost source and per-call fee).
		if m != nil {
			plan.PriceNano = func(u classify.Usage, calls int) int64 {
				cost, _ := s.decisionCost(context.Background(), m, decisionCallFromUsage(u, calls))
				return cost
			}
		}
		if maxOpts > 0 && len(tax.Roots) > maxOpts {
			return nil, fmt.Sprintf("%d top-level categories are enabled but this classifier accepts at most %d choices in one question. Disable some, or raise the limit under Performance.", len(tax.Roots), maxOpts), nil
		}
		plan.Key, plan.Style, plan.MaxOptions = secgwclassify.DecisionKey(upstream.ID, settings.Model), style, maxOpts
	case store.ClassificationProtocolChat:
		provider, err := adapter.Get(upstream.AdapterType)
		if err != nil {
			return nil, "The classifier's provider type " + upstream.AdapterType + " is not supported.", nil
		}
		modelName := settings.Model
		model, err := s.Store.ModelByUpstreamAndName(ctx, upstream.ID, settings.Model)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
		up := adapter.Upstream{ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType}
		plan.Backend = &classify.Chat{Model: modelName, Send: s.classifierChatSender(provider, up, modelName)}
		if model != nil {
			plan.PriceNano = func(u classify.Usage, _ int) int64 {
				rates := usage.Rates{InNano: model.RateInNano, OutNano: model.RateOutNano, CachedNano: model.RateCachedNano}
				if card := s.cachedRateCard(context.Background(), model.ID); card.OK {
					rates = usage.Rates{InNano: card.In, OutNano: card.Out, CachedNano: card.Cached}
				}
				return usage.ComputeCost(u.TokensIn, u.TokensOut, 0, rates)
			}
		}
	default:
		return nil, "Unknown classifier protocol " + settings.Protocol + ".", nil
	}
	return plan, "", nil
}

// classifierChatSender sends a classification prompt through the upstream's
// own adapter, exactly as a proxied chat completion would be.
func (s *Server) classifierChatSender(provider adapter.Adapter, up adapter.Upstream, modelName string) classify.ChatSender {
	send := s.classifierChatCall(provider, up, modelName)
	return func(ctx context.Context, body []byte) ([]byte, classify.Usage, error) {
		// Reasoning models refuse max_tokens and a fixed temperature with
		// a 400 naming the parameter: fix it and resend (at most twice).
		for attempt := 0; ; attempt++ {
			out, u, refusal, err := send(ctx, body)
			if refusal != nil && attempt < 2 {
				if fixed, ok := adjustRefusedParam(body, refusal); ok {
					body = fixed
					continue
				}
			}
			return out, u, err
		}
	}
}

// classifierChatCall sends one chat classification. refusal is the body of
// a 400 response, so the caller can fix a refused parameter and resend.
func (s *Server) classifierChatCall(provider adapter.Adapter, up adapter.Upstream, modelName string) func(ctx context.Context, body []byte) ([]byte, classify.Usage, []byte, error) {
	return func(ctx context.Context, body []byte) ([]byte, classify.Usage, []byte, error) {
		req := &adapter.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: http.Header{"Content-Type": {"application/json"}},
			Body: body, Model: modelName, Context: ctx, Client: s.upstreamHTTPClient(ctx)}
		prepared, err := provider.Prepare(up, req)
		if err != nil {
			return nil, classify.Usage{}, nil, fmt.Errorf("prepare: %w", err)
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, bytes.NewReader(prepared.Body))
		if err != nil {
			return nil, classify.Usage{}, nil, err
		}
		hreq.Header = prepared.Header
		if hreq.Header == nil {
			hreq.Header = http.Header{}
		}
		if hreq.Header.Get("Content-Type") == "" {
			hreq.Header.Set("Content-Type", "application/json")
		}
		resp, err := s.upstreamHTTPClient(ctx).Do(hreq)
		if err != nil {
			return nil, classify.Usage{}, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, classify.Usage{}, nil, err
		}
		if resp.StatusCode/100 != 2 {
			var refusal []byte
			if resp.StatusCode == http.StatusBadRequest {
				refusal = raw
			}
			return nil, classify.Usage{}, refusal, fmt.Errorf("HTTP %d from %s", resp.StatusCode, up.Name)
		}
		u, _ := provider.ExtractUsage(raw)
		out, err := provider.TransformResponse(req, raw)
		if err != nil {
			return nil, classify.Usage{}, nil, fmt.Errorf("translate response: %w", err)
		}
		return out, classify.Usage{TokensIn: u.TokensIn, TokensOut: u.TokensOut}, nil, nil
	}
}

// classificationEligible reports whether a request should be considered:
// a text-generation endpoint with a body Janus could read in full.
func classificationEligible(path string, overflow bool) bool {
	if overflow {
		return false
	}
	switch strings.TrimSuffix(path, "/") {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages":
		return true
	}
	return false
}

// pendingClassification is a sampled request's classification, prepared
// before the Security Gateway runs so its question can share a guard call.
type pendingClassification struct {
	st    *classificationState
	text  string
	job   *classify.Job
	extra *secgw.Extra
}

// prepareClassification decides whether the request will be classified and
// builds the job. It makes no classifier call.
func (s *Server) prepareClassification(ctx context.Context, event *store.UsageEvent, body []byte, overflow bool) *pendingClassification {
	if !classificationEligible(event.EndpointPath, overflow) {
		return nil
	}
	st, err := s.classificationState(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "classification: settings unavailable", "error", err.Error())
		return nil
	}
	if st.plan == nil || !st.settings.Enabled {
		return nil
	}
	if event.ServiceTokenID != "" && !st.settings.IncludeServiceTokens {
		return nil
	}
	if !st.plan.Sampled() {
		return nil
	}
	text := classify.ExtractText(body, st.plan.TextScope, st.plan.IncludeSystem)
	if text == "" {
		return nil
	}
	base := store.ClassificationResult{RequestID: event.RequestID, UserID: event.UserID, ServiceTokenID: event.ServiceTokenID,
		TeamIDs: event.TeamIDs, ModelName: classifiedModelName(event)}
	job := &classify.Job{Plan: st.plan, Text: text, Done: func(o classify.Outcome) { s.persistClassification(base, o) }, Trace: classtrace.From(ctx)}
	pc := &pendingClassification{st: st, text: text, job: job}
	// A decision classifier that takes combined questions answers this
	// question alongside the guards' at no extra wait, so offer it to
	// them. It rides along only if a guard calls the same classifier
	// about the same text (the latest user message).
	// Only when that one question is the whole classification: a
	// two-stage plan (subcategories on, taxonomy above the classifier's
	// option limit) would get its category on the guard call and never be
	// asked for the subcategory.
	oneQuestion := !st.plan.Subcategories || st.plan.Flat()
	if st.plan.Key != "" && st.plan.Style == classify.StyleCombined && st.plan.TextScope == classify.ScopeLastUser && !st.plan.IncludeSystem && oneQuestion {
		pc.extra = &secgw.Extra{ClassifierKey: st.plan.Key, Text: classify.TrimText(text, st.plan.MaxChars), Questions: st.plan.Questions(), Feature: "usage_insights"}
	}
	return pc
}

// prepareClassificationUnsampled re-extracts the classified text from the
// body that will be forwarded (after any Security Gateway redaction),
// keeping the earlier sampling decision. nil when nothing is left to
// classify.
func (s *Server) prepareClassificationUnsampled(_ context.Context, pc *pendingClassification, body []byte) *pendingClassification {
	text := classify.ExtractText(body, pc.st.plan.TextScope, pc.st.plan.IncludeSystem)
	if text == "" {
		return nil
	}
	out := *pc
	job := *pc.job
	job.Text = text
	out.job, out.text, out.extra = &job, text, nil
	return &out
}

// classificationStats is what classification cost the request itself.
type classificationStats struct {
	waited time.Duration
	calls  int
	label  string
}

// finishClassification runs after admission and before the request is
// forwarded. If the question already rode on a guard call it records that
// answer; otherwise inline mode classifies now (waiting at most the
// configured timeout) and async mode queues the job and returns at once.
// It never fails the request.
func (s *Server) finishClassification(ctx context.Context, w http.ResponseWriter, pc *pendingClassification) classificationStats {
	var cs classificationStats
	if pc == nil {
		return cs
	}
	// The shared call itself failed (timeout, open breaker, HTTP error):
	// classify on our own below, as if nothing had ridden along.
	sharedCallFailed := pc.extra != nil && pc.extra.Ran && pc.extra.Err != nil && pc.extra.Result.Answers == nil
	if pc.extra != nil && pc.extra.Ran && !sharedCallFailed {
		// Asked on the guard call: no extra wait or request of its own
		// (the call's time is already in the guards' share of
		// classifier_ms). A bad answer is recorded, not re-asked.
		o := classify.FromResult(pc.st.plan, pc.extra.Result, pc.extra.Err, pc.extra.Elapsed, len([]rune(pc.extra.Text)))
		// Answered before forwarding, whatever the configured mode: the
		// label was produced inline (at no cost of its own).
		o.Mode = store.ClassificationModeInline
		s.classifyRunner().Finish(pc.job, o)
		if o.Status == store.ClassificationStatusOK {
			cs.label = o.Label()
			// The header is the inline mode's contract; background mode
			// adds nothing to the response even when the label came free.
			if pc.st.plan.Mode == store.ClassificationModeInline {
				w.Header().Set(headerCategory, o.Label())
			}
		}
		return cs
	}
	if pc.st.plan.Mode != store.ClassificationModeInline {
		s.classifyRunner().Enqueue(pc.job)
		s.observeClassificationQueue()
		return cs
	}
	// Inline: detach from the client's cancellation only up to the plan's
	// own timeout; the request waits at most that long.
	started := time.Now()
	outcome, ok, ran := s.classifyRunner().Inline(context.WithoutCancel(ctx), pc.job, pc.st.settings.Concurrency)
	if ran {
		// Counted whether or not a label came back: a timeout is exactly
		// the wait the request log must show.
		cs.waited, cs.calls = time.Since(started), outcome.Calls
	}
	if ok {
		w.Header().Set(headerCategory, outcome.Label())
		cs.label = outcome.Label()
	}
	return cs
}

func (s *Server) observeClassificationQueue() {
	if s.Metrics != nil {
		s.Metrics.ClassificationQueue.Set(float64(s.classifyRunner().Pending()))
	}
}

// classifiedModelName is the name the caller asked for (the managed alias
// when there is one), so reports read in the terms people use.
func classifiedModelName(event *store.UsageEvent) string {
	if event.RequestedModelName != "" {
		return event.RequestedModelName
	}
	return event.ModelName
}

// persistClassification writes one result on a goroutine tracked so
// shutdown drains it. Dropped samples share a small fixed set of writers.
func (s *Server) persistClassification(base store.ClassificationResult, o classify.Outcome) {
	r := base
	r.Category, r.Subcategory, r.Confidence, r.Probabilities = o.Category, o.Subcategory, o.Confidence, o.Probabilities
	r.Status, r.Error, r.Mode, r.Protocol, r.Classifier = o.Status, o.Err, o.Mode, o.Protocol, o.Classifier
	r.LatencyMs, r.QueueMs, r.InputChars = int(o.Timing.Elapsed.Milliseconds()), int(o.Timing.Queue.Milliseconds()), o.InputChars
	r.TokensIn, r.TokensOut, r.CostNano, r.Calls = o.Usage.TokensIn, o.Usage.TokensOut, o.CostNano, o.Calls
	if s.Config != nil && s.Config.LocalOnly {
		r.CostNano = 0
	}
	if r.Status == store.ClassificationStatusDropped {
		// Drops happen when the classifier cannot keep up: never let them
		// pile up goroutines and writes on the database as well. A drop
		// that finds the writers busy is counted in metrics only.
		select {
		case dropWriters <- struct{}{}:
		default:
			return
		}
		s.pending.Add(1)
		go func() {
			defer func() { <-dropWriters }()
			defer s.pending.Done()
			s.writeClassification(&r)
		}()
		return
	}
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.writeClassification(&r)
	}()
}

// dropWriters bounds the concurrent writes of dropped-sample rows.
var dropWriters = make(chan struct{}, 4)

func (s *Server) writeClassification(r *store.ClassificationResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Store.InsertClassificationResult(ctx, r); err != nil {
		s.Logger.Warn("classification: write result", "error", err.Error(), "request_id", r.RequestID)
	}
	s.observeClassificationQueue()
}
