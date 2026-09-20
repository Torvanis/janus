package discovery_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// fakeProvider is a mutable OpenAI-compatible /v1/models endpoint.
type fakeProvider struct {
	mu       sync.Mutex
	models   []string
	status   int
	lastAuth string
}

func (f *fakeProvider) set(status int, models ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	f.models = models
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuth = r.Header.Get("Authorization")
	if f.status != http.StatusOK {
		w.WriteHeader(f.status)
		return
	}
	type entry struct {
		ID string `json:"id"`
	}
	payload := struct {
		Data []entry `json:"data"`
	}{Data: []entry{}}
	for _, m := range f.models {
		payload.Data = append(payload.Data, entry{ID: m})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

type harness struct {
	store    *store.Store
	cipher   *crypto.Cipher
	svc      *discovery.Service
	provider *fakeProvider
	server   *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "discovery-test.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := crypto.New(key)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}

	provider := &fakeProvider{status: http.StatusOK}
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.New("test", "test")
	alerts := alerting.New(st, alerting.SMTPConfig{}, server.Client(), metrics, logger)
	svc := discovery.New(st, cipher, server.Client(), metrics, alerts, logger, time.Hour)

	return &harness{store: st, cipher: cipher, svc: svc, provider: provider, server: server}
}

func (h *harness) createUpstream(t *testing.T, name string) *store.Upstream {
	t.Helper()
	encrypted, err := h.cipher.Encrypt("sk-test-credential")
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	up, err := h.store.CreateUpstream(context.Background(), name, "openai_compatible", h.server.URL, encrypted, "sk-t…tial")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	return up
}

func (h *harness) modelStatuses(t *testing.T, upstreamID string) map[string]string {
	t.Helper()
	models, err := h.store.ListModels(context.Background(), store.ModelFilter{UpstreamID: upstreamID})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	out := map[string]string{}
	for _, m := range models {
		out[m.Name] = m.Status
	}
	return out
}

func TestDiscoveryMergeAddStaleAndCurationLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "openai-prod")

	// First run: two brand-new models land as pending_approval.
	h.provider.set(http.StatusOK, "gpt-a", "text-embed-b")
	res := h.svc.RunFor(ctx, up)
	if res.Error != "" {
		t.Fatalf("first run failed: %s", res.Error)
	}
	if res.Found != 2 || res.New != 2 || res.Stale != 0 {
		t.Fatalf("first run = found %d new %d stale %d, want 2/2/0", res.Found, res.New, res.Stale)
	}
	statuses := h.modelStatuses(t, up.ID)
	if statuses["gpt-a"] != store.ModelPending || statuses["text-embed-b"] != store.ModelPending {
		t.Fatalf("new models must stay disabled (pending_approval) until an admin approves them: %v", statuses)
	}
	// The decrypted credential must reach the provider.
	if h.provider.lastAuth != "Bearer sk-test-credential" {
		t.Fatalf("provider saw Authorization %q, want the decrypted credential", h.provider.lastAuth)
	}

	// An admin enables one model; that curation decision must survive re-discovery.
	if err := h.store.SetModelStatus(ctx, modelID(t, h, up.ID, "gpt-a"), store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}

	// Second run: one model renamed away (text-embed-b gone, gpt-c new).
	h.provider.set(http.StatusOK, "gpt-a", "gpt-c")
	res = h.svc.RunFor(ctx, up)
	if res.Error != "" {
		t.Fatalf("second run failed: %s", res.Error)
	}
	if res.Found != 2 || res.New != 1 || res.Stale != 1 {
		t.Fatalf("second run = found %d new %d stale %d, want 2/1/1", res.Found, res.New, res.Stale)
	}
	statuses = h.modelStatuses(t, up.ID)
	if statuses["gpt-a"] != store.ModelEnabled {
		t.Fatalf("re-discovery reset an enabled model to %q", statuses["gpt-a"])
	}
	if statuses["text-embed-b"] != store.ModelStale {
		t.Fatalf("vanished model status = %q, want stale (rows are never deleted)", statuses["text-embed-b"])
	}
	if statuses["gpt-c"] != store.ModelPending {
		t.Fatalf("new model status = %q, want pending_approval", statuses["gpt-c"])
	}

	// Third run: the stale model comes back and returns to the review queue.
	h.provider.set(http.StatusOK, "gpt-a", "gpt-c", "text-embed-b")
	res = h.svc.RunFor(ctx, up)
	if res.Error != "" {
		t.Fatalf("third run failed: %s", res.Error)
	}
	if res.New != 0 {
		t.Fatalf("a re-reported stale model counted as new (%d)", res.New)
	}
	statuses = h.modelStatuses(t, up.ID)
	if statuses["text-embed-b"] != store.ModelPending {
		t.Fatalf("re-reported stale model status = %q, want pending_approval", statuses["text-embed-b"])
	}
}

