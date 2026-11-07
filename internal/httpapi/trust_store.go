package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

// Trusted certificates (Administration → Settings → Certificates): the
// outbound trust store's sources, with administrator uploads stored in the
// database. Mounted sources (DoD roots, JANUS_CA_BUNDLE, JANUS_CA_DIR) are
// listed read-only: they belong to the deployment.

func (s *Server) mountTrustStoreRoutes(r chi.Router) {
	r.Get("/admin/certificates", s.handleListCertificates)
	r.Post("/admin/certificates", s.handleUploadCertificate)
	r.Delete("/admin/certificates/{id}", s.handleDeleteCertificate)
}

func (s *Server) trustStoreView() map[string]any {
	st := truststore.Current()
	return map[string]any{
		"trust_store": st,
		// Whether this edition/profile offers "allow untrusted certificate".
		"untrusted_tls_available": stigprofile.Allowed(fipsmode.FeatureUntrustedTLS),
		"expiry_warning_days":     int(truststore.ExpiryWarning / (24 * time.Hour)),
	}
}

func (s *Server) handleListCertificates(w http.ResponseWriter, r *http.Request) {
	// Pick up uploads made on another replica before answering.
	if err := truststore.Reload(r.Context()); err != nil {
		s.Logger.WarnContext(r.Context(), "trust store reload", "error", err.Error())
	}
	WriteJSON(w, http.StatusOK, s.trustStoreView())
}

func (s *Server) handleUploadCertificate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		PEM  string `json:"pem"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	certs, err := truststore.ValidateUpload(body.PEM, time.Now())
	if err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("pem"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = certs[0].Subject.CommonName
	}
	if name == "" {
		name = certs[0].Subject.String()
	}
	if len(name) > 200 {
		WriteError(w, r, ErrInvalidRequest("The name must be 200 characters or fewer.").WithParam("name"))
		return
	}
	sum := sha256.Sum256(certs[0].Raw)
	fp := hex.EncodeToString(sum[:])
	actor := ""
	if u := UserFrom(r.Context()); u != nil {
		actor = u.Email
	}
	ca, err := s.Store.AddTrustedCA(r.Context(), name, strings.TrimSpace(body.PEM)+"\n", fp, actor)
	if errors.Is(err, store.ErrTrustedCAExists) {
		WriteError(w, r, ErrConflict("That certificate is already trusted.").WithParam("pem"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	subjects := make([]string, 0, len(certs))
	for _, c := range certs {
		subjects = append(subjects, c.Subject.String())
	}
	s.audit(r, "trusted_certificate_added", "trusted_certificate", ca.ID, nil, map[string]any{
		"name": name, "sha256": fp, "subjects": subjects, "not_after": certs[0].NotAfter.UTC(), "is_ca": certs[0].IsCA,
	})
	s.afterCommit(r.Context(), func() { s.reloadTrustStore(context.WithoutCancel(r.Context())) })
	WriteJSON(w, http.StatusCreated, map[string]any{"certificate": ca})
}

func (s *Server) handleDeleteCertificate(w http.ResponseWriter, r *http.Request) {
	ca, err := s.Store.DeleteTrustedCA(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That certificate"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "trusted_certificate_removed", "trusted_certificate", ca.ID, map[string]any{"name": ca.Name, "sha256": ca.SHA256}, nil)
	s.afterCommit(r.Context(), func() { s.reloadTrustStore(context.WithoutCancel(r.Context())) })
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": ca.ID})
}

// reloadTrustStore re-reads uploads and untrusted-upstream hosts after a
// change on this replica (other replicas follow within 30 seconds).
func (s *Server) reloadTrustStore(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ups, err := s.Store.UntrustedTLSUpstreams(ctx)
	if err == nil {
		hosts := make([]string, 0, len(ups))
		for _, u := range ups {
			if p, err := url.Parse(u.BaseURL); err == nil && p.Host != "" {
				hosts = append(hosts, truststore.HostPort(p.Scheme, p.Host))
			}
		}
		truststore.SetUntrustedHosts(hosts)
	}
	if err := truststore.Reload(ctx); err != nil {
		s.Logger.Warn("trust store reload", "error", err.Error())
	}
}

// untrustedTLSRefusal explains why baseURL may not skip verification, or
// returns nil.
func untrustedTLSRefusal(baseURL string) *APIError {
	if !stigprofile.Allowed(fipsmode.FeatureUntrustedTLS) {
		return ErrInvalidRequest("Allow untrusted certificate is not available in the Janus FIPS edition or the STIG profile. Upload the upstream's CA under Administration → Settings → Certificates instead.").WithParam("allow_untrusted_tls")
	}
	if p, err := url.Parse(strings.TrimSpace(baseURL)); err != nil || p.Scheme != "https" {
		return ErrInvalidRequest("Allow untrusted certificate only applies to https:// upstreams.").WithParam("allow_untrusted_tls")
	}
	return nil
}

// applyUpstreamUntrustedTLS saves the per-upstream "allow untrusted
// certificate" choice. It refuses turning it on where the FIPS edition or
// the STIG profile forbid it, and on plain-HTTP upstreams (nothing to
// verify).
func (s *Server) applyUpstreamUntrustedTLS(r *http.Request, up *store.Upstream, baseURL string, want *bool) *APIError {
	if want == nil {
		return nil
	}
	if *want {
		// Validate against the address being saved (it may have changed).
		if apiErr := untrustedTLSRefusal(baseURL); apiErr != nil {
			return apiErr
		}
	}
	if *want == up.AllowUntrustedTLS {
		return nil
	}
	if err := s.Store.SetUpstreamAllowUntrustedTLS(r.Context(), up.ID, *want); err != nil {
		return ErrInternal()
	}
	s.audit(r, "upstream_untrusted_tls_changed", "upstream", up.ID,
		map[string]any{"allow_untrusted_tls": up.AllowUntrustedTLS}, map[string]any{"allow_untrusted_tls": *want, "base_url": baseURL})
	up.AllowUntrustedTLS = *want
	s.afterCommit(r.Context(), func() { s.reloadTrustStore(context.WithoutCancel(r.Context())) })
	return nil
}
