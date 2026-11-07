package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// chatUpload posts one file to /api/v1/chat/attachments as the session.
func chatUpload(t *testing.T, h *harness, sess *auth.Session, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess.ID})
	req.Header.Set(auth.CSRFHeader, sess.CSRFToken)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func chatUploadOK(t *testing.T, h *harness, sess *auth.Session, filename string, data []byte) map[string]any {
	t.Helper()
	rec := chatUpload(t, h, sess, filename, data)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", filename, rec.Code, rec.Body.String())
	}
	var out map[string]any
	decodeInto(t, rec, &out)
	return out
}

func testPNG(t *testing.T, w, hgt int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, hgt))
	img.Set(0, 0, color.RGBA{G: 200, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func chatConversationFor(t *testing.T, h *harness, sess *auth.Session, model string) string {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": model})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	return conv.ID
}

// addVisionModel registers an enabled, priced, granted model whose name the
// vision heuristic recognizes.
func addVisionModel(t *testing.T, h *harness, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, h.model.UpstreamID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: usage.NanoPerUSD, RateOutNano: usage.NanoPerUSD,
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

func lastUpstreamChat(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	for i := len(h.upstreamRequests) - 1; i >= 0; i-- {
		var body struct {
			Messages []map[string]any `json:"messages"`
			Stream   bool             `json:"stream"`
		}
		if json.Unmarshal(h.upstreamRequests[i].Body, &body) == nil && body.Stream {
			return body.Messages
		}
	}
	t.Fatal("no streaming upstream request")
	return nil
}

// TestChatAttachmentsReachTheModel: a document's extracted text and an
// image (as a data URL) are put into the upstream request for a vision
// model, the thread lists the files, and the blobs are encrypted at rest.
func TestChatAttachmentsReachTheModel(t *testing.T) {
	h, sess := chatHarness(t)
	addVisionModel(t, h, "gpt-4o-mini")
	conv := chatConversationFor(t, h, sess, "gpt-4o-mini")

	doc := chatUploadOK(t, h, sess, "permit notes.md", []byte("Boundary Waters canoe permit for entry point 25"))
	if doc["kind"] != "document" || doc["filename"] != "permit notes.md" || doc["est_tokens"].(float64) < 10 {
		t.Fatalf("doc upload = %v", doc)
	}
	pic := chatUploadOK(t, h, sess, "lake.png", testPNG(t, 4, 3))
	if pic["kind"] != "image" || pic["width"].(float64) != 4 || pic["height"].(float64) != 3 || pic["est_tokens"].(float64) != chatImageTokens {
		t.Fatalf("image upload = %v", pic)
	}

	sent := chatSend(t, h, sess, conv, map[string]any{"content": "What do these say?",
		"attachment_ids": []string{doc["id"].(string), pic["id"].(string)}})
	if w := sent["warnings"].([]any); len(w) != 0 {
		t.Fatalf("vision model got warnings: %v", w)
	}
	um := sent["user_message"].(map[string]any)
	if atts, _ := um["attachments"].([]any); len(atts) != 2 {
		t.Fatalf("user message attachments = %v", um["attachments"])
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	msgs := lastUpstreamChat(t, h)
	parts, ok := msgs[len(msgs)-1]["content"].([]any)
	if !ok {
		t.Fatalf("last message content is not parts: %v", msgs[len(msgs)-1])
	}
	var sawDoc, sawImage, sawText bool
	for _, p := range parts {
		part := p.(map[string]any)
		switch part["type"] {
		case "text":
			text := part["text"].(string)
			if strings.HasPrefix(text, "<file name=\"permit notes.md\">\n") && strings.Contains(text, "Boundary Waters canoe permit") && strings.HasSuffix(text, "\n</file>") {
				sawDoc = true
			}
			if text == "What do these say?" {
				sawText = true
			}
		case "image_url":
			if strings.HasPrefix(part["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,iVBORw0KGgo") {
				sawImage = true
			}
		}
	}
	if !sawDoc || !sawImage || !sawText {
		t.Fatalf("doc %v image %v text %v; parts %v", sawDoc, sawImage, sawText, parts)
	}

	// The conversation view lists the files on the user message.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv, nil)
	var got struct {
		Messages []chatMessageView `json:"messages"`
	}
	decodeInto(t, rec, &got)
	if len(got.Messages) == 0 || len(got.Messages[0].Attachments) != 2 || got.Messages[0].Attachments[0].Filename != "permit notes.md" {
		t.Fatalf("view attachments = %+v", got.Messages)
	}

	// A follow-up still carries the document (full context every turn).
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "And again?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if !strings.Contains(string(h.upstreamRequests[len(h.upstreamRequests)-1].Body), "entry point 25") {
		t.Fatal("follow-up lost the document")
	}

	// Encrypted at rest: the raw blob_object rows never hold the plaintext.
	ctx := context.Background()
	for _, id := range []string{doc["id"].(string), pic["id"].(string)} {
		a, err := h.store.ChatAttachment(ctx, h.user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(a.FilenameEnc, "permit") || strings.Contains(a.FilenameEnc, "lake") {
			t.Fatalf("filename stored in clear: %q", a.FilenameEnc)
		}
		for _, ref := range []string{a.StorageRef, a.TextRef} {
			if ref == "" {
				continue
			}
			key := strings.TrimPrefix(ref, "database:")
			obj, err := h.store.GetBlob(ctx, key)
			if err != nil {
				t.Fatalf("raw blob %s: %v", key, err)
			}
			if bytes.Contains(obj.Data, []byte("Boundary Waters")) || bytes.Contains(obj.Data, []byte("IHDR")) {
				t.Fatalf("blob %s holds plaintext", key)
			}
		}
	}
}

// TestChatImagesForNonVisionModel: a model that does not take images gets a
// text placeholder instead, and the send response says so.
func TestChatImagesForNonVisionModel(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatConversationFor(t, h, sess, "test-model")
	pic := chatUploadOK(t, h, sess, "lake.png", testPNG(t, 2, 2))
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Describe", "attachment_ids": []string{pic["id"].(string)}})
	if w := sent["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), "does not accept images") {
		t.Fatalf("warnings = %v", sent["warnings"])
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	body := string(h.upstreamRequests[len(h.upstreamRequests)-1].Body)
	if strings.Contains(body, "image_url") || !strings.Contains(body, "[image lake.png omitted: this model does not accept images]") {
		t.Fatalf("upstream body = %s", body)
	}
}

// TestChatAttachmentOwnership: another user can neither read, delete nor
// send someone else's upload, and a sent file cannot be deleted alone.
func TestChatAttachmentOwnership(t *testing.T) {
	h, sess := chatHarness(t)
	pic := chatUploadOK(t, h, sess, "lake.png", testPNG(t, 2, 2))
	id := pic["id"].(string)

	other, _, err := h.store.UpsertUserFromIdentity(context.Background(), "sub-other", "other@example.com", "Other", true, false)
	if err != nil {
		t.Fatal(err)
	}
	otherSess, err := h.server.Sessions.Create(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(otherSess, http.MethodGet, "/api/v1/chat/attachments/"+id+"/content", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other GET = %d", rec.Code)
	}
	if rec := h.doAsSession(otherSess, http.MethodDelete, "/api/v1/chat/attachments/"+id, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other DELETE = %d", rec.Code)
	}
	otherConv := chatConversationFor(t, h, otherSess, "test-model")
	if rec := h.doAsSession(otherSess, http.MethodPost, "/api/v1/chat/conversations/"+otherConv+"/messages",
		map[string]any{"content": "hi", "attachment_ids": []string{id}}); rec.Code != http.StatusNotFound {
		t.Fatalf("other send with my file = %d %s", rec.Code, rec.Body.String())
	}

	// Too many files.
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = "x" + string(rune('a'+i))
	}
	conv := chatConversationFor(t, h, sess, "test-model")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/messages",
		map[string]any{"content": "hi", "attachment_ids": ids}); rec.Code != http.StatusBadRequest {
		t.Fatalf("11 files = %d", rec.Code)
	}

	// Once sent, the file cannot be deleted on its own.
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "hi", "attachment_ids": []string{id}})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/attachments/"+id, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete sent file = %d", rec.Code)
	}
	// ...nor sent into another conversation.
	conv2 := chatConversationFor(t, h, sess, "test-model")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+conv2+"/messages",
		map[string]any{"content": "hi", "attachment_ids": []string{id}}); rec.Code != http.StatusNotFound {
		t.Fatalf("reuse in another conversation = %d", rec.Code)
	}

	// An unsent upload can be deleted, and its object goes with it.
	spare := chatUploadOK(t, h, sess, "spare.txt", []byte("spare words"))
	a, _ := h.store.ChatAttachment(context.Background(), h.user.ID, spare["id"].(string))
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/attachments/"+a.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unsent = %d", rec.Code)
	}
	if _, err := h.store.GetBlob(context.Background(), strings.TrimPrefix(a.StorageRef, "database:")); err == nil {
		t.Fatal("deleted upload left its object behind")
	}
}

