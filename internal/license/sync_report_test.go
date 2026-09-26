package license

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

// The portal can only chart renewal delivery if the gateway identifies itself;
// the report must carry exactly the documented fields and the observer must
// see every completed attempt (success and failure) with the installed expiry.
func TestSyncSendsReportAndObservesOutcome(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	priv, pubs := testKeys(t)
	old := business(now.Add(time.Hour), 7)
	old.Issued = now.Add(-time.Hour)
	old.Site = "hq"
	raw, _ := Sign(priv, old)
	if err = db.PutLicenseKey(ctx, raw); err != nil {
		t.Fatal(err)
	}
	m := NewManager("", db, pubs, nil)
	m.now = func() time.Time { return now }
	_ = m.Refresh(ctx)
	c, _ := crypto.New(make([]byte, 32))
	next := old
	exp := now.Add(48 * time.Hour)
	next.Exp, next.Issued = &exp, now
	renewed, _ := Sign(priv, next)
	var sent map[string]any
	status := 200
	transport := syncTransport(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = nil
		_ = json.Unmarshal(b, &sent)
		body, _ := json.Marshal(map[string]any{"license": renewed, "status": "active", "license_id": old.LicenseID})
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	seats, nodes := 7, 2
	var codes []string
	var lastExp *time.Time
	engine := NewSync(m, db, c, SyncOptions{Now: func() time.Time { return now }, Transport: transport,
		Report: func(context.Context) SyncReport {
			return SyncReport{InstanceID: "inst-1", Version: "2026.9.2", SeatsUsed: &seats, Nodes: &nodes}
		},
		Observe: func(code string, e *time.Time) { codes = append(codes, code); lastExp = e },
	})
	if err = engine.Configure(ctx, true, "secret", false); err != nil {
		t.Fatal(err)
	}
	if err = engine.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"instance_id": "inst-1", "version": "2026.9.2", "site": "hq", "seats_used": float64(7), "nodes": float64(2)}
	if len(sent) != len(want) {
		t.Fatalf("report fields %v", sent)
	}
	for k, v := range want {
		if sent[k] != v {
			t.Fatalf("report %s = %v, want %v (%v)", k, sent[k], v, sent)
		}
	}
	if len(codes) != 1 || codes[0] != "" || lastExp == nil || !lastExp.Equal(exp) {
		t.Fatalf("observe success: %v %v", codes, lastExp)
	}
	status = 401
	if err = engine.Sync(ctx, true); err == nil {
		t.Fatal("401 must fail")
	}
	if len(codes) != 2 || codes[1] != "credentials_rejected" {
		t.Fatalf("observe failure: %v", codes)
	}
}
