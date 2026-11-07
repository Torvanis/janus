package httpapi

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/torvanis/janus/internal/listenertls"
)

func selfSignedProvider(t *testing.T) *listenertls.Provider {
	t.Helper()
	var mu sync.Mutex
	raw := ""
	p, err := listenertls.New(context.Background(), listenertls.Options{
		SelfSigned: true, Names: []string{"janus.test", "127.0.0.1"}, Days: 365, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Persist: listenertls.Persistence{
			Load: func(context.Context) (*listenertls.Stored, error) {
				mu.Lock()
				defer mu.Unlock()
				if raw == "" {
					return nil, nil
				}
				return listenertls.UnmarshalStored(raw)
			},
			Save: func(_ context.Context, s *listenertls.Stored) error {
				v, err := listenertls.MarshalStored(s)
				mu.Lock()
				raw = v
				mu.Unlock()
				return err
			},
			Encrypt: func(s string) (string, error) { return base64.StdEncoding.EncodeToString([]byte(s)), nil },
			Decrypt: func(s string) (string, error) { b, err := base64.StdEncoding.DecodeString(s); return string(b), err },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListenerTLSAdminAPI(t *testing.T) {
	h := newHarness(t)
	promote(t, h)

	// No listener TLS: status says so; download and regenerate refuse.
	body := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/listener-tls", nil))
	if body["listener"].(map[string]any)["mode"] != listenertls.ModeNone {
		t.Fatalf("status: %v", body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/admin/listener-tls/certificate.pem", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("download without TLS: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/admin/listener-tls/regenerate", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("regenerate without self-signed: %d", rec.Code)
	}

	h.server.ListenerTLS = selfSignedProvider(t)
	before := h.server.ListenerTLS.Info().SHA256

	rec := h.do(http.MethodGet, "/api/v1/admin/listener-tls/certificate.pem", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-pem-file" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "janus-listener.crt") || strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Fatalf("download: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	block, _ := pem.Decode(rec.Body.Bytes())
	if c, err := x509.ParseCertificate(block.Bytes); err != nil || c.DNSNames[0] != "janus.test" {
		t.Fatalf("downloaded certificate: %v", err)
	}

	rec = h.do(http.MethodPost, "/api/v1/admin/listener-tls/regenerate", map[string]any{"names": []string{"janus.ns.svc", "10.1.2.3"}, "days": 90})
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate: %d %s", rec.Code, rec.Body)
	}
	info := decodeBody(t, rec)["listener"].(map[string]any)
	if info["sha256"] == before || info["generated_by"] != h.user.Email {
		t.Fatalf("regenerated info: %v", info)
	}
	if auditCount(t, h, "listener_certificate_regenerated") != 1 {
		t.Fatal("regeneration not audited")
	}
	for _, bad := range []map[string]any{{"days": 5000}, {"names": []string{"bad name!"}}} {
		if rec := h.do(http.MethodPost, "/api/v1/admin/listener-tls/regenerate", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v accepted: %d", bad, rec.Code)
		}
	}
	// The Cryptography card reports the listener.
	status := decodeBody(t, h.do(http.MethodGet, "/api/v1/admin/system/status", nil))
	lt := status["cryptography"].(map[string]any)["listener_tls"].(map[string]any)
	if lt["mode"] != listenertls.ModeSelfSigned {
		t.Fatalf("cryptography listener_tls: %v", lt)
	}
}
