package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/secgw"
	secgwclassify "github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
)

// Security categories (Security → Categories): the categories a
// content-safety check with method "categories" asks a decision model
// about, plus a try box and a batch test of every category's examples.
//
// The test endpoints use a resolver of their own, so a slow or failing
// classifier under test never opens the breaker live guards depend on.
// They store nothing: the text is sent to the classifier and dropped, and
// errors are described in Janus's words, never by quoting the classifier
// or provider (small models echo the prompt).

func (s *Server) mountSecgwCategoryRoutes(r chi.Router) {
	r.Get("/admin/secgw/categories", s.handleListSecgwCategories)
	r.Post("/admin/secgw/categories", s.handleCreateSecgwCategory)
	r.Post("/admin/secgw/categories/test", s.handleTestSecgwCategories)
	r.Post("/admin/secgw/categories/test-batch", s.handleTestSecgwCategoriesBatch)
	r.Patch("/admin/secgw/categories/{id}", s.handleUpdateSecgwCategory)
	r.Delete("/admin/secgw/categories/{id}", s.handleDeleteSecgwCategory)
	r.Post("/admin/secgw/categories/{id}/reset", s.handleResetSecgwCategory)
}

// secgwCategoryClassifier is a decision model the Categories tab can pick.
type secgwCategoryClassifier struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	UpstreamName string `json:"upstream_name"`
	Status       string `json:"status"`
}

func (s *Server) handleListSecgwCategories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cats, err := s.Store.ListSecgwCategories(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	usage, err := s.Store.SecgwCategoryUsage(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for i := range cats {
		if u := usage[cats[i].Slug]; len(u) > 0 {
			cats[i].UsedBy = u
		}
	}
	insights, _ := s.Store.ListClassificationCategories(ctx)
	if insights == nil {
		insights = []store.ClassificationCategory{}
	}
	classifiers := []secgwCategoryClassifier{}
	if models, err := s.Store.ListModels(ctx, store.ModelFilter{ClassifierOnly: true}); err == nil {
		for _, m := range models {
			if m.ClassifierRole == store.ClassifierRoleDecision {
				classifiers = append(classifiers, secgwCategoryClassifier{ID: m.ID, Name: m.PublicName(), UpstreamName: m.UpstreamName, Status: m.Status})
			}
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"categories": cats, "insights_categories": insights, "classifiers": classifiers,
		"default_slugs": store.DefaultSecgwCategorySlugs(), "max_per_check": store.MaxSecgwCategoriesPerCheck,
	})
}

func (s *Server) secgwCategoryOr404(w http.ResponseWriter, r *http.Request) *store.SecgwCategory {
	c, err := s.Store.SecgwCategoryByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That security category"))
		return nil
	}
	if err != nil {
		WriteError(w, r, err)
		return nil
	}
	return c
}

