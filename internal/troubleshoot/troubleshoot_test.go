package troubleshoot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func n(v int64) *int64 { return &v }

// TestFilterMatchesCombinators covers AND / OR across the criterion groups,
// including the mixed pre/post-response case the proxy relies on.
func TestFilterMatchesCombinators(t *testing.T) {
	ev := &store.UsageEvent{ModelName: "gpt-x", UserID: "u1", UpstreamID: "up1", HTTPStatus: 429, ErrorCode: "rate_limited", TokensIn: 5000, TokensOut: 10}

	cases := []struct {
		name   string
		filter store.TroubleshootingFilter
		want   bool
	}{
		{"empty matches everything", store.TroubleshootingFilter{}, true},
		{"model AND user both match", store.TroubleshootingFilter{Match: "all", Models: []string{"GPT-X"}, UserIDs: []string{"u1"}}, true},
		{"model AND error: error mismatch fails", store.TroubleshootingFilter{Match: "all", Models: []string{"gpt-x"}, ErrorCodes: []string{"quota_exceeded"}}, false},
		{"model OR error: error mismatch still matches via model", store.TroubleshootingFilter{Match: "any", Models: []string{"gpt-x"}, ErrorCodes: []string{"quota_exceeded"}}, true},
		{"OR with nothing matching", store.TroubleshootingFilter{Match: "any", Models: []string{"other"}, UpstreamIDs: []string{"up9"}}, false},
		{"OR upstream alone", store.TroubleshootingFilter{Match: "any", Models: []string{"other"}, UpstreamIDs: []string{"up1"}}, true},
		{"failure outcome", store.TroubleshootingFilter{Outcome: "failure"}, true},
		{"success outcome", store.TroubleshootingFilter{Outcome: "success"}, false},
		{"http status list", store.TroubleshootingFilter{HTTPStatuses: []int{500, 429}}, true},
		{"token size AND", store.TroubleshootingFilter{TokensInGT: n(1000), TokensOutLT: n(100)}, true},
		{"token size strict boundary", store.TroubleshootingFilter{TokensInGT: n(5000)}, false},
		{"service token principal matches user_ids", store.TroubleshootingFilter{UserIDs: []string{"svc-1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(ev); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
	svc := &store.UsageEvent{ServiceTokenID: "svc-1", HTTPStatus: 200}
	if !(store.TroubleshootingFilter{UserIDs: []string{"svc-1"}}).Matches(svc) {
		t.Fatal("user_ids must also match a service-token principal")
	}
}

// TestFilterCouldMatchPreResponse proves the up-front check never rejects a
// request that the full filter could still match after the response.
func TestFilterCouldMatchPreResponse(t *testing.T) {
	ev := &store.UsageEvent{ModelName: "gpt-x", UserID: "u1"}
	if (store.TroubleshootingFilter{Match: "all", Models: []string{"other"}}).CouldMatch(ev) {
		t.Fatal("AND with a failing model criterion can never match; must not buffer")
	}
	if !(store.TroubleshootingFilter{Match: "all", Models: []string{"gpt-x"}, ErrorCodes: []string{"x"}}).CouldMatch(ev) {
		t.Fatal("AND with matching up-front criteria may still match after the response")
	}
	if !(store.TroubleshootingFilter{Match: "any", Models: []string{"other"}, ErrorCodes: []string{"x"}}).CouldMatch(ev) {
		t.Fatal("OR with a post-response criterion pending must keep buffering")
	}
	if (store.TroubleshootingFilter{Match: "any", Models: []string{"other"}, UserIDs: []string{"u9"}}).CouldMatch(ev) {
		t.Fatal("OR whose only criteria are up-front and all fail can never match")
	}
	if !(store.TroubleshootingFilter{Outcome: "failure"}).CouldMatch(ev) {
		t.Fatal("a filter with only post-response criteria must buffer every request")
	}
}

// TestFilterGates covers the rules-builder shape: include (OR) + require (AND)
// + exclude (NOT) evaluated together, and the pre-response short-circuits.
func TestFilterGates(t *testing.T) {
	ev := &store.UsageEvent{ModelName: "gpt-x", UserID: "u1", UpstreamID: "up1", HTTPStatus: 429, ErrorCode: "rate_limited", TokensIn: 5000, TokensOut: 10}
	crit := func(c store.TroubleshootingCriteria) *store.TroubleshootingCriteria { return &c }

	cases := []struct {
		name   string
		filter store.TroubleshootingFilter
		want   bool
	}{
		{"require alone: all satisfied", store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{Models: []string{"gpt-x"}, Outcome: "failure"})}, true},
		{"require alone: one fails", store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{Models: []string{"gpt-x"}, Outcome: "success"})}, false},
		{"include OR passes, require vetoes", store.TroubleshootingFilter{Match: "any", Models: []string{"gpt-x"}, Require: crit(store.TroubleshootingCriteria{UserIDs: []string{"u9"}})}, false},
		{"include OR passes, require passes", store.TroubleshootingFilter{Match: "any", Models: []string{"other"}, ErrorCodes: []string{"rate_limited"}, Require: crit(store.TroubleshootingCriteria{UserIDs: []string{"u1"}})}, true},
		{"exclude alone: match vetoes", store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{Models: []string{"GPT-X"}})}, false},
		{"exclude alone: no match admits", store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{Models: []string{"other"}, HTTPStatuses: []int{500}})}, true},
		{"exclude is any-of", store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{Models: []string{"other"}, HTTPStatuses: []int{429}})}, false},
		{"all three gates", store.TroubleshootingFilter{Match: "any", Models: []string{"gpt-x"}, UpstreamIDs: []string{"up9"}, Require: crit(store.TroubleshootingCriteria{Outcome: "failure"}), Exclude: crit(store.TroubleshootingCriteria{UserIDs: []string{"u2"}})}, true},
		{"empty optional gates are inert", store.TroubleshootingFilter{Require: &store.TroubleshootingCriteria{}, Exclude: &store.TroubleshootingCriteria{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(ev); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	// Pre-response: a failing require or a matching exclude on an up-front
	// criterion means the request can never be captured.
	pre := &store.UsageEvent{ModelName: "gpt-x", UserID: "u1"}
	if (store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{Models: []string{"other"}})}).CouldMatch(pre) {
		t.Fatal("require with a failing model can never match; must not buffer")
	}
	if (store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{UserIDs: []string{"u1"}})}).CouldMatch(pre) {
		t.Fatal("exclude matching the principal can never match; must not buffer")
	}
	if !(store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{HTTPStatuses: []int{500}})}).CouldMatch(pre) {
		t.Fatal("an exclude decided by the response must keep buffering")
	}
	if !(store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{Models: []string{"gpt-x"}, Outcome: "failure"})}).CouldMatch(pre) {
		t.Fatal("require satisfied up front must keep buffering")
	}

	// IsEmpty / Validate treat the optional gates as part of the filter and
	// drop them when they carry nothing.
	f := store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{Models: []string{"x"}})}
	if f.IsEmpty() {
		t.Fatal("a filter with only an exclude gate is not empty")
	}
	f = store.TroubleshootingFilter{Require: &store.TroubleshootingCriteria{}, Exclude: &store.TroubleshootingCriteria{}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Require != nil || f.Exclude != nil || !f.IsEmpty() {
		t.Fatalf("empty gates must be dropped by Validate: %+v", f)
	}
	bad := store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{Outcome: "meh"})}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "filter.exclude.outcome") {
		t.Fatalf("exclude vocabulary must be validated, got %v", err)
	}
	raw, _ := json.Marshal(store.TroubleshootingFilter{Match: "any", Models: []string{"a"}})
	if strings.Contains(string(raw), "require") || strings.Contains(string(raw), "exclude") {
		t.Fatalf("legacy wire shape must be unchanged when gates are absent: %s", raw)
	}
}

