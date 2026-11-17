package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/endpoints"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Regression tests for the 2026-11 security review, batch 2b-2: batch jobs
// and fine-tuning jobs. A batch names its models inside an uploaded file,
// past the grant check; and neither job is metered by the call that starts it.

func (h *harness) route(key, reply string) {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()
	if h.upstreamRoutes == nil {
		h.upstreamRoutes = map[string]string{}
	}
	h.upstreamRoutes[key] = reply
}

// uploadFile stores a file at the fake provider as the given token.
func (h *harness) uploadFile(t *testing.T, token, id string) {
	t.Helper()
	h.upstreamReply = `{"id":"` + id + `","object":"file","purpose":"batch"}`
	ct, form := multipartBody(t, [][2]string{{"purpose", "batch"}, {"file", ""}}, 16)
	req := httptest.NewRequest(http.MethodPost, "/v1/files", bytes.NewReader(form))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Janus-Model", "test-model")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s: %d %s", id, rec.Code, rec.Body.String())
	}
	h.upstreamReply = ""
}

func batchLineJSON(id, url, body string) string {
	return `{"custom_id":"` + id + `","method":"POST","url":"` + url + `","body":` + body + `}`
}

func (h *harness) createBatch(token, fileID string) *httptest.ResponseRecorder {
	return h.doWithToken(token, http.MethodPost, "/v1/batches", map[string]any{
		"input_file_id": fileID, "endpoint": "/v1/chat/completions", "completion_window": "24h"})
}

