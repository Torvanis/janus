package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/auditlog"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/listenertls"
	"github.com/torvanis/janus/internal/store"
)

// AuditForwarding is the syslog/SIEM forwarder's surface the API uses.
type AuditForwarding interface {
	Status() auditlog.Status
	Test(ctx context.Context, cfg store.SyslogPolicy, e store.AuditEntry) error
}

// Security settings (Administration → Settings → Security): session limits,
// the notice and consent banner, lockout, password policy, audit forwarding
// and smart-card sign-in. The policy itself lives in store.SecurityPolicy;
// this file is its HTTP surface and the sign-in / audit hooks.

// --- authentication audit ---------------------------------------------------

type signInMethodKey struct{}

// withSignInMethod records how the person is signing in (local, directory,
// oidc, smart_card, dev) for the audit record and the smart-card rule.
func withSignInMethod(r *http.Request, method string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), signInMethodKey{}, method))
}

func signInMethod(ctx context.Context) string {
	if m, _ := ctx.Value(signInMethodKey{}).(string); m != "" {
		return m
	}
	return "oidc"
}

// authAudit writes an authentication or access event. Failures to write are
// logged; they never block the sign-in path (the STIG's fail-closed rule is
// for administrative changes).
func (s *Server) authAudit(r *http.Request, action string, user *store.User, label, outcome string, detail map[string]any) {
	e := &store.AuditEntry{Action: action, ResourceType: "session", SourceIP: s.clientIP(r), Outcome: outcome}
	if user != nil {
		e.ActorUserID, e.ActorLabel, e.ResourceType, e.ResourceID = user.ID, user.Email, "user", user.ID
	}
	if label != "" && e.ActorLabel == "" {
		e.ActorLabel = label
	}
	if len(detail) > 0 {
		e.NewValue = encodeAuditValue(detail)
	}
	if err := s.Store.AppendAudit(context.WithoutCancel(r.Context()), e); err != nil {
		s.Logger.ErrorContext(r.Context(), "append authentication audit", "error", err.Error(), "action", action)
	}
}

// auditAccessDenied records refused requests on the application API that
// mean something: a signed-in person refused a permission (403), or a
// presented API token that does not work. A request with no credential, or
// with a session cookie that has ended, is just "not signed in": failed
// sign-ins, lockouts and session endings have their own records, and these
// are not audited (a tab left open after sign-out would write one a
// minute). At most one record per person (or address), address and path
// per minute.
func (s *Server) auditAccessDenied(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status != http.StatusUnauthorized && rec.status != http.StatusForbidden {
			return
		}
		user := UserFrom(r.Context())
		token := TokenFrom(r.Context()) != nil
		bearer := r.Header.Get("Authorization") != ""
		if user == nil && !token && !bearer {
			return
		}
		who := "anonymous"
		if user != nil {
			who = user.ID
		}
		ip := s.clientIP(r)
		if ok, _ := s.RateLimits.Allow("audit-denied:"+who+"|"+ip+"|"+r.URL.Path, 1, time.Now()); !ok {
			return
		}
		via := "session"
		if token {
			via = "token"
		} else if user == nil {
			via = "invalid_token"
		}
		s.authAudit(r, "access_denied", user, "", store.AuditFailure, map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": rec.status, "credential": via,
		})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming responses streaming.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// sessionEnded is called when a session ended on a limit while resolving
// the caller: it is audited once (the row is gone after this).
func (s *Server) sessionEnded(r *http.Request, err error) {
	var te *auth.TimeoutError
	if !errors.As(err, &te) {
		return
	}
	u, _ := s.Store.UserByID(r.Context(), te.UserID)
	s.authAudit(r, "session_timeout", u, te.UserID, store.AuditSuccess, map[string]any{"reason": te.Reason})
}

// --- notice and consent banner ----------------------------------------------

// consentCookie carries the banner acknowledgment from the banner to the
// sign-in that follows (any method). It is signed, short-lived, and cleared
// at sign-in, so every sign-in needs its own acknowledgment.
const (
	consentCookie = "janus_consent"
	consentTTL    = 30 * time.Minute
)

