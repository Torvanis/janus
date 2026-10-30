package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// addImageModel registers an enabled, priced, granted image model (catalog
// modality "image" only) on the harness upstream.
func addImageModel(t *testing.T, h *harness, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"image"}); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, h.model.UpstreamID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 5 * usage.NanoPerUSD, RateOutNano: 40 * usage.NanoPerUSD,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
}

// fakeImageUpstream answers /v1/images/generations with one PNG, either
// inline (b64_json, OpenAI) or as an https URL it also serves (xAI), and
// /v1/chat/completions like the harness default. It records image prompts.
type fakeImageUpstream struct {
	srv     *httptest.Server
	asURL   bool
	prompts []string
	png     []byte
	fail    string // when set, image requests answer this error JSON with 400
	chats   [][]byte
	// callImageTool (opt-in): a chat request offered create_image answers
	// with a call to it instead of text.
	callImageTool bool
}

func newFakeImageUpstream(t *testing.T, h *harness) *fakeImageUpstream {
	t.Helper()
	f := &fakeImageUpstream{png: testPNG(t, 48, 32)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
		case "/files/img.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(f.png)
		case "/v1/images/generations":
			var body struct {
				Prompt string `json:"prompt"`
				Model  string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.prompts = append(f.prompts, body.Prompt)
			w.Header().Set("Content-Type", "application/json")
			if f.fail != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(f.fail))
				return
			}
			item := map[string]any{"b64_json": base64.StdEncoding.EncodeToString(f.png), "revised_prompt": "A tidy red square, studio light"}
			if f.asURL {
				item = map[string]any{"url": f.srv.URL + "/files/img.png", "mime_type": "image/png"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []any{item},
				"usage": map[string]any{"input_tokens": 12, "output_tokens": 1056, "total_tokens": 1068}})
		default:
			body, _ := io.ReadAll(r.Body)
			f.chats = append(f.chats, body)
			w.Header().Set("Content-Type", "text/event-stream")
			if f.callImageTool && strings.Contains(string(body), `"create_image"`) && !strings.Contains(string(body), `"role":"tool"`) {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Here you go.\"}}]}\n\n" +
					"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"create_image\",\"arguments\":\"{\\\"prompt\\\":\"}}]}}]}\n\n" +
					"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a lion on a cliff\\\"}\"}}]}}]}\n\n" +
					"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Nice image.\"}}]}\n\n" +
				"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"))
		}
	}))
	t.Cleanup(f.srv.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, f.srv.URL+"/v1", true, "", ""); err != nil {
		t.Fatal(err)
	}
	// The gateway must trust the fake's certificate (provider URLs are
	// https-only), as it would a private CA from JANUS_CA_BUNDLE.
	h.server.Config.CABundle = f.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return f
}

