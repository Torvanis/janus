package discovery_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// Discovery records what the provider's list says a model can do, follows
// the list when it changes, and never touches what a probe measured or an
// administrator set.
func TestDiscoveryRecordsProviderCapabilities(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "caps")
	body := `{"data":[{"id":"grok-4.7","input_modalities":["text","image"],"output_modalities":["text"],"capabilities":{"reasoning_effort":["low","high"],"default_reasoning_effort":"high"}},{"id":"plain"}]}`
	client := &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.New("caps", "test")
	svc := discovery.New(h.store, h.cipher, client, metrics, alerting.New(h.store, alerting.SMTPConfig{}, client, metrics, log), log, time.Hour)
	up.BaseURL = "https://api.x.ai"
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
	m := get("grok-4.7")
	c := m.Capabilities
	if c[adapter.CapInImage].Value != true || c[adapter.CapInImage].Source != capability.SourceProvider || c[adapter.CapThinkingOff].Value != adapter.ThinkingOffNever ||
		!reflect.DeepEqual(c[adapter.CapThinkingLevels].Value, []string{"low", "high"}) {
		t.Fatalf("provider capabilities not recorded: %+v", c)
	}
	if plain := get("plain"); len(plain.Capabilities) != 0 || plain.CapabilityProbe != nil {
		t.Fatalf("a model the list says nothing about must have nothing claimed: %+v", plain.Capabilities)
	}
	first := c[adapter.CapInImage].At

	// A probe and an override are recorded; the next discovery leaves them.
	if _, err := h.store.UpdateModelCapabilities(ctx, m.ID, func(s *capability.State) (bool, error) {
		s.Probe = capability.Facts{adapter.CapTools: {Value: true, At: time.Now().UTC(), Note: "called the tool"}}
		s.Probed = &capability.ProbeRun{At: time.Now().UTC(), Status: capability.ProbeOK, Requests: 3}
		return true, s.SetAdmin(map[string]any{adapter.CapInImage: false}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	body = `{"data":[{"id":"grok-4.7","input_modalities":["text","image","audio"],"output_modalities":["text"],"capabilities":{"reasoning_effort":["none","low","high"]}},{"id":"plain"}]}`
	time.Sleep(5 * time.Millisecond)
	run()
	m = get("grok-4.7")
	c = m.Capabilities
	if c[adapter.CapInImage].Value != false || c[adapter.CapInImage].Source != capability.SourceAdmin {
		t.Errorf("discovery overwrote the administrator: %+v", c[adapter.CapInImage])
	}
	if c[adapter.CapTools].Source != capability.SourceProbe || m.CapabilityProbe == nil || m.CapabilityProbe.Requests != 3 {
		t.Errorf("discovery dropped what the probe measured: %+v %+v", c[adapter.CapTools], m.CapabilityProbe)
	}
	if c[adapter.CapInAudio].Value != true || c[adapter.CapThinkingOff].Value != adapter.ThinkingOffEffortNone {
		t.Errorf("discovery did not follow the provider's list: %+v", c)
	}
	if got := m.CapabilityState().Provider[adapter.CapInImage].At; !got.Equal(first) {
		t.Errorf("an unchanged fact lost its first-seen date: %v, want %v", got, first)
	}
}
