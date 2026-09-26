package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
)

// activationHarness trusts a throwaway signer and returns a signed online key.
func activationHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	promote(t, h)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(300 * 24 * time.Hour)
	raw, err := license.Sign(priv, license.Claims{KeyID: "test", LicenseID: "JNS-COM-ACTV-TEST", Org: "Acme", IssuedTo: "a@b",
		Edition: license.EditionCommunity, Seats: 25, Nodes: 1, Features: []string{}, Issued: time.Now().Add(-time.Hour),
		Exp: &exp, GraceDays: 30, Term: license.TermSubscription, SyncURL: license.SyncEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	h.server.License = license.NewManager("", h.store, map[string]ed25519.PublicKey{"test": pub}, nil)
	h.server.LicenseSync = license.NewSync(h.server.License, h.store, h.server.Cipher, license.SyncOptions{})
	return h, raw
}

func syncState(t *testing.T, h *harness) map[string]any {
	t.Helper()
	rec := h.do(http.MethodGet, "/api/v1/admin/system/license/sync", nil)
	if rec.Code != 200 {
		t.Fatalf("sync get %d %s", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)["license_sync"].(map[string]any)
}

func TestActivationCodeInstallsKeyAndTokenButStaysOffUnlessTicked(t *testing.T) {
	h, raw := activationHarness(t)
	code := license.EncodeActivation(raw, "portal-sync-token")

	rec := h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": code})
	if rec.Code != 200 {
		t.Fatalf("install %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "portal-sync-token") {
		t.Fatal("token echoed")
	}
	if id := h.server.License.State().Claims; id == nil || id.LicenseID != "JNS-COM-ACTV-TEST" {
		t.Fatalf("license not installed: %+v", h.server.License.State())
	}
	if st := syncState(t, h); st["has_token"] != true || st["enabled"] != false {
		t.Fatalf("activation without tick must store token only: %v", st)
	}

	rec = h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": code, "enable_sync": true})
	if rec.Code != 200 {
		t.Fatalf("install+enable %d %s", rec.Code, rec.Body.String())
	}
	if st := syncState(t, h); st["has_token"] != true || st["enabled"] != true || st["mode"] != "online" {
		t.Fatalf("tick must enable: %v", st)
	}

	// A manual plain-key install is a deliberate replacement: the existing
	// fence resets sync, and enable_sync on a plain key can never turn it on.
	rec = h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": raw, "enable_sync": true})
	if rec.Code != 200 {
		t.Fatalf("plain %d %s", rec.Code, rec.Body.String())
	}
	if st := syncState(t, h); st["enabled"] != false {
		t.Fatalf("plain key enabled sync: %v", st)
	}
}

func TestActivationCodeRejectsDamagedOrForeign(t *testing.T) {
	h, raw := activationHarness(t)
	for name, code := range map[string]string{
		"damaged": license.ActivationPrefix + ".!!",
		"notoken": license.EncodeActivation(raw, ""),
		"forged":  license.EncodeActivation(license.Prefix+".e30.AAAA", "tok"),
	} {
		rec := h.do(http.MethodPut, "/api/v1/admin/system/license", map[string]any{"key": code, "enable_sync": true})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st := syncState(t, h); st["has_token"] != false || st["enabled"] != false {
		t.Fatalf("rejected code changed sync: %v", st)
	}
}
