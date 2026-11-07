package httpapi

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/truststore"
)

// withTrustStore configures the process trust store for one test, with the
// harness database as the uploads source (as main does).
func withTrustStore(t *testing.T, h *harness) {
	t.Helper()
	restore, err := truststore.Configure(truststore.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	truststore.SetUploadedSource(func(ctx context.Context) ([]truststore.Uploaded, error) {
		cas, err := h.store.ListTrustedCAs(ctx)
		if err != nil {
			return nil, err
		}
		out := []truststore.Uploaded{}
		for _, c := range cas {
			out = append(out, truststore.Uploaded{ID: c.ID, Name: c.Name, PEM: c.PEM, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt})
		}
		return out, nil
	})
	truststore.SetUntrustedAllowed(true)
	t.Cleanup(func() {
		restore()
		truststore.SetUntrustedAllowed(false)
		truststore.SetUntrustedHosts(nil)
	})
}

func tlsUpstreamFor(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-ts","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatOnce(h *harness) *httptest.ResponseRecorder {
	return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
}

// The whole path: a private-CA upstream fails, an admin uploads its CA,
// the very next proxied request succeeds (no restart, same HTTP client),
// and removing the CA makes it fail again. Both changes are audited.
func TestUploadedCAAppliesToTheProxyAtOnce(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withTrustStore(t, h)
	up := tlsUpstreamFor(t)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if rec := chatOnce(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("untrusted upstream: %d %s", rec.Code, rec.Body)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}))

	rec := h.do(http.MethodPost, "/api/v1/admin/certificates", map[string]any{"name": "lab CA", "pem": caPEM})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	id := decodeBody(t, rec)["certificate"].(map[string]any)["id"].(string)
	if rec := chatOnce(h); rec.Code != http.StatusOK {
		t.Fatalf("after upload: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/certificates", map[string]any{"pem": caPEM}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate upload: %d", rec.Code)
	}
	list := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/certificates", nil))
	cas := list["trust_store"].(map[string]any)["certificates"].([]any)
	if len(cas) != 1 || cas[0].(map[string]any)["name"] != "lab CA" || cas[0].(map[string]any)["read_only"] != false {
		t.Fatalf("list: %v", list)
	}

	if rec := h.do(http.MethodDelete, "/api/v1/admin/certificates/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := chatOnce(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after delete: %d", rec.Code)
	}
	if auditCount(t, h, "trusted_certificate_added") != 1 || auditCount(t, h, "trusted_certificate_removed") != 1 {
		t.Fatal("certificate changes not audited")
	}
}

func TestUploadValidation(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withTrustStore(t, h)
	for name, body := range map[string]string{
		"not pem":     "hello",
		"private key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2}})),
	} {
		if rec := h.do(http.MethodPost, "/api/v1/admin/certificates", map[string]any{"pem": body}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d", name, rec.Code)
		}
	}
	if rec := h.do(http.MethodDelete, "/api/v1/admin/certificates/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", rec.Code)
	}
}

// Allow untrusted certificate: per upstream, audited, routed only for that
// host, refused in the STIG profile and on http:// upstreams.
func TestUpstreamAllowUntrustedTLS(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	withTrustStore(t, h)
	up := tlsUpstreamFor(t)
	ctx := context.Background()
	if err := h.store.UpdateUpstream(ctx, h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	put := func(allow bool) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/admin/upstreams/"+h.model.UpstreamID, map[string]any{"base_url": up.URL, "allow_untrusted_tls": allow})
	}
	if rec := chatOnce(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("baseline: %d", rec.Code)
	}
	if fipsmode.IsFIPSEdition() {
		// The FIPS edition refuses the setting outright.
		if rec := put(true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "FIPS edition") {
			t.Fatalf("FIPS edition accepted untrusted TLS: %d %s", rec.Code, rec.Body)
		}
		return
	}
	if rec := put(true); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	if got, _ := h.store.UpstreamByID(ctx, h.model.UpstreamID); !got.AllowUntrustedTLS {
		t.Fatal("not saved")
	}
	if rec := chatOnce(h); rec.Code != http.StatusOK {
		t.Fatalf("untrusted upstream still refused: %d %s", rec.Code, rec.Body)
	}
	if auditCount(t, h, "upstream_untrusted_tls_changed") != 1 {
		t.Fatal("not audited")
	}
	if rec := put(false); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if rec := chatOnce(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("verification not restored: %d", rec.Code)
	}

	// http:// upstream: nothing to skip.
	if rec := h.do(http.MethodPut, "/api/v1/admin/upstreams/"+h.model.UpstreamID, map[string]any{"base_url": "http://127.0.0.1:1", "allow_untrusted_tls": true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("http upstream accepted: %d", rec.Code)
	}

	// STIG profile: refused when saved; a stored setting is ignored.
	withSTIGProfile(t)
	truststore.SetUntrustedAllowed(false)
	if rec := put(true); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "STIG") {
		t.Fatalf("STIG profile accepted untrusted TLS: %d %s", rec.Code, rec.Body)
	}
	if err := h.store.SetUpstreamAllowUntrustedTLS(ctx, h.model.UpstreamID, true); err != nil {
		t.Fatal(err)
	}
	h.server.reloadTrustStore(ctx)
	if rec := chatOnce(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stored untrusted setting honored in the STIG profile: %d", rec.Code)
	}
	if ups, _ := h.store.UntrustedTLSUpstreams(ctx); len(ups) != 1 {
		t.Fatalf("UntrustedTLSUpstreams = %d", len(ups))
	}
}