func TestBatchLinesAreHeldToTheCallersGrants(t *testing.T) {
	h := newHarness(t)
	h.restrictedModel(t)
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	h.uploadFile(t, h.token, "file-batchin001")
	h.uploadFile(t, other, "file-otherfile1")
	const content = "GET /v1/files/file-batchin001/content"
	ok := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

	for name, c := range map[string]struct {
		lines string
		want  int
	}{
		"a model the caller is not granted": {batchLineJSON("1", "/v1/chat/completions", ok) + "\n" +
			batchLineJSON("2", "/v1/chat/completions", `{"model":"restricted-model","messages":[]}`), http.StatusForbidden},
		"a model the gateway does not know": {batchLineJSON("1", "/v1/chat/completions", `{"model":"gpt-unknown","messages":[]}`), http.StatusForbidden},
		"a line for another endpoint":       {batchLineJSON("1", "/v1/embeddings", ok), http.StatusBadRequest},
		"a line with two model fields":      {batchLineJSON("1", "/v1/chat/completions", `{"model":"test-model","MODEL":"restricted-model","messages":[]}`), http.StatusBadRequest},
		"a line with no model":              {batchLineJSON("1", "/v1/chat/completions", `{"messages":[]}`), http.StatusBadRequest},
		"a line that is not a request":      {`["not","a","request"]`, http.StatusBadRequest},
		"a line referring to another user's file": {batchLineJSON("1", "/v1/chat/completions",
			`{"model":"test-model","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-otherfile1"}}]}]}`), http.StatusNotFound},
		"an empty file": {"\n\n", http.StatusBadRequest},
	} {
		h.route(content, c.lines)
		if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != c.want {
			t.Errorf("%s: got %d %s, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}
	// Someone else's input file, and a file nobody recorded.
	h.route(content, batchLineJSON("1", "/v1/chat/completions", ok))
	if rec := h.createBatch(other, "file-batchin001"); rec.Code != http.StatusNotFound {
		t.Errorf("another user's input file: got %d, want 404", rec.Code)
	}
	if rec := h.createBatch(h.token, "file-neverseen01"); rec.Code != http.StatusNotFound {
		t.Errorf("an unrecorded input file: got %d, want 404", rec.Code)
	}
	// A batch for an endpoint the gateway cannot meter.
	if rec := h.do(http.MethodPost, "/v1/batches", map[string]any{"input_file_id": "file-batchin001", "endpoint": "/v1/images/generations"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unsupported endpoint: got %d, want 400", rec.Code)
	}
	for _, path := range h.upstreamPaths() {
		if path == "/v1/batches" {
			t.Fatal("a refused batch was sent to the provider")
		}
	}

	// A clean batch goes through and belongs to its creator.
	h.route("POST /v1/batches", `{"id":"batch_good0001","object":"batch","status":"validating"}`)
	if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != http.StatusOK {
		t.Fatalf("clean batch: %d %s", rec.Code, rec.Body.String())
	}
	h.route("GET /v1/batches/batch_good0001", `{"id":"batch_good0001","status":"in_progress"}`)
	if rec := h.do(http.MethodGet, "/v1/batches/batch_good0001", nil); rec.Code != http.StatusOK {
		t.Errorf("owner reads the batch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doWithToken(other, http.MethodGet, "/v1/batches/batch_good0001", nil); rec.Code != http.StatusNotFound {
		t.Errorf("another user reads the batch: got %d, want 404", rec.Code)
	}
	if rec := h.doWithToken(other, http.MethodPost, "/v1/batches/batch_good0001/cancel", nil); rec.Code != http.StatusNotFound {
		t.Errorf("another user cancels the batch: got %d, want 404", rec.Code)
	}

}

// The requests inside a batch run later, without passing through the
// gateway again, so the Security Gateway reads each line when the batch is
// created: the secrets, personal-data and term checks apply to it as they
// would to the request it holds.
func TestBatchLinesAreInspectedByTheSecurityGateway(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE" // AWS's documentation example
	const content = "GET /v1/files/file-batchin001/content"
	clean := batchLineJSON("1", "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	leaky := batchLineJSON("2", "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"my key is `+strings.Replace(key, "I", `\u0049`, 1)+`"}]}`)
	violations := func(h *harness) []*store.SecgwViolation {
		t.Helper()
		if err := h.server.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		vs, err := h.store.ListSecgwViolations(context.Background(), store.SecgwViolationFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return vs
	}
	for mode, want := range map[string]int{store.SecgwModeObserve: http.StatusOK, store.SecgwModeRedact: http.StatusForbidden, store.SecgwModeBlock: http.StatusForbidden} {
		h := newHarness(t)
		h.uploadFile(t, h.token, "file-batchin001")
		h.secgwPolicy(t, store.SecgwPolicy{Name: "secrets", Enabled: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: mode}}})
		h.route("POST /v1/batches", `{"id":"batch_good0001","object":"batch","status":"validating"}`)

		// A clean batch is no longer refused because a policy exists.
		h.route(content, clean+"\n")
		if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != http.StatusOK {
			t.Fatalf("%s: clean batch under a policy: %d %s", mode, rec.Code, rec.Body.String())
		}
		if vs := violations(h); len(vs) != 0 {
			t.Fatalf("%s: a clean batch recorded %d violations", mode, len(vs))
		}
		// A line with a secret (JSON-escaped, as a line may be).
		h.route(content, clean+"\n"+leaky+"\n")
		rec := h.createBatch(h.token, "file-batchin001")
		if rec.Code != want {
			t.Fatalf("%s: batch with a secret in line 2: got %d %s, want %d", mode, rec.Code, rec.Body.String(), want)
		}
		if want == http.StatusForbidden && (!strings.Contains(rec.Body.String(), "Line 2") || !strings.Contains(rec.Body.String(), CodeSecurityBlocked)) {
			t.Errorf("%s: refusal does not name the line: %s", mode, rec.Body.String())
		}
		vs := violations(h)
		if len(vs) != 1 || vs[0].Kind != string(store.SecgwCheckSecrets) {
			t.Fatalf("%s: violations recorded: %+v", mode, vs)
		}
		if wantAction := map[bool]string{true: store.SecgwActionObserved, false: store.SecgwActionBlocked}[want == http.StatusOK]; vs[0].Action != wantAction {
			t.Errorf("%s: violation recorded as %q, want %q", mode, vs[0].Action, wantAction)
		}
		sent := 0
		for _, path := range h.upstreamPaths() {
			if path == "/v1/batches" {
				sent++
			}
		}
		if wantSent := map[bool]int{true: 2, false: 1}[want == http.StatusOK]; sent != wantSent {
			t.Errorf("%s: %d batches reached the provider, want %d", mode, sent, wantSent)
		}
	}

	// A check that asks a model about each message and stops what it flags
	// cannot run on requests made later: the batch is refused.
	h := newHarness(t)
	h.uploadFile(t, h.token, "file-batchin001")
	h.route(content, clean+"\n")
	h.secgwPolicy(t, store.SecgwPolicy{Name: "injection", Enabled: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckPromptInjection, Enabled: true,
		Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress, ClassifierModelID: h.model.ID}}})
	if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "prompt injection") {
		t.Errorf("batch under a blocking prompt-injection check: got %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// A batch reports no usage when it is created. The gateway follows it up and
// records what it used when it ends, once, at the batch price.
func TestFinishedBatchIsMeteredOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	h.uploadFile(t, h.token, "file-batchin001")
	line := batchLineJSON("1", "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	h.route("GET /v1/files/file-batchin001/content", line+"\n"+strings.Replace(line, `"1"`, `"2"`, 1)+"\n")
	h.route("POST /v1/batches", `{"id":"batch_good0001","object":"batch","status":"validating"}`)
	if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != http.StatusOK {
		t.Fatalf("create batch: %d %s", rec.Code, rec.Body.String())
	}
	before := len(h.waitForUsageEvents(2)) // the upload and the create

	// Still running: nothing is recorded and the job stays open.
	h.route("GET /v1/batches/batch_good0001", `{"id":"batch_good0001","status":"in_progress"}`)
	jobs, err := h.store.ClaimObjectJobs(ctx, 10, 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("open jobs: %d %v", len(jobs), err)
	}
	if h.server.settleJob(ctx, jobs[0]) {
		t.Fatal("a running batch was settled")
	}

	// Finished: two requests, each 100 in and 50 out, reported under the
	// provider's dated model name.
	h.route("GET /v1/batches/batch_good0001", `{"id":"batch_good0001","status":"completed","output_file_id":"file-batchout01","error_file_id":"file-batcherr01"}`)
	out := `{"custom_id":"%s","response":{"status_code":200,"body":{"model":"test-model-2026-01-01","usage":{"prompt_tokens":100,"completion_tokens":50}}}}`
	h.route("GET /v1/files/file-batchout01/content", strings.Replace(out, "%s", "1", 1)+"\n"+strings.Replace(out, "%s", "2", 1)+"\n")
	jobs, err = h.store.ClaimObjectJobs(ctx, 10, 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("open jobs after the first round: %d %v", len(jobs), err)
	}
	if !h.server.settleJob(ctx, jobs[0]) {
		t.Fatal("the finished batch was not settled")
	}
	if err := h.server.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var metered *store.UsageEvent
	events := h.waitForUsageEvents(before + 1)
	for _, e := range events {
		if strings.HasPrefix(e.RequestID, "batch:") {
			metered = e
		}
		if e.EndpointPath == "/v1/batches" && !strings.HasPrefix(e.RequestID, "batch:") && (e.TokensIn != 0 || e.CostNano != 0) {
			t.Errorf("starting the batch was billed: in %d cost %d (%s)", e.TokensIn, e.CostNano, e.AccountingMode)
		}
	}
	if len(events) != before+1 || metered == nil {
		t.Fatalf("usage events after settlement: %d, want %d with one metered batch event", len(events), before+1)
	}
	half := usage.Rates{InNano: 5 * usage.NanoPerUSD, OutNano: 15 * usage.NanoPerUSD}
	want := usage.ComputeCost(200, 100, 0, half)
	if metered.TokensIn != 200 || metered.TokensOut != 100 || metered.CostNano != want || metered.UserID != h.user.ID || metered.ModelID != h.model.ID {
		t.Fatalf("metered event: in %d out %d cost %d (want %d) user %q model %q", metered.TokensIn, metered.TokensOut, metered.CostNano, want, metered.UserID, metered.ModelID)
	}
	if metered.AccountingMode != usage.AccountingUpstream {
		t.Errorf("accounting mode %q, want provider-reported", metered.AccountingMode)
	}

	// Settled once: nothing is left to claim, and a second round adds nothing.
	if again := h.server.SettleStoredJobs(ctx); again != 0 {
		t.Fatalf("the batch was settled again (%d)", again)
	}
	if jobs, _ := h.store.ClaimObjectJobs(ctx, 10, 0); len(jobs) != 0 {
		t.Fatalf("%d jobs still open", len(jobs))
	}

	// The output belongs to whoever ran the batch.
	h.upstreamReply = `{"id":"file-batchout01"}`
	if rec := h.do(http.MethodGet, "/v1/files/file-batchout01", nil); rec.Code != http.StatusOK {
		t.Errorf("owner reads the output file: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doWithToken(other, http.MethodGet, "/v1/files/file-batchout01/content", nil); rec.Code != http.StatusNotFound {
		t.Errorf("another user reads the output file: got %d, want 404", rec.Code)
	}
}

func TestFineTuningJobIsOwnedAndFollowedUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, other := h.otherUser(t, "sub-b", "b@example.com")
	h.uploadFile(t, h.token, "file-training01")
	h.uploadFile(t, other, "file-otherfile1")

	if rec := h.do(http.MethodPost, "/v1/fine_tuning/jobs", map[string]any{"model": "test-model", "training_file": "file-otherfile1"}); rec.Code != http.StatusNotFound {
		t.Fatalf("training on another user's file: got %d, want 404", rec.Code)
	}
	h.route("POST /v1/fine_tuning/jobs", `{"id":"ftjob-abc12345","object":"fine_tuning.job","status":"queued"}`)
	if rec := h.do(http.MethodPost, "/v1/fine_tuning/jobs", map[string]any{"model": "test-model", "training_file": "file-training01"}); rec.Code != http.StatusOK {
		t.Fatalf("create job: %d %s", rec.Code, rec.Body.String())
	}
	h.route("GET /v1/fine_tuning/jobs/ftjob-abc12345", `{"id":"ftjob-abc12345","status":"running"}`)
	if rec := h.do(http.MethodGet, "/v1/fine_tuning/jobs/ftjob-abc12345", nil); rec.Code != http.StatusOK {
		t.Errorf("owner reads the job: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/fine_tuning/jobs/ftjob-abc12345"},
		{http.MethodGet, "/v1/fine_tuning/jobs/ftjob-abc12345/events"},
		{http.MethodPost, "/v1/fine_tuning/jobs/ftjob-abc12345/cancel"},
	} {
		if rec := h.doWithToken(other, c.method, c.path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("another user: %s %s got %d, want 404", c.method, c.path, rec.Code)
		}
	}
	before := len(h.waitForUsageEvents(1))
	h.route("GET /v1/fine_tuning/jobs/ftjob-abc12345", `{"id":"ftjob-abc12345","status":"succeeded","trained_tokens":1234,"result_files":["file-ftresult01"],"fine_tuned_model":"ft:test-model:org::abc"}`)
	if closed := h.server.SettleStoredJobs(ctx); closed != 1 {
		t.Fatalf("settled %d jobs, want 1", closed)
	}
	if err := h.server.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var trained *store.UsageEvent
	for _, e := range h.waitForUsageEvents(before + 1) {
		if e.EndpointPath == "/v1/fine_tuning/jobs" && e.TokensIn == 1234 {
			trained = e
		}
	}
	if trained == nil || trained.UserID != h.user.ID || trained.CostNano != 0 || trained.AccountingMode != usage.AccountingUnmetered {
		t.Fatalf("training usage event: %+v", trained)
	}
	h.upstreamReply = `{"id":"file-ftresult01"}`
	if rec := h.do(http.MethodGet, "/v1/files/file-ftresult01", nil); rec.Code != http.StatusOK {
		t.Errorf("owner reads the result file: %d", rec.Code)
	}
	if rec := h.doWithToken(other, http.MethodGet, "/v1/files/file-ftresult01", nil); rec.Code != http.StatusNotFound {
		t.Errorf("another user reads the result file: got %d, want 404", rec.Code)
	}
}

func TestRequestedBatchModel(t *testing.T) {
	for _, c := range []struct {
		served    string
		requested []string
		want      string
	}{
		{"gpt-4o-mini-2024-07-18", []string{"gpt-4o", "gpt-4o-mini"}, "gpt-4o-mini"},
		{"gpt-4o", []string{"gpt-4o", "gpt-4o-mini"}, "gpt-4o"},
		{"something-else", []string{"only-model"}, "only-model"},
		{"lonely", nil, "lonely"},
	} {
		if got := requestedBatchModel(c.served, c.requested); got != c.want {
			t.Errorf("requestedBatchModel(%q, %v) = %q, want %q", c.served, c.requested, got, c.want)
		}
	}
	if !jobEnded(endpoints.FamilyBatch, "completed") || jobEnded(endpoints.FamilyBatch, "finalizing") || !jobEnded(endpoints.FamilyFineTune, "succeeded") || jobEnded(endpoints.FamilyFineTune, "running") {
		t.Error("jobEnded misreads a provider status")
	}
}

func TestCreatedIDSnifferFindsALateID(t *testing.T) {
	stream := `data: {"type":"response.created","response":{"object":"response","created_at":1,"id":"resp_lateid0001","status":"in_progress"}}` + "\n\n"
	var ids []string
	sniffer := &createdIDSniffer{body: nopCloser{strings.NewReader(stream)}, found: func(id string) { ids = append(ids, id) }}
	buf := make([]byte, 4096)
	for {
		if _, err := sniffer.Read(buf); err != nil {
			break
		}
	}
	if len(ids) != 1 || ids[0] != "resp_lateid0001" {
		t.Fatalf("found %v", ids)
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }
