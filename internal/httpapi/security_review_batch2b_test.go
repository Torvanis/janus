package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/endpoints"
	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 2b: every caller of
// an upstream shares its provider key, and /v1/* used to forward any path, so
// one caller could list, read and delete what another had stored there.

func (h *harness) upstreamPaths() []string {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	out := make([]string, len(h.upstreamRequests))
	for i, req := range h.upstreamRequests {
		out[i] = req.Path
	}
	return out
}

// createVideo makes a video job as the given token; the fake provider answers
// with the given id.
func (h *harness) createVideo(t *testing.T, token, id string) {
	t.Helper()
	h.upstreamReply = `{"id":"` + id + `","object":"video","status":"queued"}`
	rec := h.doWithToken(token, http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

func TestStoredObjectsBelongToTheirCreator(t *testing.T) {
	h := newHarness(t)
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	_, third := h.otherUser(t, "sub-c", "c@example.com")
	h.createVideo(t, h.token, "video_aaaaaaaa1")
	h.createVideo(t, other, "video_bbbbbbbb2")
	h.upstreamReply = `{"id":"video_aaaaaaaa1","status":"completed"}`

	// The creator reaches the job, and need not name a model to do it.
	rec := h.do(http.MethodGet, "/v1/videos/video_aaaaaaaa1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status check: %d %s", rec.Code, rec.Body.String())
	}
	if paths := h.upstreamPaths(); paths[len(paths)-1] != "/v1/videos/video_aaaaaaaa1" {
		t.Fatalf("the status check went to %q", paths[len(paths)-1])
	}
	if rec := h.do(http.MethodGet, "/v1/videos/video_aaaaaaaa1/content", nil); rec.Code != http.StatusOK {
		t.Fatalf("owner download: %d", rec.Code)
	}

	// Nobody else does: not to read, download, remix or delete it, with or
	// without naming a model, and the provider is never asked.
	before := h.upstreamCount()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/videos/video_aaaaaaaa1"},
		{http.MethodGet, "/v1/videos/video_aaaaaaaa1/content"},
		{http.MethodDelete, "/v1/videos/video_aaaaaaaa1"},
		{http.MethodPost, "/v1/videos/video_aaaaaaaa1/remix"},
		{http.MethodGet, "/v1/videos/video_never_recorded"},
		{http.MethodGet, "/v1/files/video_aaaaaaaa1"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+other)
		req.Header.Set("X-Janus-Model", "test-model")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another user: got %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if rec := h.do(http.MethodGet, "/v1/videos/video_never_recorded", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an object never created through the gateway: got %d, want 404", rec.Code)
	}
	if h.upstreamCount() != before {
		t.Fatal("a refused object call reached the provider")
	}

	// A list shows only the caller's objects, whatever the provider returns.
	h.upstreamReply = `{"object":"list","data":[{"id":"video_bbbbbbbb2","prompt":"secret"},{"id":"video_aaaaaaaa1"},{"id":"video_outside"}],"first_id":"video_bbbbbbbb2","last_id":"video_outside","has_more":true}`
	rec = h.do(http.MethodGet, "/v1/videos", nil)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if len(list.Data) != 1 || list.Data[0].ID != "video_aaaaaaaa1" || list.FirstID != "video_aaaaaaaa1" || list.LastID != "video_aaaaaaaa1" || !list.HasMore {
		t.Fatalf("list was not filtered to the caller's objects: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("the list leaked another user's object")
	}
	// A caller who owns nothing gets an empty list without a provider call.
	before = h.upstreamCount()
	rec = h.doWithToken(third, http.MethodGet, "/v1/videos", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) || h.upstreamCount() != before {
		t.Fatalf("empty list: %d %s (provider calls %d)", rec.Code, rec.Body.String(), h.upstreamCount()-before)
	}
	// A list the gateway cannot read is refused, never passed through.
	h.upstreamReply = `[{"id":"video_bbbbbbbb2"}]`
	if rec := h.do(http.MethodGet, "/v1/videos", nil); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "video_bbbbbbbb2") {
		t.Fatalf("unreadable list: %d %s", rec.Code, rec.Body.String())
	}

	// Deleting is the owner's, and the object is gone afterwards.
	h.upstreamReply = `{"id":"video_aaaaaaaa1","deleted":true}`
	if rec := h.do(http.MethodDelete, "/v1/videos/video_aaaaaaaa1", nil); rec.Code != http.StatusOK {
		t.Fatalf("owner delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/v1/videos/video_aaaaaaaa1", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted object: got %d, want 404", rec.Code)
	}
}

// The three shapes video providers use for a job id.
func TestVideoJobIDShapes(t *testing.T) {
	h := newHarness(t)
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	// xAI answers POST /v1/videos/generations with request_id.
	h.upstreamReply = `{"request_id":"0f8fad5b-d9cb-469f-a165-70867728950e"}`
	if rec := h.do(http.MethodPost, "/v1/videos/generations", map[string]any{"model": "test-model", "prompt": "x"}); rec.Code != http.StatusOK {
		t.Fatalf("xai create: %d %s", rec.Code, rec.Body.String())
	}
	// Gemini's id is an operation name with slashes.
	h.upstreamReply = `{"id":"models/veo-3.1/operations/op123","status":"queued"}`
	if rec := h.do(http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "x"}); rec.Code != http.StatusOK {
		t.Fatalf("gemini create: %d %s", rec.Code, rec.Body.String())
	}
	h.upstreamReply = `{"status":"done"}`
	for _, path := range []string{
		"/v1/videos/0f8fad5b-d9cb-469f-a165-70867728950e",
		"/v1/videos/models/veo-3.1/operations/op123",
		"/v1/videos/models/veo-3.1/operations/op123/content",
	} {
		if rec := h.do(http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("owner GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec := h.doWithToken(other, http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("other user GET %s: got %d, want 404", path, rec.Code)
		}
	}
}

func TestUnclassifiedPathsAreNotForwarded(t *testing.T) {
	h := newHarness(t)
	before := h.upstreamCount()
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodDelete, "/v1/batches/batch_1", http.StatusNotFound},
		{http.MethodGet, "/v1/fine_tuning/checkpoints/ckpt_1/permissions", http.StatusNotFound},
		{http.MethodPost, "/v1/fine_tuning/alpha/graders/run", http.StatusNotFound},
		{http.MethodGet, "/v1/organization/users", http.StatusNotFound},
		{http.MethodGet, "/v1/organization/admin_api_keys", http.StatusNotFound},
		{http.MethodPost, "/v1/realtime/client_secrets", http.StatusNotFound},
		{http.MethodGet, "/v1/assistants", http.StatusNotFound},
		{http.MethodGet, "/v1/threads/thread_1/messages", http.StatusNotFound},
		{http.MethodGet, "/v1/chat/completions", http.StatusNotFound},
		{http.MethodGet, "/v1/chat/completions/chatcmpl-1", http.StatusNotFound},
		{http.MethodGet, "/v1/something-new", http.StatusNotFound},
		{http.MethodGet, "/v1/files/../organization", http.StatusBadRequest},
		{http.MethodGet, "/v1//organization", http.StatusBadRequest},
		{http.MethodGet, "/v1/files/x%2f..%2f..%2fbatches", http.StatusBadRequest},
		{http.MethodGet, "/v1/files/file-1%3fpurpose=x", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("X-Janus-Model", "test-model")
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s: got %d %s, want %d", c.method, c.path, rec.Code, rec.Body.String(), c.want)
		}
	}
	if h.upstreamCount() != before {
		t.Fatalf("refused paths reached the provider: %v", h.upstreamPaths()[before:])
	}
}

