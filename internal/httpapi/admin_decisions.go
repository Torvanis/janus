package httpapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/decisions"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// decisionModelView is one decision model as the admin sees it: how Janus
// talks to it, how its calls are priced, and whether callers can use it.
type decisionModelView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	UpstreamID   string `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Status       string `json:"status"`
	// Dialect is the wire format in use; DialectSource says whether the
	// admin chose it or it was detected.
	Dialect       string `json:"dialect"`
	DialectSource string `json:"dialect_source"`
	WireModel     string `json:"wire_model"`
	Endpoint      string `json:"endpoint"`
	// Pricing.
	CostSource      string  `json:"cost_source"`
	RequestFeeUSD   float64 `json:"request_fee_usd"`
	RateInUSD       float64 `json:"usd_per_mtok_in"`
	RateOutUSD      float64 `json:"usd_per_mtok_out"`
	RateCachedUSD   float64 `json:"usd_per_mtok_cached"`
	ProviderReports bool    `json:"provider_reports_cost"`
	// Access.
	GrantCount        int      `json:"grant_count"`
	Guarding          []string `json:"guarding"`
	GrantWhenGuarding bool     `json:"grant_when_guarding"`
	Grantable         bool     `json:"grantable"`
	NotGrantable      string   `json:"not_grantable_reason,omitempty"`
}

func (s *Server) mountDecisionAdminRoutes(r chi.Router) {
	r.Get("/admin/decision-models", s.handleListDecisionModels)
	r.Put("/admin/decision-models/{id}/settings", s.handlePutDecisionSettings)
}

func (s *Server) handleListDecisionModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, err := s.Store.ListModels(ctx, store.ModelFilter{ClassifierOnly: true})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	settings, err := s.Store.ListDecisionSettings(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := []decisionModelView{}
	for _, m := range models {
		if m.ClassifierRole != store.ClassifierRoleDecision {
			continue
		}
		up, err := s.Store.UpstreamByID(ctx, m.UpstreamID)
		if err != nil {
			continue
		}
		prof, _ := s.Store.ClassifierProfileFor(ctx, m.UpstreamID, m.Name)
		v := decisionModelView{ID: m.ID, Name: m.PublicName(), UpstreamID: m.UpstreamID, UpstreamName: m.UpstreamName, Status: m.Status,
			Dialect: decisionDialect(prof, up.BaseURL), DialectSource: "detected", WireModel: prof.WireModel,
			RateInUSD: usage.USD(m.RateInNano), RateOutUSD: usage.USD(m.RateOutNano), RateCachedUSD: usage.USD(m.RateCachedNano),
			ProviderReports: adapterReportsCost(up.BaseURL), GrantCount: m.GrantCount}
		if prof.Dialect != "" {
			v.DialectSource = "admin"
		}
		if v.WireModel == "" {
			v.WireModel = decisions.WireModel(m.Name, up.BaseURL)
		}
		v.Endpoint = decisions.Endpoint(up.BaseURL, v.Dialect)
		set := settings[m.ID]
		v.CostSource, v.RequestFeeUSD, v.GrantWhenGuarding = set.CostSource, usage.USD(set.RequestFeeNano), set.GrantWhenGuarding
		if v.Guarding, err = s.Store.DecisionModelGuarding(ctx, m.ID); err != nil {
			WriteError(w, r, err)
			return
		}
		if v.Guarding == nil {
			v.Guarding = []string{}
		}
		ok, why, err := s.Store.DecisionGrantable(ctx, m)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		v.Grantable, v.NotGrantable = ok, why
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpstreamName != out[j].UpstreamName {
			return out[i].UpstreamName < out[j].UpstreamName
		}
		return out[i].Name < out[j].Name
	})
	WriteJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (s *Server) handlePutDecisionSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CostSource        string  `json:"cost_source"`
		RequestFeeUSD     float64 `json:"request_fee_usd"`
		GrantWhenGuarding bool    `json:"grant_when_guarding"`
	}
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	old, err := s.Store.DecisionSettingFor(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := store.DecisionSetting{ModelID: id, CostSource: in.CostSource, RequestFeeNano: usage.NanoFromUSD(in.RequestFeeUSD), GrantWhenGuarding: in.GrantWhenGuarding}
	if err := s.Store.SetDecisionSetting(r.Context(), next); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "decision_model.settings_updated", "model", id, old, next)
	WriteJSON(w, http.StatusOK, next)
}

// adapterReportsCost: providers that return the charge with each answer.
func adapterReportsCost(baseURL string) bool {
	return strings.Contains(baseURL, "openrouter.ai")
}
