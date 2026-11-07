package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/listenertls"
)

// Listener TLS (Administration → Settings → Certificates → Janus listener):
// what Janus serves on its own port, the certificate for download into a
// proxy's trust store, and regenerating the self-signed certificate.

func (s *Server) mountListenerTLSRoutes(r chi.Router) {
	r.Get("/admin/listener-tls", s.handleGetListenerTLS)
	r.Get("/admin/listener-tls/certificate.pem", s.handleDownloadListenerCert)
	r.Post("/admin/listener-tls/regenerate", s.handleRegenerateListenerCert)
}

func (s *Server) listenerTLSInfo() listenertls.Info {
	if s.ListenerTLS == nil {
		return listenertls.Info{Mode: listenertls.ModeNone, Required: s.Config != nil && s.Config.TLSRequire}
	}
	return s.ListenerTLS.Info()
}

func (s *Server) handleGetListenerTLS(w http.ResponseWriter, r *http.Request) {
	if s.ListenerTLS != nil {
		_ = s.ListenerTLS.Refresh(r.Context())
	}
	WriteJSON(w, http.StatusOK, map[string]any{"listener": s.listenerTLSInfo()})
}

func (s *Server) handleDownloadListenerCert(w http.ResponseWriter, r *http.Request) {
	if s.ListenerTLS == nil || !s.ListenerTLS.Enabled() {
		WriteError(w, r, ErrNotFoundf("A listener certificate"))
		return
	}
	pemText := s.ListenerTLS.CertificatePEM()
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-listener.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(pemText))
}

func (s *Server) handleRegenerateListenerCert(w http.ResponseWriter, r *http.Request) {
	if s.ListenerTLS == nil || s.listenerTLSInfo().Mode != listenertls.ModeSelfSigned {
		WriteError(w, r, ErrConflict("The listener is not using a self-signed certificate. A mounted certificate is renewed where it is issued (for example cert-manager)."))
		return
	}
	var body struct {
		Names []string `json:"names"`
		Days  int      `json:"days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	names := []string{}
	for _, n := range body.Names {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 50 {
		WriteError(w, r, ErrInvalidRequest("At most 50 names.").WithParam("names"))
		return
	}
	if body.Days < 0 || body.Days > 1095 {
		WriteError(w, r, ErrInvalidRequest("Validity must be between 1 and 1095 days.").WithParam("days"))
		return
	}
	before := s.listenerTLSInfo()
	actor := ""
	if u := UserFrom(r.Context()); u != nil {
		actor = u.Email
	}
	info, err := s.ListenerTLS.Regenerate(r.Context(), names, body.Days, actor)
	if err != nil {
		if errors.Is(err, listenertls.ErrNotSelfSigned) {
			WriteError(w, r, ErrConflict(err.Error()))
			return
		}
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("names"))
		return
	}
	s.audit(r, "listener_certificate_regenerated", "listener_tls", "self_signed",
		map[string]any{"sha256": before.SHA256, "not_after": before.NotAfter, "names": before.Names},
		map[string]any{"sha256": info.SHA256, "not_after": info.NotAfter, "names": info.Names})
	WriteJSON(w, http.StatusOK, map[string]any{"listener": info})
}

// CheckListenerCertExpiry alerts administrators once when the listener
// certificate enters its expiry warning window and once when it expires.
// main calls it every hour.
func (s *Server) CheckListenerCertExpiry(ctx context.Context) {
	if s.Alerts == nil || s.Store == nil {
		return
	}
	info := s.listenerTLSInfo()
	if info.Mode == listenertls.ModeNone || (!info.ExpiresSoon && !info.Expired) {
		return
	}
	state, sev := "expiring", "warning"
	title := "Janus listener certificate expires on " + info.NotAfter.UTC().Format("2 January 2006")
	if info.Expired {
		state, sev = "expired", "critical"
		title = "Janus listener certificate has expired"
	}
	if !s.claimAlert(ctx, fmt.Sprintf("listener_tls:%s:%s", info.SHA256, state), 45*24*time.Hour) {
		return
	}
	body := "Renew the mounted certificate where it is issued (" + info.CertPath + "); Janus picks up the new files without a restart."
	if info.Mode == listenertls.ModeSelfSigned {
		body = "Regenerate the self-signed certificate under Administration → Settings → Certificates and install the new one in the proxy in front of Janus, or switch to an issued certificate."
	}
	s.Alerts.Dispatch(ctx, alerting.Event{
		Trigger: alerting.TriggerSystem, Severity: sev, NotifyAdmins: true, Title: title, Body: body,
		Data: map[string]any{"mode": info.Mode, "sha256": info.SHA256, "not_after": info.NotAfter.UTC()},
	})
}