func (s *Server) consentKey() []byte {
	if s.Cipher != nil {
		return s.Cipher.DeriveKey("janus-consent-banner-v1")
	}
	return []byte("janus-consent-banner-test-key-only")
}

func (s *Server) consentValue(now time.Time, bannerHash string) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	m := hmac.New(sha256.New, s.consentKey())
	m.Write([]byte(ts + "|" + bannerHash))
	return ts + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func bannerHash(b store.BannerPolicy) string {
	h := sha256.Sum256([]byte(b.Title + "\x00" + b.Text))
	return hex.EncodeToString(h[:8])
}

// consentGiven reports whether the request carries a valid acknowledgment of
// the banner in force (always true when the banner is off).
func (s *Server) consentGiven(r *http.Request) bool {
	b := s.Store.SecurityPolicy(r.Context()).Banner
	if !b.Enabled {
		return true
	}
	c, err := r.Cookie(consentCookie)
	if err != nil {
		return false
	}
	ts, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(sec, 0)
	if time.Since(at) > consentTTL || time.Until(at) > time.Minute {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.consentValue(at, bannerHash(b))))
}

func (s *Server) handleBanner(w http.ResponseWriter, r *http.Request) {
	b := s.Store.SecurityPolicy(r.Context()).Banner
	if !b.Enabled {
		WriteJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": true, "title": b.Title, "text": b.Text, "button": b.Button,
		"acknowledged": s.consentGiven(r)})
}

func (s *Server) handleBannerAck(w http.ResponseWriter, r *http.Request) {
	b := s.Store.SecurityPolicy(r.Context()).Banner
	if !b.Enabled {
		WriteJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
		return
	}
	if ok, _ := s.RateLimits.Allow("banner:"+s.clientIP(r), 30, time.Now()); !ok {
		WriteError(w, r, newError(http.StatusTooManyRequests, "rate_limited", "auth_error", "Too many requests from this address. Wait a minute and try again."))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Value: s.consentValue(time.Now(), bannerHash(b)), Path: "/",
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(consentTTL.Seconds())})
	s.authAudit(r, "banner_acknowledged", nil, "", store.AuditSuccess, map[string]any{"banner": bannerHash(b), "user_agent": truncate(r.UserAgent(), 200)})
	WriteJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
}

func clearConsent(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// --- session cap ------------------------------------------------------------

// enforceSessionCap ends the person's oldest sessions so at most max remain
// (keep is the session just created).
func (s *Server) enforceSessionCap(r *http.Request, user *store.User, keep string) {
	limit := s.Store.SecurityPolicy(r.Context()).Sessions.MaxPerUser
	if limit <= 0 {
		return
	}
	ids, err := s.Store.OldestWebSessions(r.Context(), user.ID, time.Now().UTC())
	if err != nil {
		s.Logger.WarnContext(r.Context(), "list sessions for the session limit", "error", err.Error())
		return
	}
	extra := len(ids) - limit
	for _, id := range ids {
		if extra <= 0 {
			break
		}
		if id == keep {
			continue
		}
		if err := s.Sessions.Delete(r.Context(), id); err == nil {
			extra--
			s.authAudit(r, "session_ended_cap", user, "", store.AuditSuccess, map[string]any{"limit": limit})
		}
	}
}

// --- lockout ----------------------------------------------------------------

// signInFailed counts a failed attempt for login, audits it, and answers.
// It reports the response it wrote.
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request, login, reason string) {
	pol := s.Store.SecurityPolicy(r.Context()).Lockout
	lockedNow, err := s.Store.RecordSignInFailure(r.Context(), login, pol)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "record failed sign-in", "error", err.Error())
	}
	var u *store.User
	if login != "" {
		u, _ = s.Store.UserByEmail(r.Context(), login)
	}
	s.authAudit(r, "sign_in_failed", u, strings.ToLower(strings.TrimSpace(login)), store.AuditFailure, map[string]any{"reason": reason, "method": signInMethod(r.Context())})
	if lockedNow {
		s.authAudit(r, "account_locked", u, strings.ToLower(strings.TrimSpace(login)), store.AuditFailure, map[string]any{
			"failures": pol.MaxFailures, "window_minutes": pol.WindowMinutes, "lock_minutes": pol.LockMinutes,
		})
		_, until := s.Store.SignInLocked(r.Context(), login)
		WriteError(w, r, newError(http.StatusLocked, "locked", "auth_error", store.LockedMessage(until)))
		return
	}
	WriteError(w, r, newError(http.StatusUnauthorized, "bad_credentials", "auth_error", store.ErrBadCredentials.Error()))
}

