package discovery_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

type rewriteRT struct{ target *url.URL }

func (r rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// The OpenRouter decisions listing is a second best-effort call. A failed
// listing must not be read as "the decision models are gone": they stay as
// they are (enabled stays enabled), and the chat models are still checked.
func TestFailedDecisionListingKeepsDecisionModels(t *testing.T) {
	ctx := context.Background()
	var failDecisions, hide, hideChat atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("output_modalities") == "decisions" {
			if failDecisions.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			data := []map[string]any{}
			if !hide.Load() {
				data = append(data, map[string]any{"id": "typesafe/jev-router", "context_length": 8192})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		data := []map[string]any{{"id": "openai/gpt-y"}}
		if !hideChat.Load() {
			data = append(data, map[string]any{"id": "openai/gpt-x"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: rewriteRT{target}}

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := make([]byte, 32)
	cipher, _ := crypto.New(key)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.New("test", "test")
	alerts := alerting.New(st, alerting.SMTPConfig{}, client, metrics, logger)
	svc := discovery.New(st, cipher, client, metrics, alerts, logger, time.Hour)
	enc, _ := cipher.Encrypt("sk-x")
	up, err := st.CreateUpstream(ctx, "or", "openai_compatible", "https://openrouter.ai/api/v1", enc, "sk-x")
	if err != nil {
		t.Fatal(err)
	}
	if res := svc.RunFor(ctx, up); res.Error != "" {
		t.Fatal(res.Error)
	}
	m, err := st.ModelByUpstreamAndName(ctx, up.ID, "typesafe/jev-router")
	if err != nil {
		t.Fatal(err)
	}
	if m.ClassifierRole != store.ClassifierRoleDecision {
		t.Fatalf("role %q", m.ClassifierRole)
	}
	_ = st.SetModelStatus(ctx, m.ID, store.ModelEnabled)

	failDecisions.Store(true)
	_ = svc.RunFor(ctx, up)
	m, _ = st.ModelByUpstreamAndName(ctx, up.ID, "typesafe/jev-router")
	if m.Status != store.ModelEnabled {
		t.Errorf("during a failed decisions listing the enabled decision model is %q, want enabled", m.Status)
	}
	failDecisions.Store(false)
	_ = svc.RunFor(ctx, up)
	m, _ = st.ModelByUpstreamAndName(ctx, up.ID, "typesafe/jev-router")
	if m.Status != store.ModelEnabled {
		t.Errorf("after one failed decisions listing the enabled decision model is %q, want enabled", m.Status)
	}

	// A decision model that really drops out of a good listing goes stale,
	// and resumes its approval state when it is listed again.
	hide.Store(true)
	_ = svc.RunFor(ctx, up)
	m, _ = st.ModelByUpstreamAndName(ctx, up.ID, "typesafe/jev-router")
	if m.Status != store.ModelStale {
		t.Fatalf("a decision model missing from a good listing is %q, want stale", m.Status)
	}
	hide.Store(false)
	_ = svc.RunFor(ctx, up)
	m, _ = st.ModelByUpstreamAndName(ctx, up.ID, "typesafe/jev-router")
	if m.Status != store.ModelEnabled {
		t.Errorf("a decision model that reappears is %q, want its previous state enabled", m.Status)
	}
	// Chat models keep the old rule: back from stale means re-approval.
	_ = st.SetModelStatus(ctx, chat(t, st, up.ID).ID, store.ModelEnabled)
	hideChat.Store(true)
	_ = svc.RunFor(ctx, up)
	hideChat.Store(false)
	_ = svc.RunFor(ctx, up)
	if c := chat(t, st, up.ID); c.Status != store.ModelPending {
		t.Errorf("a chat model back from stale is %q, want pending_approval", c.Status)
	}
}

func chat(t *testing.T, st *store.Store, upstreamID string) *store.Model {
	t.Helper()
	m, err := st.ModelByUpstreamAndName(context.Background(), upstreamID, "openai/gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
