package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/blobstore/blobstoretest"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 5 (rulings of
// 2026-11-11). The sign-in binding (AUTH-06) is in identity_providers_test.go.

// ADM-02: a stored credential is shown to no one, but an administrator who
// never knew it could change only the address it is sent to and receive it.
// It now stays with the server it was saved for.
func TestStoredSecretsDoNotFollowAChangedAddress(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	ctx := context.Background()
	var hits atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer collector.Close()
	needsReentry := func(what string, rec *httptest.ResponseRecorder, param string) {
		t.Helper()
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "again") || !strings.Contains(rec.Body.String(), param) {
			t.Errorf("%s: got %d %s, want a 400 asking for %s again", what, rec.Code, rec.Body.String(), param)
		}
	}

	// A provider key.
	enc, err := h.server.Cipher.Encrypt("sk-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	up, err := h.store.CreateUpstream(ctx, "keyed", "openai_compatible", "https://api.provider.example/v1", enc, crypto.Mask("sk-provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/upstreams/" + up.ID
	needsReentry("upstream moved to another host", h.do(http.MethodPut, path, map[string]any{"name": "keyed", "base_url": collector.URL + "/v1"}), "api_key")
	needsReentry("upstream moved to another port", h.do(http.MethodPut, path, map[string]any{"name": "keyed", "base_url": "https://api.provider.example:8443/v1"}), "api_key")
	needsReentry("upstream moved to plain http", h.do(http.MethodPut, path, map[string]any{"name": "keyed", "base_url": "http://api.provider.example/v1"}), "api_key")
	if got, _ := h.store.UpstreamByID(ctx, up.ID); got.BaseURL != "https://api.provider.example/v1" {
		t.Fatalf("a refused change was saved: %s", got.BaseURL)
	}
	if rec := h.do(http.MethodPut, path, map[string]any{"name": "keyed", "base_url": "https://API.provider.example:443/v2"}); rec.Code != http.StatusOK {
		t.Errorf("same server, another path: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(http.MethodPut, path, map[string]any{"name": "keyed", "base_url": "https://other.example/v1", "api_key": "sk-new"}); rec.Code != http.StatusOK {
		t.Errorf("another server with the key entered: %d %s", rec.Code, rec.Body.String())
	}
	// An upstream with no key has nothing to protect.
	if rec := h.do(http.MethodPut, "/api/v1/admin/upstreams/"+h.model.UpstreamID, map[string]any{"name": "test-upstream", "base_url": "http://127.0.0.1:9/v1"}); rec.Code != http.StatusOK {
		t.Errorf("keyless upstream moved: %d %s", rec.Code, rec.Body.String())
	}

	// The directory bind password: saving, and the test button.
	dir := fixtureDirectory("ldaps://ldap.corp.example:636")
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", dir); rec.Code != http.StatusOK {
		t.Fatalf("save directory: %d %s", rec.Code, rec.Body.String())
	}
	moved := fixtureDirectory("ldap://" + strings.TrimPrefix(collector.URL, "http://"))
	delete(moved, "bind_password")
	needsReentry("directory moved", h.do(http.MethodPut, "/api/v1/admin/directory", moved), "bind_password")
	needsReentry("directory test against another server", h.do(http.MethodPost, "/api/v1/admin/directory/test", moved), "bind_password")
	downgraded := fixtureDirectory("ldap://ldap.corp.example:636")
	delete(downgraded, "bind_password")
	needsReentry("directory moved to plain ldap", h.do(http.MethodPut, "/api/v1/admin/directory", downgraded), "bind_password")
	same := fixtureDirectory("ldaps://ldap.corp.example")
	delete(same, "bind_password")
	same["base_dn"] = "ou=people,dc=example,dc=com"
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", same); rec.Code != http.StatusOK {
		t.Errorf("same directory server, other settings: %d %s", rec.Code, rec.Body.String())
	}
	// A test is on the record.
	withPassword := fixtureDirectory("ldap://127.0.0.1:1")
	_ = h.do(http.MethodPost, "/api/v1/admin/directory/test", withPassword)
	if entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "directory.test", Limit: 10}); err != nil || len(entries) != 1 || !strings.Contains(string(entries[0].NewValue), "127.0.0.1:1") {
		t.Errorf("directory test not audited: %v %+v", err, entries)
	}

	// An identity provider's client secret.
	rec := h.do(http.MethodPost, "/api/v1/admin/identity-providers", json.RawMessage(`{"slug":"okta","name":"Okta","issuer_url":"https://okta.example.com","client_id":"c","client_secret":"s"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider: %d %s", rec.Code, rec.Body.String())
	}
	idp := "/api/v1/admin/identity-providers/" + decodeBody(t, rec)["id"].(string)
	needsReentry("identity provider moved", h.do(http.MethodPut, idp, json.RawMessage(`{"slug":"okta","name":"Okta","issuer_url":"https://login.attacker.example","client_id":"c"}`)), "client_secret")
	if rec = h.do(http.MethodPut, idp, json.RawMessage(`{"slug":"okta","name":"Okta SSO","issuer_url":"https://okta.example.com/oauth2/default","client_id":"c"}`)); rec.Code != http.StatusOK {
		t.Errorf("same issuer host, other path: %d %s", rec.Code, rec.Body.String())
	}
	if entries, _, _ := h.store.ListAudit(ctx, store.AuditFilter{Action: "identity_provider.update", Limit: 10}); len(entries) != 1 || !strings.Contains(string(entries[0].NewValue), "oauth2/default") {
		t.Errorf("issuer change not on the audit record: %+v", entries)
	}

	// The file storage secret key.
	fake := blobstoretest.NewFakeS3(t)
	s3 := map[string]any{"endpoint": fake.URL, "bucket": "janus-files", "path_style": true, "access_key_id": "AKID", "secret_access_key": "s3cr3t-value"}
	if rec = h.do(http.MethodPut, "/api/v1/admin/storage", map[string]any{"mode": "s3", "s3": s3}); rec.Code != http.StatusOK {
		t.Fatalf("save storage: %d %s", rec.Code, rec.Body.String())
	}
	elsewhere := map[string]any{"endpoint": collector.URL, "bucket": "janus-files", "path_style": true, "access_key_id": "AKID"}
	needsReentry("storage moved", h.do(http.MethodPut, "/api/v1/admin/storage", map[string]any{"mode": "s3", "s3": elsewhere}), "secret_access_key")
	rec = h.do(http.MethodPost, "/api/v1/admin/storage/test", map[string]any{"mode": "s3", "s3": elsewhere})
	if res := decodeBody(t, rec); res["ok"] != false || !strings.Contains(rec.Body.String(), "again") {
		t.Errorf("storage test against another server: %s", rec.Body.String())
	}
	kept := map[string]any{"endpoint": fake.URL, "bucket": "janus-files", "path_style": true, "access_key_id": "AKID", "prefix": "p"}
	if rec = h.do(http.MethodPost, "/api/v1/admin/storage/test", map[string]any{"mode": "s3", "s3": kept}); decodeBody(t, rec)["ok"] != true {
		t.Errorf("storage test against the saved server: %s", rec.Body.String())
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("the other server was contacted %d times", n)
	}
}

// DATA-04: what a check matched is identified by a keyed hash. The same
// text links across requests; the value cannot be worked back to the text.
func TestMatchHashIsKeyedAndLinksTheSameText(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "observe", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeObserve},
	}})
	other := strings.Replace(testAWSKey, "EXAMPLE", "EXAMPLF", 1)
	for _, text := range []string{"first " + testAWSKey, "again, in other words: " + testAWSKey + " ok", "a different one " + other} {
		if rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody(text, false)); rec.Code != http.StatusOK {
			t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
		}
	}
	vs := h.waitForViolations(t, 3)
	if len(vs) != 3 {
		t.Fatalf("%d violations", len(vs))
	}
	counts := map[string]int{}
	for _, v := range vs {
		counts[v.MatchHash]++
		if !strings.HasPrefix(v.MatchHash, "hmac-sha256:") || len(v.MatchHash) != len("hmac-sha256:")+64 {
			t.Fatalf("match hash %q is not a keyed hash", v.MatchHash)
		}
		// Not the plain SHA-256, which anyone can compute for a guess.
		for _, guess := range []string{testAWSKey, other} {
			if plain := sha256.Sum256([]byte(guess)); strings.Contains(v.MatchHash, hex.EncodeToString(plain[:])) {
				t.Fatal("unkeyed hash stored")
			}
		}
	}
	if len(counts) != 2 {
		t.Fatalf("hashes %v, want the repeated key to share one and the other key to differ", counts)
	}
	repeated := ""
	for hash, n := range counts {
		if n == 2 {
			repeated = hash
		}
	}
	rec := h.do(http.MethodGet, "/api/v1/admin/secgw/violations?match="+repeated, nil)
	var out struct {
		Violations []store.SecgwViolation `json:"violations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK || len(out.Violations) != 2 {
		t.Fatalf("filter by match: %d %s", rec.Code, rec.Body.String())
	}
	if len(h.server.matchHashKey()) != 32 {
		t.Fatal("no hash key")
	}
}

