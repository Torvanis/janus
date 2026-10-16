package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/routing"
	"github.com/torvanis/janus/internal/store"
)

// Admin API for auto-routing (Administration → Auto-routing).

func (s *Server) mountRoutingRoutes(r chi.Router) {
	r.Get("/admin/routing", s.handleGetRouting)
	r.Put("/admin/routing/settings", s.handlePutRoutingSettings)
	r.Get("/admin/routing/targets", s.handleRoutingTargets)
	r.Get("/admin/routing/categories", s.handleListRoutingCategories)
	r.Post("/admin/routing/categories", s.handleCreateRoutingCategory)
	r.Patch("/admin/routing/categories/{id}", s.handleUpdateRoutingCategory)
	r.Delete("/admin/routing/categories/{id}", s.handleDeleteRoutingCategory)
	r.Post("/admin/routing/categories/{id}/reset", s.handleResetRoutingCategory)
	r.Get("/admin/routing/routers", s.handleListRouters)
	r.Post("/admin/routing/routers", s.gateCreate("router", s.handleCreateRouter))
	r.Get("/admin/routing/routers/{id}", s.handleGetRouter)
	r.Put("/admin/routing/routers/{id}", s.handlePutRouter)
	r.Delete("/admin/routing/routers/{id}", s.handleDeleteRouter)
	r.Post("/admin/routing/test", s.handleTestRouting)
	r.Post("/admin/routing/test-batch", s.handleTestRoutingBatch)
	r.Get("/admin/routing/stats", s.handleRoutingStats)
	r.Get("/admin/routing/decisions", s.handleRoutingDecisions)
	r.Get("/admin/routing/evaluations", s.handleRoutingEvaluations)
	r.Get("/admin/routing/evaluations/{router}", s.handleRoutingEvaluationResults)
}

// routingClassifierOption is a decision model the admin can pick.
type routingClassifierOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	UpstreamName string `json:"upstream_name"`
	Hosting      string `json:"hosting"`
	Status       string `json:"status"`
	// Style is how it takes several questions (combined / separate).
	Style string `json:"style"`
	// UsedBySecurity reports that a security policy already calls this
	// classifier, so routing questions can share its call.
	UsedBySecurity bool `json:"used_by_security"`
}

type routingDocument struct {
	Settings    store.RoutingSettings     `json:"settings"`
	Classifiers []routingClassifierOption `json:"classifiers"`
	JudgeModels []routingTarget           `json:"judge_models"`
	Running     bool                      `json:"running"`
	Problem     string                    `json:"problem,omitempty"`
	// Warnings are configuration notes the overview surfaces (a hosted
	// classifier reading prompts routed on-prem).
	Warnings []string `json:"warnings"`
	// EvalWarnings are the evaluation-specific notes (also in Warnings),
	// shown on the Evaluations tab: a judge that is one of the models it
	// compares, or evaluation sending on-prem work to a hosted model.
	EvalWarnings   []string `json:"eval_warnings"`
	EvalQueue      int64    `json:"eval_queue"`
	EvalRunning    int64    `json:"eval_running"`
	InFlight       int64    `json:"in_flight_decisions"`
	MaxCategories  int      `json:"max_categories"`
	ThinkingLevels []string `json:"thinking_levels"`
}

