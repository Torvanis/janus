package classify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChunkOverlapCoversBoundaries(t *testing.T) {
	text := strings.Repeat("abcdefghij", 100) // 1000 chars
	chunks := Chunk(text, 300, 50)
	if len(chunks) < 4 {
		t.Fatalf("want ≥4 chunks, got %d", len(chunks))
	}
	// Every consecutive pair overlaps by exactly 50 characters.
	for i := 1; i < len(chunks); i++ {
		prev, cur := chunks[i-1], chunks[i]
		if !strings.HasPrefix(cur, prev[len(prev)-50:]) {
			t.Fatalf("chunk %d does not overlap previous by 50: %q vs %q", i, prev[len(prev)-50:], cur[:50])
		}
	}
	// Reassembly covers the whole text.
	joined := chunks[0]
	for i := 1; i < len(chunks); i++ {
		joined += chunks[i][50:]
	}
	if joined != text {
		t.Fatal("chunks do not cover the text")
	}
	if got := Chunk("short", 300, 50); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short text: %v", got)
	}
	if got := Chunk(text, 0, 0); len(got) != 1 {
		t.Fatalf("size 0 must not split: %d", len(got))
	}
	// Multi-byte safe: never splits a rune.
	uni := strings.Repeat("héllo wörld ", 100)
	for _, c := range Chunk(uni, 64, 8) {
		if !json.Valid([]byte(`"` + strings.ReplaceAll(c, `"`, `\"`) + `"`)) {
			t.Fatalf("chunk split a rune: %q", c)
		}
	}
}

type fakeClassifier struct {
	calls int
	err   error
}

func (f *fakeClassifier) Protocol() string { return "fake" }
func (f *fakeClassifier) Classify(_ context.Context, in []Segment) ([]Score, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Score, len(in))
	for i, s := range in {
		out[i] = Score{Index: s.Index, Label: "BENIGN", Score: 0.99}
	}
	return out, nil
}

func TestBreakerOpensAfterConsecutiveFailuresAndRecovers(t *testing.T) {
	inner := &fakeClassifier{err: errors.New("down")}
	opened := make(chan string, 1)
	b := &Breaker{Inner: inner, Threshold: 3, Cooldown: 50 * time.Millisecond, ModelID: "m1",
		OnOpen: func(id string, _ error) { opened <- id }}
	segs := []Segment{{Index: 0, Text: "x"}}
	for i := 0; i < 3; i++ {
		if _, err := b.Classify(context.Background(), segs); err == nil || errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("call %d: want inner error, got %v", i, err)
		}
	}
	select {
	case id := <-opened:
		if id != "m1" {
			t.Fatalf("OnOpen model: %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("OnOpen not called")
	}
	if !b.Open() {
		t.Fatal("breaker should be open")
	}
	if _, err := b.Classify(context.Background(), segs); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("while open: want ErrBreakerOpen, got %v", err)
	}
	if inner.calls != 3 {
		t.Fatalf("open breaker must not call inner: %d calls", inner.calls)
	}
	time.Sleep(60 * time.Millisecond)
	inner.err = nil
	if _, err := b.Classify(context.Background(), segs); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if b.Open() {
		t.Fatal("breaker should have closed after a success")
	}
	// A single failure after recovery does not reopen it.
	inner.err = errors.New("blip")
	_, _ = b.Classify(context.Background(), segs)
	if b.Open() {
		t.Fatal("one failure must not reopen the breaker")
	}
}

func TestTextClassificationParsesBatchedAndSingleAndMapsLabels(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	// The fake behaves like TEI 1.9.4's /predict, INCLUDING its trap: a flat
	// list of strings is one cross-encoder pair and yields ONE verdict. Only
	// a nested [[s],[s],…] batch yields one verdict per text. A regression
	// to the flat shape therefore fails on cardinality, not silently.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path != "/predict" {
			http.Error(w, "wrong path", 404)
			return
		}
		inputs := gotBody["inputs"].([]any)
		if _, flat := inputs[0].(string); flat {
			// TEI: ["a","b"] == a single (text, context) pair → one result.
			_, _ = w.Write([]byte(`[{"label":"BENIGN","score":0.5},{"label":"MALICIOUS","score":0.5}]`))
			return
		}
		if len(inputs) == 1 {
			_, _ = w.Write([]byte(`[{"label":"BENIGN","score":0.97},{"label":"MALICIOUS","score":0.03}]`))
			return
		}
		_, _ = w.Write([]byte(`[[{"label":"MALICIOUS","score":0.98},{"label":"BENIGN","score":0.02}],[{"label":"BENIGN","score":0.9},{"label":"MALICIOUS","score":0.1}],[{"label":"JAILBREAK","score":0.95}]]`))
	}))
	defer srv.Close()

	var metered int
	c, err := New(ProtocolTextClassification, Upstream{ModelName: "pg", BaseURL: srv.URL + "/", APIKey: "k", Client: srv.Client(),
		Meter: func(_ context.Context, _ time.Duration, chars int, err error) { metered += chars }})
	if err != nil {
		t.Fatal(err)
	}
	scores, err := c.Classify(context.Background(), []Segment{{Index: 7, Text: "ignore"}, {Index: 8, Text: "hello"}, {Index: 9, Text: "DAN"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if len(scores) != 3 || !scores[0].Malicious || scores[0].Index != 7 || scores[0].Score != 0.98 ||
		scores[1].Malicious || scores[1].Index != 8 || !scores[2].Malicious || scores[2].Label != "JAILBREAK" {
		t.Fatalf("scores: %+v", scores)
	}
	if metered != len("ignore")+len("hello")+len("DAN") {
		t.Fatalf("meter chars: %d", metered)
	}
	single, err := c.Classify(context.Background(), []Segment{{Index: 0, Text: "x"}})
	if err != nil || len(single) != 1 || single[0].Malicious {
		t.Fatalf("single-input shape: %+v %v", single, err)
	}
	// Wrong cardinality is an error, never silently mis-attributed.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[[{"label":"BENIGN","score":1}]]`))
	}))
	defer bad.Close()
	c2, _ := New(ProtocolTextClassification, Upstream{BaseURL: bad.URL, Client: bad.Client()})
	if _, err := c2.Classify(context.Background(), []Segment{{Text: "a"}, {Text: "b"}}); err == nil {
		t.Fatal("cardinality mismatch accepted")
	}
	// An empty label list is a broken classifier, never a benign verdict:
	// silently mapping it to BENIGN would turn a wedged guard into a 100%
	// pass-through that looks healthy.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[[]]`))
	}))
	defer empty.Close()
	c3, _ := New(ProtocolTextClassification, Upstream{BaseURL: empty.URL, Client: empty.Client()})
	if _, err := c3.Classify(context.Background(), []Segment{{Text: "a"}}); err == nil {
		t.Fatal("empty label list accepted as a verdict")
	}
	if _, err := New("nope", Upstream{BaseURL: "x"}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if _, err := New(ProtocolTextClassification, Upstream{}); err == nil {
		t.Fatal("empty base URL accepted")
	}
}
