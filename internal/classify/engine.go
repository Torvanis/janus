package classify

import (
	"github.com/torvanis/janus/internal/classtrace"

	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Plan is one immutable configuration snapshot: who classifies, against
// what, and how. The gateway rebuilds it when settings or the taxonomy
// change; jobs carry the plan they were admitted under.
type Plan struct {
	Backend       Backend
	Taxonomy      Taxonomy
	Mode          string // "async" or "inline"
	SampleRate    int    // 1-100
	Timeout       time.Duration
	TextScope     string
	IncludeSystem bool
	MaxChars      int
	Subcategories bool
	// Classifier is the label stored with each result ("Openrouter /
	// ~typesafe/jev-latest"), so latency can be compared across changes.
	Classifier string
	// PriceNano prices a classification from its summed usage and the
	// number of classifier requests it took (per-request fees apply per
	// call). Nil prices from the provider's reported charge only.
	PriceNano func(u Usage, calls int) int64
	// Key identifies the decision classifier ("upstream_id/model") so the
	// Security Gateway can carry this plan's question on a guard call to
	// the same classifier. Empty for chat classifiers.
	Key string
	// Style is how the classifier takes several questions; questions ride
	// along on a guard call only when it takes them combined.
	Style string
	// MaxOptions is the most options the classifier accepts in one choice
	// question (0 = no known limit). A taxonomy that fits is asked as ONE
	// flat question (every category and subcategory as its own option),
	// which measured both fastest and most accurate; a larger one falls
	// back to category first, then subcategory.
	MaxOptions int
}

// Flat reports whether the taxonomy is asked as one flat question.
func (p *Plan) Flat() bool {
	if p == nil || p.Backend == nil || p.Backend.Protocol() == "chat" {
		return false
	}
	if !p.Subcategories {
		return false
	}
	return p.MaxOptions <= 0 || p.Taxonomy.Count() <= p.MaxOptions
}

// Questions returns the question(s) the plan asks first: one flat
// question, or the top-level category question.
func (p *Plan) Questions() []Question {
	if p.Flat() {
		return []Question{p.Taxonomy.FlatQuestion()}
	}
	return []Question{p.Taxonomy.CategoryQuestion()}
}

// Interpret turns the answer to Questions()[0] into an outcome's category,
// subcategory, confidence and per-category probabilities. For a two-stage
// plan the subcategory is filled in by the second call.
func (p *Plan) Interpret(a Answer) (category, subcategory string, confidence float64, probs map[string]float64) {
	if !p.Flat() {
		return a.Choice, "", a.Confidence, a.Probabilities
	}
	cat, sub := SplitFlat(a.Choice)
	// The category's probability is the sum over its leaves, so a request
	// split between "Coding" and "Coding > Rust" still reads as confidently
	// Coding.
	top := map[string]float64{}
	for k, v := range a.Probabilities {
		c, _ := SplitFlat(k)
		top[c] += v
	}
	conf := top[cat]
	if conf == 0 {
		conf = a.Confidence
	}
	return cat, sub, conf, top
}

// Sampled reports whether this request is in the sample.
func (p *Plan) Sampled() bool {
	if p == nil || p.Backend == nil || p.Taxonomy.Empty() {
		return false
	}
	return p.SampleRate >= 100 || rand.IntN(100) < p.SampleRate
}

// Outcome is a finished classification.
type Outcome struct {
	Status        string // ok, error, timeout, dropped
	Err           string
	Category      string
	Subcategory   string
	Confidence    float64
	Probabilities map[string]float64
	Mode          string
	Protocol      string
	Classifier    string
	Model         string
	Timing        Timing
	InputChars    int
	Usage         Usage
	CostNano      int64
	// Calls is how many requests the classifier received for this outcome.
	Calls int
}

// Label renders the outcome for a response header: "coding/rust".
func (o Outcome) Label() string {
	if o.Status != "ok" {
		return ""
	}
	if o.Subcategory != "" {
		return o.Category + "/" + o.Subcategory
	}
	return o.Category
}

// Run classifies text under plan, within plan.Timeout. It never panics on a
// misbehaving backend's answer; every failure becomes an error outcome.
func Run(ctx context.Context, plan *Plan, text string) Outcome {
	started := time.Now()
	out := run(ctx, plan, text)
	out.Timing.Elapsed = time.Since(started)
	return out
}

func run(ctx context.Context, plan *Plan, text string) Outcome {
	text = trimText(text, plan.MaxChars)
	out := Outcome{Mode: plan.Mode, Protocol: plan.Backend.Protocol(), Classifier: plan.Classifier, InputChars: len([]rune(text))}
	ctx, cancel := context.WithTimeout(ctx, plan.Timeout)
	defer cancel()

	questions := plan.Questions()
	catQ := questions[0]
	chat := plan.Backend.Protocol() == "chat"
	if chat && plan.Subcategories {
		// A chat model sees the whole tree and answers both in one call.
		for _, root := range plan.Taxonomy.Roots {
			if q, ok := plan.Taxonomy.SubcategoryQuestion(root); ok {
				q.ID = "subcategory:" + root.Slug
				questions = append(questions, q)
			}
		}
	}
	res, err := plan.Backend.Classify(ctx, text, questions)
	if err != nil {
		if !errors.Is(err, ErrTooManyOptions) {
			out.Calls = 1 // the request was made (and waited for)
		}
		return out.fail(ctx, err)
	}
	out.add(res)
	cat := res.Answers[catQ.ID]
	out.Status = "ok"
	out.Category, out.Subcategory, out.Confidence, out.Probabilities = plan.Interpret(cat)
	if out.Subcategory == "none" {
		out.Subcategory = ""
	}
	if !plan.Subcategories || plan.Flat() {
		return out.priced(plan)
	}
	if chat {
		if sub, ok := res.Answers["subcategory"]; ok && sub.Choice != "none" {
			out.Subcategory = sub.Choice
		}
		return out.priced(plan)
	}
	// Decision models: a second, single question only when the chosen
	// category has subcategories.
	var parent Option
	for _, r := range plan.Taxonomy.Roots {
		if r.Slug == out.Category {
			parent = r
		}
	}
	subQ, ok := plan.Taxonomy.SubcategoryQuestion(parent)
	if !ok {
		return out.priced(plan)
	}
	res2, err := plan.Backend.Classify(ctx, text, []Question{subQ})
	if err != nil {
		// The category stands; record why the refinement is missing.
		if !errors.Is(err, ErrTooManyOptions) {
			out.Calls++
		}
		out.Err = "subcategory: " + sanitizeError(err)
		return out.priced(plan)
	}
	out.add(res2)
	if sub := res2.Answers[subQ.ID]; sub.Choice != "none" {
		out.Subcategory = sub.Choice
	}
	return out.priced(plan)
}

func (o *Outcome) add(r Result) {
	if r.Calls == 0 {
		r.Calls = 1
	}
	o.Calls += r.Calls
	o.Usage.TokensIn += r.Usage.TokensIn
	o.Usage.TokensOut += r.Usage.TokensOut
	o.Usage.TokensCached += r.Usage.TokensCached
	o.Usage.CostUSD += r.Usage.CostUSD
	o.Usage.CostReported = o.Usage.CostReported || r.Usage.CostReported
	if r.Model != "" {
		o.Model = r.Model
	}
}

func (o Outcome) priced(plan *Plan) Outcome {
	switch {
	case o.Calls == 0:
		// No request of its own (answered on another feature's call).
		o.CostNano = 0
	case plan.PriceNano != nil:
		// The gateway's pricer applies the model's cost source (reported
		// charge, rate card, request fee per call).
		o.CostNano = plan.PriceNano(o.Usage, o.Calls)
	case o.Usage.CostUSD > 0:
		o.CostNano = int64(o.Usage.CostUSD*1e9 + 0.5)
	}
	return o
}

func (o Outcome) fail(ctx context.Context, err error) Outcome {
	o.Status = "error"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		o.Status = "timeout"
	}
	o.Err = sanitizeError(err)
	return o
}