// TestChatAttachmentContentHeaders: images come back as themselves, every
// other file as a download, always sandboxed and nosniff.
func TestChatAttachmentContentHeaders(t *testing.T) {
	h, sess := chatHarness(t)
	png := testPNG(t, 2, 2)
	pic := chatUploadOK(t, h, sess, "lake.png", png)
	doc := chatUploadOK(t, h, sess, "page.html", []byte("<script>alert(1)</script><p>hello</p>"))

	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+pic["id"].(string)+"/content", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("image content = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec2 := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/attachments/"+doc["id"].(string)+"/content", nil)
	if rec2.Code != http.StatusOK || rec2.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(rec2.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("html content = %d %q %q", rec2.Code, rec2.Header().Get("Content-Type"), rec2.Header().Get("Content-Disposition"))
	}
	for _, r := range []*httptest.ResponseRecorder{rec, rec2} {
		if r.Header().Get("X-Content-Type-Options") != "nosniff" || r.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
			t.Fatalf("headers = %v", r.Header())
		}
	}
}

// TestChatUploadRefusals: renamed executables, oversize files and API
// tokens are refused.
func TestChatUploadRefusals(t *testing.T) {
	h, sess := chatHarness(t)
	if rec := chatUpload(t, h, sess, "notes.txt", append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 64)...)); rec.Code != http.StatusBadRequest {
		t.Fatalf("renamed exe = %d", rec.Code)
	}
	h.server.Config.ChatMaxUploadBytes = 1024
	if rec := chatUpload(t, h, sess, "big.txt", bytes.Repeat([]byte("a"), 4096)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d %s", rec.Code, rec.Body.String())
	}
	// Bearer tokens are not chat clients.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		t.Fatal("API token uploaded a chat file")
	}
}