// TestChatGeneratesImages: picking an image model sends the message as an
// image request through the proxy (metered), stores the image encrypted in
// file storage on the reply, serves it to the owner only, gives later chat
// turns a note instead of pixels, and refuses attachments on image models.
func TestChatGeneratesImages(t *testing.T) {
	h, sess := chatHarness(t)
	addImageModel(t, h, "test-image")
	f := newFakeImageUpstream(t, h)
	ctx := context.Background()

	conv := chatConversationFor(t, h, sess, "test-image")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "A red square", "model": "test-image"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("done = %v", done)
	}
	if len(f.prompts) != 1 || f.prompts[0] != "A red square" {
		t.Fatalf("upstream image prompts = %q", f.prompts)
	}
	atts, _ := done["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("reply attachments = %v", done["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["kind"] != "image" || att["mime"] != "image/png" || att["width"].(float64) != 48 || att["height"].(float64) != 32 {
		t.Fatalf("attachment = %v", att)
	}
	if done["content"] != "A tidy red square, studio light" {
		t.Errorf("caption = %q, want the revised prompt", done["content"])
	}
	if done["tokens_out"].(float64) != 1056 || done["cost_usd"].(float64) <= 0 {
		t.Errorf("usage not attached: in=%v out=%v cost=%v", done["tokens_in"], done["tokens_out"], done["cost_usd"])
	}

	// Metered as an image request under the chat client app.
	var modality, path, app string
	_ = h.store.DB().QueryRowContext(ctx, `SELECT modality, endpoint_path, client_app FROM usage_event WHERE request_id = ?`,
		done["request_id"]).Scan(&modality, &path, &app)
	if modality != "image" || path != "/v1/images/generations" || app != chatClientApp {
		t.Errorf("usage event = %s %s %s", modality, path, app)
	}

	// Served to the owner as the PNG; nobody else.
	id := att["id"].(string)
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+id+"/content", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), f.png) {
		t.Fatalf("content: %d %s (%d bytes)", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
	other := h.sessionFor(t, "other@example.com")
	if rec := h.doAsSession(other, http.MethodGet, "/api/v1/chat/attachments/"+id+"/content", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user read the image: %d", rec.Code)
	}
	// Stored encrypted: the raw blob row never holds the PNG.
	a, err := h.store.ChatAttachment(ctx, h.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := h.store.GetBlob(ctx, strings.TrimPrefix(a.StorageRef, "database:"))
	if err != nil {
		t.Fatalf("raw blob: %v", err)
	}
	if bytes.Contains(obj.Data, []byte("IHDR")) {
		t.Fatal("generated image stored in plaintext")
	}

	// Reopening the chat shows the image on the reply.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv, nil)
	if !strings.Contains(rec.Body.String(), id) {
		t.Fatal("reopened chat lost the generated image")
	}

	// A chat model later in the same conversation gets a note, not pixels.
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "Describe what you made", "model": "test-model"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("follow-up = %v", done)
	}
	var last struct {
		Messages []map[string]any `json:"messages"`
	}
	if len(f.chats) == 0 || json.Unmarshal(f.chats[len(f.chats)-1], &last) != nil {
		t.Fatal("no chat request reached the upstream")
	}
	var found bool
	for _, m := range last.Messages {
		c, _ := m["content"].(string)
		if m["role"] == "assistant" && strings.Contains(c, "generated 1 image(s)") && strings.Contains(c, "A tidy red square") {
			found = true
		}
		if strings.Contains(c, "data:image") {
			t.Fatal("generated pixels were sent to the chat model")
		}
	}
	if !found {
		t.Fatalf("history lacks the image note: %v", last.Messages)
	}

	// Image models make new images only: attachments are refused up front.
	up := chatUploadOK(t, h, sess, "photo.png", testPNG(t, 10, 10))
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/messages",
		map[string]any{"content": "make it blue", "model": "test-image", "attachment_ids": []string{up["id"].(string)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("attachment on an image model: %d %s", rec.Code, rec.Body.String())
	}

	// Deleting the chat purges the generated image's row.
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+conv, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+id+"/content", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("image of a deleted chat still served: %d", rec.Code)
	}
}

// Providers that answer with a short-lived URL (xAI) are fetched once and
// kept, and a refused prompt is a named error, not a blank reply.
func TestChatImageFromURLAndRefusal(t *testing.T) {
	h, sess := chatHarness(t)
	addImageModel(t, h, "test-image")
	f := newFakeImageUpstream(t, h)
	f.asURL = true
	conv := chatConversationFor(t, h, sess, "test-image")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "A red square", "model": "test-image"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	atts, _ := done["attachments"].([]any)
	if done["status"] != store.ChatStatusComplete || len(atts) != 1 {
		t.Fatalf("url image: %v", done)
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+atts[0].(map[string]any)["id"].(string)+"/content", nil)
	if !bytes.Equal(rec.Body.Bytes(), f.png) {
		t.Fatal("stored image differs from the provider's")
	}

	f.fail = `{"error":{"message":"Your request was rejected by the safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "something it refuses", "model": "test-image"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusError || !strings.Contains(done["error_message"].(string), "safety system") {
		t.Fatalf("refusal = %v", done)
	}
}

// chatImageModel: image-only models make images; vision chat models (chat
// + image) and personal plans stay chat models.
func TestChatImageModelDetection(t *testing.T) {
	h, _ := chatHarness(t)
	addImageModel(t, h, "test-image")
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "vision-chat", []string{"chat", "image"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"test-image": true, "test-model": false, "vision-chat": false, "my/openai/gpt-image": false, "missing": false} {
		if got := h.server.chatImageModel(ctx, name); got != want {
			t.Errorf("chatImageModel(%q) = %v, want %v", name, got, want)
		}
	}
}
