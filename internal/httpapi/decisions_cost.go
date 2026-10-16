package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/decisions"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// decisionCall is what one decision request consumed, from either dialect.
type decisionCall struct {
	TokensIn, TokensOut, TokensCached int64
	CostUSD                           float64
	CostReported                      bool
	// Requests is how many requests the call took (the per-request fee
	// applies to each); 0 counts as one.
	Requests int
}

// decisionCost prices one decision call by the model's cost source:
//
//   - automatic: the provider's reported charge (OpenRouter's usage.cost)
//     when present, else the rate card;
//   - provider: only the reported charge;
//   - card: always the rate card, even when the provider reports a charge;
//   - none: free.
//
// The per-request fee is added for every source except none. The returned
// accounting mode says where the figure came from.
func (s *Server) decisionCost(ctx context.Context, model *store.Model, c decisionCall) (int64, string) {
	if s.Config != nil && s.Config.LocalOnly {
		return 0, usage.AccountingUpstream
	}
	set, _ := s.cachedDecisionSetting(ctx, model.ID)
	if set.CostSource == store.CostSourceNone {
		return 0, usage.AccountingUpstream
	}
	var cost int64
	mode := usage.AccountingUpstream
	switch {
	case c.CostReported && set.CostSource != store.CostSourceCard:
		cost, mode = int64(c.CostUSD*1e9+0.5), usage.AccountingUpstreamCost
	case set.CostSource == store.CostSourceProvider:
		cost = 0
	default:
		rates := usage.Rates{InNano: model.RateInNano, OutNano: model.RateOutNano, CachedNano: model.RateCachedNano}
		if card := s.cachedRateCard(ctx, model.ID); card.OK {
			rates = usage.Rates{InNano: card.In, OutNano: card.Out, CachedNano: card.Cached}
		}
		cost = usage.ComputeCostAll(usage.TokenCounts{In: c.TokensIn, Out: c.TokensOut, Cached: c.TokensCached}, rates)
	}
	return cost + set.RequestFeeNano*int64(max(1, c.Requests)), mode
}

// decisionCallFromUsage is the decisionCall of a classifier result, so
// Janus's own decision calls are priced from the same inputs as a served
// decision (cached tokens, an explicit reported $0).
func decisionCallFromUsage(u classify.Usage, calls int) decisionCall {
	return decisionCall{TokensIn: u.TokensIn, TokensOut: u.TokensOut, TokensCached: u.TokensCached,
		CostUSD: u.CostUSD, CostReported: u.CostReported, Requests: calls}
}

// decisionCostStatus is the reporting cost status of a priced decision
// call. A zero cost is known_free only when it is known to be free: the
// model is set to cost nothing, the provider reported the price (or is the
// only price source the admin chose), or the model has a saved rate card.
// A model nobody ever priced is unpriced, as on the chat path.
func (s *Server) decisionCostStatus(ctx context.Context, model *store.Model, c decisionCall, cost int64) string {
	switch {
	case s.Config != nil && s.Config.LocalOnly:
		return "disabled"
	case cost > 0:
		return "priced"
	}
	set, _ := s.cachedDecisionSetting(ctx, model.ID)
	switch {
	case set.CostSource == store.CostSourceNone, set.CostSource == store.CostSourceProvider:
		return "known_free"
	case c.CostReported && set.CostSource != store.CostSourceCard:
		return "known_free"
	case !model.RateEffectiveFrom.IsZero():
		return "known_free"
	}
	return "unpriced"
}

func (s *Server) cachedDecisionSetting(ctx context.Context, modelID string) (store.DecisionSetting, error) {
	return cachedModelRead(&s.configCache, s.Store.ModelRevision(), "decisionsetting|"+modelID, func() (store.DecisionSetting, error) {
		return s.Store.DecisionSettingFor(ctx, modelID)
	})
}

// decisionDialect is the wire format a decision model's provider speaks.
func decisionDialect(prof store.ClassifierProfile, baseURL string) string {
	if prof.Dialect != "" {
		return prof.Dialect
	}
	return decisions.DefaultDialect(baseURL)
}

// meterDecisionCall writes the usage event of one decision call Janus made
// for itself (guards, Auto-routing), priced like a served decision.
func (s *Server) meterDecisionCall(model *store.Model, upstream *store.Upstream) func(context.Context, time.Duration, int, classify.Result, error) {
	return func(ctx context.Context, elapsed time.Duration, inputChars int, res classify.Result, callErr error) {
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
		} else {
			event.TokensIn, event.TokensOut, event.TokensCached = res.Usage.TokensIn, res.Usage.TokensOut, res.Usage.TokensCached
			event.CostNano, event.AccountingMode = s.decisionCost(ctx, model, decisionCallFromUsage(res.Usage, res.Calls))
		}
		s.writeUsage(ctx, event, "secgw: write classifier usage event", nil)
	}
}

// decisionModelForWire finds the decision catalog row a provider serves
// under wire name (catalog name, its :decisions row, or a profile's wire
// model). nil when the classifier has no catalog row.
func (s *Server) decisionModelForWire(ctx context.Context, upstream *store.Upstream, wire string) (*store.Model, error) {
	models, err := s.Store.ListModels(ctx, store.ModelFilter{UpstreamID: upstream.ID, ClassifierOnly: true})
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.ClassifierRole != store.ClassifierRoleDecision {
			continue
		}
		if m.Name == wire || decisions.WireModel(m.Name, upstream.BaseURL) == wire {
			return m, nil
		}
		if prof, perr := s.Store.ClassifierProfileFor(ctx, upstream.ID, m.Name); perr == nil && prof.WireModel == wire {
			return m, nil
		}
	}
	return nil, nil
}