// TestChatConversationDeleteRemovesFiles and search over attachments.
func TestChatAttachmentSearchAndDelete(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatConversationFor(t, h, sess, "test-model")
	doc := chatUploadOK(t, h, sess, "itinerary.txt", []byte("Day three: portage to Kawishiwi lake"))
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Summarize", "attachment_ids": []string{doc["id"].(string)}})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	for _, q := range []string{"kawishiwi", "itinerary"} {
		rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/search?q="+q, nil)
		var res struct {
			Results []chatSearchResult `json:"results"`
		}
		decodeInto(t, rec, &res)
		if len(res.Results) != 1 || res.Results[0].ConversationID != conv {
			t.Fatalf("search %q = %+v", q, res.Results)
		}
	}

	a, err := h.store.ChatAttachment(context.Background(), h.user.ID, doc["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+conv, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete conversation = %d", rec.Code)
	}
	for _, ref := range []string{a.StorageRef, a.TextRef} {
		if _, err := h.store.GetBlob(context.Background(), strings.TrimPrefix(ref, "database:")); err == nil {
			t.Fatalf("object %s survived the conversation delete", ref)
		}
	}
	if _, err := h.store.ChatAttachment(context.Background(), h.user.ID, a.ID); err == nil {
		t.Fatal("attachment row survived the conversation delete")
	}
}