func TestRunSkipsDisabledUpstreamsAndRecordsLastRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.provider.set(http.StatusOK, "gpt-a")

	enabled := h.createUpstream(t, "enabled-upstream")
	disabled := h.createUpstream(t, "disabled-upstream")
	if err := h.store.UpdateUpstream(ctx, disabled.ID, h.server.URL, false, "", ""); err != nil {
		t.Fatalf("disable upstream: %v", err)
	}

	if last := h.svc.LastRun(); !last.IsZero() {
		t.Fatalf("LastRun before any run = %v, want zero", last)
	}
	results := h.svc.Run(ctx)
	if len(results) != 1 || results[0].UpstreamID != enabled.ID {
		t.Fatalf("Run polled %d upstream(s) %v, want only the enabled one", len(results), results)
	}
	if h.svc.LastRun().IsZero() {
		t.Fatal("LastRun was not recorded after a run")
	}
}

func TestUpstreamErrorIsRecordedAndAlertsAfterThreeFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	admin, _, err := h.store.UpsertUserFromIdentity(ctx, "admin-sub", "admin@example.com", "Admin", true, false)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	up := h.createUpstream(t, "flaky-upstream")

	// Seed one model so we can prove errors never mark existing models stale.
	h.provider.set(http.StatusOK, "gpt-a")
	if res := h.svc.RunFor(ctx, up); res.Error != "" {
		t.Fatalf("seed run failed: %s", res.Error)
	}

	h.provider.set(http.StatusInternalServerError)
	for i := 1; i <= 2; i++ {
		res := h.svc.RunFor(ctx, up)
		if res.Error == "" || !strings.Contains(res.Error, "500") {
			t.Fatalf("failure %d: error = %q, want the HTTP 500 surfaced", i, res.Error)
		}
	}

	// A single blip (here: two) must not page anyone.
	if notes, _, err := h.store.ListNotifications(ctx, admin.ID, false, 50); err != nil {
		t.Fatalf("list notifications: %v", err)
	} else {
		for _, n := range notes {
			if strings.Contains(n.Title, "unreachable") {
				t.Fatalf("upstream-down alert fired after only two failures: %q", n.Title)
			}
		}
	}

	// Third consecutive failure crosses the persistence threshold.
	if res := h.svc.RunFor(ctx, up); res.Error == "" {
		t.Fatal("third run unexpectedly succeeded")
	}
	notes, _, err := h.store.ListNotifications(ctx, admin.ID, false, 50)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n.Title, "flaky-upstream") && strings.Contains(n.Title, "unreachable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no upstream-down alert after three consecutive failures; inbox: %+v", titles(notes))
	}

	// The failure is visible on the upstream row, and models are untouched.
	reloaded, err := h.store.UpstreamByID(ctx, up.ID)
	if err != nil {
		t.Fatalf("reload upstream: %v", err)
	}
	if reloaded.LastError == "" {
		t.Fatal("last_error was not recorded on the upstream row")
	}
	if status := h.modelStatuses(t, up.ID)["gpt-a"]; status != store.ModelPending {
		t.Fatalf("a failed discovery run changed model status to %q; errors must not mark models stale", status)
	}

	// Recovery clears the recorded error.
	h.provider.set(http.StatusOK, "gpt-a")
	if res := h.svc.RunFor(ctx, up); res.Error != "" {
		t.Fatalf("recovery run failed: %s", res.Error)
	}
	reloaded, err = h.store.UpstreamByID(ctx, up.ID)
	if err != nil {
		t.Fatalf("reload upstream: %v", err)
	}
	if reloaded.LastError != "" {
		t.Fatalf("last_error = %q after a successful run, want empty", reloaded.LastError)
	}
}

func TestWrongEncryptionKeySurfacesOperatorError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "keyed-upstream")

	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(255 - i)
	}
	wrong, err := crypto.New(wrongKey)
	if err != nil {
		t.Fatalf("build wrong cipher: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.New("test", "test")
	alerts := alerting.New(h.store, alerting.SMTPConfig{}, h.server.Client(), metrics, logger)
	svc := discovery.New(h.store, wrong, h.server.Client(), metrics, alerts, logger, time.Hour)

	h.provider.set(http.StatusOK, "gpt-a")
	res := svc.RunFor(ctx, up)
	if !strings.Contains(res.Error, "JANUS_ENCRYPTION_KEY") {
		t.Fatalf("error = %q, want the operator-facing wrong-key message", res.Error)
	}
	if res.Found != 0 || res.New != 0 {
		t.Fatalf("a run that cannot decrypt credentials reported found=%d new=%d", res.Found, res.New)
	}
}

func TestUnknownAdapterTypeFailsTheRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "bad-adapter", "no_such_provider", h.server.URL, "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	res := h.svc.RunFor(ctx, up)
	if !strings.Contains(res.Error, "no adapter registered") {
		t.Fatalf("error = %q, want the unregistered-adapter message", res.Error)
	}
}

func modelID(t *testing.T, h *harness, upstreamID, name string) string {
	t.Helper()
	models, err := h.store.ListModels(context.Background(), store.ModelFilter{UpstreamID: upstreamID})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	for _, m := range models {
		if m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("model %q not found for upstream %s", name, upstreamID)
	return ""
}

func titles(notes []*store.Notification) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Title)
	}
	return out
}