// Job is one request waiting for async classification.
type Job struct {
	Plan     *Plan
	Text     string
	Enqueued time.Time
	// Done receives the outcome; it persists the result.
	Done func(Outcome)
	// Trace receives the job's classifier calls when troubleshooting is
	// capturing the request (optional).
	Trace classtrace.Sink
}

// jobContext is the context a job's calls run under: detached from the
// request, labelled for traces.
func jobContext(ctx context.Context, j *Job) context.Context {
	return WithFeature(classtrace.With(ctx, j.Trace), "usage_insights", j.Plan.Mode)
}

// Runner owns the async queue and its workers. One per gateway process.
// The queue and worker count follow the settings: workers above the current
// target exit after their current job; QueueSize is enforced on admission.
type Runner struct {
	queue    chan *Job
	pending  atomic.Int64
	maxQueue atomic.Int64
	target   atomic.Int64
	running  atomic.Int64
	inline   atomic.Int64
	mu       sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
	// Observe is called for every finished outcome (metrics).
	Observe func(Outcome)
}

// maxQueueCapacity is the hard ceiling of the channel; the admin's queue size
// is enforced under it.
const maxQueueCapacity = 100000

// NewRunner starts a runner with no workers; Configure starts them.
func NewRunner() *Runner {
	return &Runner{queue: make(chan *Job, maxQueueCapacity), stop: make(chan struct{})}
}

