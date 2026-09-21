package license

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyncMissingPaidOrStaleMetadataPreservesSignedRenewal(t *testing.T) {
	for _, kind := range []string{"missing_paid", "stale"} {
		t.Run(kind, func(t *testing.T) {
			f := syncSetup(t)
			f.enable(t)
			sub := f.response["subscription"].(*Subscription)
			if kind == "missing_paid" {
				sub.PaidThrough = nil
			} else {
				sub.VerifiedAt = f.now.Add(-7 * time.Hour)
				sub.FreshUntil = sub.VerifiedAt.Add(6 * time.Hour)
			}
			if err := f.e.Sync(context.Background(), true); err != nil {
				t.Fatalf("valid signed renewal rejected: %v", err)
			}
			_, notice := f.e.View(context.Background())
			if notice.SuppressExpiring {
				t.Fatal("insufficient payment evidence suppressed")
			}
		})
	}
}

func TestSyncMissingMetadataFieldsNeverSuppress(t *testing.T) {
	for _, field := range []string{"revision", "cancel_at_period_end", "verified_at", "paid_through"} {
		t.Run(field, func(t *testing.T) {
			f := syncSetup(t)
			f.enable(t)
			b, _ := json.Marshal(f.metadata())
			var sub map[string]any
			_ = json.Unmarshal(b, &sub)
			delete(sub, field)
			f.response["subscription"] = sub
			_ = f.e.Sync(context.Background(), true)
			_, notice := f.e.View(context.Background())
			if notice.SuppressExpiring {
				t.Fatalf("missing %s suppressed", field)
			}
		})
	}
}

func TestSyncNetworkBoundsAndSanitization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		header http.Header
		err    error
	}{
		{"malformed", 200, "{", nil, nil}, {"trailing", 200, "{} {}", nil, nil}, {"oversized", 200, strings.Repeat("x", 128*1024+1), nil, nil},
		{"redirect", 302, "", http.Header{"Location": []string{"https://attacker.invalid/steal"}}, nil},
		{"network", 0, "", nil, errors.New("secret token in upstream failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := syncSetup(t)
			f.enable(t)
			f.e.client.Transport = syncTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != SyncEndpoint {
					t.Fatal("bearer redirect")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: tc.header, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			err := f.e.Sync(context.Background(), true)
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			state, notice := f.e.View(context.Background())
			b, _ := json.Marshal(state)
			if strings.Contains(string(b), "secret") || strings.Contains(err.Error(), "secret") || notice.SuppressExpiring {
				t.Fatal("leaked error or suppressed")
			}
		})
	}
}

func TestSyncCancellationPersistsFailure(t *testing.T) {
	f := syncSetup(t)
	f.enable(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.e.client.Transport = syncTransport(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if err := f.e.Sync(ctx, true); err == nil {
		t.Fatal("cancellation lost")
	}
	state, n := f.e.View(context.Background())
	if state.Health != "error" || state.ErrorCode != "network" || state.LastAttemptAt == nil || state.NextRetryAt == nil || n.SuppressExpiring {
		t.Fatal(state, n)
	}
}

func TestSyncFileOfflineAndEndpointNeverCallNetwork(t *testing.T) {
	for _, mode := range []string{"file", "signed_offline", "unsafe_url"} {
		t.Run(mode, func(t *testing.T) {
			f := syncSetup(t)
			f.enable(t)
			c := f.claims
			switch mode {
			case "file":
				p := filepath.Join(t.TempDir(), "license")
				_ = os.WriteFile(p, []byte("mounted"), 0600)
				f.e.manager.file = p
			case "signed_offline":
				c.Offline = true
			case "unsafe_url":
				c.SyncURL = "http://127.0.0.1/latest/meta-data"
			}
			if mode != "file" {
				raw, _ := Sign(f.priv, c)
				_ = f.db.PutLicenseKey(context.Background(), raw)
				f.enable(t)
			}
			_ = f.e.Sync(context.Background(), true)
			if f.calls.Load() != 0 {
				t.Fatal("forbidden network")
			}
			if mode == "file" {
				b, _ := os.ReadFile(f.e.manager.file)
				if string(b) != "mounted" {
					t.Fatal("mounted file modified")
				}
				if err := f.e.Configure(context.Background(), true, "", false); err == nil || !strings.Contains(err.Error(), "managed database") {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSyncSevenDayBoundariesAndProductionTrust(t *testing.T) {
	f := syncSetup(t)
	raw, _ := Sign(f.priv, f.claims)
	if _, err := Verify(raw, TrustedKeys()); err == nil {
		t.Fatal("production trusts sandbox fixture")
	}
	exp := *f.claims.Exp
	for _, tc := range []struct {
		at     time.Time
		status Status
	}{{exp.Add(-time.Nanosecond), StatusExpiring}, {exp, StatusGrace}, {exp.Add(7*24*time.Hour - time.Nanosecond), StatusGrace}, {exp.Add(7 * 24 * time.Hour), StatusExpired}} {
		f.now = tc.at
		if got := f.e.manager.State().Status; got != tc.status {
			t.Fatalf("%s: %s", tc.at, got)
		}
	}
}