// TestChatSweepPurgesUnsentUploads: the reaper's sweep removes uploads
// never sent after a day, objects included.
func TestChatSweepPurgesUnsentUploads(t *testing.T) {
	h, sess := chatHarness(t)
	up := chatUploadOK(t, h, sess, "old.txt", []byte("old words"))
	ctx := context.Background()
	a, err := h.store.ChatAttachment(ctx, h.user.ID, up["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	h.server.chatSweepAttachments(ctx)
	if _, err := h.store.ChatAttachment(ctx, h.user.ID, a.ID); err != nil {
		t.Fatal("fresh upload swept")
	}
	// Age it past the TTL by re-inserting with an old timestamp.
	if err := h.store.DeleteChatAttachmentRows(ctx, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	a.CreatedAt = time.Now().UTC().Add(-25 * time.Hour)
	if err := h.store.InsertChatAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	h.server.chatSweepAttachments(ctx)
	if _, err := h.store.ChatAttachment(ctx, h.user.ID, a.ID); err == nil {
		t.Fatal("stale upload not swept")
	}
	if _, err := h.store.GetBlob(ctx, strings.TrimPrefix(a.StorageRef, "database:")); err == nil {
		t.Fatal("stale upload's object not removed")
	}
}

func TestChatModelAcceptsImages(t *testing.T) {
	cases := []struct {
		name       string
		modalities []string
		want       bool
	}{
		{"gpt-4o-mini", nil, true},
		{"gpt-4.1", nil, true},
		{"gpt-5-mini", nil, true},
		{"openai/gpt-4o", nil, true},
		{"claude-sonnet-4-5", nil, true},
		{"gemini-2.5-flash", nil, true},
		{"grok-2-vision-1212", nil, true},
		{"grok-4", nil, true},
		{"llava:13b", nil, true},
		{"qwen2.5-vl-7b-instruct", nil, true},
		{"Qwen3-VL-30B", nil, true},
		{"pixtral-12b", nil, true},
		{"llama-3.2-11b-vision-instruct", nil, true},
		{"gemma3:27b", nil, true},
		{"minicpm-v", nil, true},
		{"o3", nil, true},
		// Personal ChatGPT plan models (my/openai/…) see images; the
		// live proof must show the color of an unnamed image.
		{"my/openai/gpt-6.1-sol", nil, true},
		{"gpt-6-astra", nil, true},
		{"my-local-model", []string{"chat", "vision"}, true},
		{"my-local-model", []string{"chat", "image"}, true},
		{"test-model", []string{"chat"}, false},
		{"llama-3.1-8b-instruct", nil, false},
		{"qwen2.5-7b-instruct", nil, false},
		{"mistral-small", nil, false},
		{"gpt-4o-mini-tts", nil, false},
		{"gemini-embedding-001", nil, false},
		{"llama-3.2-3b-instruct", nil, false},
		{"o3hermes", nil, false},
		{"flux-image-gen", []string{"image"}, false},
	}
	for _, tc := range cases {
		if got := chatModelAcceptsImages(tc.name, tc.modalities); got != tc.want {
			t.Errorf("chatModelAcceptsImages(%q, %v) = %v, want %v", tc.name, tc.modalities, got, tc.want)
		}
	}
}

// TestChatAdminSaysAModelSeesImages: a self-hosted model whose name gives
// no hint (test-model) gets attached images once the admin marks it
// "Sees images: Yes", and stops getting them on "No". Bad values are refused.
func TestChatAdminSaysAModelSeesImages(t *testing.T) {
	h, sess := chatHarness(t)
	setImages := func(v string) *http.Response {
		return chatAdminPut(t, h, sess, func(m map[string]any) {
			m["models"] = map[string]any{"test-model": map[string]any{"images": v}}
		})
	}
	if res := setImages("maybe"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad value = %d", res.StatusCode)
	}
	if res := setImages("yes"); res.StatusCode != http.StatusOK {
		t.Fatalf("set yes = %d", res.StatusCode)
	}
	conv := chatConversationFor(t, h, sess, "test-model")
	pic := chatUploadOK(t, h, sess, "lake.png", testPNG(t, 2, 2))
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Describe", "attachment_ids": []string{pic["id"].(string)}})
	if w, _ := sent["warnings"].([]any); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if body := string(h.upstreamRequests[len(h.upstreamRequests)-1].Body); !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("image not sent: %s", body)
	}
	if res := setImages("no"); res.StatusCode != http.StatusOK {
		t.Fatalf("set no = %d", res.StatusCode)
	}
	h.server.invalidateChatAdmin()
	if !strings.Contains(fmt.Sprint(chatSend(t, h, sess, conv, map[string]any{"content": "Again", "attachment_ids": []string{chatUploadOK(t, h, sess, "b.png", testPNG(t, 2, 2))["id"].(string)}})["warnings"]), "does not accept images") {
		t.Fatal("images still sent after No")
	}
}