func (s *Server) routingDocument(ctx context.Context) (*routingDocument, error) {
	settings, err := s.Store.RoutingSettings(ctx)
	if err != nil {
		return nil, err
	}
	doc := &routingDocument{Settings: settings, Classifiers: []routingClassifierOption{}, JudgeModels: []routingTarget{}, Warnings: []string{}, EvalWarnings: []string{},
		MaxCategories: store.MaxRoutingCategories, ThinkingLevels: store.ThinkingLevels}
	models, err := s.Store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		return nil, err
	}
	hosting := map[string]string{}
	if rows, err := s.Store.ListReportModelClassifications(ctx); err == nil {
		for _, r := range rows {
			hosting[r.ModelID] = r.Hosting
		}
	}
	secgwUses := map[string]bool{}
	if snap, err := s.Store.LoadSecgwSnapshot(ctx); err == nil && snap != nil {
		for _, p := range snap.Policies {
			if !p.Enabled {
				continue
			}
			for _, c := range p.Checks {
				if c.Enabled && c.ClassifierModelID != "" {
					secgwUses[c.ClassifierModelID] = true
				}
			}
		}
	}
	for _, m := range models {
		if m.ClassifierRole == store.ClassifierRoleDecision {
			opt := routingClassifierOption{ID: m.ID, Name: m.PublicName(), UpstreamName: m.UpstreamName, Hosting: hosting[m.ID], Status: m.Status,
				UsedBySecurity: secgwUses[m.ID]}
			if cl, err := s.secgwClassifiers().For(ctx, m.ID); err == nil {
				if d, ok := cl.(routingClassifier); ok {
					opt.Style = d.Style()
				}
			}
			doc.Classifiers = append(doc.Classifiers, opt)
			continue
		}
		if m.ClassifierRole == "" && m.Status == store.ModelEnabled && hasTextModality(m.Modalities) {
			doc.JudgeModels = append(doc.JudgeModels, routingTarget{ID: m.ID, Kind: store.ModelKindModel, Name: m.PublicName(), UpstreamName: m.UpstreamName,
				Hosting: hosting[m.ID], ContextWindow: m.ContextWindow})
		}
	}
	// Read the routing state fresh (a classifier may have been disabled
	// elsewhere) without flushing the rest of the hot-path cache: a GET
	// must not make every request on the replica re-read its config.
	s.configCache.entries.Delete("routing_state")
	st, err := s.routingState(ctx)
	if err != nil {
		return nil, err
	}
	doc.Running, doc.Problem = st.classifier != nil, st.problem
	rt := s.routingRT()
	doc.EvalQueue, doc.InFlight, doc.EvalRunning = rt.eval.Pending(), rt.inflight.Load(), rt.eval.Running()
	routers, _ := s.Store.ListRouters(ctx)
	doc.EvalWarnings = s.evalWarnings(ctx, settings, routers, hosting)
	doc.Warnings = append(doc.Warnings, doc.EvalWarnings...)
	// Residency: a hosted classifier reads every routed prompt first.
	if settings.ClassifierModelID != "" && hosting[settings.ClassifierModelID] == "external" {
		for _, r := range routers {
			for _, rt := range append([]store.RouterRoute{}, r.Config.Routes...) {
				if rt.ModelHosting == "self_hosted" {
					doc.Warnings = append(doc.Warnings, "Router "+r.Name+" sends some requests to a self-hosted model, but the routing classifier is externally hosted: those prompts are read by the classifier's provider before they reach your model.")
					break
				}
			}
		}
	}
	return doc, nil
}

// evalWarnings lists evaluation set-ups an admin should know about:
//   - the judge is (or serves from) one of the two models it compares:
//     judges favour their own answers;
//   - a route to a self-hosted model is evaluated against an externally
//     hosted default or judge: the evaluation copies those prompts off
//     site. Policies that block or redact still stop the copy (the sample
//     is skipped), but nothing else does.
func (s *Server) evalWarnings(ctx context.Context, settings store.RoutingSettings, routers []*store.Router, hosting map[string]string) []string {
	out := []string{}
	// servedBy maps an id (catalog model or alias) to the catalog model it
	// serves from, so a judge reached through an alias is still detected.
	servedBy := func(id string) string {
		if id == "" {
			return ""
		}
		if mm, err := s.Store.ManagedModelByID(ctx, id); err == nil {
			return mm.TargetModelID
		}
		return id
	}
	for _, r := range routers {
		def := servedBy(r.Config.Default.ModelID)
		for i, rt := range r.Config.Routes {
			if !rt.Eval.Enabled {
				continue
			}
			label := rt.Name
			if label == "" {
				label = fmt.Sprintf("route %d", i+1)
			}
			judgeID := rt.Eval.JudgeModelID
			if judgeID == "" {
				judgeID = settings.EvalJudgeModelID
			}
			judge := servedBy(judgeID)
			if judge != "" && (judge == servedBy(rt.ModelID) || judge == def) {
				out = append(out, fmt.Sprintf("Router %s, %s: the evaluation judge is one of the two models it compares, and judges favour their own answers. Pick a third model.", r.Name, label))
			}
			if hosting[servedBy(rt.ModelID)] != "self_hosted" {
				continue
			}
			var hosted []string
			if hosting[def] == "external" {
				hosted = append(hosted, "the default ("+r.Config.Default.ModelName+")")
			}
			if judge != "" && hosting[judge] == "external" {
				hosted = append(hosted, "the judge")
			}
			if len(hosted) > 0 {
				out = append(out, fmt.Sprintf("Router %s, %s: this route keeps work on a self-hosted model, but evaluating it sends a sample of those prompts to %s, which is externally hosted. Security Gateway policies on those models still apply (a sample they would block or redact is skipped); turn evaluation off or use a self-hosted default and judge if the prompts must stay on site.", r.Name, label, strings.Join(hosted, " and ")))
			}
		}
	}
	return out
}

