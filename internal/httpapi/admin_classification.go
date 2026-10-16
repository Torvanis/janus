package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/decisions"
	"github.com/torvanis/janus/internal/store"
)

// Admin API for prompt classification (Administration → Usage insights).

func (s *Server) mountClassificationRoutes(r chi.Router) {
	r.Get("/admin/classification", s.handleGetClassification)
	r.Put("/admin/classification", s.handlePutClassification)
	r.Post("/admin/classification/test", s.handleTestClassification)
	r.Get("/admin/classification/report", s.handleClassificationReport)
	r.Get("/admin/classification/results", s.handleClassificationResults)
	r.Get("/admin/classification/categories", s.handleListClassificationCategories)
	r.Post("/admin/classification/categories", s.handleCreateClassificationCategory)
	r.Patch("/admin/classification/categories/{id}", s.handleUpdateClassificationCategory)
	r.Delete("/admin/classification/categories/{id}", s.handleDeleteClassificationCategory)
}

// classifierUpstream is a provider the admin can pick, with the models it
// offers for each protocol.
type classifierUpstream struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AdapterType string   `json:"adapter_type"`
	BaseURL     string   `json:"base_url"`
	Enabled     bool     `json:"enabled"`
	Models      []string `json:"models"`
	// DecisionModels are suggestions for the System One protocol: decision
	// models this provider is known to serve, plus any discovered model
	// whose name marks it as one.
	DecisionModels []string `json:"decision_models"`
	// SystemOneURL is where decision calls would go (named for the first
	// dialect; for OpenAI it is the /v1/decisions URL).
	SystemOneURL string `json:"systemone_url"`
	// Dialect is the decision wire format this provider speaks.
	Dialect string `json:"dialect"`
}

type classificationDocument struct {
	Settings  store.ClassificationSettings `json:"settings"`
	Upstreams []classifierUpstream         `json:"upstreams"`
	// Running is true when the saved settings produced a working plan.
	Running bool `json:"running"`
	// Problem says why classification is enabled but not running.
	Problem string `json:"problem,omitempty"`
	// QueueDepth is this replica's async backlog.
	QueueDepth int64 `json:"queue_depth"`
	Limits     struct {
		TopLevel      int `json:"top_level"`
		Subcategories int `json:"subcategories"`
	} `json:"limits"`
}

// decisionModelHints are decision models a known provider serves but may
// not list in its catalog.
func decisionModelHints(adapterType, baseURL string) []string {
	switch {
	case strings.Contains(baseURL, "openrouter.ai"):
		return []string{"~typesafe/jev-latest"}
	case strings.Contains(baseURL, "typesafe.ai"):
		return []string{"jev-latest"}
	case decisions.DefaultDialect(baseURL) == decisions.DialectOpenAI:
		return []string{"gpt-6-luna"}
	case adapterType == "ollama" || strings.Contains(baseURL, ":11434"):
		return []string{"nimble", "nimble:9b-q8_0", "nimble:9b-q4_K_M"}
	}
	return nil
}