// PROXY-10: a non-streamed reply sends nothing until it is finished, so a
// caller asking for long answers ran every member of a pool past the wait
// for the first byte. Each timeout counted against the member and was tried
// again on the next one, until the whole pool was ejected for everyone.
func TestSlowBufferedRepliesDoNotEjectPoolMembers(t *testing.T) {
	h := newHarness(t)
	h.server.Config.UpstreamTTFBTimeout = 250 * time.Millisecond
	_, engines := seedPool(t, h, "round_robin", "off", "a", "b")
	for _, e := range engines {
		e.slowMs.Store(1500)
	}
	for i := 0; i < 4; i++ {
		if rec := h.chatPool(t, "slow", nil); rec.Code < 500 {
			t.Fatalf("slow request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	h.server.pending.Wait()
	if ej, err := h.store.PoolMemberEjections(context.Background(), time.Now().UTC()); err != nil || len(ej) != 0 {
		t.Fatalf("members ejected for slow replies: %+v %v", ej, err)
	}
	if total := engines[0].count() + engines[1].count(); total != 4 {
		t.Fatalf("4 slow requests reached the members %d times; a slow reply is not tried again on the next member", total)
	}
	// The pool is whole for the next caller.
	for _, e := range engines {
		e.slowMs.Store(0)
	}
	served := map[string]bool{}
	for i := 0; i < 4; i++ {
		rec := h.chatPool(t, "quick", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("after the slow requests: %d %s", rec.Code, rec.Body.String())
		}
		served[servedBy(rec)] = true
	}
	if len(served) != 2 {
		t.Fatalf("served by %v, want both members still in the pool", served)
	}
}
