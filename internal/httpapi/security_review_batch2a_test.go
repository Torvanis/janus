package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 2a.

// --- the model a request is authorized for is the model the provider sees -----

// restrictedModel adds a second enabled model that nobody is granted.
func (h *harness) restrictedModel(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "restricted-model", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	all, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, m := range all {
		if m.Name == "restricted-model" {
			id = m.ID
		}
	}
	if id == "" {
		t.Fatal("restricted model was not created")
	}
	if err := h.store.SetModelRates(ctx, id, store.RateCard{RateInNano: 1, RateOutNano: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, id, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
}

func (h *harness) upstreamSaw(text string) bool {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	for _, req := range h.upstreamRequests {
		if bytes.Contains(req.Body, []byte(text)) {
			return true
		}
	}
	return false
}

// upstreamGot reports whether the upstream received exactly this body.
func (h *harness) upstreamGot(body []byte) bool {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	for _, req := range h.upstreamRequests {
		if bytes.Equal(req.Body, body) {
			return true
		}
	}
	return false
}

func (h *harness) upstreamCount() int {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	return len(h.upstreamRequests)
}

func multipartBody(t *testing.T, fields [][2]string, fileBytes int) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if f[0] == "file" {
			fw, _ := mw.CreateFormFile("file", "a.wav")
			_, _ = fw.Write(bytes.Repeat([]byte("A"), fileBytes))
			continue
		}
		_ = mw.WriteField(f[0], f[1])
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

// The grant check used to read the model from a header or query parameter
// while the provider read it from the body, so a caller granted one model
// could be served another.
func TestModelNamedOutsideTheBodyCannotOverrideIt(t *testing.T) {
	h := newHarness(t)
	h.restrictedModel(t)

	// Multipart: header says the granted model, the form says the restricted one.
	ct, body := multipartBody(t, [][2]string{{"model", "restricted-model"}, {"file", ""}}, 64)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("multipart naming an ungranted model: got %d %s, want 403", rec.Code, rec.Body.String())
	}

	// The same with ?model=.
	req = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions?model=test-model", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("multipart with ?model=: got %d, want 403", rec.Code)
	}

	// JSON under a non-JSON content type, routed by the header.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"restricted-model","messages":[]}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("JSON sent as text/plain: got %d, want 400", rec.Code)
	}
	if h.upstreamSaw("restricted-model") {
		t.Fatal("the upstream received a request for a model the caller is not granted")
	}
}

// Two fields that read as "model" let the gateway and the provider pick
// different ones.
func TestAmbiguousModelFieldsAreRefused(t *testing.T) {
	h := newHarness(t)
	h.restrictedModel(t)
	for name, raw := range map[string]string{
		"case variant":       `{"model":"restricted-model","MODEL":"test-model","messages":[]}`,
		"case variant first": `{"Model":"test-model","model":"restricted-model","messages":[]}`,
		"repeated":           `{"model":"test-model","messages":[],"model":"restricted-model"}`,
		"escaped repeat":     `{"model":"test-model","messages":[],"\u006dodel":"restricted-model"}`,
		"not a string":       `{"model":["restricted-model"],"messages":[]}`,
		"trailing document":  `{"model":"test-model","messages":[]}{"model":"restricted-model"}`,
		"byte order mark":    "\xef\xbb\xbf" + `{"model":"test-model","messages":[]}`,
		"invalid UTF-8":      `{"model":"test-model","messages":[{"role":"user","content":"` + "\xff" + `"}]}`,
		"array":              `[{"model":"test-model"}]`,
		"empty model":        `{"model":"","messages":[]}`,
	} {
		before := h.upstreamCount()
		rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json", []byte(raw))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body.String())
		}
		if h.upstreamCount() != before {
			t.Errorf("%s: the request reached the upstream", name)
		}
	}
	// A nested "model" is not the request's model and is left alone.
	rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json",
		[]byte(`{"model":"test-model","metadata":{"model":"x","MODEL":"y"},"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("nested model keys: got %d %s, want 200", rec.Code, rec.Body.String())
	}
	// Two multipart model fields.
	ct, body := multipartBody(t, [][2]string{{"model", "test-model"}, {"file", ""}, {"model", "restricted-model"}}, 64)
	if rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions", ct, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("two multipart model fields: got %d, want 400", rec.Code)
	}
	ct, body = multipartBody(t, [][2]string{{"Model", "restricted-model"}, {"file", ""}}, 64)
	if rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions", ct, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("multipart field named Model: got %d, want 400", rec.Code)
	}
}

// A body too large to buffer is forwarded as it arrives. The model must be in
// the buffered part, and a second one arriving later ends the upload before
// the provider has a complete request.
func TestOversizedBodyCannotNameASecondModelLater(t *testing.T) {
	h := newHarness(t)
	h.restrictedModel(t)
	old := maxInMemoryBody
	maxInMemoryBody = 4 << 10
	t.Cleanup(func() { maxInMemoryBody = old })
	pad := strings.Repeat("a", 256<<10)

	// JSON: the granted model first, the restricted one after the buffer.
	raw := `{"model":"test-model","messages":[{"role":"user","content":"` + pad + `"}],"model":"restricted-model"}`
	rec := h.doRaw(http.MethodPost, "/v1/chat/completions", "application/json", []byte(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("late second model (JSON): got %d %s, want 400", rec.Code, rec.Body.String())
	}
	// JSON: no model in the buffered part, a header naming the granted one.
	raw = `{"messages":[{"role":"user","content":"` + pad + `"}],"model":"restricted-model"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("model after the buffer (JSON): got %d, want 400", rec.Code)
	}
	// Multipart: model after the file.
	ct, body := multipartBody(t, [][2]string{{"file", ""}, {"model", "restricted-model"}}, 256<<10)
	req = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("model after the file (multipart): got %d, want 400", rec.Code)
	}
	// Multipart: the granted model before the file, the restricted one after.
	ct, body = multipartBody(t, [][2]string{{"model", "test-model"}, {"file", ""}, {"model", "restricted-model"}}, 256<<10)
	if rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions", ct, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("late second model (multipart): got %d %s, want 400", rec.Code, rec.Body.String())
	}
	if h.upstreamSaw("restricted-model") {
		t.Fatal("the upstream received the restricted model's name")
	}

	// A well-formed oversized upload still goes through, byte for byte.
	ct, body = multipartBody(t, [][2]string{{"model", "test-model"}, {"file", ""}}, 256<<10)
	if rec := h.doRaw(http.MethodPost, "/v1/audio/transcriptions", ct, body); rec.Code != http.StatusOK {
		t.Fatalf("well-formed oversized upload: got %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !h.upstreamGot(body) {
		t.Fatal("the oversized upload was not forwarded unchanged")
	}
}