func looksLikeDecisionModel(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"jev", "nimble", "clef", "laya", "julia-1", "kev-4b", "lev-", "openjev", "decision"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func (s *Server) classificationDocument(ctx context.Context) (*classificationDocument, error) {
	settings, err := s.Store.ClassificationSettings(ctx)
	if err != nil {
		return nil, err
	}
	doc := &classificationDocument{Settings: settings, Upstreams: []classifierUpstream{}}
	doc.Limits.TopLevel, doc.Limits.Subcategories = store.MaxTopLevelCategories, store.MaxSubcategoriesPerRoot
	ups, err := s.Store.ListUpstreams(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range ups {
		dialect := decisions.DefaultDialect(u.BaseURL)
		cu := classifierUpstream{ID: u.ID, Name: u.Name, AdapterType: u.AdapterType, BaseURL: u.BaseURL, Enabled: u.Enabled,
			Models: []string{}, DecisionModels: decisionModelHints(u.AdapterType, u.BaseURL), SystemOneURL: decisions.Endpoint(u.BaseURL, dialect), Dialect: dialect}
		if cu.DecisionModels == nil {
			cu.DecisionModels = []string{}
		}
		models, err := s.Store.ListModels(ctx, store.ModelFilter{UpstreamID: u.ID})
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if m.ClassifierRole == store.ClassifierRoleDecision {
				// Catalog decision models are offered under the name the
				// decision API answers to.
				if w := decisions.WireModel(m.Name, u.BaseURL); !containsString(cu.DecisionModels, w) {
					cu.DecisionModels = append(cu.DecisionModels, w)
				}
				continue
			}
			if m.ClassifierRole != "" || !hasTextModality(m.Modalities) {
				continue
			}
			cu.Models = append(cu.Models, m.Name)
			if looksLikeDecisionModel(m.Name) && !containsString(cu.DecisionModels, m.Name) {
				cu.DecisionModels = append(cu.DecisionModels, m.Name)
			}
		}
		doc.Upstreams = append(doc.Upstreams, cu)
	}
	if settings.Enabled {
		// Build the plan fresh for the page rather than clearing the
		// shared hot-path cache: a page load must not make the next
		// proxied requests rebuild every cached configuration.
		plan, problem, err := s.buildClassificationPlan(ctx, settings)
		if err != nil {
			return nil, err
		}
		doc.Running, doc.Problem = plan != nil, problem
	}
	doc.QueueDepth = s.classifyRunner().Pending()
	return doc, nil
}

func hasTextModality(modalities []string) bool {
	if len(modalities) == 0 {
		return true
	}
	for _, m := range modalities {
		if m == "chat" || m == "text" || m == "completion" {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) handleGetClassification(w http.ResponseWriter, r *http.Request) {
	doc, err := s.classificationDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

func (s *Server) handlePutClassification(w http.ResponseWriter, r *http.Request) {
	before, err := s.Store.ClassificationSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := before
	if err := decodeJSON(r, &next); err != nil {
		WriteError(w, r, err)
		return
	}
	if next.UpstreamID != "" {
		if _, err := s.Store.UpstreamByID(r.Context(), next.UpstreamID); errors.Is(err, store.ErrNotFound) {
			WriteError(w, r, ErrInvalidRequest("The chosen provider does not exist.").WithParam("upstream_id"))
			return
		} else if err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if err := s.Store.SetClassificationSettings(r.Context(), next); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "classification_settings_updated", "classification", "settings", before, next)
	doc, err := s.classificationDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

func writeValidation(w http.ResponseWriter, r *http.Request, err error) {
	var v *store.ValidationError
	if errors.As(err, &v) {
		WriteError(w, r, ErrInvalidRequest(v.Message).WithParam(v.Field))
		return
	}
	WriteError(w, r, err)
}

// handleTestClassification classifies a sample text with the posted (not
// necessarily saved) settings, so an admin can compare classifiers and see
// their latency before switching. Nothing is stored.
func (s *Server) handleTestClassification(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string                        `json:"text"`
		Settings *store.ClassificationSettings `json:"settings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		WriteError(w, r, ErrInvalidRequest("Enter some text to classify.").WithParam("text"))
		return
	}
	settings, err := s.Store.ClassificationSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Settings != nil {
		settings = *body.Settings
	}
	settings.Enabled = true
	if err := settings.Validate(); err != nil {
		writeValidation(w, r, err)
		return
	}
	plan, problem, err := s.buildClassificationPlan(r.Context(), settings)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if plan == nil {
		WriteError(w, r, ErrInvalidRequest(problem))
		return
	}
	plan.Mode = "test"
	o := classify.Run(r.Context(), plan, text)
	resp := map[string]any{
		"status": o.Status, "error": o.Err, "category": o.Category, "subcategory": o.Subcategory,
		"confidence": o.Confidence, "probabilities": o.Probabilities, "latency_ms": o.Timing.Elapsed.Milliseconds(),
		"classifier": o.Classifier, "model": o.Model, "protocol": o.Protocol, "input_chars": o.InputChars,
		"tokens_in": o.Usage.TokensIn, "tokens_out": o.Usage.TokensOut, "cost_nanousd": o.CostNano,
	}
	if s.Config != nil && s.Config.LocalOnly {
		resp["cost_nanousd"] = 0
	}
	WriteJSON(w, http.StatusOK, resp)
}

// parseRange reads from/to (RFC 3339 or YYYY-MM-DD) or days, defaulting to
// the last 30 days.
func parseRange(r *http.Request) (time.Time, time.Time) {
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -30)
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 366 {
		from = to.AddDate(0, 0, -d)
	}
	if h, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && h > 0 && h <= 72 {
		from = to.Add(-time.Duration(h) * time.Hour)
	}
	parse := func(v string) (time.Time, bool) {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC(), true
			}
		}
		return time.Time{}, false
	}
	if t, ok := parse(r.URL.Query().Get("from")); ok {
		from = t
	}
	if t, ok := parse(r.URL.Query().Get("to")); ok {
		to = t
	}
	return from, to
}

func (s *Server) handleClassificationReport(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	if !from.Before(to) {
		WriteError(w, r, ErrInvalidRequest("from must be before to.").WithParam("from"))
		return
	}
	rep, err := s.Store.ClassificationReport(r.Context(), from, to)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if s.Config != nil && s.Config.LocalOnly {
		rep.Health.CostNano = 0
	}
	WriteJSON(w, http.StatusOK, rep)
}

func (s *Server) handleClassificationResults(w http.ResponseWriter, r *http.Request) {
	from, to := parseRange(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Store.ListClassificationResults(r.Context(), store.ClassificationResultFilter{
		From: from, To: to, Category: r.URL.Query().Get("category"), Status: r.URL.Query().Get("status"),
		RequestID: r.URL.Query().Get("request_id"), Limit: limit,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": rows})
}

func (s *Server) handleListClassificationCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.Store.ListClassificationCategories(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if cats == nil {
		cats = []store.ClassificationCategory{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

func (s *Server) handleCreateClassificationCategory(w http.ResponseWriter, r *http.Request) {
	var in store.ClassificationCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.CreateClassificationCategory(r.Context(), in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "classification_category_created", "classification_category", c.Slug, nil, c)
	WriteJSON(w, http.StatusCreated, map[string]any{"category": c})
}

func (s *Server) handleUpdateClassificationCategory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ClassificationCategoryByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("category"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in store.ClassificationCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.UpdateClassificationCategory(r.Context(), id, in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "classification_category_updated", "classification_category", c.Slug, before, c)
	WriteJSON(w, http.StatusOK, map[string]any{"category": c})
}

func (s *Server) handleDeleteClassificationCategory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	before, err := s.Store.ClassificationCategoryByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("category"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DeleteClassificationCategory(r.Context(), id); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "classification_category_deleted", "classification_category", before.Slug, before, nil)
	w.WriteHeader(http.StatusNoContent)
}
