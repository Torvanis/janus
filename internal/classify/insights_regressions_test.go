package classify

// Usage insights regressions found in the release review (area A).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A1: lowering concurrency then raising it again leaves fewer workers than
// configured. Retired workers have ids >= target; Configure only spawns new
// ids while running < workers, but the surviving workers are not 0..n-1
// necessarily, and ids keep growing (nextID), so new workers spawned after a
// scale-down have ids >= the lowered target and immediately retire if the
// target is lowered again; more importantly, after scale 4->2->4 the new
// workers get ids 4,5 which are >= target 4 and retire at once.
func TestInsightsRunnerScaleDownThenUpKeepsConfiguredWorkers(t *testing.T) {
	r := NewRunner()
	defer r.Close()
	r.Configure(4, 100) // ids 0..3
	r.Configure(2, 100) // ids 2,3 retire on their next loop
	// Wake idle workers so 2 and 3 notice the new target: they only check
	// it between jobs.
	plan := &Plan{Backend: blockingBackend{release: closedChan()}, Taxonomy: testTaxonomy(), Mode: "async", Timeout: time.Second, MaxChars: 100}
	for i := 0; i < 8; i++ {
		r.Enqueue(&Job{Plan: plan, Text: "x"})
	}
	waitFor(t, func() bool { return r.running.Load() == 2 && r.Pending() == 0 })
	r.Configure(4, 100) // spawns ids 4,5: id >= target(4) -> they retire at once
	time.Sleep(100 * time.Millisecond)
	if got := r.running.Load(); got != 4 {
		t.Fatalf("after scaling 4 -> 2 -> 4 the runner has %d workers, want 4", got)
	}
}

func closedChan() chan struct{} { c := make(chan struct{}); close(c); return c }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// A5: a chat model that returns the confidence as a string ("0.9") – common
// for small models – makes the whole answer unparseable, so a correct label
// is recorded as an error.
func TestInsightsChatAnswerToleratesStringConfidence(t *testing.T) {
	a, err := parseChatAnswer(`{"category": "coding", "subcategory": "none", "confidence": "0.9"}`)
	if err != nil || a.Category != "coding" {
		t.Fatalf("string confidence rejected the whole answer: %v", err)
	}
}

// A4: when a chat classifier echoes the prompt instead of answering, the
// stored error (classification_result.error, shown in the admin UI and
// written to the DB) carries up to 120 characters of the reply, i.e. of the
// user's prompt.
func TestInsightsChatErrorDoesNotCarryPromptText(t *testing.T) {
	secret := "my SSN is 078-05-1120 please draft a letter to the IRS"
	c := &Chat{Model: "m", Send: func(ctx context.Context, body []byte) ([]byte, Usage, error) {
		return []byte(`{"choices":[{"message":{"content":"Sure! ` + secret + `"}}]}`), Usage{}, nil
	}}
	_, err := c.Classify(context.Background(), secret, []Question{testTaxonomy().CategoryQuestion()})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(sanitizeError(err), "078-05-1120") {
		t.Fatalf("stored classifier error contains prompt text: %q", sanitizeError(err))
	}
}

// A9: error strings are cut at a byte offset (sanitizeError 300, chat
// snippet 120, errorSnippet 200, store 500). A cut inside a multi-byte rune
// yields invalid UTF-8, which Postgres refuses in a TEXT column, so the
// result row (and the miss it records) is lost on Postgres only.
func TestInsightsErrorTruncationKeepsValidUTF8(t *testing.T) {
	c := &Chat{Model: "m", Send: func(ctx context.Context, body []byte) ([]byte, Usage, error) {
		return []byte(`{"choices":[{"message":{"content":"` + strings.Repeat("é", 200) + `"}}]}`), Usage{}, nil
	}}
	_, err := c.Classify(context.Background(), "x", []Question{testTaxonomy().CategoryQuestion()})
	msg := sanitizeError(err)
	if !utf8.ValidString(msg) {
		t.Fatalf("stored error is not valid UTF-8 (Postgres rejects it): %q", msg[len(msg)-8:])
	}
	long := errors.New(strings.Repeat("a", 299) + "é")
	if s := sanitizeError(long); !utf8.ValidString(s) {
		t.Fatalf("sanitizeError cut a rune in half: %q", s[len(s)-4:])
	}
}

// A0 (unit): an optional question with a bad answer is recorded in
// Missing, the required questions still answer, in both call styles and on
// the OpenAI dialect (where a refusal is per question).
func TestInsightsOptionalQuestionNeverFailsTheCall(t *testing.T) {
	srv, _ := fakeSystemOne(t, func(q string, crit map[string]string, state string) string {
		if q == "category" {
			return "not-offered"
		}
		return "legal"
	}, 0)
	defer srv.Close()
	required := Question{ID: "req", Instructions: "x", Options: []Option{{Slug: "legal", Name: "Legal"}, {Slug: "other", Name: "Other"}}}
	opt := testTaxonomy().CategoryQuestion()
	opt.Optional = true
	for _, style := range []string{StyleCombined, StyleSeparate} {
		b := &SystemOne{URL: srv.URL + "/v1/systemone", APIKey: "k", Model: "m", Style: style}
		res, err := b.Classify(context.Background(), "a contract", []Question{required, opt})
		if err != nil {
			t.Fatalf("%s: an optional question's bad answer failed the call: %v", style, err)
		}
		if res.Answers["req"].Choice != "legal" || !errors.Is(res.Missing["category"], ErrNoAnswer) {
			t.Fatalf("%s: answers %+v missing %v", style, res.Answers, res.Missing)
		}
		// Not optional: the call fails as before.
		opt2 := opt
		opt2.Optional = false
		if _, err := b.Classify(context.Background(), "a contract", []Question{required, opt2}); err == nil {
			t.Fatalf("%s: a required bad answer must fail the call", style)
		}
	}
	// Too many options on an optional question: skipped, not sent.
	b := &SystemOne{URL: srv.URL + "/v1/systemone", APIKey: "k", Model: "m", MaxOptions: 2}
	res, err := b.Classify(context.Background(), "a contract", []Question{required, opt})
	if err != nil || !errors.Is(res.Missing["category"], ErrTooManyOptions) {
		t.Fatalf("too many options on an optional question: %v %v", err, res.Missing)
	}
}

func TestInsightsOpenAIRefusalOfOptionalQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"prompt_injection","probability":0.02},{"type":"refusal","name":"category"}],"usage":{"input_tokens":50}}`))
	}))
	defer srv.Close()
	opt := testTaxonomy().CategoryQuestion()
	opt.Optional = true
	b := &SystemOne{URL: srv.URL + "/v1/decisions", Model: "gpt-6-luna", Dialect: "openai"}
	res, err := b.Classify(context.Background(), "x", []Question{InjectionQuestion(), opt})
	if err != nil || res.Answers["prompt_injection"].Yes != 0.02 || res.Missing["category"] == nil {
		t.Fatalf("refusal of the optional question: err=%v res=%+v", err, res)
	}
}

// A3 (unit): a failed or timed-out call still counts as one call.
func TestInsightsFailedCallCountsAsACall(t *testing.T) {
	plan := &Plan{Backend: blockingBackend{release: make(chan struct{})}, Taxonomy: testTaxonomy(), Mode: "inline", Timeout: 30 * time.Millisecond, MaxChars: 100}
	o := Run(context.Background(), plan, "x")
	if o.Status != "timeout" || o.Calls != 1 {
		t.Fatalf("timeout outcome %+v", o)
	}
}