// markCurrentPairings flags the evaluation tallies whose pairing (route
// model, default, judge) is the one each route is configured with now.
func markCurrentPairings(rows []store.RouteEvalSummary, routers []*store.Router, settings store.RoutingSettings) {
	type key struct{ router, route, routed, def, judge string }
	current := map[key]bool{}
	for _, r := range routers {
		for _, rt := range r.Config.Routes {
			judge := rt.Eval.JudgeModelID
			if judge == "" {
				judge = settings.EvalJudgeModelID
			}
			current[key{r.ID, rt.ID, rt.ModelID, r.Config.Default.ModelID, judge}] = true
		}
	}
	for i := range rows {
		e := &rows[i]
		e.Current = current[key{e.RouterID, e.RouteID, e.RoutedModelID, e.DefaultModelID, e.JudgeModelID}]
	}
}

func (s *Server) handleGetRouting(w http.ResponseWriter, r *http.Request) {
	doc, err := s.routingDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

func (s *Server) handlePutRoutingSettings(w http.ResponseWriter, r *http.Request) {
	before, err := s.Store.RoutingSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := before
	if err := decodeJSON(r, &next); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.SetRoutingSettings(r.Context(), next); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "routing_settings_updated", "routing", "settings", before, next)
	doc, err := s.routingDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// routingTarget is a model a route can send requests to.
type routingTarget struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"` // model or managed
	Name          string `json:"name"`
	UpstreamName  string `json:"upstream_name,omitempty"`
	Hosting       string `json:"hosting,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	// Pool reports a managed model that balances several models.
	Pool bool `json:"pool,omitempty"`
	// Reasoning lists the efforts the model is known to accept (nil =
	// unknown).
	Description string `json:"description,omitempty"`
}

func (s *Server) handleRoutingTargets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []routingTarget{}
	hosting := map[string]string{}
	if rows, err := s.Store.ListReportModelClassifications(ctx); err == nil {
		for _, r := range rows {
			hosting[r.ModelID] = r.Hosting
		}
	}
	managed, err := s.Store.ListManagedModels(ctx, store.ManagedModelFilter{Kind: store.ManagedKindAlias})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for _, m := range managed {
		if m.Status != store.ManagedModelEnabled {
			continue
		}
		out = append(out, routingTarget{ID: m.ID, Kind: store.ModelKindManaged, Name: m.Name, UpstreamName: m.TargetUpstreamName,
			Hosting: hosting[m.TargetModelID], ContextWindow: m.ContextWindow, Pool: len(m.Pool.Members) > 1, Description: m.Description})
	}
	models, err := s.Store.ListModels(ctx, store.ModelFilter{Status: store.ModelEnabled})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for _, m := range models {
		if m.ClassifierRole != "" || !hasTextModality(m.Modalities) {
			continue
		}
		out = append(out, routingTarget{ID: m.ID, Kind: store.ModelKindModel, Name: m.PublicName(), UpstreamName: m.UpstreamName,
			Hosting: hosting[m.ID], ContextWindow: m.ContextWindow})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// --- categories ---------------------------------------------------------------

func (s *Server) handleListRoutingCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.Store.ListRoutingCategories(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	routers, err := s.Store.ListRouters(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for i := range cats {
		for _, rt := range routers {
			for _, route := range rt.Config.Routes {
				if containsString(route.Categories, cats[i].Slug) && !containsString(cats[i].UsedBy, rt.Name) {
					cats[i].UsedBy = append(cats[i].UsedBy, rt.Name)
				}
			}
		}
	}
	insights, _ := s.Store.ListClassificationCategories(r.Context())
	if insights == nil {
		insights = []store.ClassificationCategory{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"categories": cats, "insights_categories": insights, "max_enabled": store.MaxRoutingCategories})
}

func (s *Server) handleCreateRoutingCategory(w http.ResponseWriter, r *http.Request) {
	var in store.RoutingCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.CreateRoutingCategory(r.Context(), in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "routing_category_created", "routing_category", c.Slug, nil, c)
	WriteJSON(w, http.StatusCreated, map[string]any{"category": c})
}

func (s *Server) routingCategoryOr404(w http.ResponseWriter, r *http.Request) *store.RoutingCategory {
	c, err := s.Store.RoutingCategoryByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That routing category"))
		return nil
	}
	if err != nil {
		WriteError(w, r, err)
		return nil
	}
	return c
}

func (s *Server) handleUpdateRoutingCategory(w http.ResponseWriter, r *http.Request) {
	before := s.routingCategoryOr404(w, r)
	if before == nil {
		return
	}
	var in store.RoutingCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.UpdateRoutingCategory(r.Context(), before.ID, in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "routing_category_updated", "routing_category", c.Slug, before, c)
	WriteJSON(w, http.StatusOK, map[string]any{"category": c})
}

func (s *Server) handleResetRoutingCategory(w http.ResponseWriter, r *http.Request) {
	before := s.routingCategoryOr404(w, r)
	if before == nil {
		return
	}
	c, err := s.Store.ResetRoutingCategory(r.Context(), before.ID)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "routing_category_reset", "routing_category", c.Slug, before, c)
	WriteJSON(w, http.StatusOK, map[string]any{"category": c})
}

func (s *Server) handleDeleteRoutingCategory(w http.ResponseWriter, r *http.Request) {
	before := s.routingCategoryOr404(w, r)
	if before == nil {
		return
	}
	if err := s.Store.DeleteRoutingCategory(r.Context(), before.ID); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "routing_category_deleted", "routing_category", before.Slug, before, nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- routers --------------------------------------------------------------------

func (s *Server) handleListRouters(w http.ResponseWriter, r *http.Request) {
	routers, err := s.Store.ListRouters(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	to := time.Now().UTC()
	stats, err := s.Store.RoutingStats(r.Context(), to.Add(-24*time.Hour), to)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	evals, err := s.Store.RoutingEvalSummaries(r.Context(), to.AddDate(0, 0, -30))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if settings, err := s.Store.RoutingSettings(r.Context()); err == nil {
		markCurrentPairings(evals, routers, settings)
	}
	type row struct {
		*store.Router
		Stats *store.RouterStats       `json:"stats"`
		Evals []store.RouteEvalSummary `json:"evaluations"`
	}
	out := make([]row, 0, len(routers))
	for _, rt := range routers {
		rw := row{Router: rt, Stats: stats[rt.ID], Evals: []store.RouteEvalSummary{}}
		if rw.Stats == nil {
			rw.Stats = &store.RouterStats{RouterID: rt.ID, Mix: []store.RouteMixRow{}, Reasons: []store.ReasonRow{}, Categories: []store.ReasonRow{}}
		}
		for _, e := range evals {
			if e.RouterID == rt.ID {
				rw.Evals = append(rw.Evals, e)
			}
		}
		out = append(out, rw)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"routers": out})
}

func (s *Server) routerOr404(w http.ResponseWriter, r *http.Request) *store.Router {
	rt, err := s.Store.RouterByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That router"))
		return nil
	}
	if err != nil {
		WriteError(w, r, err)
		return nil
	}
	return rt
}

func (s *Server) handleGetRouter(w http.ResponseWriter, r *http.Request) {
	rt := s.routerOr404(w, r)
	if rt == nil {
		return
	}
	grants, err := s.Store.ListGrants(r.Context(), rt.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"router": rt, "grants": grants})
}

func (s *Server) handleCreateRouter(w http.ResponseWriter, r *http.Request) {
	var in store.RouterInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	rt, err := s.Store.CreateRouter(r.Context(), in, actorID(UserFrom(r.Context())))
	if err != nil {
		WriteError(w, r, managedModelWriteError(err))
		return
	}
	s.InvalidateConfigCache()
	s.InvalidateResolvedModelCache(rt.Name)
	s.audit(r, "router_created", "managed_model", rt.ID, nil, rt)
	WriteJSON(w, http.StatusCreated, map[string]any{"router": rt})
}

func (s *Server) handlePutRouter(w http.ResponseWriter, r *http.Request) {
	before := s.routerOr404(w, r)
	if before == nil {
		return
	}
	var in store.RouterInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	rt, err := s.Store.UpdateRouter(r.Context(), before.ID, in)
	if err != nil {
		WriteError(w, r, managedModelWriteError(err))
		return
	}
	s.InvalidateConfigCache()
	s.InvalidateResolvedModelCache(before.Name, rt.Name)
	s.audit(r, "router_updated", "managed_model", rt.ID, before, rt)
	WriteJSON(w, http.StatusOK, map[string]any{"router": rt})
}

func (s *Server) handleDeleteRouter(w http.ResponseWriter, r *http.Request) {
	before := s.routerOr404(w, r)
	if before == nil {
		return
	}
	if err := s.Store.DeleteRouter(r.Context(), before.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.InvalidateResolvedModelCache(before.Name)
	s.audit(r, "router_deleted", "managed_model", before.ID, before, nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- try routing -------------------------------------------------------------------

type routingTestRouter struct {
	RouterID   string         `json:"router_id"`
	RouterName string         `json:"router_name"`
	RouteID    string         `json:"route_id"`
	RouteName  string         `json:"route_name"`
	ModelID    string         `json:"model_id"`
	ModelName  string         `json:"model_name"`
	Thinking   string         `json:"thinking"`
	Reason     string         `json:"reason"`
	Trace      []routing.Step `json:"trace"`
}

type routingTestResult struct {
	Status        string              `json:"status"`
	Error         string              `json:"error,omitempty"`
	Category      string              `json:"category"`
	Confidence    float64             `json:"confidence"`
	Probabilities map[string]float64  `json:"probabilities"`
	Difficulty    float64             `json:"difficulty"`
	LatencyMs     int64               `json:"latency_ms"`
	Calls         int                 `json:"calls"`
	Classifier    string              `json:"classifier"`
	Questions     []string            `json:"questions"`
	Routers       []routingTestRouter `json:"routers"`
}

// routingTestClassifier resolves the classifier a test uses: the one named
// in the request (so admins can compare before switching), else the saved
// one.
func (s *Server) routingTestClassifier(ctx context.Context, modelID string) (routingClassifier, string, error) {
	if modelID == "" {
		settings, err := s.Store.RoutingSettings(ctx)
		if err != nil {
			return nil, "", err
		}
		modelID = settings.ClassifierModelID
	}
	if modelID == "" {
		return nil, "", ErrInvalidRequest("Choose a routing classifier first.").WithParam("classifier_model_id")
	}
	m, err := s.Store.ModelByID(ctx, modelID)
	if err != nil {
		return nil, "", ErrInvalidRequest("That classifier does not exist.").WithParam("classifier_model_id")
	}
	cl, err := s.secgwClassifiers().For(ctx, modelID)
	if err != nil {
		return nil, "", ErrInvalidRequest("That classifier cannot be used: " + err.Error()).WithParam("classifier_model_id")
	}
	d, ok := cl.(routingClassifier)
	if !ok || !d.IsDecision() {
		return nil, "", ErrInvalidRequest("Routing needs a decision model (role: decision).").WithParam("classifier_model_id")
	}
	return d, m.PublicName() + " · " + m.UpstreamName, nil
}

// routingTestCategories is the category list a test asks, from the saved
// categories (the builder tests unsaved routers, never unsaved categories).
func (s *Server) routingTestCategories(ctx context.Context) ([]routing.Category, map[string]float64, error) {
	cats, err := s.Store.ListRoutingCategories(ctx)
	if err != nil {
		return nil, nil, err
	}
	insights := map[string][2]string{}
	if ins, err := s.Store.ListClassificationCategories(ctx); err == nil {
		for _, c := range ins {
			insights[c.Slug] = [2]string{c.Name, c.Description}
		}
	}
	out := routing.CategoriesFrom(cats, insights)
	min := map[string]float64{}
	for _, c := range out {
		min[c.Slug] = c.MinConfidence
	}
	return out, min, nil
}

func (s *Server) handleTestRouting(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text              string `json:"text"`
		ClassifierModelID string `json:"classifier_model_id"`
		RouterID          string `json:"router_id"`
		// Config tests an unsaved router from the builder.
		Config     *store.RouterConfig `json:"config"`
		RouterName string              `json:"router_name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		WriteError(w, r, ErrInvalidRequest("Enter a prompt to route.").WithParam("text"))
		return
	}
	ctx := r.Context()
	cl, label, err := s.routingTestClassifier(ctx, body.ClassifierModelID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	settings, _ := s.Store.RoutingSettings(ctx)
	cats, minConf, err := s.routingTestCategories(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	type target struct {
		id, name string
		cfg      store.RouterConfig
	}
	var targets []target
	switch {
	case body.Config != nil:
		targets = append(targets, target{id: body.RouterID, name: body.RouterName, cfg: *body.Config})
	case body.RouterID != "":
		rt, err := s.Store.RouterByID(ctx, body.RouterID)
		if err != nil {
			WriteError(w, r, ErrNotFoundf("That router"))
			return
		}
		targets = append(targets, target{id: rt.ID, name: rt.Name, cfg: rt.Config})
	default:
		routers, err := s.Store.ListRouters(ctx)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		for _, rt := range routers {
			targets = append(targets, target{id: rt.ID, name: rt.Name, cfg: rt.Config})
		}
	}
	// Ask every question any of the routers needs (category always, so the
	// admin sees what the classifier thinks even for a difficulty-only
	// router).
	questions := []classify.Question{routing.CategoryQuestion(cats), routing.DifficultyQuestion()}
	if settings.DifficultyStyle == store.RoutingDifficultyCategory {
		questions = append(questions, routing.CodeDifficultyQuestion())
	}
	out := routingTestResult{Classifier: label, Routers: []routingTestRouter{}, Questions: []string{}}
	for _, q := range questions {
		out.Questions = append(out.Questions, q.ID)
	}
	cctx, cancel := context.WithTimeout(classify.WithFeature(ctx, routingFeature, "test"), 30*time.Second)
	defer cancel()
	started := time.Now()
	res, err := cl.Decide(cctx, classify.TrimText(text, settings.MaxChars), questions)
	out.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		out.Status, out.Error = "error", err.Error()
		WriteJSON(w, http.StatusOK, out)
		return
	}
	out.Status, out.Calls = "ok", max(1, res.Calls)
	ans := routing.Interpret(res)
	out.Category, out.Confidence, out.Probabilities, out.Difficulty = ans.Category, ans.Confidence, ans.Probabilities, ans.Difficulty
	if out.Probabilities == nil {
		out.Probabilities = map[string]float64{}
	}
	for _, t := range targets {
		cands := s.routeCandidates(ctx, t.cfg)
		d := routing.Choose(t.cfg, cands, ans, minConf, routing.Context{EstimatedTokens: int64(len(text) / 4)})
		tr := routingTestRouter{RouterID: t.id, RouterName: t.name, RouteID: d.RouteID, RouteName: routeLabel(t.cfg, d), ModelID: d.ModelID,
			Thinking: d.Thinking, Reason: d.Reason, Trace: d.Trace}
		if res, err := s.resolveRouteTarget(ctx, d.ModelID); err == nil && res.Model != nil {
			tr.ModelName = res.Model.PublicName()
			if res.Managed != nil {
				tr.ModelName = res.Managed.Name + " → " + res.Model.PublicName()
			}
		}
		out.Routers = append(out.Routers, tr)
	}
	WriteJSON(w, http.StatusOK, out)
}

