package httpapi

import (
	"context"
	"net/http"

	"github.com/torvanis/janus/internal/license"
)

// noticeForLicense fences the projection against a concurrent database renewal
// or manual replacement between the displayed snapshot and the sync read.
func noticeForLicense(st license.State, syncState license.SyncState, n license.RenewalNotice) license.RenewalNotice {
	if !n.SuppressExpiring {
		return n
	}
	sub := syncState.Subscription
	if (st.Status != license.StatusValid && st.Status != license.StatusExpiring) || st.Claims == nil || sub == nil || sub.LicenseID != st.Claims.LicenseID || sub.PaidThrough == nil || st.ExpiresAt == nil || !sub.PaidThrough.Equal(*st.ExpiresAt) {
		return license.RenewalNotice{Reason: "license_changed"}
	}
	return n
}

func (s *Server) licenseSyncView(ctx context.Context) (license.SyncState, license.RenewalNotice) {
	if s.LicenseSync == nil {
		return license.SyncState{Mode: "manual", Health: "disabled", Configurable: true}, license.RenewalNotice{Reason: "manual"}
	}
	return s.LicenseSync.View(ctx)
}

func (s *Server) handleGetLicenseSync(w http.ResponseWriter, r *http.Request) {
	state, notice := s.licenseSyncView(r.Context())
	WriteJSON(w, http.StatusOK, map[string]any{"license_sync": state, "renewal_notice": notice})
}

func (s *Server) handlePutLicenseSync(w http.ResponseWriter, r *http.Request) {
	if s.LicenseSync == nil {
		WriteError(w, r, ErrInvalidRequest("License sync is unavailable on this server"))
		return
	}
	var body struct {
		Enabled    *bool  `json:"enabled"`
		Token      string `json:"token"`
		ClearToken bool   `json:"clear_token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.LicenseSync.ConfigurePatch(r.Context(), body.Enabled, body.Token, body.ClearToken); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	s.audit(r, "license.sync.configure", "license", "", nil, map[string]any{"enabled": body.Enabled, "token_changed": body.Token != "" || body.ClearToken})
	s.handleGetLicenseSync(w, r)
}

func (s *Server) handlePostLicenseSync(w http.ResponseWriter, r *http.Request) {
	if s.LicenseSync == nil {
		WriteError(w, r, ErrInvalidRequest("License sync is unavailable on this server"))
		return
	}
	err := s.LicenseSync.Sync(r.Context(), true)
	outcome := "success"
	if err != nil {
		outcome = err.Error()
	}
	s.audit(r, "license.sync", "license", "", nil, map[string]any{"outcome": outcome})
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("License sync: "+outcome))
		return
	}
	s.handleGetLicenseSync(w, r)
}
