package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
	"github.com/torvanis/janus/internal/usage"
)

// Capabilities of personal-plan models (my/<provider>/<model>).
//
// Each connection holds its own capability record per model (store
// SubscriptionConnection.Capabilities), so what one person measured or set
// never reaches anyone else's plan: plans differ by account tier. The value
// in force for a key is, strongest first:
//
//  1. what the owner set ("Set by you", the record's admin layer);
//  2. what the owner's Measure found (the probe layer), sent through their
//     own connection and counted against their plan;
//  3. what the vendor's model list reports (the provider layer, refreshed
//     on connect, Refresh models and the background check);
//  4. the built-in default for that vendor's plans
//     (subscription.PlanDefaults, shipped in code, never stored).
//
// Chat reads the result exactly as it reads an organization model's record:
// unknown means "do not send this directly".

// planMeasureEvery is how often one model of one connection may be
// measured: a measurement spends up to capability.MaxProbeRequests requests
// of the person's plan.
const planMeasureEvery = 10 * time.Minute

// storeReportedCapabilities records what a fresh vendor catalog said about
// each model. Failures only log: the catalog refresh itself succeeded.
func (s *Server) storeReportedCapabilities(ctx context.Context, connID string, models []string, reported map[string]adapter.Capabilities) {
	facts := map[string]capability.Facts{}
	now := time.Now().UTC()
	for _, m := range models {
		if c, ok := reported[m]; ok {
			if f := capability.FromProvider(c, now); len(f) > 0 {
				facts[m] = f
			}
		}
	}
	if err := s.Store.SetSubscriptionReportedCapabilities(ctx, connID, facts); err != nil {
		s.Logger.WarnContext(ctx, "store plan capabilities", "subscription_id", connID, "error", err.Error())
	}
}

// planCapabilityProfile is what is known about one of the caller's plan
// models (my/<provider>/<model>), defaults included. A model the caller has
// no connection for is an empty profile.
func (s *Server) planCapabilityProfile(ctx context.Context, user *store.User, modelName string) capability.Profile {
	providerID, native, ok := subscription.ParseModel(modelName)
	if !ok || user == nil {
		return capability.Profile{}
	}
	conn, err := s.Store.UserSubscription(ctx, user.ID, providerID)
	if err != nil {
		return capability.Profile{}
	}
	return conn.Capabilities[native].ProfileWith(subscription.PlanDefaults(providerID, native))
}

// planCapabilityView is one plan model's capabilities as the owner sees
// them: the value in force with its source, and each layer.
type planCapabilityView struct {
	Model    string                         `json:"model"`
	Name     string                         `json:"name"`
	Selected bool                           `json:"selected"`
	Resolved map[string]capability.Resolved `json:"resolved"`
	Defaults capability.Facts               `json:"defaults"`
	Reported capability.Facts               `json:"reported"`
	Measured capability.Facts               `json:"measured"`
	Set      capability.Facts               `json:"set"`
	Probed   *capability.ProbeRun           `json:"probed,omitempty"`
	// MeasureAfter is when Measure may be pressed again (zero = now).
	MeasureAfter time.Time `json:"measure_after,omitzero"`
	// NotMeasurable says why this model cannot be measured ("" = it can).
	NotMeasurable string `json:"not_measurable,omitempty"`
}

func planCapabilityViewOf(conn *store.SubscriptionConnection, native string, now time.Time) planCapabilityView {
	state := conn.Capabilities[native]
	defaults := subscription.PlanDefaults(conn.Provider, native)
	v := planCapabilityView{
		Model: native, Name: subscription.ModelName(conn.Provider, native),
		Selected: slices.Contains(conn.SelectedModels, native),
		Resolved: state.ResolveWith(defaults), Defaults: defaults,
		Reported: emptyFacts(state.Provider), Measured: emptyFacts(state.Probe), Set: emptyFacts(state.Admin),
		Probed: state.Probed, NotMeasurable: planNotMeasurable(native),
	}
	if p := state.Probed; p != nil {
		if next := p.At.Add(planMeasureEvery); next.After(now) {
			v.MeasureAfter = next
		}
	}
	return v
}

// planNotMeasurable says why a plan model is not measured: only chat
// models are (a probe of an image or video model would make media on the
// person's plan).
func planNotMeasurable(native string) string {
	if mods := adapter.GuessModalities(native); len(mods) > 0 && !slices.Contains(mods, adapter.ModalityChat) {
		return "only chat models are measured"
	}
	return ""
}

