package httpapi

import (
	"net/http"
	"testing"

	"github.com/torvanis/janus/internal/license"
)

func TestLicenseSyncOptionalEnabledPreservesExplicitChoice(t *testing.T) {
	for _, pin := range []bool{false, true} {
		t.Run(map[bool]string{false: "UI", true: "environment"}[pin], func(t *testing.T) {
			h := newHarness(t)
			promote(t, h)
			withBusinessLicense(t, h)
			options := license.SyncOptions{}
			yes := true
			if pin {
				options.Enabled = &yes
			}
			h.server.LicenseSync = license.NewSync(h.server.License, h.store, h.server.Cipher, options)
			path := "/api/v1/admin/system/license/sync"
			check := func(body map[string]any, enabled, token bool) {
				t.Helper()
				rec := h.do(http.MethodPut, path, body)
				if rec.Code != 200 {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				s := decodeBody(t, rec)["license_sync"].(map[string]any)
				if s["enabled"] != enabled || s["has_token"] != token {
					t.Fatal(s)
				}
			}
			check(map[string]any{"token": "secret"}, pin, true)
			check(map[string]any{"clear_token": true}, pin, false)
			check(map[string]any{"token": "secret"}, pin, true)
			check(map[string]any{"enabled": true}, true, true)
			check(map[string]any{"token": "rotated"}, true, true)
			check(map[string]any{"clear_token": true}, true, false)
		})
	}
}
