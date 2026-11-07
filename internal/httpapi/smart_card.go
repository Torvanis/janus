package httpapi

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/pki"
	"github.com/torvanis/janus/internal/revocation"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

// Smart-card (CAC/PIV) sign-in: GET /auth/pki.
//
// The browser presents the card's certificate during the TLS handshake,
// either to Janus (listener TLS: the server asks for a client certificate
// on every handshake and the browser offers the card) or to the proxy in
// front, which forwards it in a header. Janus then verifies the chain
// against the configured authorities (never the public web roots), checks
// revocation, matches the card to a person and starts a session through
// the same sign-in path as every other method (banner, lockout, seats,
// audit).

// cardCertificate returns the presented certificate and any intermediates.
func (s *Server) cardCertificate(r *http.Request, p store.SmartCardPolicy) (*x509.Certificate, []*x509.Certificate, error) {
	if p.Source == "proxy" {
		if !s.peerTrusted(r) {
			return nil, nil, errors.New("the certificate header came from an address that is not a trusted proxy (JANUS_TRUSTED_PROXIES)")
		}
		return pki.ParseHeader(r.Header.Get(p.Header), p.HeaderFormat)
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, nil, pki.ErrNoCertificate
	}
	return r.TLS.PeerCertificates[0], r.TLS.PeerCertificates[1:], nil
}

// cardUser finds the person a card belongs to, in the policy's order.
func (s *Server) cardUser(ctx context.Context, p store.SmartCardPolicy, id pki.Identity) (*store.User, string, error) {
	for _, m := range p.Match {
		switch m {
		case "upn":
			if id.UPN == "" {
				continue
			}
			if u, err := s.Store.UserByPKIIdentity(ctx, id.UPN); err == nil {
				return u, "upn", nil
			}
			if strings.Contains(id.UPN, "@") {
				if u, err := s.Store.UserByEmail(ctx, id.UPN); err == nil {
					return u, "upn", nil
				}
			}
		case "email":
			for _, e := range id.Emails {
				if u, err := s.Store.UserByEmail(ctx, e); err == nil {
					return u, "email", nil
				}
			}
		case "edipi":
			if id.EDIPI == "" {
				continue
			}
			if u, err := s.Store.UserByPKIIdentity(ctx, id.EDIPI); err == nil {
				return u, "edipi", nil
			}
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *Server) handleSmartCardLogin(w http.ResponseWriter, r *http.Request) {
	r = withSignInMethod(r, "smart_card")
	redirectTo := safeRedirect(r.URL.Query().Get("redirect_uri"))
	p := s.Store.SecurityPolicy(r.Context()).SmartCard
	if !p.Enabled {
		s.renderLoginError(w, r, "Smart-card sign-in is not turned on.")
		return
	}
	if ok, _ := s.RateLimits.Allow("login:"+s.clientIP(r), loginRatePerMin, time.Now()); !ok {
		s.renderLoginError(w, r, "Too many sign-in attempts from this address. Wait a minute and try again.")
		return
	}
	refuse := func(reason, msg string, id *pki.Identity) {
		detail := map[string]any{"reason": reason, "method": "smart_card"}
		label := ""
		if id != nil {
			detail["subject"], detail["issuer"], detail["serial"] = id.Subject, id.Issuer, id.Serial
			label = firstNonEmptyStr(id.UPN, id.EDIPI, id.Subject)
		}
		s.authAudit(r, "sign_in_failed", nil, label, store.AuditFailure, detail)
		s.renderLoginError(w, r, msg)
	}
	leaf, inter, err := s.cardCertificate(r, p)
	if err != nil {
		if errors.Is(err, pki.ErrNoCertificate) {
			refuse("no_certificate", "No smart-card certificate was received. Insert your card, make sure your browser offers its certificate to this site, and try again.", nil)
			return
		}
		refuse("bad_certificate", "The smart-card certificate could not be read: "+err.Error(), nil)
		return
	}
	id := pki.Extract(leaf)
	chains, err := pki.Verify(leaf, inter, truststore.ClientCAPool(), time.Now())
	if err != nil {
		refuse("untrusted", "Your card's certificate is not trusted here. Ask an administrator to add your certificate authority under Settings → Certificates.", &id)
		return
	}
	if c := revocationChecker(); c != nil {
		if err := c(r.Context(), chains[0]); err != nil {
			reason, msg := "revocation_unavailable", "Your card's revocation status could not be checked, so sign-in was refused. Try again later."
			if errors.Is(err, revocation.ErrRevoked) {
				reason, msg = "revoked", "Your card's certificate has been revoked."
			}
			refuse(reason, msg, &id)
			return
		}
	}
	user, how, err := s.cardUser(r.Context(), p, id)
	if err != nil && !p.Provision {
		refuse("no_matching_person", "No Janus account matches this card. Ask an administrator to set your card identity (UPN or DoD ID number) on your account.", &id)
		return
	}
	login := firstNonEmptyStr(id.UPN, id.EDIPI, id.Subject)
	if user != nil {
		login = user.Email
	}
	if s.refuseIfLocked(w, r, login) {
		return
	}
	var identity *auth.Identity
	if user != nil {
		identity = &auth.Identity{Subject: user.AuthID, Email: user.Email, EmailVerified: true, Name: user.Name}
	} else {
		email := ""
		if len(id.Emails) > 0 {
			email = id.Emails[0]
		} else if strings.Contains(id.UPN, "@") {
			email = id.UPN
		}
		if email == "" {
			refuse("no_email", "This card carries no email address or UPN to create an account from. Ask an administrator to create your account.", &id)
			return
		}
		subject := "pki:" + firstNonEmptyStr(id.EDIPI, id.UPN, email)
		identity = &auth.Identity{Subject: subject, Email: email, EmailVerified: true, Name: firstNonEmptyStr(id.Name, email)}
		how = "provisioned"
	}
	r = r.WithContext(context.WithValue(r.Context(), cardSignInKey{}, map[string]any{"matched_by": how, "subject": id.Subject, "issuer": id.Issuer, "serial": id.Serial}))
	s.completeSignIn(w, r, identity, redirectTo)
}

type cardSignInKey struct{}

func cardDetail(ctx context.Context) map[string]any {
	m, _ := ctx.Value(cardSignInKey{}).(map[string]any)
	return m
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// revocationChecker checks a verified chain with the installed revocation
// checker; nil when revocation checking is off. JANUS_TLS_REVOCATION
// applies to smart cards as to outbound connections (the STIG profile sets
// it to hard).
var revocationChecker = func() func(ctx context.Context, chain []*x509.Certificate) error {
	if revocation.Current().Mode == revocation.ModeOff {
		return nil
	}
	return revocation.CheckVerifiedChain
}
