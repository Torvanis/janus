package httpapi

import (
	"context"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/license"
)

func TestRenewalNoticeRequiresMatchingDisplayedLicense(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	st := license.State{Status: license.StatusGrace, Claims: &license.Claims{LicenseID: "shown"}, ExpiresAt: &expiry}
	state := license.SyncState{Subscription: &license.Subscription{LicenseID: "shown", PaidThrough: &expiry}}
	n := license.RenewalNotice{SuppressExpiring: true, Reason: "auto_renew"}
	if noticeForLicense(st, state, n).SuppressExpiring {
		t.Fatal("grace suppressed")
	}
	st.Status = license.StatusExpiring
	state.Subscription.LicenseID = "different"
	if noticeForLicense(st, state, n).SuppressExpiring {
		t.Fatal("changed identity suppressed")
	}
	state.Subscription.LicenseID = "shown"
	if !noticeForLicense(st, state, n).SuppressExpiring {
		t.Fatal("matched observation lost")
	}
}

func TestLicenseSyncAdminConfiguration(t *testing.T) {
	h := newHarness(t)
	withBusinessLicense(t, h)
	h.server.LicenseSync = license.NewSync(h.server.License, h.store, h.server.Cipher, license.SyncOptions{})
	path := "/api/v1/admin/system/license/sync"
	if err := h.store.UpdateUser(context.Background(), h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateTokenCache()
	if rec := h.do(http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("nonadmin %d", rec.Code)
	}
	promote(t, h)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		if denied := h.doAsSessionCSRF(session, method, path, "", map[string]any{"enabled": true, "token": "secret"}); denied.Code != http.StatusForbidden {
			t.Fatal("missing CSRF accepted", method, denied.Code)
		}
	}
	rec := h.do(http.MethodGet, path, nil)
	if rec.Code != 200 {
		t.Fatalf("get %d %s", rec.Code, rec.Body.String())
	}
	state := decodeBody(t, rec)["license_sync"].(map[string]any)
	if state["enabled"] != false || state["has_token"] != false {
		t.Fatal(state)
	}
	rec = h.do(http.MethodPut, path, map[string]any{"enabled": true, "token": "super-secret"})
	if rec.Code != 200 {
		t.Fatalf("put %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Fatal("credential disclosure")
	}
	state = decodeBody(t, rec)["license_sync"].(map[string]any)
	if state["enabled"] != true || state["has_token"] != true {
		t.Fatal(state)
	}
	rec = h.do(http.MethodPut, path, map[string]any{"enabled": false, "token": "", "clear_token": false})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	state = decodeBody(t, rec)["license_sync"].(map[string]any)
	if state["has_token"] != true {
		t.Fatal("blank must preserve")
	}
	rec = h.do(http.MethodPut, path, map[string]any{"enabled": false, "token": "x", "clear_token": true})
	if rec.Code != 400 {
		t.Fatal("contradiction accepted")
	}
	rec = h.do(http.MethodPut, path, map[string]any{"enabled": false, "clear_token": true})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	me := decodeBody(t, h.do(http.MethodGet, "/api/v1/me", nil))["license"].(map[string]any)
	notice := me["renewal_notice"].(map[string]any)
	if notice["suppress_expiring"] != false {
		t.Fatal(notice)
	}
	if _, ok := me["license_sync"]; ok {
		t.Fatal("admin state exposed to member")
	}
}