// A request body can point at a stored object (a file to read, a stored
// response to continue). Only the caller's own.
func TestRequestsMayOnlyReferToOwnObjects(t *testing.T) {
	h := newHarness(t)
	_, other := h.otherUser(t, "sub-b", "b@example.com")

	// A uploads a file. The form names no model; the header picks the provider.
	h.upstreamReply = `{"id":"file-AAAAaaaa1","object":"file","purpose":"user_data"}`
	ct, form := multipartBody(t, [][2]string{{"purpose", "user_data"}, {"file", ""}}, 32)
	req := httptest.NewRequest(http.MethodPost, "/v1/files", bytes.NewReader(form))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	// A stores a response.
	h.upstreamReply = `{"id":"resp_aaaaaaa1","object":"response","output":[],"usage":{"input_tokens":3,"output_tokens":2}}`
	if rec := h.do(http.MethodPost, "/v1/responses", map[string]any{"model": "test-model", "input": "hi"}); rec.Code != http.StatusOK {
		t.Fatalf("create response: %d %s", rec.Code, rec.Body.String())
	}
	h.upstreamReply = ""

	withFile := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "file", "file": map[string]any{"file_id": "file-AAAAaaaa1"}},
	}}}}
	continued := map[string]any{"model": "test-model", "input": "and then?", "previous_response_id": "resp_aaaaaaa1"}
	inTools := map[string]any{"model": "test-model", "input": "x", "tools": []any{map[string]any{"type": "file_search", "vector_store_ids": []string{"vs_notmine123"}}}}
	inConversation := map[string]any{"model": "test-model", "input": "x", "conversation": map[string]any{"id": "conv_notmine123"}}

	if rec := h.do(http.MethodPost, "/v1/chat/completions", withFile); rec.Code != http.StatusOK {
		t.Fatalf("owner refers to own file: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPost, "/v1/responses", continued); rec.Code != http.StatusOK {
		t.Fatalf("owner continues own response: %d %s", rec.Code, rec.Body.String())
	}
	before := h.upstreamCount()
	for name, c := range map[string]struct {
		token, path string
		body        map[string]any
	}{
		"another user's file":            {other, "/v1/chat/completions", withFile},
		"another user's stored response": {other, "/v1/responses", continued},
		"a vector store never recorded":  {h.token, "/v1/responses", inTools},
		"a conversation never recorded":  {h.token, "/v1/responses", inConversation},
	} {
		if rec := h.doWithToken(c.token, http.MethodPost, c.path, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d %s, want 404", name, rec.Code, rec.Body.String())
		}
	}
	if h.upstreamCount() != before {
		t.Fatal("a request referring to someone else's object reached the provider")
	}
	// Text that merely looks like an id is not a reference.
	plain := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "what is file-AAAAaaaa1 and resp_aaaaaaa1?"}},
		"metadata": map[string]any{"note": "file-ZZZZzzzz9"}, "tool_call_id": "call_abc123456"}
	if rec := h.doWithToken(other, http.MethodPost, "/v1/chat/completions", plain); rec.Code != http.StatusOK {
		t.Fatalf("id-like text in a prompt: %d %s", rec.Code, rec.Body.String())
	}
}

