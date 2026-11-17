package capability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// mediaPart finds the request whose user message carries a part of the
// given type, and returns that part.
func mediaPart(t *testing.T, bodies []map[string]any, partType string) (map[string]any, map[string]any) {
	t.Helper()
	for _, b := range bodies {
		parts, ok := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			if part := p.(map[string]any); part["type"] == partType {
				return b, part
			}
		}
	}
	return nil, nil
}

func TestProbeSoundAndVideoEvidence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sound, video string
		// want: true, false, or nil for not recorded.
		wantSound, wantVideo any
		noteSound, noteVideo string
	}{
		{"takes both", "takes", "takes", true, true, "it named the number spoken in a test clip", "it named the color of a test video"},
		{"refuses both", "refuses", "refuses", false, false, "At most 0 audio(s)", "Invalid value: 'video_url'"},
		{"answers wrongly", "wrong", "wrong", nil, nil, "", ""},
		{"accepts and ignores", "ignores", "ignores", nil, nil, "", ""},
		{"hears but does not watch", "takes", "refuses", true, false, "spoken", "video_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeModel{efforts: []string{"none", "low"}, effortError: "Supported values are: 'none' and 'low'.", onBy: "effort",
				image: "sees", tools: "calls", schema: "fits", sound: tc.sound, video: tc.video}
			facts, run := probe(t, f, "openai_compatible", nil)
			if run.Status != ProbeOK {
				t.Fatalf("run: %+v", run)
			}
			for _, c := range []struct {
				key  string
				want any
				note string
			}{{adapter.CapInAudio, tc.wantSound, tc.noteSound}, {adapter.CapInVideo, tc.wantVideo, tc.noteVideo}} {
				got, recorded := facts[c.key]
				switch {
				case c.want == nil && recorded:
					t.Errorf("%s recorded %v without evidence (%q)", c.key, got.Value, got.Note)
				case c.want != nil && (!recorded || got.Value != c.want):
					t.Errorf("%s = %+v, want %v", c.key, got, c.want)
				case c.want != nil && !strings.Contains(got.Note, c.note):
					t.Errorf("%s note %q should contain %q", c.key, got.Note, c.note)
				}
			}
		})
	}
}

// The probe sends sound and video exactly as Chat sends attachments: an
// input_audio part (base64 WAV, format wav) and a video_url data URL, with
// the same quiet reasoning setting as the other content probes.
func TestProbeSoundAndVideoRequestShape(t *testing.T) {
	f := &fakeModel{efforts: []string{"none", "low"}, effortError: "Supported values are: 'none' and 'low'.", onBy: "effort",
		image: "sees", tools: "calls", schema: "fits", sound: "takes", video: "takes"}
	probe(t, f, "openai_compatible", nil)

	body, part := mediaPart(t, f.bodies, "input_audio")
	if part == nil {
		t.Fatal("no request carried an input_audio part")
	}
	audio := part["input_audio"].(map[string]any)
	data, err := base64.StdEncoding.DecodeString(audio["data"].(string))
	if err != nil || !bytes.Equal(data, ProbeSound()) || audio["format"] != "wav" {
		t.Errorf("input_audio part: format %v, %d bytes, err %v", audio["format"], len(data), err)
	}
	if body["reasoning_effort"] != "none" || body["max_tokens"] != 256 {
		t.Errorf("sound request should be quiet and capped like the image one: %v / %v", body["reasoning_effort"], body["max_tokens"])
	}
	text := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if text["type"] != "text" || !strings.Contains(text["text"].(string), "What number is spoken") {
		t.Errorf("sound question: %v", text)
	}

	body, part = mediaPart(t, f.bodies, "video_url")
	if part == nil {
		t.Fatal("no request carried a video_url part")
	}
	url := part["video_url"].(map[string]any)["url"].(string)
	enc, ok := strings.CutPrefix(url, "data:video/mp4;base64,")
	data, err = base64.StdEncoding.DecodeString(enc)
	if !ok || err != nil || !bytes.Equal(data, ProbeVideo()) {
		t.Errorf("video_url part: %.40s… (%d bytes, err %v)", url, len(data), err)
	}
	if body["reasoning_effort"] != "none" {
		t.Errorf("video request should be quiet: %v", body["reasoning_effort"])
	}
}

// OpenAI chat models that take only text and images refuse sound without
// naming it; that is still a refusal of the content type.
func TestProbeSoundOpenAIContentBlockRefusal(t *testing.T) {
	call := func(_ context.Context, body map[string]any) (Reply, error) {
		if _, part := mediaPart(t, []map[string]any{body}, "input_audio"); part != nil {
			return Reply{Status: 400, Param: "messages[0]", Error: "Invalid 'messages[0]'. Content blocks are expected to be either text or image_url type."}, nil
		}
		return Reply{Status: 200, Text: "43"}, nil
	}
	facts, _ := RunProbe(context.Background(), call, ProbeInput{AdapterType: "openai_compatible", Now: func() time.Time { return probeNow }})
	if got := facts[adapter.CapInAudio]; got.Value != false || !strings.Contains(got.Note, "Content blocks are expected") {
		t.Errorf("input.audio = %+v, want no with the refusal quoted", got)
	}
}