// ownedPlanConnection loads the caller's connection for a capabilities
// route, refusing while the feature or the provider is off.
func (s *Server) ownedPlanConnection(w http.ResponseWriter, r *http.Request) (*store.SubscriptionConnection, subscription.Provider, bool) {
	if !s.requireSubscriptions(w, r) {
		return nil, nil, false
	}
	conn, ok := s.ownedSubscription(w, r)
	if !ok {
		return nil, nil, false
	}
	provider, found := subscription.Get(conn.Provider)
	if !found {
		WriteError(w, r, ErrNotFoundf("That subscription provider"))
		return nil, nil, false
	}
	enabled, err := s.enabledSubscriptionProviders(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return nil, nil, false
	}
	if !enabled[conn.Provider] {
		WriteError(w, r, ErrForbidden("Personal "+provider.DisplayName()+" subscriptions are turned off for this organization."))
		return nil, nil, false
	}
	return conn, provider, true
}

// handleGetPlanCapabilities serves GET /api/v1/me/subscriptions/{id}/capabilities:
// every model the plan offers, with what is known about it and from where.
func (s *Server) handleGetPlanCapabilities(w http.ResponseWriter, r *http.Request) {
	conn, provider, ok := s.ownedPlanConnection(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, s.planCapabilitiesBody(conn, provider))
}

func (s *Server) planCapabilitiesBody(conn *store.SubscriptionConnection, provider subscription.Provider) map[string]any {
	now := time.Now().UTC()
	models := make([]planCapabilityView, 0, len(conn.Models))
	for _, m := range conn.Models {
		models = append(models, planCapabilityViewOf(conn, m, now))
	}
	return map[string]any{
		"subscription_id": conn.ID, "provider": conn.Provider, "provider_name": provider.DisplayName(),
		"keys": adapter.CapabilityKeys(), "max_requests": capability.MaxProbeRequests,
		"measure_every_seconds": int(planMeasureEvery / time.Second), "models": models,
	}
}

// planModelParam reads {"model": ...} and checks the plan offers it.
func planModelParam(w http.ResponseWriter, r *http.Request, conn *store.SubscriptionConnection, model string) bool {
	if strings.TrimSpace(model) == "" {
		WriteError(w, r, ErrInvalidRequest("Say which model (model).").WithParam("model"))
		return false
	}
	if !slices.Contains(conn.Models, model) {
		WriteError(w, r, ErrInvalidRequest("Your plan does not offer "+model+". Refresh the model list and try again.").WithParam("model"))
		return false
	}
	return true
}