func (s *Server) handleCreateSecgwCategory(w http.ResponseWriter, r *http.Request) {
	var in store.SecgwCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.CreateSecgwCategory(r.Context(), in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_category_created", "secgw_category", c.Slug, nil, c)
	WriteJSON(w, http.StatusCreated, map[string]any{"category": c})
}

func (s *Server) handleUpdateSecgwCategory(w http.ResponseWriter, r *http.Request) {
	before := s.secgwCategoryOr404(w, r)
	if before == nil {
		return
	}
	var in store.SecgwCategoryInput
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := s.Store.UpdateSecgwCategory(r.Context(), before.ID, in)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_category_updated", "secgw_category", c.Slug, before, c)
	WriteJSON(w, http.StatusOK, map[string]any{"category": c})
}

func (s *Server) handleResetSecgwCategory(w http.ResponseWriter, r *http.Request) {
	before := s.secgwCategoryOr404(w, r)
	if before == nil {
		return
	}
	c, err := s.Store.ResetSecgwCategory(r.Context(), before.ID)
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_category_reset", "secgw_category", c.Slug, before, c)
	WriteJSON(w, http.StatusOK, map[string]any{"category": c})
}

func (s *Server) handleDeleteSecgwCategory(w http.ResponseWriter, r *http.Request) {
	before := s.secgwCategoryOr404(w, r)
	if before == nil {
		return
	}
	if err := s.Store.DeleteSecgwCategory(r.Context(), before.ID); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_category_deleted", "secgw_category", before.Slug, before, nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- tests ----------------------------------------------------------------------

// secgwCategoryTester resolves the decision model a category test uses,
// through a private resolver (its own breaker, no alerts).
func (s *Server) secgwCategoryTester(ctx context.Context, modelID string) (routingClassifier, string, error) {
	if modelID == "" {
		return nil, "", ErrInvalidRequest("Choose a decision model to test with.").WithParam("classifier_model_id")
	}
	m, err := s.Store.ModelByID(ctx, modelID)
	if err != nil {
		return nil, "", ErrInvalidRequest("That classifier does not exist. Choose another decision model.").WithParam("classifier_model_id")
	}
	if m.ClassifierRole != store.ClassifierRoleDecision {
		return nil, "", ErrInvalidRequest(m.PublicName() + " is not a decision model. Security categories need a model with the decision role.").WithParam("classifier_model_id")
	}
	resolver := &classifierResolver{s: s, breakers: map[string]*secgwclassify.Breaker{}, roles: map[string]string{}, bench: true}
	cl, err := resolver.For(ctx, modelID)
	if err != nil {
		return nil, "", ErrInvalidRequest("That classifier cannot be used right now. Check its upstream on the Models page.").WithParam("classifier_model_id")
	}
	d, ok := cl.(routingClassifier)
	if !ok || !d.IsDecision() {
		return nil, "", ErrInvalidRequest(m.PublicName() + " is not a decision model. Security categories need a model with the decision role.").WithParam("classifier_model_id")
	}
	return d, m.PublicName() + " · " + m.UpstreamName, nil
}

// secgwTestCategories is the category set a test asks: the named slugs, or
// every enabled category. Disabled and unknown slugs are refused.
func (s *Server) secgwTestCategories(ctx context.Context, slugs []string) ([]*store.SecgwCategory, error) {
	all, err := s.Store.SecgwCategoryMap(ctx)
	if err != nil {
		return nil, err
	}
	var out []*store.SecgwCategory
	if len(slugs) == 0 {
		cats, err := s.Store.ListSecgwCategories(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range cats {
			if c.Enabled {
				out = append(out, all[c.Slug])
			}
		}
	} else {
		seen := map[string]bool{}
		for _, slug := range slugs {
			if seen[slug] {
				continue
			}
			seen[slug] = true
			c := all[slug]
			if c == nil {
				return nil, ErrInvalidRequest("Category " + slug + " does not exist or is turned off. Turn it on or leave it out.").WithParam("slugs")
			}
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, ErrInvalidRequest("Turn on at least one security category first.").WithParam("slugs")
	}
	return out, nil
}

func normalizeAsk(ask string) (string, error) {
	switch ask {
	case "", store.SecgwAskTogether:
		return store.SecgwAskTogether, nil
	case store.SecgwAskSeparately:
		return ask, nil
	}
	return "", ErrInvalidRequest("Set ask to together or separately.").WithParam("ask")
}

var httpStatusRE = regexp.MustCompile(`HTTP (\d{3})`)

// describeClassifierError says why a test call failed in Janus's words:
// classifier replies and provider error bodies are never quoted, since a
// small model can echo the prompt back.
func describeClassifierError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "The classifier did not answer in time."
	case errors.Is(err, classify.ErrTooManyOptions):
		return "The classifier takes fewer options than this question has. Ask one question per category instead."
	case errors.Is(err, classify.ErrNoAnswer):
		return "The classifier answered without a usable choice."
	case errors.Is(err, secgwclassify.ErrBreakerOpen):
		return "The classifier failed several times in a row and is resting for a moment. Try again shortly."
	}
	if m := httpStatusRE.FindStringSubmatch(err.Error()); m != nil {
		return "The classifier's server answered HTTP " + m[1] + "."
	}
	return "The classifier call failed. Check its upstream on the Models page."
}

type secgwCategoryTestResult struct {
	Status     string                `json:"status"`
	Error      string                `json:"error,omitempty"`
	Classifier string                `json:"classifier"`
	Ask        string                `json:"ask"`
	LatencyMs  int64                 `json:"latency_ms"`
	Calls      int                   `json:"calls"`
	Scores     []secgw.CategoryScore `json:"scores"`
	Counted    []string              `json:"counted"`
}

// handleTestSecgwCategories asks a decision model about a pasted text and
// returns every category's probability and which ones would count.
// Nothing is stored.
func (s *Server) handleTestSecgwCategories(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text              string   `json:"text"`
		ClassifierModelID string   `json:"classifier_model_id"`
		Slugs             []string `json:"slugs"`
		Ask               string   `json:"ask"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		WriteError(w, r, ErrInvalidRequest("Enter a request to test.").WithParam("text"))
		return
	}
	ask, err := normalizeAsk(body.Ask)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	cl, label, err := s.secgwCategoryTester(ctx, body.ClassifierModelID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cats, err := s.secgwTestCategories(ctx, body.Slugs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := secgwCategoryTestResult{Classifier: label, Ask: ask, Scores: []secgw.CategoryScore{}, Counted: []string{}}
	cctx, cancel := context.WithTimeout(classify.WithFeature(ctx, string(store.SecgwCheckContentSafety), "test"), 60*time.Second)
	defer cancel()
	started := time.Now()
	res, err := cl.Decide(cctx, classify.TrimText(text, 12000), secgw.CategoryQuestions(cats, ask, false))
	out.LatencyMs = time.Since(started).Milliseconds()
	var probs map[string]float64
	if err == nil {
		probs, err = secgw.CategoryProbabilities(res, cats, ask)
	}
	if err != nil {
		out.Status, out.Error = "error", describeClassifierError(err)
		WriteJSON(w, http.StatusOK, out)
		return
	}
	out.Status, out.Calls = "ok", max(1, res.Calls)
	out.Scores = secgw.ScoreCategories(probs, cats)
	for _, sc := range out.Scores {
		if sc.Counts {
			out.Counted = append(out.Counted, sc.Slug)
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// secgwBatchMiss is one example the classifier did not catch.
type secgwBatchMiss struct {
	Expected string `json:"expected"`
	// Got is the category the classifier rated highest ("none" when it
	// rated every category below its minimum confidence), and Confidence
	// its probability.
	Got        string  `json:"got"`
	GotName    string  `json:"got_name"`
	Confidence float64 `json:"confidence"`
	// ExpectedProbability is what the classifier gave the expected
	// category, against its minimum confidence.
	ExpectedProbability float64 `json:"expected_probability"`
	Text                string  `json:"text"`
	Error               string  `json:"error,omitempty"`
}

type secgwBatchCategory struct {
	Slug     string           `json:"slug"`
	Name     string           `json:"name"`
	Group    string           `json:"group"`
	Total    int              `json:"total"`
	Correct  int              `json:"correct"`
	Accuracy float64          `json:"accuracy"`
	Misses   []secgwBatchMiss `json:"misses"`
}

// handleTestSecgwCategoriesBatch runs every enabled category's examples
// through the classifier, asking about every enabled category at once (as
// a check selecting them all would), and reports per-category accuracy
// and each miss. An example counts as caught when its own category counts.
// Examples are admin-written configuration, not traffic, so echoing them
// back is fine; nothing is stored.
func (s *Server) handleTestSecgwCategoriesBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClassifierModelID string   `json:"classifier_model_id"`
		Slugs             []string `json:"slugs"`
		Ask               string   `json:"ask"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	ask, err := normalizeAsk(body.Ask)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ctx := r.Context()
	cl, label, err := s.secgwCategoryTester(ctx, body.ClassifierModelID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cats, err := s.secgwTestCategories(ctx, body.Slugs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	type job struct {
		cat  *store.SecgwCategory
		text string
	}
	var jobs []job
	for _, c := range cats {
		for _, e := range c.Examples {
			jobs = append(jobs, job{c, e})
		}
	}
	if len(jobs) == 0 {
		WriteError(w, r, ErrInvalidRequest("No category has test examples yet. Add a few requests that each category should catch."))
		return
	}
	if len(jobs) > 300 {
		jobs = jobs[:300]
	}
	names := map[string]string{store.SecgwCategoryNone: "None of these"}
	for _, c := range cats {
		names[c.Slug] = c.Name
	}
	questions := secgw.CategoryQuestions(cats, ask, false)
	type row struct {
		ok   bool
		miss secgwBatchMiss
	}
	rows := make([]row, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	started := time.Now()
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(classify.WithFeature(ctx, string(store.SecgwCheckContentSafety), "test"), 60*time.Second)
			defer cancel()
			miss := secgwBatchMiss{Expected: j.cat.Slug, Text: j.text}
			res, err := cl.Decide(cctx, classify.TrimText(j.text, 12000), questions)
			var probs map[string]float64
			if err == nil {
				probs, err = secgw.CategoryProbabilities(res, cats, ask)
			}
			if err != nil {
				miss.Error = describeClassifierError(err)
				rows[i] = row{miss: miss}
				return
			}
			scores := secgw.ScoreCategories(probs, cats)
			miss.ExpectedProbability = probs[j.cat.Slug]
			miss.Got, miss.Confidence = store.SecgwCategoryNone, 0
			for _, sc := range scores {
				if sc.Counts {
					miss.Got, miss.Confidence = sc.Slug, sc.Probability
					break
				}
			}
			if miss.Got == store.SecgwCategoryNone && len(scores) > 0 {
				miss.Confidence = scores[0].Probability
			}
			miss.GotName = names[miss.Got]
			rows[i] = row{ok: miss.ExpectedProbability >= j.cat.MinConfidence, miss: miss}
		}(i, j)
	}
	wg.Wait()
	byCat := map[string]*secgwBatchCategory{}
	var order []string
	total, correct, failed := 0, 0, 0
	for i, rw := range rows {
		c := jobs[i].cat
		bc := byCat[c.Slug]
		if bc == nil {
			bc = &secgwBatchCategory{Slug: c.Slug, Name: c.Name, Group: c.Group, Misses: []secgwBatchMiss{}}
			byCat[c.Slug] = bc
			order = append(order, c.Slug)
		}
		if rw.miss.Error != "" {
			failed++
			bc.Misses = append(bc.Misses, rw.miss)
			continue
		}
		bc.Total++
		total++
		if rw.ok {
			bc.Correct++
			correct++
		} else {
			bc.Misses = append(bc.Misses, rw.miss)
		}
	}
	out := make([]secgwBatchCategory, 0, len(order))
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
	WriteJSON(w, http.StatusOK, map[string]any{"classifier": label, "ask": ask, "total": total, "correct": correct, "failed": failed,
		"accuracy": acc, "elapsed_ms": time.Since(started).Milliseconds(), "calls_per_example": secgw.CategoryCalls(len(cats), ask), "categories": out})
}

// validateContentSafetyMethod checks a content_safety check's method
// options: a categories check needs a decision model and between one and
// 25 enabled categories.
func (s *Server) validateContentSafetyMethod(ctx context.Context, i int, c store.SecgwCheck, o secgw.ContentSafetyOptions, m *store.Model) error {
	field := func(f string) string { return "checks[" + strconv.Itoa(i) + "].options." + f }
	switch o.Method {
	case "", store.SecgwMethodLlamaGuard:
		return nil
	case store.SecgwMethodCategories:
	default:
		return ErrInvalidRequest("Set the content safety method to categories or llama_guard (" + field("method") + ").").WithParam("checks")
	}
	if o.Ask != "" && o.Ask != store.SecgwAskTogether && o.Ask != store.SecgwAskSeparately {
		return ErrInvalidRequest("Set ask to together or separately (" + field("ask") + ").").WithParam("checks")
	}
	if m != nil && m.ClassifierRole != store.ClassifierRoleDecision {
		return ErrInvalidRequest("Security categories need a decision model, and " + m.PublicName() + " is not one. For a Llama Guard model choose the \"Llama Guard compatible\" method; otherwise pick a decision model.").WithParam("checks")
	}
	if !c.Enabled {
		// A switched-off check only has to be well-formed: its categories
		// are checked again when it is switched on.
		return nil
	}
	if len(o.Safety) == 0 {
		return ErrInvalidRequest("Pick at least one security category for the content safety check.").WithParam("checks")
	}
	if len(o.Safety) > store.MaxSecgwCategoriesPerCheck {
		return ErrInvalidRequest("A content safety check can ask about at most " + strconv.Itoa(store.MaxSecgwCategoriesPerCheck) + " categories. Remove some, or split them over two policies.").WithParam("checks")
	}
	cats, err := s.Store.SecgwCategoryMap(ctx)
	if err != nil {
		return err
	}
	for _, slug := range o.Safety {
		if cats[slug] == nil {
			return ErrInvalidRequest("Security category " + slug + " does not exist or is turned off. Turn it on under Security → Categories, or remove it from the check.").WithParam("checks")
		}
	}
	return nil
}