// Configure applies the worker count and queue bound.
func (r *Runner) Configure(workers, queueSize int) {
	if workers < 1 {
		workers = 1
	}
	if queueSize > maxQueueCapacity {
		queueSize = maxQueueCapacity
	}
	r.maxQueue.Store(int64(queueSize))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target.Store(int64(workers))
	for r.running.Load() < int64(workers) {
		r.running.Add(1)
		go r.worker()
	}
}

// retire reports whether this worker should exit because more are running
// than configured; when it says yes the worker is already uncounted.
func (r *Runner) retire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running.Load() > r.target.Load() {
		r.running.Add(-1)
		return true
	}
	return false
}

// Close stops the workers; queued jobs are abandoned.
func (r *Runner) Close() { r.stopOnce.Do(func() { close(r.stop) }) }

// Pending is the async backlog.
func (r *Runner) Pending() int64 { return r.pending.Load() }

// Enqueue admits a job without blocking. A full queue drops it and reports
// the drop through Done, so the miss is visible in the classifier health.
func (r *Runner) Enqueue(j *Job) bool {
	if r.pending.Load() >= r.maxQueue.Load() {
		r.drop(j, "queue full")
		return false
	}
	j.Enqueued = time.Now()
	r.pending.Add(1)
	select {
	case r.queue <- j:
		return true
	default:
		r.pending.Add(-1)
		r.drop(j, "queue full")
		return false
	}
}

func (r *Runner) drop(j *Job, why string) {
	o := Outcome{Status: "dropped", Err: why, Mode: j.Plan.Mode, Protocol: j.Plan.Backend.Protocol(), Classifier: j.Plan.Classifier}
	r.finish(j, o)
}

func (r *Runner) finish(j *Job, o Outcome) {
	if r.Observe != nil {
		r.Observe(o)
	}
	if j.Done != nil {
		j.Done(o)
	}
}

func (r *Runner) worker() {
	for {
		// Scale down: any worker beyond the target retires (counted under
		// the lock, so Configure cannot race a retirement into too few).
		if r.retire() {
			return
		}
		select {
		case <-r.stop:
			r.running.Add(-1)
			return
		case j := <-r.queue:
			r.pending.Add(-1)
			queued := time.Since(j.Enqueued)
			o := Run(jobContext(context.Background(), j), j.Plan, j.Text)
			o.Timing.Queue = queued
			r.finish(j, o)
		}
	}
}

// Inline classifies on the caller's goroutine within the plan's timeout,
// limited to limit concurrent inline calls per process. When the limit is
// reached the job goes to the async queue instead, so a slow classifier can
// never stack up request latency. ok tells the caller a label is
// available; ran tells it the request waited on the classifier (false when
// the job overflowed to the queue).
func (r *Runner) Inline(ctx context.Context, j *Job, limit int) (o Outcome, ok, ran bool) {
	if r.inline.Add(1) > int64(limit) {
		r.inline.Add(-1)
		async := *j.Plan
		async.Mode = "async"
		j.Plan = &async
		r.Enqueue(j)
		return Outcome{}, false, false
	}
	defer r.inline.Add(-1)
	o = Run(jobContext(ctx, j), j.Plan, j.Text)
	r.finish(j, o)
	return o, o.Status == "ok", true
}

// FromResult builds an outcome from an answer obtained elsewhere (a guard
// call that carried this plan's question). elapsed is the shared call's
// time; it is what the request waited for.
func FromResult(plan *Plan, res Result, err error, elapsed time.Duration, inputChars int) Outcome {
	out := Outcome{Mode: plan.Mode, Protocol: plan.Backend.Protocol(), Classifier: plan.Classifier, InputChars: inputChars}
	out.Timing.Elapsed = elapsed
	if err != nil {
		out.Status, out.Err = "error", sanitizeError(err)
		return out
	}
	q := plan.Questions()[0]
	a, ok := res.Answers[q.ID]
	if !ok {
		why := res.MissingErr([]string{q.ID})
		if why == nil {
			why = ErrNoAnswer
		}
		out.Status, out.Err = "error", sanitizeError(why)
		return out
	}
	out.Status = "ok"
	out.Category, out.Subcategory, out.Confidence, out.Probabilities = plan.Interpret(a)
	if out.Subcategory == "none" {
		out.Subcategory = ""
	}
	// The call was shared: its usage and cost are the guard's, metered
	// there, so this outcome has no calls and no cost of its own.
	out.Model = res.Model
	return out
}

// Finish runs the runner's observe + persist hooks for an outcome produced
// outside it.
func (r *Runner) Finish(j *Job, o Outcome) { r.finish(j, o) }