// handleSetPlanCapabilities serves PATCH /api/v1/me/subscriptions/{id}/capabilities:
// {"model": "gpt-6.1-sol", "set": {"input.image": true, "input.audio": null}}.
// A value is the owner's own setting and beats everything else; null
// returns the key to its automatic value (measured, reported or default).
func (s *Server) handleSetPlanCapabilities(w http.ResponseWriter, r *http.Request) {
	conn, provider, ok := s.ownedPlanConnection(w, r)
	if !ok {
		return
	}
	var body struct {
		Model string         `json:"model"`
		Set   map[string]any `json:"set"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if !planModelParam(w, r, conn, body.Model) {
		return
	}
	if len(body.Set) == 0 {
		WriteError(w, r, ErrInvalidRequest("set is required: capability keys with a value, or null to use the automatic value.").WithParam("set"))
		return
	}
	var invalid error
	before := planSetValues(conn.Capabilities[body.Model])
	updated, err := s.Store.UpdateSubscriptionCapabilities(r.Context(), conn.ID, func(rec capability.Record) (bool, error) {
		state := rec[body.Model]
		if invalid = state.SetAdmin(body.Set, time.Now().UTC()); invalid != nil {
			return false, nil
		}
		rec[body.Model] = state
		return true, nil
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if invalid != nil {
		WriteError(w, r, ErrInvalidRequest(invalid.Error()).WithParam("set"))
		return
	}
	s.audit(r, "subscription_capabilities_set", "subscription", conn.ID,
		map[string]any{"provider": provider.ID(), "model": body.Model, "set": before},
		map[string]any{"provider": provider.ID(), "model": body.Model, "set": planSetValues(updated.Capabilities[body.Model])})
	WriteJSON(w, http.StatusOK, planCapabilityViewOf(updated, body.Model, time.Now().UTC()))
}

func planSetValues(state capability.State) map[string]any {
	out := map[string]any{}
	for k, f := range state.Admin {
		out[k] = f.Value
	}
	return out
}

// handleMeasurePlanCapabilities serves POST
// /api/v1/me/subscriptions/{id}/capabilities/measure: {"model": ...}. It
// measures the model through the owner's own connection in the background
// (up to capability.MaxProbeRequests small requests on their plan) and
// answers at once with the model's view, its probed status "running". One
// model of one connection is measured at most once per planMeasureEvery.
func (s *Server) handleMeasurePlanCapabilities(w http.ResponseWriter, r *http.Request) {
	conn, provider, ok := s.ownedPlanConnection(w, r)
	if !ok {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if !planModelParam(w, r, conn, body.Model) {
		return
	}
	if conn.Status != store.SubscriptionActive {
		WriteError(w, r, subscriptionReauthError(provider, conn.LastError))
		return
	}
	if reason := planNotMeasurable(body.Model); reason != "" {
		WriteError(w, r, ErrInvalidRequest("This model cannot be measured: "+reason+".").WithParam("model"))
		return
	}
	updated, started, wait, err := s.startPlanMeasure(r.Context(), conn.ID, body.Model, "manual")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !started {
		msg := "This model is being measured now."
		if wait > 0 {
			msg = fmt.Sprintf("This model was measured recently. Try again in %s.", planWaitText(wait))
		}
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		WriteError(w, r, ErrRateLimited(msg))
		return
	}
	s.audit(r, "subscription_capabilities_measured", "subscription", conn.ID, nil,
		map[string]any{"provider": provider.ID(), "model": body.Model})
	WriteJSON(w, http.StatusAccepted, planCapabilityViewOf(updated, body.Model, time.Now().UTC()))
}

func planWaitText(d time.Duration) string {
	if m := int(d.Round(time.Minute) / time.Minute); m > 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "a minute"
}

// startPlanMeasure claims a measurement of one plan model and runs it in
// the background. started is false when one is running or the last began
// less than planMeasureEvery ago; wait is then how long until the next may
// start (0 while one is running).
func (s *Server) startPlanMeasure(ctx context.Context, connID, model, by string) (*store.SubscriptionConnection, bool, time.Duration, error) {
	now := time.Now().UTC()
	claimed := false
	var wait time.Duration
	updated, err := s.Store.UpdateSubscriptionCapabilities(ctx, connID, func(rec capability.Record) (bool, error) {
		state := rec[model]
		if p := state.Probed; p != nil && p.Status != capability.ProbeRunning && p.Status != capability.ProbeQueued {
			if next := p.At.Add(planMeasureEvery); next.After(now) {
				wait = next.Sub(now)
				return false, nil
			}
		}
		if claimed = state.Claim(by, now); !claimed {
			return false, nil
		}
		rec[model] = state
		return true, nil
	})
	if err != nil || !claimed {
		return updated, false, wait, err
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("plan capability measure panicked", "panic", p, "subscription_id", connID)
			}
		}()
		bg, cancel := context.WithTimeout(context.Background(), capabilityProbeTimeout)
		defer cancel()
		s.runPlanMeasure(bg, connID, model, by)
	}()
	return updated, true, 0, nil
}

// runPlanMeasure measures one plan model whose claim the caller holds, and
// stores the outcome. It always ends the claim.
func (s *Server) runPlanMeasure(ctx context.Context, connID, model, by string) {
	run := capability.ProbeRun{At: time.Now().UTC(), By: by, Status: capability.ProbeFailed}
	var facts capability.Facts
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := s.Store.UpdateSubscriptionCapabilities(ctx, connID, func(rec capability.Record) (bool, error) {
			state := rec[model]
			state.Finish(facts, run)
			rec[model] = state
			return true, nil
		}); err != nil {
			s.Logger.ErrorContext(ctx, "plan capability measure: store result", "error", err.Error(), "subscription_id", connID)
		}
		s.Logger.InfoContext(ctx, "plan capability measure finished", "subscription_id", connID, "model", model, "by", by,
			"status", run.Status, "detail", run.Detail, "requests", run.Requests, "facts", len(facts))
	}()
	conn, err := s.Store.SubscriptionByID(ctx, connID)
	if err != nil {
		run.Detail = "the subscription could not be read"
		return
	}
	provider, ok := subscription.Get(conn.Provider)
	if !ok || conn.Status != store.SubscriptionActive {
		run.Detail = "the subscription needs to be reconnected"
		return
	}
	route := &personalRoute{provider: provider, conn: conn}
	token, apiErr := s.subscriptionAccessToken(ctx, route)
	if apiErr != nil {
		run.Detail = "the plan's sign-in could not be used: " + apiErr.Message
		return
	}
	wire, err := adapter.Get(provider.AdapterType())
	if err != nil {
		run.Detail = "the plan's route is not known"
		return
	}
	up := adapter.Upstream{ID: "subscription:" + provider.ID(), Name: provider.DisplayName(), BaseURL: route.baseURL(),
		APIKey: token, AdapterType: provider.AdapterType()}
	requestID := "capprobe-" + store.NewID()
	call := func(ctx context.Context, body map[string]any) (capability.Reply, error) {
		return s.planProbeCall(ctx, conn, provider, model, up, wire, body, requestID)
	}
	// What the vendor reported and the owner set are not measured again;
	// built-in defaults are assumptions, so Measure checks them.
	known := conn.Capabilities[model].WithoutProbe().Profile()
	facts, run = capability.RunProbe(ctx, call, capability.ProbeInput{Known: known, AdapterType: provider.AdapterType(), By: by})
}

// planProbeCall sends one probe request through the plan's own route,
// exactly as written (nothing fitted, nothing retried: a refusal is the
// measurement), and records it on the owner's plan like any personal-plan
// request: zero cost to the organization, attributed to the connection,
// and marked as Janus's own call so it stays out of request logs and Chat
// totals.
func (s *Server) planProbeCall(ctx context.Context, conn *store.SubscriptionConnection, provider subscription.Provider, model string,
	up adapter.Upstream, wire adapter.Adapter, body map[string]any, requestID string) (capability.Reply, error) {
	body["model"] = model
	raw, err := json.Marshal(body)
	if err != nil {
		return capability.Reply{}, err
	}
	client := s.ownPersonalClient(ctx, provider)
	req := &adapter.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: http.Header{"Content-Type": {"application/json"}},
		Body: raw, Model: model, Context: ctx, Client: client}
	prepared, err := wire.Prepare(up, req)
	if err != nil {
		// Janus's own route refused the shape: an answer about the route.
		return capability.Reply{Status: http.StatusBadRequest, Error: err.Error(), Local: true}, nil
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, bytes.NewReader(prepared.Body))
	if err != nil {
		return capability.Reply{}, err
	}
	hreq.Header = prepared.Header
	event := &store.UsageEvent{UserID: conn.UserID, SubscriptionID: conn.ID, ModelID: "subscription:" + provider.ID() + ":" + model,
		ModelName: subscription.ModelName(provider.ID(), model), UpstreamID: up.ID, EndpointPath: "/internal/capability/probe",
		HTTPMethod: http.MethodPost, Modality: "chat", RequestBytes: int64(len(raw)), ClientApp: store.InternalProbeClientApp,
		RequestID: requestID, CostStatus: "known_free"}
	started := time.Now()
	defer func() {
		event.LatencyMs = int(time.Since(started).Milliseconds())
		event.UpstreamMs = event.LatencyMs
		event.CostNano = 0
		s.writeUsage(ctx, event, "plan capability measure: write usage event", nil)
	}()
	resp, err := client.Do(hreq)
	if err != nil {
		event.HTTPStatus, event.ErrorCode, event.AccountingMode = http.StatusBadGateway, CodeUpstreamDown, usage.AccountingNone
		return capability.Reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	event.HTTPStatus, event.ResponseBytes = resp.StatusCode, int64(len(answer))
	if err != nil {
		event.AccountingMode = usage.AccountingNone
		return capability.Reply{}, err
	}
	reply := capability.Reply{Status: resp.StatusCode}
	if resp.StatusCode/100 != 2 {
		event.AccountingMode = usage.AccountingNone
		if resp.StatusCode == http.StatusUnauthorized {
			s.notePersonalResponse(ctx, &personalRoute{provider: provider, conn: conn}, resp.StatusCode)
		}
		reply.Error, reply.Param = upstreamErrorText(answer), upstreamErrorParam(answer)
		if reply.Error == "" {
			reply.Error = truncate(strings.Join(strings.Fields(string(answer)), " "), 200)
		}
		return reply, nil
	}
	out, err := wire.TransformResponse(req, answer)
	if err != nil {
		return capability.Reply{}, fmt.Errorf("read the model's answer: %w", err)
	}
	u, reported := wire.ExtractUsage(answer)
	if !reported && u.TokensIn == 0 && u.TokensOut == 0 {
		// Stream-only plan routes (the Codex backend) report usage in the
		// folded answer, not the raw stream.
		u, _ = wire.ExtractUsage(out)
	}
	s.applyUsage(event, u, usage.Rates{}, -1)
	reply.InputTokens, reply.OutputTokens = event.TokensIn, event.TokensOut
	readProbeAnswer(out, &reply)
	if reply.ReasoningTokens == 0 {
		var native capability.Reply
		readProbeAnswer(answer, &native)
		reply.ReasoningTokens = native.ReasoningTokens
	}
	return reply, nil
}

// measurePlanSelection measures newly selected models of providers listed
// in subscription.PlanAutoMeasure (none by default: see there).
func (s *Server) measurePlanSelection(ctx context.Context, conn *store.SubscriptionConnection, before, after []string) {
	if !subscription.PlanAutoMeasure[conn.Provider] {
		return
	}
	for _, m := range after {
		if slices.Contains(before, m) || planNotMeasurable(m) != "" {
			continue
		}
		if _, _, _, err := s.startPlanMeasure(context.WithoutCancel(ctx), conn.ID, m, "selected"); err != nil {
			s.Logger.WarnContext(ctx, "plan capability measure on select: could not start", "error", err.Error(), "subscription_id", conn.ID)
		}
	}
}
