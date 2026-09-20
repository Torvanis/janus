package discovery_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDiscoveryMetadataRefreshAndExactReference(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "metadata")
	body := `{"data":[{"id":"gpt-6-astra"},{"id":"gpt-5.6-sol"},{"id":"gpt-4o"},{"id":"GPT-4o"},{"id":"gpt-4o-made-up"},{"id":"claude-sonnet-5"}]}`
	client := &http.Client{Transport: metadataTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.New("metadata", "test")
	svc := discovery.New(h.store, h.cipher, client, metrics, alerting.New(h.store, alerting.SMTPConfig{}, client, metrics, log), log, time.Hour)
	up.BaseURL = "https://api.openai.com"
	run := func() {
		t.Helper()
		if res := svc.RunFor(ctx, up); res.Error != "" {
			t.Fatal(res.Error)
		}
	}
	get := func(name string) *store.Model {
		t.Helper()
		m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	run()
	for _, name := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		m := get(name)
		if m.ContextWindow != 1050000 || m.Metadata["context_window"].Source != "reference" || m.Metadata["rate_in_nanousd"].Value != nil || len(m.MetadataWarnings) == 0 {
			t.Fatalf("exact reference %s: %+v", name, m)
		}
	}
	if m := get("gpt-4o"); m.RateInNano != 2500000000 || m.Metadata["rate_in_nanousd"].Source != "reference" {
		t.Fatalf("reference price=%+v", m)
	}
	for _, name := range []string{"GPT-4o", "gpt-4o-made-up", "claude-sonnet-5"} {
		if m := get(name); m.ContextWindow != 0 || m.RateInNano != 0 {
			t.Fatalf("cross-provider or fuzzy match: %+v", m)
		}
	}
	m := get("gpt-6-astra")
	body = `{"data":[{"id":"gpt-6-astra","context_length":200000}]}`
	run()
	if get(m.Name).ContextWindow != 200000 {
		t.Fatal("upstream must beat reference")
	}
	if err := h.store.SetModelContextWindow(ctx, m.ID, 123); err != nil {
		t.Fatal(err)
	}
	body = `{"data":[{"id":"gpt-6-astra","context_length":300000}]}`
	run()
	if get(m.Name).ContextWindow != 123 {
		t.Fatal("discovery overwrote admin")
	}
	if err := h.store.PatchModel(ctx, m.ID, store.ModelPatch{Overrides: map[string]*int64{"context_window": nil}}); err != nil {
		t.Fatal(err)
	}
	if get(m.Name).ContextWindow != 300000 {
		t.Fatal("reset did not use refreshed automatic value")
	}
	// The same exact ID on an arbitrary compatible host has no reference fallback.
	up.BaseURL = "https://example.org"
	body = `{"data":[{"id":"gpt-6-astra"}]}`
	run()
	if get(m.Name).ContextWindow != 0 {
		t.Fatal("unscoped reference")
	}
	// Real xAI wire prices reach persisted billing columns and keep overrides.
	up.BaseURL = "https://api.x.ai"
	body = `{"data":[{"id":"grok-test","context_length":128000,"prompt_text_token_price":20000,"completion_text_token_price":0}]}`
	run()
	x := get("grok-test")
	if x.RateInNano != 2000000000 || x.Metadata["rate_out_nanousd"].Source != "upstream" || x.Metadata["rate_cached_nanousd"].Value != nil {
		t.Fatalf("xai metadata=%+v", x)
	}
	if err := h.store.SetModelRates(ctx, x.ID, store.RateCard{RateInNano: 99, EffectiveFrom: time.Now()}); err != nil {
		t.Fatal(err)
	}
	body = `{"data":[{"id":"grok-test","prompt_text_token_price":30000}]}`
	run()
	if x = get(x.Name); x.RateInNano != 99 || *x.Metadata["rate_in_nanousd"].AutomaticValue != 3000000000 {
		t.Fatal("legacy rates path did not create overrides")
	}
}