func TestConfigValidateDefaultsAndBounds(t *testing.T) {
	cfg := store.TroubleshootingConfig{Retention: store.TroubleshootingRetention{MaxCount: 10}}
	if err := cfg.Validate(false); err != nil {
		t.Fatalf("minimal config must validate: %v", err)
	}
	if cfg.Storage != store.TroubleshootStorageDatabase || cfg.MaxBodyBytes != store.TroubleshootDefaultMaxBodyBytes || cfg.Filter.Match != store.TroubleshootMatchAll {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if err := (&store.TroubleshootingConfig{}).Validate(false); err == nil {
		t.Fatal("an unbounded retention policy must be rejected")
	}
	disk := store.TroubleshootingConfig{Storage: "disk", Retention: store.TroubleshootingRetention{MaxAgeHours: 1}}
	if err := disk.Validate(false); err == nil {
		t.Fatal("disk backend must be rejected when no directory is configured")
	}
	if err := disk.Validate(true); err != nil {
		t.Fatalf("disk backend with a directory: %v", err)
	}
	huge := store.TroubleshootingConfig{MaxBodyBytes: store.TroubleshootMaxBodyBytesCeiling + 1, Retention: store.TroubleshootingRetention{MaxCount: 1}}
	if err := huge.Validate(false); err == nil {
		t.Fatal("max_body_bytes beyond the ceiling must be rejected")
	}
	bad := store.TroubleshootingConfig{Filter: store.TroubleshootingFilter{Match: "sometimes"}, Retention: store.TroubleshootingRetention{MaxCount: 1}}
	if err := bad.Validate(false); err == nil {
		t.Fatal("unknown combinator must be rejected")
	}
}

func TestCapturingWriterBoundsBodyAndForwardsEverything(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := NewCapturingWriter(rec, 8)
	cw.Header().Set("Content-Type", "text/event-stream")
	cw.WriteHeader(http.StatusAccepted)
	_, _ = cw.Write([]byte("hello "))
	_, _ = cw.Write([]byte("world!"))
	cw.Flush()
	if rec.Code != http.StatusAccepted || rec.Body.String() != "hello world!" {
		t.Fatalf("client must receive everything unchanged: %d %q", rec.Code, rec.Body.String())
	}
	if string(cw.Body()) != "hello wo" || !cw.Truncated() || cw.Status() != http.StatusAccepted {
		t.Fatalf("capture = %q truncated=%v status=%d", cw.Body(), cw.Truncated(), cw.Status())
	}
	if !rec.Flushed {
		t.Fatal("Flush must reach the underlying writer so streams keep streaming")
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("X-Api-Key", "k")
	h.Set("Cookie", "janus_session=abc")
	h.Set("Content-Type", "application/json")
	h.Add("Accept", "a")
	h.Add("Accept", "b")
	out := RedactHeaders(h)
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if out[name] != "[redacted]" {
			t.Fatalf("%s = %q, want redacted", name, out[name])
		}
	}
	if out["Content-Type"] != "application/json" || out["Accept"] != "a, b" {
		t.Fatalf("plain headers must survive: %+v", out)
	}
}

type fakeCipher struct{}

func (fakeCipher) Encrypt(p string) (string, error) { return "enc:" + p, nil }
func (fakeCipher) Decrypt(e string) (string, error) {
	return strings.TrimPrefix(e, "enc:"), nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "janus-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestCaptureRoundTripDatabaseEncryptedAndDisk exercises both backends and
// encryption at rest through Capture → Bodies, including body truncation and
// the disk file lifecycle under purge.
func TestCaptureRoundTripDatabaseEncryptedAndDisk(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	dir := t.TempDir()
	rec := New(s, fakeCipher{}, dir, nil)

	for _, backend := range []string{store.TroubleshootStorageDatabase, store.TroubleshootStorageDisk} {
		t.Run(backend, func(t *testing.T) {
			cfg := store.TroubleshootingConfig{
				Storage: backend, CaptureRequestBody: true, CaptureResponseBody: true, Encrypt: true, MaxBodyBytes: 16,
				Retention: store.TroubleshootingRetention{MaxCount: 100},
			}
			if err := cfg.Validate(true); err != nil {
				t.Fatal(err)
			}
			session, err := s.CreateTroubleshootingSession(ctx, cfg, time.Now().Add(time.Hour), "admin")
			if err != nil {
				t.Fatal(err)
			}
			ev := &store.UsageEvent{ID: store.NewID(), RequestID: "req-" + backend, ModelName: "m", UserID: "u", HTTPStatus: 500, ErrorCode: "upstream_error", TokensIn: 3}
			reqH := http.Header{"Authorization": {"Bearer x"}, "Content-Type": {"application/json"}}
			respH := http.Header{"Content-Type": {"application/json"}}
			rec.Capture(ctx, session, ev, Payload{
				RequestHeaders: reqH, RequestBody: []byte(`{"prompt":"hi"}`),
				ResponseHeaders: respH, ResponseBody: []byte(`{"error":"this response is longer than sixteen bytes"}`),
			})
			c, err := s.TroubleshootingCaptureByEvent(ctx, ev.ID)
			if err != nil {
				t.Fatalf("capture not persisted: %v", err)
			}
			if !c.Encrypted || !c.Truncated || c.StorageBackend != backend || c.HTTPStatus != 500 || c.ErrorCode != "upstream_error" {
				t.Fatalf("capture record = %+v", c)
			}
			if strings.Contains(c.RequestHeaders, "Bearer") {
				t.Fatalf("authorization header leaked into capture: %s", c.RequestHeaders)
			}
			if backend == store.TroubleshootStorageDisk {
				if c.StorageRef == "" || c.RequestBody != "" {
					t.Fatalf("disk capture must reference files and keep no body in the row: %+v", c)
				}
				if _, err := os.Stat(filepath.Join(dir, c.StorageRef, "request.body")); err != nil {
					t.Fatalf("request body file missing: %v", err)
				}
			} else if !strings.HasPrefix(c.RequestBody, "enc:") {
				t.Fatalf("database body must be stored through the cipher: %q", c.RequestBody)
			}
			req, resp, err := rec.Bodies(c)
			if err != nil {
				t.Fatalf("read bodies: %v", err)
			}
			if string(req) != `{"prompt":"hi"}` || string(resp) != `{"error":"this r` {
				t.Fatalf("bodies = %q / %q", req, resp)
			}
		})
	}

	stats, err := s.TroubleshootingCaptureStats(ctx)
	if err != nil || stats.Count != 2 || stats.TotalBytes == 0 {
		t.Fatalf("stats = %+v err=%v", stats, err)
	}
	deleted, err := rec.PurgeAll(ctx)
	if err != nil || deleted != 2 {
		t.Fatalf("purge = %d, %v", deleted, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("purge must remove disk files, %d entries left", len(entries))
	}
}

// TestCaptureRespectsFilterAndBodyToggles: a non-matching event stores
// nothing; body toggles off keep only headers/metadata.
func TestCaptureRespectsFilterAndBodyToggles(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	rec := New(s, nil, "", nil)
	cfg := store.TroubleshootingConfig{
		Filter:    store.TroubleshootingFilter{Outcome: "failure"},
		Retention: store.TroubleshootingRetention{MaxCount: 10},
	}
	_ = cfg.Validate(false)
	session, _ := s.CreateTroubleshootingSession(ctx, cfg, time.Time{}, "admin")

	ok := &store.UsageEvent{ID: store.NewID(), HTTPStatus: 200}
	rec.Capture(ctx, session, ok, Payload{RequestBody: []byte("x"), ResponseBody: []byte("y")})
	if _, err := s.TroubleshootingCaptureByEvent(ctx, ok.ID); err == nil {
		t.Fatal("a successful request must not be captured under outcome=failure")
	}
	failed := &store.UsageEvent{ID: store.NewID(), HTTPStatus: 502}
	rec.Capture(ctx, session, failed, Payload{RequestHeaders: http.Header{"X-Test": {"1"}}, RequestBody: []byte("x"), ResponseBody: []byte("y")})
	c, err := s.TroubleshootingCaptureByEvent(ctx, failed.ID)
	if err != nil {
		t.Fatalf("failed request must be captured: %v", err)
	}
	if c.RequestBody != "" || c.ResponseBody != "" || c.RequestBodyBytes != 0 || !strings.Contains(c.RequestHeaders, "X-Test") {
		t.Fatalf("bodies must be omitted when their toggles are off, headers kept: %+v", c)
	}

	// Encryption requested with no cipher: the capture is dropped rather
	// than stored in the clear against the admin's instruction.
	cfg.Encrypt, cfg.CaptureRequestBody = true, true
	session, _ = s.UpdateTroubleshootingSession(ctx, session.ID, cfg, time.Time{})
	third := &store.UsageEvent{ID: store.NewID(), HTTPStatus: 500}
	rec.Capture(ctx, session, third, Payload{RequestBody: []byte("secret")})
	if _, err := s.TroubleshootingCaptureByEvent(ctx, third.ID); err == nil {
		t.Fatal("must never store a body in the clear when encryption was requested but unavailable")
	}
}

// TestEnforceRetention applies age, count and size bounds and expires the
// session.
func TestEnforceRetention(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	rec := New(s, nil, "", nil)
	cfg := store.TroubleshootingConfig{Retention: store.TroubleshootingRetention{MaxAgeHours: 24, MaxCount: 3, MaxBytes: 25}}
	_ = cfg.Validate(false)
	session, err := s.CreateTroubleshootingSession(ctx, cfg, time.Now().Add(-time.Minute), "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insert := func(id string, age time.Duration, size int64) {
		if err := s.InsertTroubleshootingCapture(ctx, &store.TroubleshootingCapture{
			ID: id, SessionID: session.ID, UsageEventID: "ev-" + id, CapturedAt: now.Add(-age), SizeBytes: size,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("stale", 48*time.Hour, 10) // age
	insert("a", 4*time.Hour, 10)      // count (4th newest of the remaining)
	insert("b", 3*time.Hour, 10)      // size (oldest of the three kept, total 30 > 25)
	insert("c", 2*time.Hour, 10)
	insert("d", 1*time.Hour, 10)

	res, err := rec.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 3 || !res.SessionExpired || !res.AppliedMaxAge || !res.AppliedMaxCount || !res.AppliedMaxBytes {
		t.Fatalf("retention result = %+v", res)
	}
	remaining, total, err := s.ListTroubleshootingCaptures(ctx, store.TroubleshootingCaptureFilter{})
	if err != nil || total != 2 {
		t.Fatalf("remaining = %d err=%v", total, err)
	}
	ids := []string{remaining[0].ID, remaining[1].ID}
	if ids[0] != "d" || ids[1] != "c" {
		t.Fatalf("kept %v, want the two newest (d, c)", ids)
	}
	sess, _ := s.GetTroubleshootingSession(ctx, session.ID)
	if sess.Enabled || sess.Active(time.Now()) {
		t.Fatal("an expired session must be disabled by the retention pass")
	}
}

// TestArchiveLayout validates the tar.gz structure for one request and for a
// bulk export (captures.jsonl present and line-per-capture).
func TestArchiveLayout(t *testing.T) {
	a := Archiver{Gateway: ManifestGateway{Version: "test"}}
	entry := Entry{
		Event:        &store.UsageEvent{ID: "ev1", RequestID: "req_1", ModelName: "m", HTTPStatus: 500},
		Capture:      &store.TroubleshootingCapture{ID: "c1", UsageEventID: "ev1", RequestID: "req_1", RequestHeaders: `{"Content-Type":"application/json"}`, ResponseHeaders: `{"Content-Type":"text/event-stream"}`},
		RequestBody:  []byte(`{"prompt":"hi"}`),
		ResponseBody: []byte("data: {}\n\n"),
	}
	var buf bytes.Buffer
	if err := a.WriteOne(&buf, entry); err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, buf.Bytes())
	for _, want := range []string{"req_1/manifest.json", "req_1/request/headers.json", "req_1/request/body.json", "req_1/response/headers.json", "req_1/response/body.sse"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("archive missing %s; have %v", want, keys(files))
		}
	}
	var m Manifest
	if err := json.Unmarshal(files["req_1/manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != ArchiveFormat || m.Event.ID != "ev1" || m.Files.ResponseBody != "req_1/response/body.sse" || m.Gateway.Version != "test" {
		t.Fatalf("manifest = %+v", m)
	}
	if string(files["req_1/request/body.json"]) != `{"prompt":"hi"}` {
		t.Fatalf("request body = %q", files["req_1/request/body.json"])
	}

	buf.Reset()
	second := entry
	second.Capture = &store.TroubleshootingCapture{ID: "c2", UsageEventID: "ev2", RequestID: "req_2"}
	second.Event = nil
	second.RequestBody, second.ResponseBody = nil, []byte("plain text")
	entries := []*Entry{&entry, &second}
	i := 0
	err := a.WriteMany(&buf, func() (*Entry, error) {
		if i >= len(entries) {
			return nil, nil
		}
		e := entries[i]
		i++
		return e, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = readTarGz(t, buf.Bytes())
	for _, want := range []string{"README.txt", "export.json", "captures.jsonl", "req_1/manifest.json", "req_2/manifest.json", "req_2/response/body.txt"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("bulk archive missing %s; have %v", want, keys(files))
		}
	}
	if _, ok := files["req_2/request/body.json"]; ok {
		t.Fatal("an absent request body must not produce a file")
	}
	lines := strings.Split(strings.TrimSpace(string(files["captures.jsonl"])), "\n")
	if len(lines) != 2 {
		t.Fatalf("captures.jsonl has %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("captures.jsonl line is not JSON: %s", line)
		}
	}
}

func readTarGz(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		content, _ := io.ReadAll(tr)
		out[hdr.Name] = content
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A group criterion follows the same "any of" rule as every other list and
// takes part in the pre-response check. The semantics an admin will reach
// for: a person in Legal AND Research matches "Legal"; excluding "Research"
// keeps them out even though they are also in Legal; a service token is in
// no group and never matches.
func TestFilterGroups(t *testing.T) {
	crit := func(c store.TroubleshootingCriteria) *store.TroubleshootingCriteria { return &c }
	legalAndResearch := &store.UsageEvent{ModelName: "gpt-x", UserID: "u1", GroupIDs: []string{"g-legal", "g-research"}, HTTPStatus: 200}
	onlyEng := &store.UsageEvent{ModelName: "gpt-x", UserID: "u2", GroupIDs: []string{"g-eng"}, HTTPStatus: 200}
	token := &store.UsageEvent{ModelName: "gpt-x", ServiceTokenID: "svc-1", HTTPStatus: 200}

	cases := []struct {
		name   string
		filter store.TroubleshootingFilter
		ev     *store.UsageEvent
		want   bool
	}{
		{"include: member of one listed group", store.TroubleshootingFilter{GroupIDs: []string{"g-legal"}}, legalAndResearch, true},
		{"include: case-insensitive like the other lists", store.TroubleshootingFilter{GroupIDs: []string{"G-LEGAL"}}, legalAndResearch, true},
		{"include: not a member", store.TroubleshootingFilter{GroupIDs: []string{"g-legal"}}, onlyEng, false},
		{"include: any-of across several listed groups", store.TroubleshootingFilter{GroupIDs: []string{"g-finance", "g-eng"}}, onlyEng, true},
		{"exclude: one of their groups is enough to keep them out", store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{GroupIDs: []string{"g-research"}})}, legalAndResearch, false},
		{"require: must be in a listed group", store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{GroupIDs: []string{"g-legal"}})}, onlyEng, false},
		{"service token: never in a group, include fails", store.TroubleshootingFilter{GroupIDs: []string{"g-legal"}}, token, false},
		{"service token: never in a group, exclude does not fire", store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{GroupIDs: []string{"g-legal"}})}, token, true},
		{"groups + users under any: either admits", store.TroubleshootingFilter{Match: "any", UserIDs: []string{"u9"}, GroupIDs: []string{"g-eng"}}, onlyEng, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(tc.ev); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	// Known before the upstream answers: a failing group require, or a
	// matching group exclude, must stop buffering up front.
	if (store.TroubleshootingFilter{Require: crit(store.TroubleshootingCriteria{GroupIDs: []string{"g-legal"}})}).CouldMatch(onlyEng) {
		t.Fatal("require on a group the caller is not in can never match; must not buffer")
	}
	if (store.TroubleshootingFilter{Exclude: crit(store.TroubleshootingCriteria{GroupIDs: []string{"g-research"}})}).CouldMatch(legalAndResearch) {
		t.Fatal("exclude on one of the caller's groups can never match; must not buffer")
	}
	if !(store.TroubleshootingFilter{GroupIDs: []string{"g-legal"}}).CouldMatch(legalAndResearch) {
		t.Fatal("a member must keep buffering")
	}

	// Validation keeps the new list and drops an empty optional gate as before.
	f := store.TroubleshootingFilter{GroupIDs: []string{"g-legal"}, Exclude: &store.TroubleshootingCriteria{}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Exclude != nil || f.IsEmpty() {
		t.Fatal("group criterion must count as populated; empty exclude must be dropped")
	}
}
