package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

var (
	testMP3 = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), []byte("fake mp3 frames")...)
	testMP4 = append([]byte{0, 0, 0, 0x18}, []byte("ftypisom\x00\x00\x02\x00isomiso2 fake video")...)
)

// fakeMediaUpstream answers chat (streamed and buffered) and speech-to-text,
// recording every request.
type fakeMediaUpstream struct {
	mu          sync.Mutex
	paths       []string
	bodies      []string
	transcribes int
	describes   int
}

func newFakeMediaUpstream(t *testing.T, h *harness) *fakeMediaUpstream {
	t.Helper()
	f := &fakeMediaUpstream{}
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case r.URL.Path == "/v1/audio/transcriptions":
			f.mu.Lock()
			f.transcribes++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"text":"meet me at the old mill at noon"}`))
		case strings.Contains(string(b), `"video_url"`) && strings.Contains(string(b), "Describe this video"):
			f.mu.Lock()
			f.describes++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"A red kite rises over a beach; someone shouts 'higher!'.\"}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":900,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n"))
		case strings.Contains(string(b), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Got it.\"}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"))
		default: // a buffered call: the video description
			f.mu.Lock()
			f.describes++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"A red kite rises over a beach; someone shouts 'higher!'."},"finish_reason":"stop"}],"usage":{"prompt_tokens":900,"completion_tokens":20}}`))
		}
	})
	return f
}

// lastChatBody is the last streamed chat request (the one that answers).
func (f *fakeMediaUpstream) lastChatBody(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.bodies) - 1; i >= 0; i-- {
		if strings.Contains(f.bodies[i], `"stream":true`) && !strings.Contains(f.bodies[i], "Describe this video") {
			return f.bodies[i]
		}
	}
	t.Fatal("no chat request")
	return ""
}

// A model that hears and watches gets the files themselves: sound as an
// input_audio part (format from the file type), video as a video_url data
// URL. Upload is accepted and the file is served back for the player.
func TestChatSendsSoundAndVideoToAModelThatTakesThem(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeMediaUpstream(t, h)
	if res := chatAdminPut(t, h, sess, func(m map[string]any) {
		m["models"] = map[string]any{"test-model": map[string]any{"audio": "yes", "video": "yes"}}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("admin = %d", res.StatusCode)
	}
	h.server.invalidateChatAdmin()
	conv := chatConversationFor(t, h, sess, "test-model")
	song := chatUploadOK(t, h, sess, "memo.mp3", testMP3)
	clip := chatUploadOK(t, h, sess, "clip.mp4", testMP4)
	if song["kind"] != "audio" || clip["kind"] != "video" {
		t.Fatalf("kinds = %v %v", song["kind"], clip["kind"])
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+clip["id"].(string)+"/content", nil)
	if rec.Header().Get("Content-Type") != "video/mp4" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("served as %q %q", rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
	}
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "What is in these?", "attachment_ids": []string{song["id"].(string), clip["id"].(string)}})
	if w := sent["warnings"].([]any); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(f.lastChatBody(t)), &body); err != nil {
		t.Fatal(err)
	}
	last := string(body.Messages[len(body.Messages)-1].Content)
	if !strings.Contains(last, `"type":"input_audio"`) || !strings.Contains(last, `"format":"mp3"`) {
		t.Errorf("no input_audio part: %.300s", last)
	}
	if !strings.Contains(last, `"type":"video_url"`) || !strings.Contains(last, `"url":"data:video/mp4;base64,`) {
		t.Errorf("no video_url part: %.300s", last)
	}
	if f.transcribes != 0 || f.describes != 0 {
		t.Errorf("converted anyway: %d transcripts, %d descriptions", f.transcribes, f.describes)
	}
}

// A model that does not hear gets a transcript from the speech-to-text
// model, made once: the next turn reuses it. The send response warns.
func TestChatTranscribesSoundForAModelThatCannotHear(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeMediaUpstream(t, h)
	addMediaModel(t, h, "whisper-test", []string{"stt"})
	conv := chatConversationFor(t, h, sess, "test-model")
	song := chatUploadOK(t, h, sess, "memo.mp3", testMP3)
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Where do we meet?", "attachment_ids": []string{song["id"].(string)}})
	if w := fmt.Sprint(sent["warnings"]); !strings.Contains(w, "whisper-test transcribes") {
		t.Fatalf("warnings = %v", w)
	}
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	body := f.lastChatBody(t)
	if !strings.Contains(body, "meet me at the old mill at noon") || strings.Contains(body, "input_audio") {
		t.Fatalf("chat body = %.400s", body)
	}
	if !strings.Contains(fmt.Sprint(done["activity"]), "transcribe") {
		t.Errorf("activity = %v", done["activity"])
	}
	// Second turn: the transcript is reused, not made again.
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "And when?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if f.transcribes != 1 {
		t.Errorf("transcribed %d times, want 1", f.transcribes)
	}
	if !strings.Contains(f.lastChatBody(t), "old mill") {
		t.Error("second turn lost the transcript")
	}
}

// A model that does not watch video gets a description from one that does
// (here a Qwen VL model), made once and kept.
func TestChatDescribesVideoForAModelThatCannotWatch(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeMediaUpstream(t, h)
	addMediaModel(t, h, "qwen3-vl-test", []string{"chat"})
	conv := chatConversationFor(t, h, sess, "test-model")
	clip := chatUploadOK(t, h, sess, "kite.mp4", testMP4)
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "What happens?", "attachment_ids": []string{clip["id"].(string)}})
	if w := fmt.Sprint(sent["warnings"]); !strings.Contains(w, "qwen3-vl-test describes") {
		t.Fatalf("warnings = %v", w)
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if f.describes != 1 {
		t.Fatalf("describes = %d", f.describes)
	}
	f.mu.Lock()
	var describeBody string
	for _, b := range f.bodies {
		if strings.Contains(b, `"video_url"`) {
			describeBody = b
		}
	}
	f.mu.Unlock()
	if !strings.Contains(describeBody, `"model":"qwen3-vl-test"`) {
		t.Errorf("description asked of: %.200s", describeBody)
	}
	body := f.lastChatBody(t)
	if !strings.Contains(body, "A red kite rises over a beach") || strings.Contains(body, "video_url") {
		t.Fatalf("chat body = %.400s", body)
	}
}

// With nothing to turn it into text, the model gets the name only and the
// send response says so (as for images).
func TestChatMediaNameOnlyWhenNothingCanConvertIt(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeMediaUpstream(t, h)
	conv := chatConversationFor(t, h, sess, "test-model")
	song := chatUploadOK(t, h, sess, "memo.mp3", testMP3)
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Hm?", "attachment_ids": []string{song["id"].(string)}})
	if w := fmt.Sprint(sent["warnings"]); !strings.Contains(w, "by name only") {
		t.Fatalf("warnings = %v", w)
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if !strings.Contains(f.lastChatBody(t), "[sound memo.mp3 omitted: this model does not take sound]") {
		t.Fatalf("chat body = %.300s", f.lastChatBody(t))
	}
}