// Requests with no body still name their model in the header or the query.
func TestBodylessRequestMayNameItsModelInTheHeader(t *testing.T) {
	h := newHarness(t)
	if err := h.store.RecordUpstreamObject(context.Background(), store.UpstreamObject{
		UpstreamID: h.model.UpstreamID, ObjectID: "video_123", Kind: "video",
		OwnerType: store.ObjectOwnerUser, OwnerID: h.user.ID, ModelID: h.model.ID, AccessID: h.model.ID, RequestedModel: "test-model",
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_123", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status check with X-Janus-Model: got %d %s", rec.Code, rec.Body.String())
	}
	// A JSON body that names no model at all may also be routed by the header.
	req = httptest.NewRequest(http.MethodPost, "/v1/videos/video_123/remix", strings.NewReader(`{"prompt":"again"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("body without a model, header naming it: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestMultipartModelScanAcrossReads(t *testing.T) {
	_, body := multipartBody(t, [][2]string{{"purpose", "x"}, {"file", ""}, {"model", "m"}, {"other", "y"}}, 9000)
	boundary := string(body[2:bytes.IndexByte(body, '\r')])
	for _, chunk := range []int{1, 7, 64, 4096, len(body)} {
		scan := newMultipartModelScan(boundary)
		for i := 0; i < len(body); i += chunk {
			end := min(i+chunk, len(body))
			scan.feed(body[i:end])
		}
		if scan.count != 1 || scan.variant {
			t.Errorf("chunk %d: count %d variant %v, want exactly one model part", chunk, scan.count, scan.variant)
		}
	}
	// File content that only looks like a form field is not one.
	_, body = multipartBody(t, [][2]string{{"file", ""}}, 10)
	decoy := bytes.Replace(body, []byte("AAAAAAAAAA"), []byte(`<input name="model"> Content-Disposition: form-data; name="model"`), 1)
	scan := newMultipartModelScan(string(decoy[2:bytes.IndexByte(decoy, '\r')]))
	scan.feed(decoy)
	if scan.count != 0 {
		t.Errorf("file content was read as a form field (count %d)", scan.count)
	}
}

func TestJSONModelScanAcrossReads(t *testing.T) {
	raw := []byte(`{"a":{"model":"no"},"b":["model",{"MODEL":1}],"s":"\"model\":\"x\"","model":"caf\u00e9-\"m\"","z":1}`)
	for _, chunk := range []int{1, 3, 16, len(raw)} {
		scan := &jsonModelScan{}
		for i := 0; i < len(raw); i += chunk {
			scan.feed(raw[i:min(i+chunk, len(raw))])
		}
		if !scan.found || scan.value != `café-"m"` || scan.count != 1 || scan.problem() != "" {
			t.Errorf("chunk %d: found %v value %q count %d problem %q", chunk, scan.found, scan.value, scan.count, scan.problem())
		}
	}
}

func TestTailGuardStopsAtASecondModel(t *testing.T) {
	scan := &jsonModelScan{}
	scan.feed([]byte(`{"model":"a","x":"`))
	guarded := requestModel{Name: "a", tail: scan}.guardTail(strings.NewReader(`pad","model":"b"}`))
	if _, err := io.ReadAll(guarded); !errors.Is(err, errModelFieldRepeated) {
		t.Fatalf("got %v, want the repeated-model error", err)
	}
}

// --- sign-in settings need a browser session ---------------------------------

// A leaked API token used to be enough to put a password on the account behind
// it and receive a browser session in return.
func TestAPITokenCannotChangeSignInSettings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/me/local/password"},
		{http.MethodPost, "/api/v1/me/local/totp/start"},
		{http.MethodPost, "/api/v1/me/local/totp/confirm"},
		{http.MethodPost, "/api/v1/me/local/totp/disable"},
		{http.MethodGet, "/api/v1/me/local/totp/qr.png"},
	} {
		rec := h.do(c.method, c.path, map[string]any{"new_password": "Correct-Horse-Battery-9!", "password": "x", "code": "000000"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with an API token: got %d, want 403", c.method, c.path, rec.Code)
		}
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == "janus_session" && cookie.Value != "" {
				t.Errorf("%s %s handed a browser session to an API token", c.method, c.path)
			}
		}
	}
	if _, err := h.store.LocalCredential(ctx, h.user.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an API token put a password on the account (err %v)", err)
	}
	// Reading the status with a token is still allowed.
	if rec := h.do(http.MethodGet, "/api/v1/me/local", nil); rec.Code != http.StatusOK {
		t.Fatalf("status read with a token: got %d", rec.Code)
	}
}

// The first password on an account that signs in through an identity provider
// comes from an administrator.
func TestFirstPasswordIsSetByAnAdministrator(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sso, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-sso", "sso@example.com", "SSO User", false, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.server.Sessions.Create(ctx, sso.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(session, http.MethodPut, "/api/v1/me/local/password", map[string]any{"new_password": "Correct-Horse-Battery-9!"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-set first password: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	if _, err := h.store.LocalCredential(ctx, sso.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the account gave itself a password")
	}
	// The administrator adds it; the person then changes it themselves.
	admin, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/users/"+sso.ID+"/password", map[string]any{"password": "Temporary-Horse-Battery-1!"}); rec.Code != http.StatusOK {
		t.Fatalf("admin sets the first password: %d %s", rec.Code, rec.Body.String())
	}
	session, err = h.server.Sessions.Create(ctx, sso.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/me/local/password", map[string]any{"new_password": "Correct-Horse-Battery-9!"})
	if rec.Code != http.StatusOK {
		t.Fatalf("changing the temporary password: got %d %s", rec.Code, rec.Body.String())
	}
}

