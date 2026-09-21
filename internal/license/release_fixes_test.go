package license

import (
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSyncCopiedCredentials(t *testing.T) {
	for _, token := range []string{"raw", "JNS-BUS-TEST.raw", "foreign.raw", "JNS-BUS-TEST.JNS-BUS-TEST.raw", "Bearer JNS-BUS-TEST.raw", "raw\r\nX:evil"} {
		got, ok := normalizeSyncToken(token, "JNS-BUS-TEST")
		want := token == "raw" || token == "JNS-BUS-TEST.raw"
		if ok != want || (ok && got != "raw") {
			t.Fatalf("credential normalization acceptance=%v want=%v", ok, want)
		}
	}
}

func TestRevocationRecoveryRequiresNewSignedAndFreshGeneration(t *testing.T) {
	for _, variant := range []string{"new", "replay", "unsigned_only", "same_revision", "stale", "future", "wrong_mode", "wrong_identity", "bad_signature", "legacy", "canceled", "missing_metadata", "unknown_schema", "future_metadata", "equal_issuance"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			db, err := store.Open(ctx, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			priv, pubs := testKeys(t)
			c := business(now.Add(48*time.Hour), 7)
			c.Issued = now.Add(-time.Hour)
			raw, _ := Sign(priv, c)
			if err = db.PutLicenseKey(ctx, raw); err != nil {
				t.Fatal(err)
			}
			m := NewManager("", db, pubs, nil)
			m.now = func() time.Time { return now }
			_ = m.Refresh(ctx)
			cipher, _ := crypto.New(make([]byte, 32))
			calls := 0
			sub := Subscription{SchemaVersion: 1, LicenseID: c.LicenseID, BillingMode: "live", Status: "active", AutoRenew: true, PaidThrough: c.Exp, VerifiedAt: now.Add(-time.Minute), FreshUntil: now.Add(-time.Minute).Add(syncTTL), Revision: 1}
			response := map[string]any{"status": "active", "license_id": c.LicenseID, "license": raw, "subscription": sub}
			tr := syncTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+c.LicenseID+".raw" {
					t.Error("bad authorization")
				}
				b, _ := json.Marshal(response)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
			})
			opts := SyncOptions{Now: func() time.Time { return now }, Transport: tr}
			e := NewSync(m, db, cipher, opts)
			if err = e.Configure(ctx, true, c.LicenseID+".raw", false); err != nil {
				t.Fatal(err)
			}
			if err = e.Sync(ctx, true); err != nil {
				t.Fatal(err)
			}
			response = map[string]any{"status": "revoked", "license_id": c.LicenseID, "revoked_at": now}
			if variant == "legacy" {
				delete(response, "revoked_at")
			}
			if err = e.Sync(ctx, true); err == nil {
				t.Fatal("revocation not observed")
			}
			_, kept, _ := db.LicenseSyncSnapshot(ctx)
			if kept != raw || m.State().Restricted() {
				t.Fatal("signed rights changed")
			}
			// Restart and credential rotation must retain the barrier and high-water.
			e = NewSync(m, db, cipher, opts)
			if err = e.Configure(ctx, true, "raw", false); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			next := c
			next.Issued = now
			sub.Revision = 2
			sub.VerifiedAt = now
			sub.FreshUntil = now.Add(syncTTL)
			switch variant {
			case "replay":
				next = c
				sub.Revision = 1
			case "unsigned_only":
				next = c
			case "same_revision":
				sub.Revision = 1
			case "stale":
				sub.VerifiedAt = now.Add(-7 * time.Hour)
				sub.FreshUntil = sub.VerifiedAt.Add(syncTTL)
			case "future":
				next.Issued = now.Add(5*time.Minute + time.Second)
			case "wrong_mode":
				sub.BillingMode = "sandbox"
			case "wrong_identity":
				next.LicenseID = "foreign"
			case "equal_issuance":
				next.Issued = now.Add(-time.Minute)
			case "unknown_schema":
				sub.SchemaVersion = 99
			case "future_metadata":
				sub.VerifiedAt = now.Add(5*time.Minute + time.Second)
				sub.FreshUntil = sub.VerifiedAt.Add(syncTTL)
			case "canceled":
				sub.Status = "canceled"
				sub.AutoRenew = false
			}
			signed, _ := Sign(priv, next)
			if variant == "bad_signature" {
				signed = "invalid"
			}
			response = map[string]any{"status": "active", "license_id": c.LicenseID, "license": signed, "subscription": sub}
			if variant == "missing_metadata" {
				delete(response, "subscription")
			}
			err = e.Sync(ctx, true)
			state, notice := e.View(ctx)
			if calls != 3 {
				t.Fatalf("revoked engine stopped authenticated contacts: %d", calls)
			}
			if variant == "new" {
				if err != nil || state.Health != "healthy" || !notice.SuppressExpiring {
					t.Fatalf("repair failed %v %+v %+v", err, state, notice)
				}
			} else if err == nil || notice.Reason != "revoked" || notice.SuppressExpiring {
				t.Fatalf("unsafe repair %v %+v", err, notice)
			}
		})
	}
}
