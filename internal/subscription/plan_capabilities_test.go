package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
)

func TestPlanDefaultsTable(t *testing.T) {
	cases := []struct {
		provider, model string
		want            map[string]any
	}{
		// ChatGPT plan: images and tools for GPT-5+, and the route's own
		// limit (no sound, no video) for every model.
		{"openai", "gpt-6.1-sol", map[string]any{adapter.CapInImage: true, adapter.CapTools: true, adapter.CapInAudio: false, adapter.CapInVideo: false}},
		{"openai", "GPT-5.5", map[string]any{adapter.CapInImage: true, adapter.CapTools: true, adapter.CapInAudio: false, adapter.CapInVideo: false}},
		{"openai", "some-future-model", map[string]any{adapter.CapInAudio: false, adapter.CapInVideo: false}},
		// xAI: Grok 4 and Grok Build families documented as text+image in.
		{"xai", "grok-4.3", map[string]any{adapter.CapInImage: true, adapter.CapInAudio: false, adapter.CapInVideo: false, adapter.CapTools: true}},
		{"xai", "grok-build-0.1", map[string]any{adapter.CapInImage: true, adapter.CapInAudio: false, adapter.CapInVideo: false, adapter.CapTools: true}},
		// Not documented as chat models taking anything: unknown.
		{"xai", "grok-imagine-video", map[string]any{}},
		// Copilot and Mistral report per model; nothing is assumed.
		{"copilot", "gpt-4o", map[string]any{}},
		{"mistral", "mistral-large-latest", map[string]any{}},
		{"nobody", "x", map[string]any{}},
	}
	for _, c := range cases {
		got := map[string]any{}
		for k, f := range PlanDefaults(c.provider, c.model) {
			got[k] = f.Value
			if f.Note == "" {
				t.Errorf("%s/%s %s: a default must say where it comes from", c.provider, c.model, k)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s: defaults %v, want %v", c.provider, c.model, got, c.want)
		}
	}
	if len(PlanAutoMeasure) != 0 {
		t.Fatalf("no plan vendor is measured without its owner asking: %v", PlanAutoMeasure)
	}
}

// What each vendor's own model list says becomes the "reported" layer.
func TestPlanCatalogsReportCapabilities(t *testing.T) {
	ctx := context.Background()

	t.Run("copilot supports.vision", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-4o","capabilities":{"type":"chat","supports":{"vision":true,"tool_calls":true,"streaming":true},"limits":{"max_output_tokens":4096}}},
				{"id":"gpt-3.5-turbo","capabilities":{"type":"chat","supports":{"tool_calls":true}}},
				{"id":"claude-x","capabilities":{"type":"chat","supports":{"vision":false,"reasoning_effort":["low","high"]}}}]}`))
		}))
		defer srv.Close()
		_, _, caps, err := FetchModels(ctx, &Copilot{CopilotAPI: srv.URL}, nil, "t")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]adapter.Capabilities{
			"gpt-4o":        {adapter.CapInImage: true, adapter.CapTools: true, adapter.CapMaxOutput: int64(4096), adapter.CapThinkingLevels: []string{}},
			"gpt-3.5-turbo": {adapter.CapTools: true, adapter.CapThinkingLevels: []string{}},
			"claude-x":      {adapter.CapInImage: false, adapter.CapThinkingLevels: []string{"low", "high"}},
		}
		if !reflect.DeepEqual(caps, want) {
			t.Fatalf("copilot caps %#v", caps)
		}
	})

	t.Run("openai input_modalities", func(t *testing.T) {
		access := fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1"}})
		o := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"models":[
				{"slug":"gpt-6.1-sol","priority":1,"input_modalities":["text","image"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"default_reasoning_level":"high"},
				{"slug":"gpt-5.5","priority":2}]}`))
		})
		_, _, caps, err := FetchModels(ctx, o, nil, access)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]adapter.Capabilities{"gpt-6.1-sol": {
			adapter.CapInText: true, adapter.CapInImage: true, adapter.CapInAudio: false, adapter.CapInVideo: false,
			adapter.CapThinkingLevels: []string{"low", "high"}, adapter.CapThinkingDefaultLevel: "high",
		}}
		if !reflect.DeepEqual(caps, want) {
			t.Fatalf("openai caps %#v", caps)
		}
	})

	t.Run("mistral capabilities", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[
				{"id":"mistral-large-latest","capabilities":{"completion_chat":true,"vision":true,"function_calling":true}},
				{"id":"codestral-latest","capabilities":{"completion_chat":true,"vision":false,"function_calling":true}}]}`))
		}))
		defer srv.Close()
		_, _, caps, err := FetchModels(ctx, &Mistral{APIURL: srv.URL}, nil, "k")
		if err != nil {
			t.Fatal(err)
		}
		if caps["mistral-large-latest"][adapter.CapInImage] != true || caps["codestral-latest"][adapter.CapInImage] != false ||
			caps["codestral-latest"][adapter.CapTools] != true {
			t.Fatalf("mistral caps %#v", caps)
		}
	})

	t.Run("xai language-models, best effort", func(t *testing.T) {
		withList := true
		x := fakeXAI(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.3"},{"id":"grok-imagine-image"},{"id":"grok-4.3-latest"}]}`))
			case "/v1/language-models":
				if !withList {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = w.Write([]byte(`{"models":[{"id":"grok-4.3","aliases":["grok-4.3-latest"],"input_modalities":["text","image"],"output_modalities":["text"]}]}`))
			default:
				w.WriteHeader(404)
			}
		})
		ids, _, caps, err := FetchModels(ctx, x, nil, "t")
		if err != nil || len(ids) != 3 {
			t.Fatalf("ids=%v err=%v", ids, err)
		}
		for _, m := range []string{"grok-4.3", "grok-4.3-latest"} {
			if caps[m][adapter.CapInImage] != true || caps[m][adapter.CapInAudio] != false {
				t.Fatalf("%s caps %#v", m, caps[m])
			}
		}
		if _, said := caps["grok-imagine-image"]; said {
			t.Fatalf("a model the list does not describe reports nothing: %#v", caps)
		}
		// A plan sign-in that cannot read the second list still lists models.
		withList = false
		ids, _, caps, err = FetchModels(ctx, x, nil, "t")
		if err != nil || len(ids) != 3 || len(caps) != 0 {
			t.Fatalf("without language-models: ids=%v caps=%v err=%v", ids, caps, err)
		}
	})
}

