package httpapi

import (
	"net/http"
	"strings"

	"github.com/torvanis/janus/internal/license"
)

// License gating (RULING 2026-09-17: expiry never disrupts work).
//
// Only CREATE operations are gated when the license is expired, invalid or
// (for Business features) absent. Everything already configured keeps working:
// proxying, SSO/SCIM sync, guardrails, quotas, HA, reports. Edits and deletes
// are never gated. The error is a 402 so clients and the SPA can tell it apart
// from an authorization failure and route the admin to Admin → System.

// CodeLicenseRequired is returned when creation is blocked by license state.
const CodeLicenseRequired = "license.required"

// CodeFeatureNotLicensed is returned when enabling a gated feature the current
// edition does not include.
const CodeFeatureNotLicensed = "license.feature_not_licensed"

// ErrLicenseRequired reports that creation is blocked until a valid key is
// installed. what names the thing being created ("upstream", "token"…).
func ErrLicenseRequired(what string) *APIError {
	return newError(http.StatusPaymentRequired, CodeLicenseRequired, "license_error",
		"Creating a new "+what+" requires a valid license. Everything already configured keeps running. Install or renew the key under Admin → System (https://janusedge.com/portal).")
}

// ErrFeatureNotLicensed reports a Business/Enterprise feature the current
// edition does not include.
func ErrFeatureNotLicensed(feature string) *APIError {
	return newError(http.StatusPaymentRequired, CodeFeatureNotLicensed, "license_error",
		"The "+featureLabel(feature)+" feature is not included in the "+"current edition. Upgrade at https://janusedge.com/pricing, then install the new key under Admin → System.")
}

func featureLabel(f string) string {
	switch f {
	case "scim":
		return "SCIM provisioning"
	case "ldap":
		return "LDAP"
	case "multi_oidc":
		return "multiple identity providers"
	case "ha":
		return "high availability"
	case "guardrails_enforce":
		return "guardrail enforcement"
	case "reports_scheduled":
		return "scheduled reports"
	case "captures":
		return "troubleshooting captures"
	case "audit_export":
		return "audit export"
	case "model_fallbacks":
		return "model fallbacks"
	case "email_alerts":
		return "email alerting"
	}
	return strings.ReplaceAll(f, "_", " ")
}

// licenseState returns the current snapshot; a nil manager (tests) is Community.
func (s *Server) licenseState() license.State {
	if s.License == nil {
		return license.State{Edition: license.EditionCommunity, Status: license.StatusValid,
			Seats: license.CommunitySeats, Nodes: license.CommunityNodes, Features: []string{}}
	}
	return s.License.State()
}

// gateCreate wraps a create handler: when the license is restricted the
// request is refused with 402 before any store work.
func (s *Server) gateCreate(what string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.licenseState().Restricted() {
			WriteError(w, r, ErrLicenseRequired(what))
			return
		}
		next(w, r)
	}
}

// gateFeature wraps a handler that enables a gated feature: refused with 402
// when the feature is not licensed OR the license is restricted. Handlers that
// both create and enable call requireFeature inline instead so an edit that
// leaves the feature untouched still passes.
func (s *Server) gateFeature(feature string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.requireFeature(feature); err != nil {
			WriteError(w, r, err)
			return
		}
		next(w, r)
	}
}

// requireFeature is the inline form for handlers that need to check only when
// a request actually turns a feature on.
func (s *Server) requireFeature(feature string) error {
	st := s.licenseState()
	if !st.Has(feature) {
		return ErrFeatureNotLicensed(feature)
	}
	if st.Restricted() {
		return ErrLicenseRequired(featureLabel(feature))
	}
	return nil
}
