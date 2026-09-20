package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/updates"
)

// seatWindow defines "active user": signed in or made a request in the last 30 days.
const seatWindow = 30 * 24 * time.Hour

// licenseSummary is the non-admin view: enough for the shell banner, nothing
// a regular user shouldn't see (no org, license_id or notes).
func (s *Server) licenseSummary() map[string]any {
	st := s.licenseState()
	features := st.Features
	if features == nil {
		features = []string{}
	}
	out := map[string]any{"edition": st.Edition, "status": st.Status, "restricted": st.Restricted(), "features": features}
	if st.ExpiresAt != nil {
		out["expires_at"] = st.ExpiresAt
	}
	if st.GraceUntil != nil {
		out["grace_until"] = st.GraceUntil
	}
	return out
}

// seatsUsed counts active users in the trailing window. Admins count too: a
// seat is a person, whatever their role.
func (s *Server) seatsUsed(r *http.Request) (int, error) {
	return s.Store.ActiveSeatCount(r.Context(), time.Now().Add(-seatWindow))
}

// checkSeatAvailable refuses a first sign-in when every licensed seat is used.
func (s *Server) checkSeatAvailable(ctx context.Context) error {
	st := s.licenseState()
	if st.Seats <= 0 {
		return nil
	}
	used, err := s.Store.ActiveSeatCount(ctx, time.Now().Add(-seatWindow))
	if err != nil {
		// Never let a counting failure lock people out.
		s.Logger.Warn("seat count failed; allowing sign-in", "error", err.Error())
		return nil
	}
	if used >= st.Seats {
		return errors.New("This Janus gateway has no free seats (" + itoa(used) + " of " + itoa(st.Seats) + " active users in the last 30 days). Ask a Janus administrator to add seats at https://janusedge.com/portal, or wait for an inactive user to free one.")
	}
	return nil
}

// handleGetLicense returns the full license state plus seat and node usage.
func (s *Server) handleGetLicense(w http.ResponseWriter, r *http.Request) {
	st := s.licenseState()
	used, err := s.seatsUsed(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	instanceID, err := s.Store.InstanceID(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	liveNodes, err := s.Store.LiveInstanceCount(r.Context(), time.Now())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"license":     st,
		"seats_used":  used,
		"nodes_live":  liveNodes,
		"seat_window": "30d",
		"instance_id": instanceID,
		"file":        s.Config.LicenseFile,
		"version":     s.Config.BuildVersion,
		"build_date":  s.Config.BuildDate,
		"portal_url":  "https://janusedge.com/portal",
		"update":      s.updateResult(),
	})
}

// updateResult is the cached update-check outcome, or a disabled marker when
// the checker was never built (offline builds, tests).
func (s *Server) updateResult() updates.Result {
	if s.Updates == nil {
		return updates.Result{Enabled: false, Offline: s.Config.Offline, Current: s.Config.BuildVersion}
	}
	return s.Updates.Last()
}

// handlePutLicense installs a pasted key after verifying it.
func (s *Server) handlePutLicense(w http.ResponseWriter, r *http.Request) {
	if s.License == nil {
		WriteError(w, r, ErrInternal())
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	key := strings.TrimSpace(body.Key)
	if !strings.HasPrefix(key, license.Prefix) {
		WriteError(w, r, ErrInvalidRequest("That is not a Janus license key. Keys start with "+license.Prefix+"."))
		return
	}
	prev := s.licenseState()
	st, err := s.License.Install(r.Context(), key)
	if err != nil {
		switch {
		case errors.Is(err, license.ErrUnknownKey):
			WriteError(w, r, ErrInvalidRequest("This key was signed with a key this build does not trust. Upgrade Janus, or contact support@janusedge.com."))
		case errors.Is(err, license.ErrBadSignature):
			WriteError(w, r, ErrInvalidRequest("The key's signature does not verify. Copy the whole key exactly as downloaded from the portal."))
		default:
			WriteError(w, r, ErrInvalidRequest("The key could not be read: "+err.Error()))
		}
		return
	}
	s.audit(r, "license.install", "license", licenseID(st), map[string]any{"edition": prev.Edition, "status": prev.Status},
		map[string]any{"edition": st.Edition, "status": st.Status, "seats": st.Seats, "source": st.Source})
	WriteJSON(w, http.StatusOK, map[string]any{"license": st})
}

// handleDeleteLicense removes the database key (a file key stays).
func (s *Server) handleDeleteLicense(w http.ResponseWriter, r *http.Request) {
	if s.License == nil {
		WriteError(w, r, ErrInternal())
		return
	}
	prev := s.licenseState()
	st, err := s.License.Remove(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "license.remove", "license", licenseID(prev), map[string]any{"edition": prev.Edition}, map[string]any{"edition": st.Edition})
	WriteJSON(w, http.StatusOK, map[string]any{"license": st})
}

func licenseID(st license.State) string {
	if st.Claims != nil {
		return st.Claims.LicenseID
	}
	return ""
}
