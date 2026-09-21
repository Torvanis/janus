package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type releaseTransport struct{ target string }

func (tr releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(tr.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

// Opt-in real HTTP/SPA fixture: test trust is constructor-injected, never released.
func TestReleaseBrowserFixture(t *testing.T) {
	dir := os.Getenv("JANUS_RELEASE_BROWSER")
	if dir == "" {
		t.Skip("opt-in browser fixture")
	}
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC().Truncate(time.Second)
	exp := now.Add(60 * 24 * time.Hour)
	c := license.Claims{LicenseID: "JNS-BROWSER", KeyID: "browser", Org: "Browser fixture", Edition: license.EditionBusiness, Term: license.TermSubscription, Seats: 100, Nodes: 3, Features: license.BusinessFeatures, Issued: now.Add(-time.Hour), Exp: &exp, GraceDays: 7}
	// Use the actual producer portal's copied token and identity for onboarding.
	var f struct {
		Token  string
		Active struct {
			LicenseID string `json:"license_id"`
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "producer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	c.LicenseID = f.Active.LicenseID
	raw, _ := license.Sign(priv, c)
	h.server.License = license.NewManager("", h.store, map[string]ed25519.PublicKey{"browser": pub}, nil)
	if _, err = h.server.License.Install(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mode := "active"
	revision := int64(1)
	producer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+c.LicenseID+"."+f.Token {
			w.WriteHeader(401)
			return
		}
		if mode == "network" {
			w.WriteHeader(503)
			return
		}
		status := "active"
		auto := true
		cancel := false
		verified := time.Now().UTC()
		switch mode {
		case "past_due":
			status = "past_due"
			auto = false
		case "canceled":
			status = "canceled"
			auto = false
			cancel = true
		case "stale":
			verified = verified.Add(-7 * time.Hour)
		}
		out := map[string]any{"license_id": c.LicenseID, "status": "active", "license": raw, "subscription": license.Subscription{SchemaVersion: 1, LicenseID: c.LicenseID, BillingMode: "live", Status: status, AutoRenew: auto, CancelAtPeriodEnd: cancel, PaidThrough: &exp, VerifiedAt: verified, FreshUntil: verified.Add(6 * time.Hour), Revision: revision}}
		if mode == "revoked" {
			out["status"] = "revoked"
			out["revoked_at"] = time.Now().UTC()
			delete(out, "license")
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer producer.Close()
	h.server.LicenseSync = license.NewSync(h.server.License, h.store, h.server.Cipher, license.SyncOptions{Transport: releaseTransport{producer.URL}})
	h.server.WebAssets = os.DirFS("../../web/dist")
	handler := h.server.Handler()
	done := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/__test/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		mu.Lock()
		mode = r.URL.Query().Get("mode")
		revision++
		mu.Unlock()
		if _, err := h.server.License.Install(ctx, raw); err != nil {
			t.Error(err)
		}
		if err := h.server.LicenseSync.Configure(ctx, true, f.Token, false); err != nil {
			t.Error(err)
		}
		_ = h.server.LicenseSync.Sync(ctx, true)
		role := store.RoleAdmin
		if r.URL.Query().Get("member") == "true" {
			role = store.RoleUser
		}
		_ = h.store.UpdateUser(ctx, h.user.ID, role, true)
		h.server.InvalidateTokenCache()
		w.WriteHeader(204)
	})
	mux.HandleFunc("/__test/done", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(done) }); w.WriteHeader(204) })
	mux.Handle("/", handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	info, _ := json.Marshal(map[string]string{"url": server.URL, "token": h.token})
	if err := os.WriteFile(filepath.Join(dir, "browser-server.json"), info, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		t.Fatal("browser timeout")
	}
}
