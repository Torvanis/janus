package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/classtrace"
	"github.com/torvanis/janus/internal/decisions"
)

// SystemOne talks to a System One decision API: TypeSafe
// (api.typesafe.ai/v1/systemone), OpenRouter (/api/alpha/decisions),
// Ollama and llama.cpp (/v1/systemone), or Bespoke's Nimble server.
type SystemOne struct {
	URL    string // full endpoint URL
	APIKey string
	Model  string
	Client *http.Client
	// Style decides how several questions are sent. StyleCombined puts
	// them in one request; StyleSeparate sends one request per question,
	// in parallel. Some decision models (Nimble) see every question of a
	// request while answering each one, so related questions in one
	// request shift each other's answers; separate requests keep each
	// answer independent at the cost of reading the prompt once per call.
	Style string
	// MaxOptions is the most options one choice question may carry on
	// this server (Ollama: 26). 0 = no known limit.
	MaxOptions int
	// Label names the classifier in traces ("Nimble (ai2) / nimble-9b").
	Label string
	// Dialect is the wire format: decisions.DialectSystemOne (default) or
	// decisions.DialectOpenAI (api.openai.com/v1/decisions).
	Dialect string
}

// Call styles.
const (
	StyleCombined = "combined"
	StyleSeparate = "separate"
)

// SystemOneEndpoint resolves the decision endpoint for a provider base URL.
// OpenRouter serves decisions at /api/alpha/decisions; every other known
// server uses /v1/systemone. A base URL that already names the endpoint is
// used as is.
func SystemOneEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case strings.HasSuffix(base, "/systemone"), strings.HasSuffix(base, "/decisions"):
		return base
	case strings.Contains(base, "openrouter.ai"):
		// https://openrouter.ai/api or https://openrouter.ai/api/v1
		base = strings.TrimSuffix(base, "/v1")
		if !strings.HasSuffix(base, "/api") {
			base += "/api"
		}
		return base + "/alpha/decisions"
	case strings.HasSuffix(base, "/v1"):
		return base + "/systemone"
	default:
		return base + "/v1/systemone"
	}
}

// Protocol names the wire protocol.
func (s *SystemOne) Protocol() string {
	if s.Dialect == decisions.DialectOpenAI {
		return "openai_decisions"
	}
	return "systemone"
}

type s1Instructions struct {
	Question    string `json:"question"`
	CountsAsYes string `json:"counts_as_yes,omitempty"`
	CountsAsNo  string `json:"counts_as_no,omitempty"`
}

type s1Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type s1Request struct {
	Model     string                `json:"model"`
	State     string                `json:"state"`
	Questions map[string]s1Question `json:"questions"`
}

