package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// TestTroubleshootingModeEndToEnd walks the headline flow: enable a session
// via the admin API with a filter, proxy matching and non-matching requests,
// see the capture marker in the admin request log, download the tar.gz for
// the captured request, then disable and purge.
func TestTroubleshootingModeEndToEnd(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // enforcing/fallback/capture behaviour is Business
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// Off by default: the status document says so and the proxy pays nothing.
	status := h.doAsSession(session, http.MethodGet, "/api/v1/admin/troubleshooting", nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"active":false`) {
		t.Fatalf("initial status = %d %s", status.Code, status.Body.String())
	}

	// Enable: capture only 4xx/5xx or explicit error codes for test-model,
	// with request + response bodies, encrypted, 24h window, 100-row cap.
	enable := h.doAsSession(session, http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{
		"enabled": true,
		"config": map[string]any{
			"filter":                map[string]any{"match": "all", "models": []string{"test-model"}, "outcome": "failure"},
			"retention":             map[string]any{"max_age_hours": 24, "max_count": 100},
			"capture_request_body":  true,
			"capture_response_body": true,
			"encrypt":               true,
		},
		"expires_in_hours": 24,
	})
	if enable.Code != http.StatusOK {
		t.Fatalf("enable returned %d: %s", enable.Code, enable.Body.String())
	}
	var doc troubleshootingStatus
	if err := json.Unmarshal(enable.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Active || doc.Session == nil || !doc.Session.Config.Encrypt || len(doc.Warnings) != 0 {
		t.Fatalf("enabled status = %+v", doc)
	}

	// A successful completion does not match outcome=failure → no capture.
	ok := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", ok.Code, ok.Body.String())
	}
	// A pre-proxy rejection (unknown model → 404) for a different model does
	// not match the model criterion either.
	other := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "nope", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if other.Code == http.StatusOK {
		t.Fatalf("expected a rejection for an unknown model, got %d", other.Code)
	}
	// Force a failure on test-model: a rate-limit rejection is the simplest
	// deterministic one — set an aggressive per-user limit.
	// Instead, reuse the unknown-model path but with the filter widened via
	// update so the capture is exercised on a real rejection envelope.
	update := h.doAsSession(session, http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{
		"config": map[string]any{
			"filter":                map[string]any{"match": "any", "models": []string{"nope"}, "http_statuses": []int{500, 502}},
			"retention":             map[string]any{"max_age_hours": 24, "max_count": 100},
			"capture_request_body":  true,
			"capture_response_body": true,
			"encrypt":               true,
		},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", update.Code, update.Body.String())
	}
	rejected := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "nope", "messages": []map[string]string{{"role": "user", "content": "capture me"}},
	})
	if rejected.Code == http.StatusOK {
		t.Fatalf("expected a rejection, got %d", rejected.Code)
	}
	events := h.waitForUsageEvents(3)
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	var target *store.UsageEvent
	for _, e := range events {
		if e.RequestedModelName == "nope" || e.ModelName == "nope" {
			if target == nil || e.CreatedAt.After(target.CreatedAt) {
				target = e
			}
		}
	}
	if target == nil {
		t.Fatalf("no usage event for the rejected request: %+v", events)
	}

	// Exactly one capture exists, for the rejected "nope" request.
	captures := h.doAsSession(session, http.MethodGet, "/api/v1/admin/troubleshooting/captures", nil)
	var list struct {
		Captures []*store.TroubleshootingCapture `json:"captures"`
		Total    int                             `json:"total_count"`
	}
	if err := json.Unmarshal(captures.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Captures[0].UsageEventID != target.ID || !list.Captures[0].Encrypted {
		t.Fatalf("captures = %+v (total %d), want exactly the rejected request, encrypted", list.Captures, list.Total)
	}

	// The admin request log marks the captured row.
	log := h.doAsSession(session, http.MethodGet, "/api/v1/requests?scope=all&limit=50", nil)
	var page struct {
		Requests []*store.UsageEvent `json:"requests"`
	}
	if err := json.Unmarshal(log.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, e := range page.Requests {
		if e.HasCapture {
			marked++
			if e.ID != target.ID {
				t.Fatalf("has_capture set on the wrong event %s", e.ID)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("has_capture marked %d rows, want 1", marked)
	}

	// Download: a tar.gz with manifest, redacted headers and both bodies.
	dl := h.doAsSession(session, http.MethodGet, "/api/v1/admin/requests/"+target.ID+"/download", nil)
	if dl.Code != http.StatusOK || dl.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("download = %d %s: %s", dl.Code, dl.Header().Get("Content-Type"), dl.Body.String())
	}
	if !strings.Contains(dl.Header().Get("Content-Disposition"), ".tar.gz") {
		t.Fatalf("Content-Disposition = %q", dl.Header().Get("Content-Disposition"))
	}
	files := readArchive(t, dl.Body.Bytes())
	dir := ""
	for name := range files {
		if strings.HasSuffix(name, "/manifest.json") {
			dir = strings.TrimSuffix(name, "/manifest.json")
		}
	}
	if dir == "" {
		t.Fatalf("archive has no manifest: %v", fileNames(files))
	}
	reqBody := string(files[dir+"/request/body.json"])
	if !strings.Contains(reqBody, "capture me") {
		t.Fatalf("request body not captured verbatim: %q (files %v)", reqBody, fileNames(files))
	}
	respBody := string(files[dir+"/response/body.json"])
	if !strings.Contains(respBody, `"error"`) {
		t.Fatalf("response body should be the error envelope the client saw: %q", respBody)
	}
	headers := string(files[dir+"/request/headers.json"])
	if strings.Contains(headers, h.token) || !strings.Contains(headers, "[redacted]") {
		t.Fatalf("Authorization must be redacted in the archive: %s", headers)
	}

	// Un-captured requests have nothing to download.
	for _, e := range events {
		if e.ID != target.ID {
			if miss := h.doAsSession(session, http.MethodGet, "/api/v1/admin/requests/"+e.ID+"/download", nil); miss.Code != http.StatusNotFound {
				t.Fatalf("download of an uncaptured request = %d, want 404", miss.Code)
			}
			break
		}
	}

	// Bulk export carries captures.jsonl.
	export := h.doAsSession(session, http.MethodGet, "/api/v1/admin/troubleshooting/export", nil)
	if export.Code != http.StatusOK {
		t.Fatalf("export = %d", export.Code)
	}
	bulk := readArchive(t, export.Body.Bytes())
	if lines := strings.TrimSpace(string(bulk["captures.jsonl"])); strings.Count(lines, "\n") != 0 || lines == "" {
		t.Fatalf("captures.jsonl should hold exactly one line: %q", lines)
	}

	// Disable, then purge. Stats go to zero and the status is inactive.
	if off := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/troubleshooting", nil); off.Code != http.StatusOK || !strings.Contains(off.Body.String(), `"active":false`) {
		t.Fatalf("disable = %d %s", off.Code, off.Body.String())
	}
	if again := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/troubleshooting", nil); again.Code != http.StatusNotFound {
		t.Fatalf("disabling twice = %d, want 404", again.Code)
	}
	purge := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/troubleshooting/data", nil)
	if purge.Code != http.StatusOK || !strings.Contains(purge.Body.String(), `"deleted":1`) {
		t.Fatalf("purge = %d %s", purge.Code, purge.Body.String())
	}
	stats := h.doAsSession(session, http.MethodGet, "/api/v1/admin/troubleshooting/stats", nil)
	if !strings.Contains(stats.Body.String(), `"count":0`) {
		t.Fatalf("stats after purge = %s", stats.Body.String())
	}

	// Audit trail names every step.
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{"troubleshooting_enabled", "troubleshooting_updated", "troubleshooting_disabled", "troubleshooting_purged", "troubleshooting_exported"} {
		if !seen[want] {
			t.Fatalf("audit missing %s: %v", want, seen)
		}
	}
}

// TestTroubleshootingPutValidation rejects unbounded retention, disk without
// a directory, and over-long windows.
func TestTroubleshootingPutValidation(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // enforcing/fallback/capture behaviour is Business
	session, _ := h.server.Sessions.Create(context.Background(), h.user.ID)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"unbounded retention", map[string]any{"enabled": true, "config": map[string]any{"retention": map[string]any{}}}},
		{"disk without dir", map[string]any{"enabled": true, "config": map[string]any{"storage": "disk", "retention": map[string]any{"max_count": 1}}}},
		{"window too long", map[string]any{"enabled": true, "config": map[string]any{"retention": map[string]any{"max_count": 1}}, "expires_in_hours": 24 * 30}},
		{"bad combinator", map[string]any{"enabled": true, "config": map[string]any{"filter": map[string]any{"match": "xor"}, "retention": map[string]any{"max_count": 1}}}},
	}
	for _, tc := range cases {
		rec := h.doAsSession(session, http.MethodPut, "/api/v1/admin/troubleshooting", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	// Default expiry is applied when omitted.
	okReq := h.doAsSession(session, http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{
		"enabled": true, "config": map[string]any{"retention": map[string]any{"max_count": 1}},
	})
	var doc troubleshootingStatus
	_ = json.Unmarshal(okReq.Body.Bytes(), &doc)
	if okReq.Code != http.StatusOK || doc.Session == nil || doc.Session.ExpiresAt.IsZero() ||
		time.Until(doc.Session.ExpiresAt) > troubleshootDefaultSessionHours*time.Hour+time.Minute {
		t.Fatalf("default expiry not applied: %d %+v", okReq.Code, doc.Session)
	}
	// Empty filter + unencrypted bodies produce warnings.
	warn := h.doAsSession(session, http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{
		"config": map[string]any{"retention": map[string]any{"max_count": 1}, "capture_request_body": true},
	})
	_ = json.Unmarshal(warn.Body.Bytes(), &doc)
	if len(doc.Warnings) < 2 {
		t.Fatalf("expected unencrypted + empty-filter warnings, got %v", doc.Warnings)
	}
}

func readArchive(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
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
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = b
	}
	return out
}

func fileNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Group membership reaches the capture decision from the real principal
// lookup: a session filtered to one group captures its members' requests,
// skips a non-member's, and skips a service token (which is in no group).
func TestTroubleshootingCapturesByGroup(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // enforcing/fallback/capture behaviour is Business
	ctx := context.Background()
	admin, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := h.store.CreateGroup(ctx, "Legal")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetGroupMembers(ctx, legal.ID, []string{h.user.ID}); err != nil {
		t.Fatal(err)
	}
	_, tokenPlain := h.newServiceToken(t, "bot", true)
	h.server.InvalidateConfigCache()

	enable := h.doAsSession(admin, http.MethodPut, "/api/v1/admin/troubleshooting", map[string]any{
		"enabled": true,
		"config": map[string]any{
			"filter":    map[string]any{"match": "all", "group_ids": []string{legal.ID}},
			"retention": map[string]any{"max_age_hours": 24, "max_count": 100},
		},
		"expires_in_hours": 24,
	})
	if enable.Code != http.StatusOK {
		t.Fatalf("enable returned %d: %s", enable.Code, enable.Body.String())
	}

	body := map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("member proxy = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsServiceToken(tokenPlain, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)); rec.Code != http.StatusOK {
		t.Fatalf("token proxy = %d: %s", rec.Code, rec.Body.String())
	}
	h.waitForUsageEvents(2)
	if err := h.server.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	captures := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/troubleshooting/captures", nil)
	var list struct {
		Captures []*store.TroubleshootingCapture `json:"captures"`
		Total    int                             `json:"total_count"`
	}
	if err := json.Unmarshal(captures.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 {
		t.Fatalf("captures total = %d, want 1 (the Legal member, not the service token): %+v", list.Total, list.Captures)
	}

	// Leave the group: the same person is no longer captured.
	if err := h.store.SetGroupMembers(ctx, legal.ID, nil); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("ex-member proxy = %d", rec.Code)
	}
	h.waitForUsageEvents(3)
	if err := h.server.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	captures = h.doAsSession(admin, http.MethodGet, "/api/v1/admin/troubleshooting/captures", nil)
	if err := json.Unmarshal(captures.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 {
		t.Fatalf("after leaving the group, captures total = %d, want still 1", list.Total)
	}
}
