package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func capsOf(t *testing.T, provider, row string) Capabilities {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row), &m); err != nil {
		t.Fatal(err)
	}
	return rowCapabilities(provider, m)
}

// Each provider says what a model can do in its own words. These rows are
// the shapes recorded in the provider research (2026-11-06).
func TestRowCapabilitiesByProvider(t *testing.T) {
	for _, tc := range []struct {
		name, provider, row string
		want                Capabilities
		absent              []string
	}{
		{
			name: "anthropic: effort levels, thinking that cannot be turned off", provider: "anthropic",
			row: `{"id":"claude-fable-5","max_tokens":64000,"capabilities":{"image_input":{"supported":true},"pdf_input":{"supported":true},
				"structured_outputs":{"supported":true},"effort":{"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":true}},
				"thinking":{"types":{"adaptive":{"supported":true},"enabled":{"supported":false},"disabled":{"supported":false}}}}}`,
			want: Capabilities{CapInText: true, CapOutText: true, CapInImage: true, CapInFile: true, CapStructured: true, CapThinking: true,
				CapThinkingOff: ThinkingOffNever, CapThinkingLevels: []string{"low", "medium", "high", "max"}, CapOutputCap: OutputCapMaxTokens, CapMaxOutput: int64(64000)},
		},
		{
			name: "anthropic: thinking can be disabled", provider: "anthropic",
			row:  `{"id":"claude-haiku-5-5","capabilities":{"image_input":{"supported":true},"thinking":{"types":{"adaptive":{"supported":true},"disabled":{"supported":true}}}}}`,
			want: Capabilities{CapInText: true, CapOutText: true, CapInImage: true, CapThinking: true, CapThinkingOff: ThinkingOffDisabled, CapOutputCap: OutputCapMaxTokens},
		},
		{
			name: "xai: the accepted effort values, no none", provider: "xai",
			row: `{"id":"grok-4.7","input_modalities":["text","image"],"output_modalities":["text"],"capabilities":{"reasoning_effort":["low","medium","high","xhigh"],"default_reasoning_effort":"high"}}`,
			want: Capabilities{CapInText: true, CapInImage: true, CapInAudio: false, CapInVideo: false, CapInFile: false, CapOutText: true, CapThinking: true,
				CapThinkingLevels: []string{"low", "medium", "high", "xhigh"}, CapThinkingOff: ThinkingOffNever, CapThinkingDefaultLevel: "high", CapThinkingDefault: true},
		},
		{
			name: "xai: none is accepted and the default is low", provider: "xai",
			row:  `{"id":"grok-4.3","capabilities":{"reasoning_effort":["none","low","medium","high","xhigh"],"default_reasoning_effort":"low"}}`,
			want: Capabilities{CapThinking: true, CapThinkingLevels: []string{"none", "low", "medium", "high", "xhigh"}, CapThinkingOff: ThinkingOffEffortNone, CapThinkingDefaultLevel: "low", CapThinkingDefault: true},
		},
		{
			name: "xai: a model with no reasoning", provider: "xai",
			row:  `{"id":"grok-4.20-non-reasoning","capabilities":{"reasoning_effort":[]}}`,
			want: Capabilities{CapThinking: false, CapThinkingLevels: []string{}},
		},
		{
			name: "openrouter: parameters, modalities, mandatory reasoning", provider: "openrouter",
			row: `{"id":"openai/gpt-6-sol","architecture":{"input_modalities":["text","image","file"],"output_modalities":["text"]},
				"supported_parameters":["tools","tool_choice","reasoning","structured_outputs","max_completion_tokens","seed"],
				"top_provider":{"max_completion_tokens":128000},"reasoning":{"supported_efforts":["high","medium","low"],"default_effort":"medium","default_enabled":true,"mandatory":true}}`,
			want: Capabilities{CapInText: true, CapInImage: true, CapInFile: true, CapInAudio: false, CapInVideo: false, CapOutText: true, CapTools: true, CapStructured: true,
				CapThinking: true, CapThinkingLevels: []string{"high", "medium", "low"}, CapThinkingDefaultLevel: "medium", CapThinkingDefault: true, CapThinkingOff: ThinkingOffNever,
				CapOutputCap: OutputCapMaxCompletionTokens, CapRefusedParams: []string{"temperature", "top_p"}, CapMaxOutput: int64(128000)},
		},
		{
			name: "openrouter: null efforts means all, so the list is not known", provider: "openrouter",
			row:  `{"id":"x/y","supported_parameters":["max_tokens","temperature","top_p"],"reasoning":{"supported_efforts":null,"mandatory":false}}`,
			want: Capabilities{CapThinking: true, CapThinkingOff: ThinkingOffEffortNone, CapTools: false, CapStructured: false, CapOutputCap: OutputCapMaxTokens}, absent: []string{CapThinkingLevels, CapRefusedParams},
		},
		{
			name: "mistral: booleans", provider: "mistral",
			row:  `{"id":"mistral-small-2603","capabilities":{"completion_chat":true,"function_calling":true,"vision":true,"reasoning":true,"audio":false}}`,
			want: Capabilities{CapInImage: true, CapInAudio: false, CapTools: true, CapThinking: true},
		},
		{
			name: "deepseek: effort object", provider: "deepseek",
			row: `{"id":"deepseek-v4-flash","max_output_tokens":393216,"input_modalities":["text","image"],"output_modalities":["text"],"effort":{"supported_levels":["low","high","max"],"default_level":"high"}}`,
			want: Capabilities{CapInText: true, CapInImage: true, CapInAudio: false, CapInVideo: false, CapInFile: false, CapOutText: true, CapThinking: true,
				CapThinkingLevels: []string{"low", "high", "max"}, CapThinkingDefaultLevel: "high", CapMaxOutput: int64(393216)},
		},
		{
			name: "moonshot: flat booleans", provider: "moonshot",
			row:  `{"id":"kimi-k3","supports_image_in":true,"supports_video_in":false,"supports_reasoning":true}`,
			want: Capabilities{CapInImage: true, CapInVideo: false, CapThinking: true},
		},
		{
			name: "cerebras: capabilities, limits and refused parameters", provider: "cerebras",
			row:  `{"id":"gpt-oss-120b","capabilities":{"vision":false,"reasoning":true,"tools":true,"structured_outputs":true},"limits":{"max_completion_tokens":40000},"supported_parameters":{"temperature":true,"presence_penalty":false}}`,
			want: Capabilities{CapInImage: false, CapThinking: true, CapTools: true, CapStructured: true, CapMaxOutput: int64(40000), CapRefusedParams: []string{"presence_penalty"}},
		},
		{
			name: "gemini native: thinking flag and output limit", provider: "google",
			row:  `{"name":"models/gemini-4-argon","thinking":true,"outputTokenLimit":65536,"inputTokenLimit":1048576}`,
			want: Capabilities{CapThinking: true, CapMaxOutput: int64(65536)},
		},
		{
			name: "bedrock: modalities in capitals", provider: "bedrock",
			row:  `{"modelId":"amazon.nova-pro-v1:0","inputModalities":["TEXT","IMAGE","VIDEO"],"outputModalities":["TEXT"]}`,
			want: Capabilities{CapInText: true, CapInImage: true, CapInVideo: true, CapInAudio: false, CapInFile: false, CapOutText: true},
		},
		{
			name: "openai: the list says nothing, so nothing is claimed", provider: "openai",
			row:  `{"id":"gpt-6-luna","object":"model","created":1,"owned_by":"openai"}`,
			want: Capabilities{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capsOf(t, tc.provider, tc.row)
			for k, want := range tc.want {
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
			for k := range got {
				if _, ok := tc.want[k]; !ok {
					t.Errorf("unexpected %s = %#v", k, got[k])
				}
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("%s should be unknown, got %#v", k, got[k])
				}
			}
		})
	}
}

