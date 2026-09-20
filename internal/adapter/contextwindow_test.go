package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDiscoveryCapturesContextWindow uses the exact /v1/models payload shapes
// the real upstreams return, so the parser is proven against production data
// rather than an invented fixture.
func TestDiscoveryCapturesContextWindow(t *testing.T) {
	cases := []struct {
		name    string
		adapter string
		body    string
		model   string
		want    int64
	}{
		{
			// Captured from llama-cpp-q38-flashnext in the live cluster.
			name:    "llama.cpp nests it under meta.n_ctx",
			adapter: "llama_cpp",
			body:    `{"object":"list","data":[{"id":"qwen3.8-flash-next","object":"model","meta":{"n_vocab":248320,"n_ctx":131072,"n_ctx_train":262144}}]}`,
			model:   "qwen3.8-flash-next",
			want:    131072,
		},
		{
			name:    "vLLM reports max_model_len",
			adapter: "vlm",
			body:    `{"object":"list","data":[{"id":"deepseek","object":"model","max_model_len":163840}]}`,
			model:   "deepseek",
			want:    163840,
		},
		{
			// Hosted OpenAI-compatible APIs report no context field at all.
			name:    "absent field stays unknown rather than guessed",
			adapter: "openai_compatible",
			body:    `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model"}]}`,
			model:   "gpt-4o-mini",
			want:    0,
		},
		{
			// Served length wins over trained length: a caller must respect
			// what the server was actually started with.
			name:    "n_ctx preferred over n_ctx_train",
			adapter: "llama_cpp",
			body:    `{"object":"list","data":[{"id":"m","object":"model","meta":{"n_ctx":8192,"n_ctx_train":262144}}]}`,
			model:   "m",
			want:    8192,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			a, err := Get(tc.adapter)
			if err != nil {
				t.Fatalf("adapter %q: %v", tc.adapter, err)
			}
			models, err := a.Discover(context.Background(), Upstream{Name: "u", BaseURL: srv.URL}, srv.Client())
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			var got int64 = -1
			for _, m := range models {
				if m.Name == tc.model {
					got = m.ContextWindow
				}
			}
			if got != tc.want {
				t.Errorf("context window = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOllamaContextWindowFromShow covers the two-call path: the tag list has
// no context length, so the adapter asks /api/show and reads the
// architecture-prefixed key.
func TestOllamaContextWindowFromShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3.8:27b","model":"qwen3.8:27b"}]}`))
		case "/api/show":
			// Real shape from the live Dragon upstream.
			_, _ = w.Write([]byte(`{"model_info":{"general.architecture":"qwen35","qwen35.context_length":262144}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a, err := Get("ollama")
	if err != nil {
		t.Fatalf("ollama adapter: %v", err)
	}
	models, err := a.Discover(context.Background(), Upstream{Name: "Dragon", BaseURL: srv.URL}, srv.Client())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("want 1 model, got %d", len(models))
	}
	if models[0].ContextWindow != 262144 {
		t.Errorf("context window = %d, want 262144", models[0].ContextWindow)
	}
}

// TestOllamaShowFailureIsNotFatal: /api/show is best-effort. A provider that
// refuses it must still yield a usable model list with an honest blank.
func TestOllamaShowFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"m","model":"m"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a, _ := Get("ollama")
	models, err := a.Discover(context.Background(), Upstream{Name: "Dragon", BaseURL: srv.URL}, srv.Client())
	if err != nil {
		t.Fatalf("discovery must survive a failing /api/show: %v", err)
	}
	if len(models) != 1 || models[0].ContextWindow != 0 {
		t.Errorf("got %+v, want one model with unknown (0) context window", models)
	}
}
