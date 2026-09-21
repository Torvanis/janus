package license

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

func TestActualProducerRoundtrip(t *testing.T) {
	path := os.Getenv("JANUS_PRODUCER_FIXTURE")
	if path == "" {
		t.Skip("export fixture from janus-web TestExportProducerRoundtrip first")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Now                       time.Time
		RepairNow                 time.Time `json:"repair_now"`
		PublicKey                 string    `json:"public_key"`
		KeyID                     string    `json:"key_id"`
		Old, Renewed, Token       string
		Active, Revoked, Repaired json.RawMessage
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(f.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubs := map[string]ed25519.PublicKey{f.KeyID: pub}
	old, err := Verify(f.Old, pubs)
	if err != nil || old.BillingMode != "sandbox" {
		t.Fatalf("producer old key: %+v %v", old, err)
	}
	if _, err = Verify(f.Renewed, TrustedKeys()); err == nil {
		t.Fatal("released production trust accepted sandbox")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.PutLicenseKey(ctx, f.Old); err != nil {
		t.Fatal(err)
	}
	now := f.Now
	m := NewManager("", db, pubs, nil)
	m.now = func() time.Time { return now }
	if err = m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	response := f.Active
	calls := 0
	tr := syncTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != SyncEndpoint || r.Header.Get("Authorization") != "Bearer "+old.LicenseID+"."+f.Token {
			t.Fatal("request contract")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
	})
	engine := NewSync(m, db, cipher, SyncOptions{Now: func() time.Time { return now }, Transport: tr})
	if err = engine.ConfigurePatch(ctx, nil, f.Token, false); err != nil {
		t.Fatal(err)
	}
	if err = engine.Sync(ctx, true); err == nil || calls != 0 {
		t.Fatal("token-only opted in")
	}
	if err = engine.Configure(ctx, true, "", false); err != nil {
		t.Fatal(err)
	}
	if err = engine.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	state, notice := engine.View(ctx)
	if state.Health != "healthy" || !notice.SuppressExpiring || notice.Reason != "auto_renew" {
		t.Fatalf("actual producer metadata: %+v %+v", state, notice)
	}
	_, key, err := db.LicenseSyncSnapshot(ctx)
	if err != nil || key != f.Renewed {
		t.Fatal("producer renewal not installed")
	}
	restarted := NewSync(m, db, cipher, SyncOptions{Now: func() time.Time { return now }, Transport: tr})
	_, notice = restarted.View(ctx)
	if !notice.SuppressExpiring {
		t.Fatal("restart lost observation")
	}
	now = now.Add(6 * time.Hour)
	state, notice = restarted.View(ctx)
	if notice.SuppressExpiring || state.Health != "stale" {
		t.Fatal("stale producer metadata suppressed")
	}
	now = f.Now
	// Unknown-version unsigned counters must not poison the known schema's
	// anti-replay watermark and strand subsequent real producer responses.
	var future map[string]any
	_ = json.Unmarshal(f.Active, &future)
	futureSub := future["subscription"].(map[string]any)
	futureSub["schema_version"], futureSub["revision"] = 99, 999999999
	response, _ = json.Marshal(future)
	if err = restarted.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	_, notice = restarted.View(ctx)
	if notice.SuppressExpiring {
		t.Fatal("unknown schema suppressed")
	}
	response = f.Active
	if err = restarted.Sync(ctx, true); err != nil {
		t.Fatalf("unknown schema poisoned known revision: %v", err)
	}
	// An authenticated response for a different license must never set revocation.
	var wrong map[string]any
	_ = json.Unmarshal(f.Revoked, &wrong)
	wrong["license_id"] = "foreign"
	response, _ = json.Marshal(wrong)
	if err = restarted.Sync(ctx, true); err == nil || err.Error() != "identity_changed" {
		t.Fatalf("foreign revocation: %v", err)
	}
	response = f.Revoked
	if err = restarted.Sync(ctx, true); err == nil || err.Error() != "revoked" {
		t.Fatalf("revoked producer response: %v", err)
	}
	restarted = NewSync(m, db, cipher, SyncOptions{Now: func() time.Time { return now }, Transport: tr})
	state, notice = restarted.View(ctx)
	if state.ErrorCode != "revoked" || notice.SuppressExpiring || notice.Reason != "revoked" {
		t.Fatal("revocation lost after restart")
	}
	_, key, _ = db.LicenseSyncSnapshot(ctx)
	if key != f.Renewed || m.State().Restricted() {
		t.Fatal("unsigned revocation changed signed rights")
	}
	response = f.Active
	before := calls
	if err = restarted.Sync(ctx, true); err == nil || calls != before+1 {
		t.Fatal("stale active replay cleared revocation")
	}
	now = f.RepairNow
	response = f.Repaired
	if err = restarted.Sync(ctx, true); err != nil {
		t.Fatalf("actual producer repair: %v", err)
	}
	state, notice = restarted.View(ctx)
	if state.Health != "healthy" || !notice.SuppressExpiring {
		t.Fatalf("repair not observed: %+v %+v", state, notice)
	}
	response = f.Revoked
	if err = restarted.Sync(ctx, true); err == nil {
		t.Fatal("old revocation replay accepted after repair")
	}
	state, notice = restarted.View(ctx)
	if notice.Reason == "revoked" {
		t.Fatal("old revocation re-established a tombstone after repair")
	}
	if err = restarted.Configure(ctx, false, "", false); err != nil {
		t.Fatal(err)
	}
	response = f.Active
	if err = restarted.Configure(ctx, true, "", false); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Sync(ctx, true); err == nil {
		t.Fatal("repaired watermark lost across configuration")
	}
	t.Log("real producer Issue/Renew/API bytes: isolated signature, renewal, token-only OFF, restart, six-hour expiry, foreign revocation rejection, durable revoked warning, preserved signed rights PASS")
}