// A request Janus's own adapter would not build reached no provider: it
// says nothing about the model, so nothing is recorded.
func TestProbeSoundAndVideoLocalRefusalRecordsNothing(t *testing.T) {
	call := func(_ context.Context, body map[string]any) (Reply, error) {
		for _, kind := range []string{"input_audio", "video_url"} {
			if _, part := mediaPart(t, []map[string]any{body}, kind); part != nil {
				return Reply{Status: 400, Error: "content[1]: audio input is not supported by this route; video has no equivalent", Local: true}, nil
			}
		}
		return Reply{Status: 200, Text: "43"}, nil
	}
	facts, _ := RunProbe(context.Background(), call, ProbeInput{AdapterType: "openai_compatible", Now: func() time.Time { return probeNow }})
	for _, k := range []string{adapter.CapInAudio, adapter.CapInVideo} {
		if got, recorded := facts[k]; recorded {
			t.Errorf("%s recorded from Janus's own refusal: %+v", k, got)
		}
	}
}

// Routes with no way to send sound or video are not asked, and what the
// provider or an administrator already said is not measured again.
func TestProbeSoundAndVideoSkipped(t *testing.T) {
	for _, adapterType := range []string{"anthropic", "bedrock", adapter.CodexAdapterType} {
		f := &fakeModel{image: "sees", tools: "calls", schema: "fits", sound: "takes", video: "takes"}
		facts, _ := probe(t, f, adapterType, nil)
		for _, kind := range []string{"input_audio", "video_url"} {
			if _, part := mediaPart(t, f.bodies, kind); part != nil {
				t.Errorf("%s: a %s part was sent on a route that cannot carry it", adapterType, kind)
			}
		}
		if _, recorded := facts[adapter.CapInAudio]; recorded {
			t.Errorf("%s: input.audio recorded", adapterType)
		}
	}

	var s State
	if err := s.SetAdmin(map[string]any{adapter.CapInAudio: false, adapter.CapInVideo: false}, probeNow); err != nil {
		t.Fatal(err)
	}
	f := &fakeModel{image: "sees", tools: "calls", schema: "fits", sound: "takes", video: "takes"}
	facts, _ := RunProbe(context.Background(), f.call, ProbeInput{Known: s.Profile(), AdapterType: "gemini", Now: func() time.Time { return probeNow }})
	for _, kind := range []string{"input_audio", "video_url"} {
		if _, part := mediaPart(t, f.bodies, kind); part != nil {
			t.Errorf("a %s part was sent although the administrator said no", kind)
		}
	}
	if _, recorded := facts[adapter.CapInAudio]; recorded {
		t.Error("the administrator's no was measured again")
	}
}

func TestProbeMediaFiles(t *testing.T) {
	wav := ProbeSound()
	if len(wav) < 44 || len(wav) > 32<<10 || string(wav[:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " {
		t.Fatalf("test sound is not a small WAV: %d bytes", len(wav))
	}
	if format, channels, rate, bits := binary.LittleEndian.Uint16(wav[20:]), binary.LittleEndian.Uint16(wav[22:]),
		binary.LittleEndian.Uint32(wav[24:]), binary.LittleEndian.Uint16(wav[34:]); format != 1 || channels != 1 || rate != 8000 || bits != 16 {
		t.Errorf("test sound format: pcm=%d channels=%d rate=%d bits=%d", format, channels, rate, bits)
	}
	mp4 := ProbeVideo()
	if len(mp4) < 12 || len(mp4) > 8<<10 || string(mp4[4:8]) != "ftyp" || !bytes.Contains(mp4, []byte("avc1")) {
		t.Errorf("test video is not a small H.264 MP4: %d bytes", len(mp4))
	}
}

func TestProbeMediaAnswers(t *testing.T) {
	for text, want := range map[string]bool{
		"47": true, "47.": true, "Forty-seven": true, "forty seven": true, "The number is 47.": true,
		"42": false, "4 7": false, "47 or 74": false, "": false, "I can't hear audio.": false, "147": false,
	} {
		n, ok := answerNumber(text)
		if got := ok && n == probeSpokenNumber; got != want {
			t.Errorf("answerNumber(%q) = %d,%v; want 47 %v", text, n, ok, want)
		}
	}
	for text, want := range map[string]bool{
		"Blue": true, "blue.": true, "It is blue": true, "Dark blue": true,
		"Green": false, "blue or green": false, "bluish": false, "I cannot see a video.": false, "colored": false,
	} {
		if got := namesColor(text, probeVideoColor); got != want {
			t.Errorf("namesColor(%q) = %v, want %v", text, got, want)
		}
	}
}
