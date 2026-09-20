package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeHealthz locks the --health-probe scheme selection (regression:
// the probe hardcoded http://, so a server configured for direct TLS via
// JANUS_TLS_CERT_PATH/JANUS_TLS_KEY_PATH rejected every probe from its own
// binary and the container crash-looped).
func TestProbeHealthz(t *testing.T) {
	healthz := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	t.Run("plain HTTP listener", func(t *testing.T) {
		srv := httptest.NewServer(healthz)
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		if err := probeHealthz(addr, false); err != nil {
			t.Fatalf("probe against a plain listener must pass, got: %v", err)
		}
	})

	t.Run("direct TLS listener with a self-signed cert", func(t *testing.T) {
		srv := httptest.NewTLSServer(healthz)
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "https://")
		if err := probeHealthz(addr, true); err != nil {
			t.Fatalf("probe against a TLS listener must pass when TLS is configured, got: %v", err)
		}
		// The old behaviour: an http:// probe against the TLS listener fails.
		if err := probeHealthz(addr, false); err == nil {
			t.Fatal("an http probe against a TLS-only listener should fail; the scheme selection is not being exercised")
		}
	})

	t.Run("unhealthy status is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		if err := probeHealthz(addr, false); err == nil {
			t.Fatal("a non-200 /healthz must fail the probe")
		}
	})
}