// refuseIfLocked answers 423 when login is locked.
func (s *Server) refuseIfLocked(w http.ResponseWriter, r *http.Request, login string) bool {
	locked, until := s.Store.SignInLocked(r.Context(), login)
	if !locked {
		return false
	}
	u, _ := s.Store.UserByEmail(r.Context(), login)
	s.authAudit(r, "sign_in_failed", u, strings.ToLower(strings.TrimSpace(login)), store.AuditFailure, map[string]any{"reason": "locked", "method": signInMethod(r.Context())})
	WriteError(w, r, newError(http.StatusLocked, "locked", "auth_error", store.LockedMessage(until)))
	return true
}

// --- admin API --------------------------------------------------------------

func (s *Server) mountSecurityRoutes(ar chi.Router) {
	ar.Get("/admin/security/policy", s.handleGetSecurityPolicy)
	ar.Put("/admin/security/policy", s.handlePutSecurityPolicy)
	ar.Get("/admin/security/lockouts", s.handleListLockouts)
	ar.Post("/admin/security/lockouts/unlock", s.handleUnlock)
	ar.Post("/admin/security/syslog/test", s.handleSyslogTest)
	ar.Put("/admin/users/{id}/smart-card", s.handleSetCardIdentity)
}

func (s *Server) handleSetCardIdentity(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Identity string `json:"identity"`
	}
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	u, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	old, _ := s.Store.PKIIdentity(r.Context(), id)
	if err := s.Store.SetPKIIdentity(r.Context(), id, in.Identity); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()).WithParam("identity"))
		return
	}
	s.audit(r, "user_smart_card_set", "user", u.ID, map[string]any{"identity": old}, map[string]any{"identity": strings.ToLower(strings.TrimSpace(in.Identity))})
	WriteJSON(w, http.StatusOK, map[string]any{"identity": strings.ToLower(strings.TrimSpace(in.Identity))})
}

func (s *Server) securityPolicyDoc(ctx context.Context) map[string]any {
	stored, err := s.Store.StoredSecurityPolicy(ctx)
	if err != nil {
		stored = store.DefaultSecurityPolicy()
	}
	findings := store.STIGFindings(stored)
	if findings == nil {
		findings = []string{}
	}
	doc := map[string]any{
		"policy":       stored,
		"effective":    s.Store.SecurityPolicy(ctx),
		"defaults":     store.DefaultSecurityPolicy(),
		"stig_values":  store.STIGSecurityPolicy(stored),
		"stig_profile": store.STIGBaseline(),
		"stig_gaps":    findings,
		"dod_banner":   store.DoDBannerText,
		"baselines":    store.SecurityBaselines(stored),
		"limits": map[string]any{
			// JANUS_SESSION_*_HOURS bound what the policy can set.
			"max_absolute_minutes": int(s.Config.SessionTTL / time.Minute),
			"max_idle_minutes":     int(s.Config.SessionIdleTimeout / time.Minute),
		},
		"smart_card_direct_available": s.listenerTLSInfo().Mode != listenertls.ModeNone,
	}
	if s.AuditForwarder != nil {
		doc["syslog_status"] = s.AuditForwarder.Status()
	}
	return doc
}

func (s *Server) handleGetSecurityPolicy(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, s.securityPolicyDoc(r.Context()))
}