type s1Response struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Noul          *float64           `json:"noul"`
		Confidence    *float64           `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int64    `json:"input_tokens"`
		OutputTokens int64    `json:"output_tokens"`
		Cost         *float64 `json:"cost"`
	} `json:"usage"`
}

// ErrTooManyOptions is returned when a choice question has more options
// than the server accepts in one question.
var ErrTooManyOptions = errors.New("question has more options than this classifier accepts")

func wireQuestion(q Question) s1Question {
	if q.IsYesNo() {
		var instr any = q.Instructions
		if q.YesMeans != "" || q.NoMeans != "" {
			instr = s1Instructions{Question: q.Instructions, CountsAsYes: q.YesMeans, CountsAsNo: q.NoMeans}
		}
		return s1Question{Type: QuestionYesNo, Instructions: instr}
	}
	crit := map[string]string{}
	for _, o := range q.Options {
		desc := o.Name
		if o.Description != "" {
			desc = o.Name + ": " + o.Description
		}
		crit[o.Slug] = desc
	}
	return s1Question{Type: QuestionChoice, Instructions: q.Instructions, Criteria: crit}
}

// Classify answers every question. With StyleSeparate and more than one
// question it sends one request per question concurrently and merges the
// answers (usage summed); otherwise one request carries them all. A
// question marked Optional never fails the call: whatever goes wrong with
// it is recorded in Result.Missing.
func (s *SystemOne) Classify(ctx context.Context, text string, questions []Question) (Result, error) {
	var missing map[string]error
	asked := make([]Question, 0, len(questions))
	for _, q := range questions {
		if !q.IsYesNo() && s.MaxOptions > 0 && len(q.Options) > s.MaxOptions {
			err := fmt.Errorf("%w (%d options, limit %d)", ErrTooManyOptions, len(q.Options), s.MaxOptions)
			if !q.Optional {
				return Result{}, err
			}
			if missing == nil {
				missing = map[string]error{}
			}
			missing[q.ID] = err
			continue
		}
		asked = append(asked, q)
	}
	withMissing := func(r Result) Result {
		for id, err := range missing {
			r.miss(id, err)
		}
		return r
	}
	if len(asked) == 0 {
		return withMissing(Result{Answers: map[string]Answer{}}), nil
	}
	if s.Style != StyleSeparate || len(asked) < 2 {
		r, err := s.call(ctx, text, asked)
		if err != nil {
			return Result{}, err
		}
		return withMissing(r), nil
	}
	type part struct {
		res Result
		err error
	}
	parts := make([]part, len(asked))
	var wg sync.WaitGroup
	for i, q := range asked {
		wg.Add(1)
		go func(i int, q Question) {
			defer wg.Done()
			// Each request carries one question; label its trace with
			// the feature that asked it.
			cctx := ctx
			if f := featureOf(ctx, q.ID); f != "" {
				cctx = WithFeature(ctx, f, ModeFrom(ctx))
			}
			r, err := s.call(cctx, text, []Question{q})
			parts[i] = part{r, err}
		}(i, q)
	}
	wg.Wait()
	out := Result{Answers: map[string]Answer{}}
	required := false
	for i, p := range parts {
		if p.err != nil {
			if !asked[i].Optional {
				return Result{}, p.err
			}
			out.miss(asked[i].ID, p.err)
			out.Calls++ // the request was made
			continue
		}
		required = required || !asked[i].Optional
		for id, a := range p.res.Answers {
			out.Answers[id] = a
		}
		for id, err := range p.res.Missing {
			out.miss(id, err)
		}
		out.Usage.TokensIn += p.res.Usage.TokensIn
		out.Usage.TokensOut += p.res.Usage.TokensOut
		out.Usage.TokensCached += p.res.Usage.TokensCached
		out.Usage.CostUSD += p.res.Usage.CostUSD
		out.Usage.CostReported = out.Usage.CostReported || p.res.Usage.CostReported
		out.Calls += p.res.Calls
		if p.res.Model != "" {
			out.Model = p.res.Model
		}
	}
	if !required && len(out.Answers) == 0 {
		// Every question was optional and every one failed: report the
		// first failure as the call's error.
		return Result{}, out.MissingErr(idsOf(asked))
	}
	return withMissing(out), nil
}

func idsOf(qs []Question) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.ID
	}
	return out
}

func (s *SystemOne) call(ctx context.Context, text string, questions []Question) (res Result, err error) {
	ids := make([]string, 0, len(questions))
	for _, q := range questions {
		ids = append(ids, q.ID)
	}
	var body []byte
	var canon decisions.Request
	if s.Dialect == decisions.DialectOpenAI {
		canon = canonicalRequest(text, questions)
		body, err = decisions.EncodeOpenAI(canon, s.Model)
	} else {
		req := s1Request{Model: s.Model, State: text, Questions: map[string]s1Question{}}
		for _, q := range questions {
			req.Questions[q.ID] = wireQuestion(q)
		}
		body, err = json.Marshal(req)
	}
	if err != nil {
		return Result{}, err
	}
	started := time.Now()
	var raw []byte
	if classtrace.Enabled(ctx) {
		defer func() {
			c := classtrace.Call{Feature: FeatureFrom(ctx), Classifier: s.Label, Protocol: s.Protocol(), URL: s.URL, Mode: ModeFrom(ctx),
				StartedAt: started.UTC(), LatencyMs: time.Since(started).Milliseconds(), Status: "ok", Questions: ids, Request: body}
			if json.Valid(raw) {
				c.Response = raw
			}
			if err != nil {
				c.Status, c.Error = "error", sanitizeError(err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
					c.Status = "timeout"
				}
			}
			classtrace.Record(ctx, c)
		}()
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if s.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		err = fmt.Errorf("systemone request: %w", err)
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		err = fmt.Errorf("systemone read: %w", err)
		return Result{}, err
	}
	if resp.StatusCode/100 != 2 {
		err = fmt.Errorf("%s: HTTP %d: %s", s.Protocol(), resp.StatusCode, withholdEcho(errorSnippet(raw), text))
		return Result{}, err
	}
	if s.Dialect == decisions.DialectOpenAI {
		return openAIResult(raw, canon, questions)
	}
	var out s1Response
	if err = json.Unmarshal(raw, &out); err != nil {
		err = fmt.Errorf("systemone: unparseable response: %w", err)
		return Result{}, err
	}
	res = Result{Answers: map[string]Answer{}, Model: out.Model, Calls: 1,
		Usage: Usage{TokensIn: out.Usage.InputTokens, TokensOut: out.Usage.OutputTokens}}
	if out.Usage.Cost != nil {
		res.Usage.CostUSD, res.Usage.CostReported = *out.Usage.Cost, true
	}
	for _, q := range questions {
		a, ok := out.Answers[q.ID]
		var bad error
		switch {
		case !ok:
			bad = fmt.Errorf("%w for %s", ErrNoAnswer, q.ID)
		case q.IsYesNo() && a.Noul == nil:
			bad = fmt.Errorf("%w for %s: no yes/no probability", ErrNoAnswer, q.ID)
		case !q.IsYesNo() && !HasSlug(q.Options, a.Choice):
			bad = fmt.Errorf("%w for %s", ErrNoAnswer, q.ID)
		}
		if bad != nil {
			if q.Optional {
				res.miss(q.ID, bad)
				continue
			}
			err = bad
			return Result{}, err
		}
		if q.IsYesNo() {
			res.Answers[q.ID] = Answer{Yes: clamp01(*a.Noul)}
			continue
		}
		conf := a.Probabilities[a.Choice]
		if a.Confidence != nil && len(a.Probabilities) == 0 {
			conf = *a.Confidence
		}
		res.Answers[q.ID] = Answer{Choice: a.Choice, Confidence: conf, Probabilities: a.Probabilities}
	}
	return res, nil
}

// errorSnippet returns a short, single-line excerpt of an error body.
func errorSnippet(raw []byte) string {
	var env struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error != nil {
		if m, ok := env.Error.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok {
				raw = []byte(msg)
			}
		} else if s, ok := env.Error.(string); ok {
			raw = []byte(s)
		}
	}
	s := strings.Join(strings.Fields(string(raw)), " ")
	return TruncateUTF8(s, 200)
}

// withholdEcho replaces a provider error message that quotes the classified
// text: errors are stored and shown to administrators, prompts never are.
func withholdEcho(msg, text string) string {
	if echoes(msg, text) {
		return "the provider's error message quoted the request text and was withheld"
	}
	return msg
}

// echoes reports whether msg contains a run of at least 24 bytes of text
// (whitespace-normalised, as errorSnippet normalises msg).
func echoes(msg, text string) bool {
	const window = 24
	norm := strings.Join(strings.Fields(text), " ")
	if len(msg) < window || len(norm) < window {
		return false
	}
	for i := 0; i+window <= len(norm); i += window / 2 {
		if strings.Contains(msg, norm[i:i+window]) {
			return true
		}
	}
	return false
}

type featureKey struct{}
type modeKey struct{}
type questionFeaturesKey struct{}

// WithQuestionFeatures records which feature asked which question id, so a
// call sent per question is traced under its own feature.
func WithQuestionFeatures(ctx context.Context, m map[string]string) context.Context {
	return context.WithValue(ctx, questionFeaturesKey{}, m)
}

func featureOf(ctx context.Context, questionID string) string {
	m, _ := ctx.Value(questionFeaturesKey{}).(map[string]string)
	return m[questionID]
}

// WithFeature labels classifier calls made under ctx for traces.
func WithFeature(ctx context.Context, feature, mode string) context.Context {
	return context.WithValue(context.WithValue(ctx, featureKey{}, feature), modeKey{}, mode)
}

// FeatureFrom returns the feature label in ctx.
func FeatureFrom(ctx context.Context) string { v, _ := ctx.Value(featureKey{}).(string); return v }

// ModeFrom returns the mode label in ctx.
func ModeFrom(ctx context.Context) string { v, _ := ctx.Value(modeKey{}).(string); return v }

// canonicalRequest is the classifier's questions in the dialect-neutral form
// (choice options keep the System One wording: "Name: description").
func canonicalRequest(text string, questions []Question) decisions.Request {
	r := decisions.Request{Parts: []decisions.Part{{Text: text}}}
	for _, q := range questions {
		cq := decisions.Question{Name: q.ID, Instructions: q.Instructions}
		if q.IsYesNo() {
			cq.Kind, cq.YesMeans, cq.NoMeans = decisions.KindYesNo, q.YesMeans, q.NoMeans
		} else {
			cq.Kind = decisions.KindChoice
			for _, o := range q.Options {
				desc := o.Name
				if o.Description != "" {
					desc = o.Name + ": " + o.Description
				}
				cq.Choices = append(cq.Choices, decisions.Choice{Value: o.Slug, Description: desc})
			}
		}
		r.Questions = append(r.Questions, cq)
	}
	return r
}

// openAIResult reads an OpenAI Decisions response. A refused question is a
// failed answer, never a guess: the caller's failure rule applies.
func openAIResult(raw []byte, canon decisions.Request, questions []Question) (Result, error) {
	resp, err := decisions.ParseOpenAIResponse(raw, canon.Questions)
	if err != nil {
		return Result{}, fmt.Errorf("openai_decisions: %w", err)
	}
	res := Result{Answers: map[string]Answer{}, Model: resp.Model, Calls: 1,
		Usage: Usage{TokensIn: resp.Usage.InputTokens, TokensOut: resp.Usage.OutputTokens, TokensCached: resp.Usage.CachedTokens}}
	for i, q := range questions {
		a := resp.Answers[i]
		var bad error
		switch {
		case a.Kind == decisions.KindRefusal:
			bad = fmt.Errorf("%w for %s: the model declined to answer", ErrNoAnswer, q.ID)
		case !q.IsYesNo() && !HasSlug(q.Options, a.Choice):
			bad = fmt.Errorf("%w for %s", ErrNoAnswer, q.ID)
		}
		if bad != nil {
			if q.Optional {
				res.miss(q.ID, bad)
				continue
			}
			return Result{}, bad
		}
		switch {
		case q.IsYesNo():
			res.Answers[q.ID] = Answer{Yes: clamp01(a.Probability)}
		default:
			probs := map[string]float64{}
			for _, p := range a.Probabilities {
				probs[p.Value] = p.Probability
			}
			res.Answers[q.ID] = Answer{Choice: a.Choice, Confidence: probs[a.Choice], Probabilities: probs}
		}
	}
	return res, nil
}
