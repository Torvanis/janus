package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// fakeGoogle answers like Google's Gemini API on both routes Janus uses:
// the OpenAI-compatible chat endpoint, which hides thinking (it is what
// total_tokens holds beyond prompt and completion), and the native
// generateContent, which counts it in usageMetadata.thoughtsTokenCount.
//   - gemini-2.5-flash thinks by default and not at all with a budget of 0.
//   - gemini-3.8-flash still thinks with a budget of 0, and refuses "minimal".
//   - gemini-2.5-pro cannot stop thinking: it refuses "none".
//   - On the native API the 2.x models refuse any thinkingLevel (they take a
//     thinkingBudget), with Google's own words and shape.
//   - nativeToolsDown makes native requests with a Google-run tool fail
//     (503), as dev's 2.5 Flash did (a 30 s timeout): its thinking switch
//     then stays unmeasured and the probe asks quietly by level.
type fakeGoogle struct {
	*httptest.Server
	mu              sync.Mutex
	requests        []googleRequest
	nativeToolsDown bool
}

type googleRequest struct {
	native bool
	model  string
	path   string
	raw    string
	body   map[string]any
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{}
	refuse := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"error": map[string]any{"code": 400, "message": msg, "status": "INVALID_ARGUMENT"}}})
	}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		req := googleRequest{body: body, path: r.URL.Path, raw: string(raw)}
		stream := strings.Contains(r.URL.Path, ":streamGenerateContent")
		if i := strings.LastIndex(r.URL.Path, ":"); i >= 0 && strings.HasSuffix(strings.ToLower(r.URL.Path), "generatecontent") {
			req.native, req.model = true, strings.TrimPrefix(r.URL.Path[:i], "/v1beta/models/")
		} else {
			req.model, _ = body["model"].(string)
		}
		g.mu.Lock()
		g.requests = append(g.requests, req)
		toolsDown := g.nativeToolsDown
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		flash38 := req.model == "gemini-3.8-flash"
		if req.native {
			if toolsDown && body["tools"] != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 503, "message": "The model is overloaded.", "status": "UNAVAILABLE"}})
				return
			}
			tc, _ := body["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
			if _, level := tc["thinkingLevel"]; level && strings.HasPrefix(req.model, "gemini-2.") {
				// Live, gemini-2.5-flash: 137 bytes, pretty-printed.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("{\n  \"error\": {\n    \"code\": 400,\n    \"message\": \"Thinking level is not supported for this model.\",\n    \"status\": \"INVALID_ARGUMENT\"\n  }\n}\n"))
				return
			}
			thoughts := 92
			if tc != nil {
				if budget, set := tc["thinkingBudget"].(float64); set && budget == 0 {
					thoughts = 0
					if flash38 {
						thoughts = 47
					}
				}
			}
			usage := map[string]any{"promptTokenCount": 17, "candidatesTokenCount": 4, "totalTokenCount": 21 + thoughts}
			if thoughts > 0 {
				usage["thoughtsTokenCount"] = thoughts
			}
			// Sound and video arrive as inlineData. 2.5 Flash hears and
			// sees them; 3.8 Flash (here) mishears the number and refuses
			// the video.
			answer := "1591"
			switch {
			case strings.Contains(string(raw), `"mimeType":"audio/wav"`) && flash38:
				answer = "17"
			case strings.Contains(string(raw), `"mimeType":"audio/wav"`):
				answer = "47"
			case strings.Contains(string(raw), `"mimeType":"video/mp4"`) && flash38:
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 400, "message": "Unsupported MIME type: video/mp4", "status": "INVALID_ARGUMENT"}})
				return
			case strings.Contains(string(raw), `"mimeType":"video/mp4"`):
				answer = "Blue"
			}
			reply := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model",
				"parts": []any{map[string]any{"text": answer}}}, "finishReason": "STOP"}}, "usageMetadata": usage}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				frame, _ := json.Marshal(reply)
				_, _ = w.Write(append(append([]byte("data: "), frame...), '\n', '\n'))
				return
			}
			_ = json.NewEncoder(w).Encode(reply)
			return
		}
		thoughts := 36
		if effort, has := body["reasoning_effort"].(string); has {
			switch {
			case effort == "minimal" && flash38:
				refuse(w, "Thinking level MINIMAL is not supported for this model. Please retry with other thinking level.")
				return
			case effort == "none" && flash38:
				thoughts = 68
			case effort == "none" && req.model == "gemini-2.5-pro":
				refuse(w, "Budget 0 is invalid. This model only works in thinking mode.")
				return
			case effort == "none":
				thoughts = 0
			case effort == "minimal", effort == "low", effort == "medium", effort == "high", effort == "xhigh":
				thoughts = 120
			default:
				refuse(w, "Invalid reasoning_effort: "+effort+". Valid values are: high, low, medium, minimal, none, xhigh")
				return
			}
		}
		msg := map[string]any{"role": "assistant", "content": "43"}
		if strings.Contains(string(raw), "image_url") {
			msg["content"] = "Green"
		}
		if _, has := body["tools"]; has {
			msg["content"] = nil
			msg["tool_calls"] = []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "janus_probe_time", "arguments": "{}"}}}
		}
		if _, has := body["response_format"]; has {
			msg["content"] = `{"ok":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 17, "completion_tokens": 2, "total_tokens": 19 + thoughts}})
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGoogle) sent(model string) []googleRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []googleRequest
	for _, r := range g.requests {
		if r.model == model {
			out = append(out, r)
		}
	}
	return out
}

// The probe measures whether a Gemini model's thinking turns off, through
// the adapter on both routes, and Chat is then offered Off only where it
// does.
func TestCapabilityProbeGeminiThinkingOff(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g := newFakeGoogle(t)
	up, err := h.store.CreateUpstream(ctx, "google", "gemini", g.URL+"/v1beta/openai", "", "")
	if err != nil {
		t.Fatal(err)
	}
	admin := adminSession(t, h)
	measure := func(name string) *store.Model {
		t.Helper()
		return h.measureGemini(t, admin, h.enableGemini(t, up.ID, name))
	}

	// The off form as each route received it: plain chat with
	// reasoning_effort "none", and the native body with thinkingBudget 0
	// beside the Google-run tool Chat sends.
	offForms := func(name string) (plain, native []map[string]any) {
		for _, r := range g.sent(name) {
			if r.native && r.body["tools"] != nil {
				native = append(native, r.body)
			} else if r.body["reasoning_effort"] == "none" && strings.Contains(string(mustJSON(t, r.body)), "37 × 43") {
				plain = append(plain, r.body)
			}
		}
		return plain, native
	}

	flash25 := measure("gemini-2.5-flash")
	caps := flash25.Capabilities
	if off := caps[adapter.CapThinkingOff]; off.Value != adapter.ThinkingOffBudgetZero || off.Source != capability.SourceProbe || !strings.Contains(off.Note, "0 thinking tokens") {
		t.Errorf("2.5 flash off: %+v", off)
	}
	if def := caps[adapter.CapThinkingDefault]; def.Value != true {
		t.Errorf("2.5 flash thinks by default (36 hidden tokens): %+v", def)
	}
	if l, _ := caps[adapter.CapThinkingLevels].Value.([]string); strings.Join(l, ",") != "none,minimal,low,medium,high,xhigh" {
		t.Errorf("2.5 flash levels: %+v", caps[adapter.CapThinkingLevels])
	}
	plain, native := offForms("gemini-2.5-flash")
	if len(plain) != 1 || len(native) != 1 {
		t.Fatalf("off forms: %d plain, %d native", len(plain), len(native))
	}
	gen, _ := native[0]["generationConfig"].(map[string]any)
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if budget, set := tc["thinkingBudget"].(float64); !set || budget != 0 {
		t.Errorf("native off form: %v", native[0])
	}
	if tools := string(mustJSON(t, native[0]["tools"])); !strings.Contains(tools, "url_context") {
		t.Errorf("native off form should carry Chat's Google-run tool: %s", tools)
	}

	// Sound and video went as Chat sends them: the adapter took the
	// input_audio and video_url parts to the native API as inlineData.
	inline := map[string]string{}
	for _, r := range g.sent("gemini-2.5-flash") {
		contents, _ := r.body["contents"].([]any)
		if !r.native || len(contents) == 0 {
			continue
		}
		for _, part := range contents[0].(map[string]any)["parts"].([]any) {
			if d, ok := part.(map[string]any)["inlineData"].(map[string]any); ok {
				inline[d["mimeType"].(string)] = d["data"].(string)
			}
		}
	}
	if inline["audio/wav"] != base64.StdEncoding.EncodeToString(capability.ProbeSound()) ||
		inline["video/mp4"] != base64.StdEncoding.EncodeToString(capability.ProbeVideo()) {
		t.Errorf("native media parts: %d types, sound %d chars, video %d chars", len(inline), len(inline["audio/wav"]), len(inline["video/mp4"]))
	}
	for key, note := range map[string]string{adapter.CapInAudio: "number spoken", adapter.CapInVideo: "color of a test video"} {
		if c := caps[key]; c.Value != true || c.Source != capability.SourceProbe || !strings.Contains(c.Note, note) {
			t.Errorf("2.5 flash %s: %+v", key, c)
		}
	}

	flash38 := measure("gemini-3.8-flash")
	caps = flash38.Capabilities
	if off := caps[adapter.CapThinkingOff]; off.Value != adapter.ThinkingOffNever || off.Source != capability.SourceProbe || !strings.Contains(off.Note, "68 thinking tokens") {
		t.Errorf("3.8 flash off: %+v", off)
	}
	if l, _ := caps[adapter.CapThinkingLevels].Value.([]string); strings.Join(l, ",") != "none,low,medium,high,xhigh" {
		t.Errorf("3.8 flash levels: %+v", caps[adapter.CapThinkingLevels])
	}
	if note := caps[adapter.CapThinkingLevels].Note; !strings.Contains(note, "MINIMAL is not supported") {
		t.Errorf("3.8 flash levels note: %q", note)
	}
	if _, native := offForms("gemini-3.8-flash"); len(native) != 0 {
		t.Errorf("it thought on the plain route; the native one need not be asked: %d", len(native))
	}
	// A wrong number is not evidence; a refusal naming the video's type is a no.
	if c, known := caps[adapter.CapInAudio]; known {
		t.Errorf("3.8 flash misheard the number, so sound stays not known: %+v", c)
	}
	if c := caps[adapter.CapInVideo]; c.Value != false || c.Source != capability.SourceProbe || !strings.Contains(c.Note, "Unsupported MIME type") {
		t.Errorf("3.8 flash video: %+v", c)
	}

	// Chat's picker reads the same answer: Off for 2.5 Flash, "always thinks" for 3.8 Flash.
	h.server.InvalidateConfigCache()
	rec := h.do(http.MethodGet, "/api/v1/models", nil)
	var list struct {
		Models []struct {
			Name        string `json:"name"`
			ThinkingOff string `json:"thinking_off"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %v", rec.Code, err)
	}
	got := map[string]string{}
	for _, m := range list.Models {
		got[m.Name] = m.ThinkingOff
	}
	if got["gemini-2.5-flash"] != adapter.ThinkingOffBudgetZero || got["gemini-3.8-flash"] != adapter.ThinkingOffNever {
		t.Errorf("thinking_off: %v", got)
	}
}

