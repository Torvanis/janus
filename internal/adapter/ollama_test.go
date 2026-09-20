package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaDiscoverUsesNativeTags(t *testing.T) {
	a := &Ollama{}
	// Discovery now makes a follow-up /api/show call per model to read the
	// context window, so record every path rather than just the last one.
	var paths []string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/api/show" {
			// No model_info: the context window stays unknown, which must
			// not disturb the tag-list results asserted below.
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"llama3.2:latest","model":"llama3.2:latest","details":{"family":"llama"}},
			{"name":"","model":"fallback-model:7b"},
			{"name":"nomic-embed-text:latest"},
			{"name":"","model":""}
		]}`))
	}))
	defer srv.Close()

	models, err := a.Discover(context.Background(), Upstream{BaseURL: srv.URL, APIKey: "secret"}, srv.Client())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(paths) == 0 || paths[0] != "/api/tags" {
		t.Errorf("discovery paths = %v, want /api/tags first (native endpoint, not the OpenAI shim)", paths)
	}
	for _, p := range paths {
		if p != "/api/tags" && p != "/api/show" {
			t.Errorf("unexpected discovery path %q", p)
		}
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the configured bearer key", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("discovered %d models, want 3 (empty entries skipped): %+v", len(models), models)
	}
	if models[0].Name != "llama3.2:latest" {
		t.Errorf("models[0] = %q, want llama3.2:latest", models[0].Name)
	}
	if models[1].Name != "fallback-model:7b" {
		t.Errorf("models[1] = %q, want the `model` field as fallback when `name` is empty", models[1].Name)
	}
	if len(models[2].Modalities) != 1 || models[2].Modalities[0] != ModalityEmbedding {
		t.Errorf("nomic-embed-text modalities = %v, want [%s]", models[2].Modalities, ModalityEmbedding)
	}
}

func TestOllamaDiscoverRejectsBadPayload(t *testing.T) {
	a := &Ollama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	if _, err := a.Discover(context.Background(), Upstream{BaseURL: srv.URL}, srv.Client()); err == nil {
		t.Fatal("Discover must fail on an unparseable tag list")
	}
}

func TestOllamaPrepareForwardsThroughOpenAIShim(t *testing.T) {
	a := &Ollama{}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Authorization", "Bearer janus-downstream-token")

	prep, err := a.Prepare(Upstream{BaseURL: "http://ollama.example.test:11434/", APIKey: "up-key"}, &Request{
		Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr,
		Model: "llama3.2", Streaming: true,
		Body: []byte(`{"model":"llama3.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := "http://ollama.example.test:11434/v1/chat/completions"; prep.URL != want {
		t.Errorf("URL = %q, want %q", prep.URL, want)
	}
	if got := prep.Header.Get("Authorization"); got != "Bearer up-key" {
		t.Errorf("Authorization = %q, want the upstream credential, never the downstream token", got)
	}
	if !strings.Contains(string(prep.Body), `"include_usage":true`) {
		t.Errorf("streaming request must opt into usage reporting: %s", prep.Body)
	}
}

func TestOllamaPrepareWithoutKeyDropsDownstreamAuth(t *testing.T) {
	a := &Ollama{}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer janus-downstream-token")
	prep, err := a.Prepare(Upstream{BaseURL: "http://ollama.example.test:11434"}, &Request{
		Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr, Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := prep.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q; the Janus token must never leak to a keyless upstream", got)
	}
}

func TestOllamaExtractUsageUnderstandsBothShapes(t *testing.T) {
	a := &Ollama{}

	// OpenAI shim shape.
	u, ok := a.ExtractUsage([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":4}}`))
	if !ok || u.TokensIn != 9 || u.TokensOut != 4 {
		t.Errorf("OpenAI shape: got %+v ok=%v, want (9,4) reported", u, ok)
	}

	// Native Ollama shape.
	u, ok = a.ExtractUsage([]byte(`{"prompt_eval_count":12,"eval_count":34,"done_reason":"stop"}`))
	if !ok || u.TokensIn != 12 || u.TokensOut != 34 {
		t.Errorf("native shape: got %+v ok=%v, want (12,34) reported", u, ok)
	}
	if u.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want done_reason mapped through", u.FinishReason)
	}

	// Nothing usable.
	if u, ok = a.ExtractUsage([]byte(`{"message":"no counts here"}`)); ok || u.Reported {
		t.Errorf("payload without counts must not report usage: %+v", u)
	}
	if _, ok = a.ExtractUsage([]byte(`not json`)); ok {
		t.Error("invalid JSON must not report usage")
	}
}

func TestOllamaStreamCollectorAccumulates(t *testing.T) {
	c := (&Ollama{}).NewStreamCollector()
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"He"}}]}` + "\n",
		"\n", // keep-alive blank line
		`data: {"prompt_eval_count":15,"eval_count":3}` + "\n",
		`data: {"prompt_eval_count":15,"eval_count":27,"done_reason":"stop"}` + "\n",
		`data: [DONE]` + "\n",
	} {
		c.Feed([]byte(line))
	}
	u := c.Usage()
	if !u.Reported {
		t.Fatal("collector saw native counts but reported nothing")
	}
	if u.TokensIn != 15 || u.TokensOut != 27 {
		t.Errorf("collected tokens = (%d,%d), want (15,27) — max across frames", u.TokensIn, u.TokensOut)
	}
	if u.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", u.FinishReason)
	}
}