// The defaults are the weakest layer: the vendor's list, a measurement and
// the owner's setting each replace them, and the source says which won.
func TestPlanDefaultsResolveUnderEveryOtherSource(t *testing.T) {
	defaults := PlanDefaults("openai", "gpt-6.1-sol")
	var st capability.State
	if r := st.ResolveWith(defaults)[adapter.CapInImage]; r.Value != true || r.Source != capability.SourceDefault || r.Note == "" {
		t.Fatalf("default only: %#v", r)
	}
	st.SetProvider(capability.Facts{adapter.CapInImage: {Value: false}})
	if r := st.ResolveWith(defaults)[adapter.CapInImage]; r.Value != false || r.Source != capability.SourceProvider {
		t.Fatalf("reported beats default: %#v", r)
	}
	st.Probe = capability.Facts{adapter.CapInImage: {Value: true}}
	if r := st.ResolveWith(defaults)[adapter.CapInImage]; r.Value != true || r.Source != capability.SourceProbe {
		t.Fatalf("measured beats reported: %#v", r)
	}
	if err := st.SetAdmin(map[string]any{adapter.CapInImage: false}, st.Probe[adapter.CapInImage].At); err != nil {
		t.Fatal(err)
	}
	if r := st.ResolveWith(defaults)[adapter.CapInImage]; r.Value != false || r.Source != capability.SourceAdmin {
		t.Fatalf("owner's setting beats all: %#v", r)
	}
	if v, known := st.ProfileWith(defaults).Bool(adapter.CapInAudio); !known || v {
		t.Fatalf("an untouched key keeps its default: %v %v", v, known)
	}
	if _, known := st.Profile().Bool(adapter.CapInAudio); known {
		t.Fatal("defaults are never part of the stored state")
	}
}