// enableGemini adds an enabled, priced model granted to everyone on a Gemini
// upstream, with what Google's model list says: it thinks, and nothing
// about turning that off.
func (h *harness) enableGemini(t *testing.T, upstreamID, name string) *store.Model {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, upstreamID, name, []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, upstreamID, name)
	if err != nil {
		t.Fatal(err)
	}
	h.record(t, m.ID, capability.SourceProvider, map[string]any{adapter.CapThinking: true})
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 1_000_000_000, RateOutNano: 2_000_000_000, EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	return m
}

// measureGemini runs Measure on m and returns the model once it finished.
func (h *harness) measureGemini(t *testing.T, admin *auth.Session, m *store.Model) *store.Model {
	t.Helper()
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+m.ID+"/capabilities/probe", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("probe %s: %d %s", m.Name, rec.Code, rec.Body.String())
	}
	done := h.probeDone(t, m.ID)
	if done.CapabilityProbe.Status != capability.ProbeOK {
		t.Fatalf("%s run: %+v", m.Name, done.CapabilityProbe)
	}
	return done
}

func TestGeminiProbeThoughts(t *testing.T) {
	for name, tc := range map[string]struct {
		raw      string
		thoughts int64
		counted  bool
	}{
		"compat, hidden thinking":  {`{"usage":{"prompt_tokens":17,"completion_tokens":2,"total_tokens":55}}`, 36, true},
		"compat, none":             {`{"usage":{"prompt_tokens":17,"completion_tokens":2,"total_tokens":19}}`, 0, true},
		"native, thoughts":         {`{"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":3,"totalTokenCount":112,"thoughtsTokenCount":92}}`, 92, true},
		"native, none":             {`{"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":2,"totalTokenCount":19}}`, 0, true},
		"no usage is not evidence": {`{"choices":[]}`, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			if n, counted := geminiProbeThoughts([]byte(tc.raw)); n != tc.thoughts || counted != tc.counted {
				t.Errorf("got %d %v, want %d %v", n, counted, tc.thoughts, tc.counted)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