// A hostile or broken list never panics and never claims anything.
func TestRowCapabilitiesMalformed(t *testing.T) {
	for _, row := range []string{`{}`, `{"capabilities":"yes"}`, `{"input_modalities":"text"}`, `{"capabilities":{"reasoning_effort":"high","vision":"true"}}`,
		`{"reasoning":[],"effort":7,"supported_parameters":42,"architecture":[1]}`, `{"capabilities":{"thinking":{"types":3},"effort":[]}}`} {
		if got := capsOf(t, "openrouter", row); len(got) != 0 && !reflect.DeepEqual(got, Capabilities{CapThinking: true}) {
			// {"reasoning":[]} is not an object, so even that must be empty.
			t.Errorf("%s => %#v", row, got)
		}
		_ = capsOf(t, "anthropic", row)
	}
	if got := rowCapabilities("xai", nil); len(got) != 0 {
		t.Errorf("nil row => %#v", got)
	}
}

func TestCapabilityProvider(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.openai.com/v1": "openai", "https://api.x.ai": "xai", "https://api.mistral.ai": "mistral", "https://api.deepseek.com": "deepseek",
		"https://api.moonshot.ai/v1": "moonshot", "https://api.cerebras.ai": "cerebras", "https://api.groq.com/openai": "groq",
		"https://api.together.xyz": "together", "https://api.fireworks.ai/inference": "fireworks", "http://vllm.internal:8000": "", "https://api.mistral.ai.evil.example": "",
	} {
		if got := CapabilityProvider("openai_compatible", base); got != want {
			t.Errorf("%s => %q, want %q", base, got, want)
		}
	}
	if CapabilityProvider("vlm", "https://api.mistral.ai") != "" {
		t.Error("the provider is only recognized on the adapter that speaks to it")
	}
}