// A streamed Responses call stores a response too; its id is in the stream.
func TestStreamedResponseIsRecordedForItsCreator(t *testing.T) {
	h := newHarness(t)
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	h.streamFrames = []string{
		`data: {"type":"response.created","response":{"id":"resp_stream0001","object":"response","status":"in_progress"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		`data: {"type":"response.completed","response":{"id":"resp_stream0001","usage":{"input_tokens":3,"output_tokens":1}}}`,
		`data: [DONE]`,
	}
	rec := h.doRaw(http.MethodPost, "/v1/responses", "application/json", []byte(`{"model":"test-model","input":"hi","stream":true}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "resp_stream0001") {
		t.Fatalf("streamed create: %d %s", rec.Code, rec.Body.String())
	}
	h.upstreamReply = `{"id":"resp_stream0001","status":"completed"}`
	if rec := h.do(http.MethodGet, "/v1/responses/resp_stream0001", nil); rec.Code != http.StatusOK {
		t.Fatalf("owner reads the streamed response: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doWithToken(other, http.MethodGet, "/v1/responses/resp_stream0001", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another user reads it: got %d, want 404", rec.Code)
	}
}

// A service token owns what it creates; the person who minted it does not.
func TestServiceTokenOwnsItsObjects(t *testing.T) {
	h := newHarness(t)
	_, svc := h.newServiceToken(t, "render-bot", true)
	h.upstreamReply = `{"id":"video_svc000001","status":"queued"}`
	if rec := h.doAsServiceToken(svc, http.MethodPost, "/v1/videos", []byte(`{"model":"test-model","prompt":"x"}`)); rec.Code != http.StatusOK {
		t.Fatalf("service token create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsServiceToken(svc, http.MethodGet, "/v1/videos/video_svc000001", nil); rec.Code != http.StatusOK {
		t.Fatalf("service token reads its job: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodGet, "/v1/videos/video_svc000001", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("a user reads a service token's job: got %d, want 404", rec.Code)
	}
}

// Losing the grant on the model loses the objects made with it.
func TestStoredObjectNeedsTheGrantItWasMadeWith(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.createVideo(t, h.token, "video_aaaaaaaa1")
	grants, err := h.store.ListGrants(ctx, h.model.ID)
	if err != nil || len(grants) == 0 {
		t.Fatalf("grants: %v", err)
	}
	for _, g := range grants {
		if err := h.store.DeleteGrant(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
	}
	h.server.InvalidateConfigCache()
	before := h.upstreamCount()
	if rec := h.do(http.MethodGet, "/v1/videos/video_aaaaaaaa1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("status check after the grant was removed: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	if h.upstreamCount() != before {
		t.Fatal("the provider was called without a grant")
	}
}

func TestReferencedObjectIDs(t *testing.T) {
	body := `{"model":"m","previous_response_id":"resp_abc12345","input":[{"type":"input_file","file_id":"file-abc12345"},
		{"type":"input_text","text":"file-zzzz9999"}],"tools":[{"type":"file_search","vector_store_ids":["vs_abc12345","vs_def67890"]}],
		"conversation":{"id":"conv_abc12345"},"user_id":"someone","tool_call_id":"call_abc12345","id":"resp_ignored01"}`
	got := map[string]bool{}
	for _, id := range referencedObjectIDs([]byte(body)) {
		got[id] = true
	}
	for _, want := range []string{"resp_abc12345", "file-abc12345", "vs_abc12345", "vs_def67890", "conv_abc12345"} {
		if !got[want] {
			t.Errorf("missed the reference %s", want)
		}
	}
	for _, not := range []string{"file-zzzz9999", "someone", "call_abc12345", "resp_ignored01"} {
		if got[not] {
			t.Errorf("%s is not a reference", not)
		}
	}
	if len(got) != 5 {
		t.Errorf("got %v", got)
	}
}

func TestCreatedIDSnifferAcrossReads(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_chunked001","object":"response"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"{\"response\":{\"id\":\"resp_decoy\"}}"}` + "\n\n"
	for _, chunk := range []int{1, 5, 40, len(stream)} {
		var ids []string
		sniffer := &createdIDSniffer{body: io.NopCloser(&slowReader{data: []byte(stream), step: chunk}), found: func(id string) { ids = append(ids, id) }}
		out, err := io.ReadAll(sniffer)
		if err != nil || string(out) != stream {
			t.Fatalf("chunk %d: the stream was changed (%v)", chunk, err)
		}
		if len(ids) != 1 || ids[0] != "resp_chunked001" {
			t.Errorf("chunk %d: found %v", chunk, ids)
		}
	}
}

type slowReader struct {
	data []byte
	step int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.step, len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestCreatedObjectID(t *testing.T) {
	upload := endpoints.Classify("POST", "/v1/uploads/upload_1/complete")
	if got := createdObjectID(upload, []byte(`{"id":"upload_1","status":"completed","file":{"id":"file-fromupload1"}}`)); got != "file-fromupload1" {
		t.Errorf("a finished upload's file: got %q", got)
	}
	if got := createdObjectID(endpoints.Classify("POST", "/v1/videos/generations"), []byte(`{"request_id":"abc"}`)); got != "abc" {
		t.Errorf("xai request id: got %q", got)
	}
	if got := createdObjectID(endpoints.Classify("POST", "/v1/files"), []byte(`not json`)); got != "" {
		t.Errorf("unreadable body: got %q", got)
	}
}

func TestRecordUpstreamObjectKeepsTheFirstOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	o := store.UpstreamObject{UpstreamID: h.model.UpstreamID, ObjectID: "file-first0001", Kind: "file", OwnerType: store.ObjectOwnerUser, OwnerID: "user-a", ModelID: h.model.ID, AccessID: h.model.ID}
	if err := h.store.RecordUpstreamObject(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordUpstreamObject(ctx, o); err != nil {
		t.Fatalf("recording the same owner again: %v", err)
	}
	o.OwnerID = "user-b"
	if err := h.store.RecordUpstreamObject(ctx, o); err == nil {
		t.Fatal("a second owner was recorded for the same object")
	}
	if _, err := h.store.OwnedUpstreamObject(ctx, store.ObjectOwnerUser, "user-b", "file-first0001"); err == nil {
		t.Fatal("the second owner can reach the object")
	}
}