// batchRow is one example's outcome in a batch test.
type batchRow struct {
	Expected   string  `json:"expected"`
	Got        string  `json:"got"`
	Confidence float64 `json:"confidence"`
	Difficulty float64 `json:"difficulty"`
	Text       string  `json:"text"`
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
}

type batchCategory struct {
	Slug     string     `json:"slug"`
	Name     string     `json:"name"`
	Total    int        `json:"total"`
	Correct  int        `json:"correct"`
	Accuracy float64    `json:"accuracy"`
	Misses   []batchRow `json:"misses"`
}

// handleTestRoutingBatch runs every enabled category's test examples
// through the classifier and reports per-category accuracy and confusions,
// so an admin can see whether their category wording works before routing
// real traffic by it. Example text is admin-authored configuration, not
// traffic, so it is safe to echo back.
func (s *Server) handleTestRoutingBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClassifierModelID string `json:"classifier_model_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	cl, label, err := s.routingTestClassifier(ctx, body.ClassifierModelID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	stored, err := s.Store.ListRoutingCategories(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cats, minConf, err := s.routingTestCategories(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := routing.CategoryQuestion(cats)
	type job struct{ slug, text string }
	var jobs []job
	for _, c := range stored {
		if !c.Enabled {
			continue
		}
		for _, e := range c.Examples {
			jobs = append(jobs, job{c.Slug, e})
		}
	}
	if len(jobs) == 0 {
		WriteError(w, r, ErrInvalidRequest("No category has test examples yet. Add a few prompts that should land in each category."))
		return
	}
	if len(jobs) > 300 {
		jobs = jobs[:300]
	}
	rows := make([]batchRow, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	started := time.Now()
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(classify.WithFeature(ctx, routingFeature, "test"), 30*time.Second)
			defer cancel()
			res, err := cl.Decide(cctx, j.text, []classify.Question{q, routing.DifficultyQuestion()})
			row := batchRow{Expected: j.slug, Text: j.text}
			if err != nil {
				row.Error = err.Error()
			} else {
				a := routing.Interpret(res)
				row.Got, row.Confidence, row.Difficulty = a.Category, a.Confidence, a.Difficulty
				row.OK = a.Category == j.slug && a.Confidence >= minConf[j.slug]
			}
			rows[i] = row
		}(i, j)
	}
	wg.Wait()
	byCat := map[string]*batchCategory{}
	var order []string
	names := map[string]string{}
	for _, c := range cats {
		names[c.Slug] = c.Name
	}
	total, correct, failed := 0, 0, 0
	for _, row := range rows {
		bc := byCat[row.Expected]
		if bc == nil {
			bc = &batchCategory{Slug: row.Expected, Name: names[row.Expected], Misses: []batchRow{}}
			byCat[row.Expected] = bc
			order = append(order, row.Expected)
		}
		if row.Error != "" {
			failed++
			bc.Misses = append(bc.Misses, row)
			continue
		}
		bc.Total++
		total++
		if row.OK {
			bc.Correct++
			correct++
		} else {
			bc.Misses = append(bc.Misses, row)
		}
	}
	out := make([]batchCategory, 0, len(order))
	for _, slug := range order {
		bc := byCat[slug]
		if bc.Total > 0 {
			bc.Accuracy = float64(bc.Correct) / float64(bc.Total)
		}
		out = append(out, *bc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Accuracy < out[j].Accuracy })
	acc := 0.0
	if total > 0 {
		acc = float64(correct) / float64(total)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"classifier": label, "total": total, "correct": correct, "failed": failed, "accuracy": acc,
		"elapsed_ms": time.Since(started).Milliseconds(), "categories": out})
}

// --- stats ---------------------------------------------------------------------------

func (s *Server) handleRoutingStats(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	stats, err := s.Store.RoutingStats(r.Context(), from, to)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	list := make([]*store.RouterStats, 0, len(stats))
	for _, st := range stats {
		list = append(list, st)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Requests > list[j].Requests })
	WriteJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "routers": list})
}

func (s *Server) handleRoutingDecisions(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Store.ListRoutingDecisions(r.Context(), store.RoutingDecisionFilter{From: from, To: to,
		RouterID: r.URL.Query().Get("router_id"), Reason: r.URL.Query().Get("reason"), RequestID: r.URL.Query().Get("request_id"), Limit: limit})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"decisions": rows})
}

func (s *Server) handleRoutingEvaluations(w http.ResponseWriter, r *http.Request) {
	from, _ := parseRange(r)
	rows, err := s.Store.RoutingEvalSummaries(r.Context(), from)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if routers, err := s.Store.ListRouters(r.Context()); err == nil {
		if settings, err := s.Store.RoutingSettings(r.Context()); err == nil {
			markCurrentPairings(rows, routers, settings)
		}
	}
	if s.Config != nil && s.Config.LocalOnly {
		for i := range rows {
			rows[i].RoutedCostNano, rows[i].DefaultCostNano, rows[i].EvalCostNano = 0, 0, 0
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"evaluations": rows, "queue": s.routingRT().eval.Pending()})
}

func (s *Server) handleRoutingEvaluationResults(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	q := r.URL.Query()
	var pair *store.RouteEvalPairing
	if q.Has("routed_model_id") || q.Has("default_model_id") || q.Has("judge_model_id") {
		pair = &store.RouteEvalPairing{RoutedModelID: q.Get("routed_model_id"), DefaultModelID: q.Get("default_model_id"), JudgeModelID: q.Get("judge_model_id")}
	}
	rows, err := s.Store.ListRoutingEvalsFor(r.Context(), chi.URLParam(r, "router"), q.Get("route_id"), pair, limit)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": rows})
}