// Discovery reads the richer lists: xAI's language-models for modalities,
// Ollama's /api/show, llama.cpp's /props.
func TestDiscoveryReadsCapabilities(t *testing.T) {
	t.Run("ollama", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/tags":
				_, _ = w.Write([]byte(`{"models":[{"name":"qwen3.5:8b"},{"name":"nomic-embed-text"}]}`))
			case "/api/show":
				var body struct{ Model string }
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Model == "qwen3.5:8b" {
					_, _ = w.Write([]byte(`{"model_info":{"qwen35.context_length":40960},"capabilities":["completion","vision","tools","thinking"]}`))
				} else {
					_, _ = w.Write([]byte(`{"model_info":{},"capabilities":["embedding"]}`))
				}
			}
		}))
		defer srv.Close()
		models, err := (&Ollama{}).Discover(context.Background(), Upstream{Name: "o", BaseURL: srv.URL}, srv.Client())
		if err != nil || len(models) != 2 {
			t.Fatalf("discover: %v %+v", err, models)
		}
		chat := models[0].Capabilities
		if chat[CapInImage] != true || chat[CapTools] != true || chat[CapThinking] != true || chat[CapThinkingOff] != nil || models[0].ContextWindow != 40960 {
			t.Errorf("chat model: %#v window %d", chat, models[0].ContextWindow)
		}
		if embed := models[1].Capabilities; embed[CapOutEmbedding] != true || embed[CapInImage] != false || embed[CapThinking] != false {
			t.Errorf("embedding model: %#v", embed)
		}
	})
	t.Run("llama.cpp", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"Qwen3-8B-Q4_K_M.gguf","meta":{"n_ctx":8192}}]}`))
			case "/props":
				_, _ = w.Write([]byte(`{"model_alias":"qwen","modalities":{"vision":true,"audio":false},"chat_template_caps":{"supports_tools":true,"supports_tool_calls":true,"supports_reasoning_effort":true,"reasoning_effort_levels":["none","low","high"]}}`))
			}
		}))
		defer srv.Close()
		models, err := (&OpenAICompatible{name: "llama_cpp"}).Discover(context.Background(), Upstream{Name: "l", BaseURL: srv.URL}, srv.Client())
		if err != nil || len(models) != 1 {
			t.Fatalf("discover: %v %+v", err, models)
		}
		c := models[0].Capabilities
		if c[CapInImage] != true || c[CapInAudio] != false || c[CapTools] != true || c[CapThinkingOff] != ThinkingOffEffortNone || !reflect.DeepEqual(c[CapThinkingLevels], []string{"none", "low", "high"}) {
			t.Errorf("llama.cpp: %#v", c)
		}
	})
	t.Run("a failing detail call costs the detail only", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
				return
			}
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()
		models, err := (&OpenAICompatible{name: "llama_cpp"}).Discover(context.Background(), Upstream{Name: "l", BaseURL: srv.URL}, srv.Client())
		if err != nil || len(models) != 1 || len(models[0].Capabilities) != 0 {
			t.Fatalf("discover: %v %+v", err, models)
		}
	})
}