// Every new API token is on the audit record, including one minted with
// another token.
func TestTokenCreationIsAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.do(http.MethodPost, "/api/v1/tokens", map[string]any{"description": "minted by a token"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint with a token: %d %s", rec.Code, rec.Body.String())
	}
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(session, http.MethodPost, "/api/v1/tokens", map[string]any{"description": "minted in the browser"}); rec.Code != http.StatusCreated {
		t.Fatalf("mint with a session: %d %s", rec.Code, rec.Body.String())
	}
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "token_created"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("token_created audit entries: got %d, want 2", len(entries))
	}
	var withToken, withSession bool
	for _, e := range entries {
		var detail map[string]any
		_ = json.Unmarshal([]byte(e.NewValue), &detail)
		switch detail["created_with"] {
		case "api token":
			withToken = detail["created_with_token_id"] != "" && detail["created_with_token_id"] != nil
		case "browser session":
			withSession = true
		}
		if strings.Contains(e.NewValue, "janus_") && strings.Contains(e.NewValue, "value") {
			t.Fatal("the audit record holds the token value")
		}
	}
	if !withToken || !withSession {
		t.Fatalf("audit entries do not say how each token was created: %+v", entries)
	}
}

// --- metrics -------------------------------------------------------------------

// Metrics used to be served, unauthenticated, on the port users reach.
func TestMetricsAreNotOnThePublicListener(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "janus_") {
		t.Fatal("the public listener serves Prometheus metrics")
	}
	metrics := h.server.MetricsHandler()
	rec = httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "janus_build_info") {
		t.Fatalf("the metrics listener: got %d", rec.Code)
	}
	for _, path := range []string{"/", "/healthz", "/api/v1/config", "/v1/models", "/metrics/x"} {
		rec = httptest.NewRecorder()
		metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("the metrics listener answered %s with %d, want 404", path, rec.Code)
		}
	}
}