func (s *Server) handlePutSecurityPolicy(w http.ResponseWriter, r *http.Request) {
	var p store.SecurityPolicy
	if err := decodeJSON(r, &p); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := p.Validate(); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	if store.STIGBaseline() {
		if gaps := store.STIGFindings(p); len(gaps) > 0 {
			WriteError(w, r, ErrInvalidRequest("The STIG profile is on, so these settings cannot be weaker than the STIG: "+strings.Join(gaps, "; ")+". Use Apply DoD STIG values."))
			return
		}
	}
	if p.SmartCard.Enabled && p.SmartCard.Source == "direct" && s.listenerTLSInfo().Mode == listenertls.ModeNone {
		WriteError(w, r, ErrInvalidRequest("Smart-card sign-in with the certificate read by Janus needs Janus to serve TLS (listener TLS). Turn listener TLS on, or choose a proxy that forwards the certificate."))
		return
	}
	if p.SmartCard.Enabled && p.SmartCard.Source == "proxy" && len(s.Config.TrustedProxies) == 0 {
		WriteError(w, r, ErrInvalidRequest("Smart-card sign-in through a proxy needs JANUS_TRUSTED_PROXIES: Janus only accepts the certificate header from a proxy it trusts."))
		return
	}
	old, _ := s.Store.StoredSecurityPolicy(r.Context())
	if err := s.Store.PutSecurityPolicy(r.Context(), p); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	s.audit(r, "security_policy_changed", "security_policy", "instance", old, p)
	WriteJSON(w, http.StatusOK, s.securityPolicyDoc(r.Context()))
}

func (s *Server) handleListLockouts(w http.ResponseWriter, r *http.Request) {
	locks, err := s.Store.LockedSignIns(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"lockouts": locks})
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Login string `json:"login"`
	}
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(in.Login) == "" {
		WriteError(w, r, ErrInvalidRequest("Give the sign-in name to unlock.").WithParam("login"))
		return
	}
	was, err := s.Store.UnlockSignIn(r.Context(), in.Login)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "account_unlocked", "sign_in", strings.ToLower(strings.TrimSpace(in.Login)), map[string]any{"locked": was}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"unlocked": true, "was_locked": was})
}

func (s *Server) handleSyslogTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Syslog *store.SyslogPolicy `json:"syslog"`
	}
	if err := decodeJSON(r, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	cfg := s.Store.SecurityPolicy(r.Context()).Syslog
	if in.Syslog != nil {
		cfg = *in.Syslog
		cfg.Enabled = true
		p := store.DefaultSecurityPolicy()
		p.Syslog = cfg
		if err := p.Validate(); err != nil {
			WriteError(w, r, ErrInvalidRequest(err.Error()))
			return
		}
	}
	if s.AuditForwarder == nil {
		WriteError(w, r, ErrInvalidRequest("Audit forwarding is not available on this server."))
		return
	}
	actor := UserFrom(r.Context())
	err := s.AuditForwarder.Test(r.Context(), cfg, store.AuditEntry{
		ID: store.NewID(), ActorUserID: actor.ID, ActorLabel: actor.Email, Action: "syslog_test", ResourceType: "audit",
		ResourceID: "syslog", SourceIP: s.clientIP(r), Outcome: store.AuditSuccess, CreatedAt: time.Now().UTC(),
		NewValue: `{"message":"Janus audit forwarding test"}`,
	})
	s.audit(r, "syslog_test", "audit", "syslog", nil, map[string]any{"address": cfg.Address, "protocol": cfg.Protocol, "format": cfg.Format, "ok": err == nil})
	if err != nil {
		WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "message": fmt.Sprintf("Sent a test event to %s over %s.", cfg.Address, strings.ToUpper(cfg.Protocol))})
}

// peerTrusted reports whether the immediate peer is a declared proxy
// (JANUS_TRUSTED_PROXIES).
func (s *Server) peerTrusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.Config.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// sessionInfo describes the caller's session limits ({} for token callers).
func (s *Server) sessionInfo(r *http.Request) map[string]any {
	sess := SessionFrom(r.Context())
	if sess == nil {
		return map[string]any{}
	}
	return map[string]any{
		"idle_timeout_seconds": int(sess.IdleTimeout.Seconds()),
		"ends_at":              sess.AbsoluteEnds,
	}
}

// requireConsent sends a sign-in that starts at an identity provider back
// to the sign-in page when the banner has not been acknowledged, rather
// than after the round trip.
func (s *Server) requireConsent(w http.ResponseWriter, r *http.Request) bool {
	if s.consentGiven(r) {
		return true
	}
	http.Redirect(w, r, "/auth/login?redirect_uri="+url.QueryEscape(safeRedirect(r.URL.Query().Get("redirect_uri"))), http.StatusFound)
	return false
}
