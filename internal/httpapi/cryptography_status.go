package httpapi

import (
	"context"

	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/revocation"
	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/truststore"
)

// trustStoreSummary is the Cryptography card's one-line trust store view.
func trustStoreSummary() map[string]any {
	st := truststore.Current()
	counts := map[string]int{}
	for _, c := range st.CAs {
		counts[c.Source]++
	}
	return map[string]any{"dod_roots": st.DoD, "counts": counts, "expiring_soon": st.ExpiringSoon, "expired": st.Expired,
		"untrusted_allowed": st.UntrustedOK, "last_problem": st.LastProblem}
}

// cryptographyStatus is the admin System page's cryptography card: the
// edition, the linked Go Cryptographic Module and FIPS 140-3 mode, the
// features this edition switches off, and (FIPS edition) how many local
// accounts still hold credentials it cannot use.
func (s *Server) cryptographyStatus(ctx context.Context) map[string]any {
	st := fipsmode.Current()
	out := map[string]any{
		"edition":             st.Edition,
		"fips140_enabled":     st.FIPS140Enabled,
		"fips140_enforced":    st.FIPS140Enforced,
		"module_version":      st.ModuleVersion,
		"validated_module":    st.ValidatedModule,
		"cmvp_certificate":    st.CMVPCertificate,
		"restricted_features": st.Restricted,
		"stig_profile":        stigprofile.Enabled(),
		"tls_revocation":      revocation.Current(),
		"trust_store":         trustStoreSummary(),
		"listener_tls":        s.listenerTLSInfo(),
	}
	if fipsmode.IsFIPSEdition() || stigprofile.Enabled() {
		// The startup check already refused to run without this; the card
		// shows what satisfied it (cheap: the cached license state).
		ls := s.licenseState()
		out["edition_license"] = map[string]any{
			"required": "business",
			"ok":       license.RequireBusinessEdition(ls) == nil,
			"edition":  ls.Edition,
			"status":   ls.Status,
		}
	}
	if stigprofile.Enabled() && s.Config != nil {
		out["stig"] = stigprofile.Effective(s.Config)
	}
	if fipsmode.IsFIPSEdition() || stigprofile.Enabled() {
		pw, rc, totp, err := s.Store.CountLegacyCredentials(ctx, s.Cipher)
		if err == nil {
			out["legacy_credentials"] = map[string]int{"bcrypt_passwords": pw, "bcrypt_recovery_codes": rc, "sha1_totp": totp}
		}
	}
	return out
}
