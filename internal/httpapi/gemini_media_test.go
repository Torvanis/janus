package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
)

// nativeBody is the generateContent body Janus must send for one user turn
// of a question and a media file, with the given generationConfig.
func nativeBody(t *testing.T, question, mime string, data []byte, gen string) map[string]any {
	t.Helper()
	var out map[string]any
	raw := `{"contents":[{"role":"user","parts":[{"text":` + string(mustJSON(t, question)) + `},` +
		`{"inlineData":{"mimeType":"` + mime + `","data":"` + base64.StdEncoding.EncodeToString(data) + `"}}]}],` +
		`"generationConfig":` + gen + `}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// On a test gateway, Measure recorded nothing for sound and video on Gemini 2.5
// Flash: its native thinking check timed out, so the probe asked quietly by
// the lowest level the OpenAI-compatible route takes ("minimal"); sound and
// video go to the native API, where the adapter sent that as thinkingLevel,
// and 2.x refuses any thinkingLevel (400 "Thinking level is not supported
// for this model.", 137 bytes). Now media questions to a Gemini model go
// with "none" where its levels hold it (thinkingBudget 0: thinking made 2.5
// Flash mishear the number), and any other level becomes the thinkingBudget
// Google maps it to (2.5 Pro, which cannot stop thinking). Both record yes.
func TestCapabilityProbeGeminiMediaWithoutOffSwitch(t *testing.T) {
	h := newHarness(t)
	g := newFakeGoogle(t)
	g.nativeToolsDown = true
	up, err := h.store.CreateUpstream(context.Background(), "google", "gemini", g.URL+"/v1beta/openai", "", "")
	if err != nil {
		t.Fatal(err)
	}
	admin := adminSession(t, h)
	for _, c := range []struct {
		model, thinking, off string
	}{
		{"gemini-2.5-flash", `{"includeThoughts":true,"thinkingBudget":0}`, ""},
		{"gemini-2.5-pro", `{"includeThoughts":true,"thinkingBudget":1024}`, adapter.ThinkingOffNever},
	} {
		t.Run(c.model, func(t *testing.T) {
			done := h.measureGemini(t, admin, h.enableGemini(t, up.ID, c.model))
			caps := done.Capabilities
			if off := caps[adapter.CapThinkingOff]; off.Value != nil && off.Value != c.off || off.Value == nil && c.off != "" {
				t.Fatalf("thinking off: %+v, want %q", off, c.off)
			}
			var audio, video []googleRequest
			for _, r := range g.sent(c.model) {
				if !r.native {
					continue
				}
				if strings.Contains(r.raw, `"thinkingLevel"`) {
					t.Errorf("a native request to 2.x carried thinkingLevel: %.300s", r.raw)
				}
				switch {
				case strings.Contains(r.raw, `"audio/wav"`):
					audio = append(audio, r)
				case strings.Contains(r.raw, `"video/mp4"`):
					video = append(video, r)
				}
			}
			if len(audio) != 1 || len(video) != 1 {
				t.Fatalf("media requests: %d sound, %d video", len(audio), len(video))
			}
			gen := `{"maxOutputTokens":256,"thinkingConfig":` + c.thinking + `}`
			for _, m := range []struct {
				got  googleRequest
				want map[string]any
			}{
				{audio[0], nativeBody(t, "What number is spoken in this sound clip? Reply with the number only.", "audio/wav", capability.ProbeSound(), gen)},
				{video[0], nativeBody(t, "What single color fills this video? Reply with one word.", "video/mp4", capability.ProbeVideo(), gen)},
			} {
				if m.got.path != "/v1beta/models/"+c.model+":generateContent" {
					t.Errorf("route: %s", m.got.path)
				}
				if !reflect.DeepEqual(m.got.body, m.want) {
					t.Errorf("native body:\n got %.400s\nwant %.400s", m.got.raw, mustJSON(t, m.want))
				}
			}
			for key, note := range map[string]string{adapter.CapInAudio: "number spoken", adapter.CapInVideo: "color of a test video"} {
				if v := caps[key]; v.Value != true || v.Source != capability.SourceProbe || !strings.Contains(v.Note, note) {
					t.Errorf("%s: %+v", key, v)
				}
			}
		})
	}
}

// Sound and video sent to a Gemini model through /v1/chat/completions reach
// it as native inlineData, with a thinking level it takes: what API callers
// and Chat send (input_audio, video_url data URL, a reasoning_effort, Chat's
// Google-run tools) is the body asserted here.
func TestGeminiChatCompletionsSendMediaNative(t *testing.T) {
	h := newHarness(t)
	g := newFakeGoogle(t)
	up, err := h.store.CreateUpstream(context.Background(), "google", "gemini", g.URL+"/v1beta/openai", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h.enableGemini(t, up.ID, "gemini-2.5-flash")
	h.enableGemini(t, up.ID, "gemini-3.8-flash")
	h.server.InvalidateConfigCache()

	soundPart := map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(capability.ProbeSound()), "format": "wav"}}
	videoPart := map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(capability.ProbeVideo())}}
	ask := func(t *testing.T, model, question string, part map[string]any, extra map[string]any) (string, googleRequest) {
		t.Helper()
		before := len(g.sent(model))
		body := map[string]any{"model": model, "max_tokens": 300,
			"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": question}, part}}}}
		for k, v := range extra {
			body[k] = v
		}
		rec := h.do(http.MethodPost, "/v1/chat/completions", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", model, rec.Code, rec.Body.String())
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		decodeInto(t, rec, &out)
		sent := g.sent(model)
		if len(sent) != before+1 {
			t.Fatalf("%s: %d upstream requests, want 1 (no refusal and resend)", model, len(sent)-before)
		}
		return out.Choices[0].Message.Content, sent[len(sent)-1]
	}

	const hear, see = "What number is spoken in this clip?", "What color fills this video?"
	for _, c := range []struct {
		name, model, question, mime, answer, gen string
		part                                     map[string]any
		data                                     []byte
		extra                                    map[string]any
	}{
		{"sound, default thinking", "gemini-2.5-flash", hear, "audio/wav", "47", `{"maxOutputTokens":300,"thinkingConfig":{"includeThoughts":true}}`,
			soundPart, capability.ProbeSound(), nil},
		{"sound, effort low on 2.5 (a budget)", "gemini-2.5-flash", hear, "audio/wav", "47",
			`{"maxOutputTokens":300,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1024}}`,
			soundPart, capability.ProbeSound(), map[string]any{"reasoning_effort": "low"}},
		{"video, effort high on 2.5 (a budget)", "gemini-2.5-flash", see, "video/mp4", "Blue",
			`{"maxOutputTokens":300,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":24576}}`,
			videoPart, capability.ProbeVideo(), map[string]any{"reasoning_effort": "high"}},
		{"sound, effort low on 3.8 (a level)", "gemini-3.8-flash", hear, "audio/wav", "17",
			`{"maxOutputTokens":300,"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"low"}}`,
			soundPart, capability.ProbeSound(), map[string]any{"reasoning_effort": "low"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer, got := ask(t, c.model, c.question, c.part, c.extra)
			if got.path != "/v1beta/models/"+c.model+":generateContent" {
				t.Errorf("route: %s", got.path)
			}
			if want := nativeBody(t, c.question, c.mime, c.data, c.gen); !reflect.DeepEqual(got.body, want) {
				t.Errorf("native body:\n got %.400s\nwant %.400s", got.raw, mustJSON(t, want))
			}
			if answer != c.answer {
				t.Errorf("answer %q, want %q", answer, c.answer)
			}
		})
	}

	// Chat's shape: streamed, with the Google-run tools Chat offers. Code
	// execution is dropped beside sound (Google refuses the pair); the
	// sound still goes as inlineData with the level's budget.
	before := len(g.sent("gemini-2.5-flash"))
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gemini-2.5-flash", "stream": true, "reasoning_effort": "medium",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": hear}, soundPart}}},
		"tools":    []any{map[string]any{"type": "web_search"}, map[string]any{"type": "code_execution"}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"47"`) {
		t.Fatalf("stream: %d %.500s", rec.Code, rec.Body.String())
	}
	sent := g.sent("gemini-2.5-flash")
	if len(sent) != before+1 {
		t.Fatalf("stream: %d upstream requests", len(sent)-before)
	}
	got := sent[len(sent)-1]
	want := nativeBody(t, hear, "audio/wav", capability.ProbeSound(), `{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":8192}}`)
	want["tools"] = []any{map[string]any{"google_search": map[string]any{}}}
	if got.path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" || !reflect.DeepEqual(got.body, want) {
		t.Errorf("stream request %s:\n got %.400s\nwant %.400s", got.path, got.raw, mustJSON(t, want))
	}
}
