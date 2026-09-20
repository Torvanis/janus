package httpapi

import (
	"encoding/json"
	"errors"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// Local accounts (email + password, optional TOTP).
//
// Why: trials and small deployments should not need an IdP on day one, and
// JANUS_DEV_AUTH (anyone types any email) was never acceptable outside a
// laptop. Local auth is a real identity source: passwords bcrypt-hashed,
// lockout after repeated failures, TOTP with recovery codes, every event
// audited. It composes with OIDC — an admin with a password is the
// break-glass when the IdP is down.
//
// Flow: POST /auth/local/login {email,password} → session, or 401 with
// `totp_required` + a short-lived pending token → POST /auth/local/totp
// {pending, code} → session. Both feed completeSignIn so seats, roles and
// group rules apply exactly as for SSO.

// clientIP honors X-Forwarded-For only from trusted proxies (same rule as
// the proxy path).
func (s *Server) clientIP(r *http.Request) string { return ClientIP(r, s.Config.TrustedProxies) }

func unescapeQuery(v string) string {
	u, err := url.QueryUnescape(v)
	if err != nil {
		return v
	}
	return u
}

const (
	pendingTTL      = 5 * time.Minute
	totpIssuer      = "Janus Edge"
	loginRatePerMin = 20 // per client IP; the bcrypt lockout is the real defense
)

// pendingLogin remembers a password-verified user awaiting their TOTP code.
// localAuthEnabled: local sign-in is offered when any local account exists
// or when there is no other way in (no IdP, no dev auth) — first-run setup.
func (s *Server) localAuthEnabled(r *http.Request) bool {
	has, err := s.Store.HasLocalCredentials(r.Context())
	if err != nil {
		return false
	}
	if has || (s.OIDC == nil && !s.Config.DevAuthEnabled) {
		return true
	}
	// A configured, enabled, licensed directory also uses the password form.
	_, _, dir := s.directoryConfig(r)
	return dir
}

// needsSetup is true on a fresh install with no way to sign in at all: the
// login page then offers "Create the first administrator".
func (s *Server) needsSetup(r *http.Request) bool {
	if s.OIDC != nil || s.Config.DevAuthEnabled {
		return false
	}
	has, err := s.Store.HasLocalCredentials(r.Context())
	return err == nil && !has
}

func (s *Server) mountLocalAuthRoutes(r chi.Router) {
	r.Get("/auth/local/status", s.handleLocalStatus)
	r.Post("/auth/local/setup", s.handleLocalSetup)
	r.Post("/auth/local/login", s.handleLocalLogin)
	r.Post("/auth/local/totp", s.handleLocalTOTP)
}

func (s *Server) handleLocalStatus(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":     s.localAuthEnabled(r),
		"needs_setup": s.needsSetup(r),
	})
}

// handleLocalSetup creates the first administrator. Only possible while no
// local account exists and no IdP/dev auth is configured — after that the
// endpoint is a 404 so it can never be used to add an admin later.
func (s *Server) handleLocalSetup(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup(r) {
		WriteError(w, r, ErrNotFoundf("setup is already complete"))
		return
	}
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	u, err := s.Store.CreateLocalUser(r.Context(), in.Email, in.Name, in.Password, true)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	s.Logger.InfoContext(r.Context(), "first administrator created", "email", u.Email)
	_ = s.Store.AppendAudit(r.Context(), &store.AuditEntry{ActorUserID: u.ID, ActorLabel: u.Email, Action: "user_created", ResourceType: "user", ResourceID: u.ID,
		NewValue: encodeAuditValue(map[string]any{"email": u.Email, "role": store.RoleAdmin, "source": "local_setup"})})
	s.finishLocalLogin(w, r, u, "/dashboard")
}

func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	if !s.localAuthEnabled(r) {
		WriteError(w, r, ErrNotFoundf("local sign-in is not enabled"))
		return
	}
	if ok, _ := s.RateLimits.Allow("login:"+s.clientIP(r), loginRatePerMin, time.Now()); !ok {
		WriteError(w, r, newError(http.StatusTooManyRequests, "rate_limited", "auth_error", "Too many sign-in attempts from this address. Wait a minute and try again."))
		return
	}
	var in struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	u, _, err := s.Store.CheckPassword(r.Context(), in.Email, in.Password)
	switch {
	case errors.Is(err, store.ErrTOTPRequired):
		tok, perr := auth.NewRandomToken()
		if perr == nil {
			perr = s.Store.PutPendingLogin(r.Context(), tok, u.ID, time.Now().Add(pendingTTL))
		}
		if perr != nil {
			WriteError(w, r, ErrInternal())
			return
		}
		// 200, not 401: no session is issued, and the SPA's error path only
		// understands the error envelope. The body says what happens next.
		WriteJSON(w, http.StatusOK, map[string]any{"signed_in": false, "totp_required": true, "pending": tok})
		return
	case errors.Is(err, store.ErrLocked):
		s.Logger.WarnContext(r.Context(), "local sign-in locked", "email", strings.ToLower(in.Email), "ip", s.clientIP(r))
		WriteError(w, r, newError(http.StatusLocked, "locked", "auth_error", err.Error()))
		return
	case err != nil:
		// Not a local account (or wrong password): try the directory if one
		// is configured. It returns the same 401 wording on a miss.
		if s.tryDirectoryLogin(w, r, in.Email, in.Password, in.RedirectTo) {
			return
		}
		WriteError(w, r, newError(http.StatusUnauthorized, "bad_credentials", "auth_error", store.ErrBadCredentials.Error()))
		return
	}
	s.finishLocalLogin(w, r, u, in.RedirectTo)
}

func (s *Server) handleLocalTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Pending    string `json:"pending"`
		Code       string `json:"code"`
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	code := strings.ReplaceAll(strings.TrimSpace(in.Code), " ", "")
	userID, ok, terr := s.Store.TakePendingLogin(r.Context(), in.Pending, false)
	if terr != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	if !ok {
		WriteError(w, r, newError(http.StatusUnauthorized, "pending_expired", "auth_error", "Sign in again to get a new code prompt."))
		return
	}
	valid := false
	if secret, err := s.Store.TOTPSecret(r.Context(), s.Cipher, userID); err == nil && secret != "" {
		valid = totp.Validate(code, secret)
	}
	if !valid && len(code) >= 8 {
		// Recovery code path (8 chars, lower-case).
		valid, _ = s.Store.UseRecoveryCode(r.Context(), userID, code)
		if valid {
			s.Logger.WarnContext(r.Context(), "recovery code used", "user_id", userID)
			_ = s.Store.AppendAudit(r.Context(), &store.AuditEntry{ActorUserID: userID, ActorLabel: userID, Action: "user_recovery_code_used", ResourceType: "user", ResourceID: userID})
		}
	}
	if !valid {
		WriteError(w, r, newError(http.StatusUnauthorized, "bad_code", "auth_error", "That code is not valid."))
		return
	}
	_, _, _ = s.Store.TakePendingLogin(r.Context(), in.Pending, true)
	u, err := s.Store.UserByID(r.Context(), userID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.finishLocalLogin(w, r, u, in.RedirectTo)
}

// finishLocalLogin runs the shared sign-in contract (seats, bootstrap admin,
// audit, session) and answers JSON with where to go next. must_change makes
// the SPA route to the password page first.
func (s *Server) finishLocalLogin(w http.ResponseWriter, r *http.Request, u *store.User, redirectTo string) {
	identity := &auth.Identity{Subject: u.AuthID, Email: u.Email, EmailVerified: true, Name: u.Name}
	// completeSignIn redirects; for the JSON API we want the outcome instead,
	// so we run it against a recorder and translate.
	rec := &redirectRecorder{header: http.Header{}}
	s.completeSignIn(rec, r, identity, safeRedirect(redirectTo))
	for _, c := range rec.cookies {
		http.SetCookie(w, c)
	}
	if loc := rec.header.Get("Location"); strings.HasPrefix(loc, "/auth/login?error=") {
		msg := loc[len("/auth/login?error="):]
		WriteError(w, r, newError(http.StatusForbidden, "sign_in_refused", "auth_error", unescapeQuery(msg)))
		return
	}
	next := safeRedirect(redirectTo)
	if cred, err := s.Store.LocalCredential(r.Context(), u.ID); err == nil && cred.MustChange {
		next = "/settings?password=change"
	}
	WriteJSON(w, http.StatusOK, map[string]any{"signed_in": true, "redirect_to": next})
}

// redirectRecorder captures the cookies and Location completeSignIn sets so
// the JSON endpoints can reuse it verbatim.
type redirectRecorder struct {
	header  http.Header
	cookies []*http.Cookie
	status  int
}

func (r *redirectRecorder) Header() http.Header { return r.header }
func (r *redirectRecorder) Write(b []byte) (int, error) {
	return len(b), nil
}
func (r *redirectRecorder) WriteHeader(code int) {
	r.status = code
	// Set-Cookie headers were written into r.header by http.SetCookie.
	resp := http.Response{Header: r.header}
	r.cookies = resp.Cookies()
}

// --- signed-in user: password + TOTP management -----------------------------

func (s *Server) mountLocalAccountRoutes(pr chi.Router) {
	pr.Get("/me/local", s.handleMeLocal)
	pr.Put("/me/local/password", s.handleMeSetPassword)
	pr.Post("/me/local/totp/start", s.handleMeTOTPStart)
	pr.Post("/me/local/totp/confirm", s.handleMeTOTPConfirm)
	pr.Post("/me/local/totp/disable", s.handleMeTOTPDisable)
	pr.Get("/me/local/totp/qr.png", s.handleMeTOTPQR)
}

// handleMeTOTPQR renders the staged (unconfirmed) secret as a QR PNG. The
// secret never appears in a URL: the image is built server-side from the
// caller's own staged enrollment.
func (s *Server) handleMeTOTPQR(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	cred, err := s.Store.LocalCredential(r.Context(), u.ID)
	if err != nil || cred.TOTPEnabled {
		WriteError(w, r, ErrNotFoundf("pending enrollment"))
		return
	}
	secret, err := s.Store.TOTPSecret(r.Context(), s.Cipher, u.ID)
	if err != nil || secret == "" {
		WriteError(w, r, ErrNotFoundf("pending enrollment"))
		return
	}
	key, err := otp.NewKeyFromURL("otpauth://totp/" + url.PathEscape(totpIssuer+":"+u.Email) + "?secret=" + secret + "&issuer=" + url.QueryEscape(totpIssuer) + "&algorithm=SHA1&digits=6&period=30")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	img, err := key.Image(200, 200)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}

func (s *Server) handleMeLocal(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	cred, err := s.Store.LocalCredential(r.Context(), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		WriteJSON(w, http.StatusOK, map[string]any{"has_password": false, "totp_enabled": false, "must_change": false, "recovery_codes_left": 0})
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"has_password": true, "totp_enabled": cred.TOTPEnabled, "must_change": cred.MustChange,
		"recovery_codes_left": cred.RecoveryCodesLeft(),
	})
}

// handleMeSetPassword changes (or adds) the caller's password. The current
// password is required when one exists, unless must_change is set (the admin
// gave a temporary one and the user is being forced through this screen).
func (s *Server) handleMeSetPassword(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	cred, err := s.Store.LocalCredential(r.Context(), u.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, err)
		return
	}
	if cred != nil && !cred.MustChange {
		if _, _, cerr := s.Store.CheckPassword(r.Context(), u.Email, in.Current); cerr != nil && !errors.Is(cerr, store.ErrTOTPRequired) {
			WriteError(w, r, newError(http.StatusForbidden, "bad_credentials", "auth_error", "Current password is incorrect."))
			return
		}
	}
	if err := s.Store.SetPassword(r.Context(), u.ID, in.New, false); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	s.audit(r, "user_password_changed", "user", u.ID, nil, nil)
	// Other sessions are revoked: a password change is the standard response
	// to a suspected compromise.
	_ = s.Sessions.DeleteForUser(r.Context(), u.ID)
	if session, err := s.Sessions.Create(r.Context(), u.ID); err == nil {
		auth.SetSessionCookies(w, session, s.Config.CookieSecure, s.Config.SessionTTL)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"changed": true})
}

func (s *Server) handleMeTOTPStart(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	if _, err := s.Store.LocalCredential(r.Context(), u.ID); err != nil {
		WriteError(w, r, ErrInvalidRequest("set a password before enabling two-factor authentication"))
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: u.Email, Algorithm: otp.AlgorithmSHA1, Digits: otp.DigitsSix})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.StageTOTP(r.Context(), s.Cipher, u.ID, key.Secret()); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"secret": key.Secret(), "otpauth_url": key.URL(), "issuer": totpIssuer, "account": u.Email})
}

func (s *Server) handleMeTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	secret, err := s.Store.TOTPSecret(r.Context(), s.Cipher, u.ID)
	if err != nil || secret == "" {
		WriteError(w, r, ErrInvalidRequest("start enrollment first"))
		return
	}
	if !totp.Validate(strings.ReplaceAll(strings.TrimSpace(in.Code), " ", ""), secret) {
		WriteError(w, r, newError(http.StatusBadRequest, "bad_code", "auth_error", "That code is not valid. Check the time on your device and try the next code."))
		return
	}
	codes, err := s.Store.ConfirmTOTP(r.Context(), u.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "user_totp_enabled", "user", u.ID, nil, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": true, "recovery_codes": codes})
}

func (s *Server) handleMeTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	if _, _, err := s.Store.CheckPassword(r.Context(), u.Email, in.Password); err != nil && !errors.Is(err, store.ErrTOTPRequired) {
		WriteError(w, r, newError(http.StatusForbidden, "bad_credentials", "auth_error", "Password is incorrect."))
		return
	}
	if err := s.Store.DisableTOTP(r.Context(), u.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "user_totp_disabled", "user", u.ID, nil, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

// --- admin: create local users, reset passwords ------------------------------

func (s *Server) mountAdminLocalAuthRoutes(ar chi.Router) {
	ar.Post("/admin/users/local", s.gateCreate("user", s.handleAdminCreateLocalUser))
	ar.Post("/admin/users/{id}/password", s.handleAdminResetPassword)
	ar.Delete("/admin/users/{id}/totp", s.handleAdminResetTOTP)

	// Admin-configured OIDC providers. Listing is always allowed (the card
	// shows the upsell); everything that changes state needs multi_oidc.
	ar.Get("/admin/identity-providers", s.handleAdminListIdentityProviders)
	ar.Post("/admin/identity-providers", s.gateFeature("multi_oidc", s.handleAdminCreateIdentityProvider))
	ar.Post("/admin/identity-providers/test", s.gateFeature("multi_oidc", s.handleAdminTestIdentityProvider))
	ar.Put("/admin/identity-providers/{id}", s.gateFeature("multi_oidc", s.handleAdminUpdateIdentityProvider))
	ar.Delete("/admin/identity-providers/{id}", s.gateFeature("multi_oidc", s.handleAdminDeleteIdentityProvider))

	// LDAP directory (Business: ldap). GET always answers so the card can upsell.
	ar.Get("/admin/directory", s.handleAdminGetDirectory)
	ar.Put("/admin/directory", s.gateFeature("ldap", s.handleAdminPutDirectory))
	ar.Delete("/admin/directory", s.gateFeature("ldap", s.handleAdminDeleteDirectory))
	ar.Post("/admin/directory/test", s.gateFeature("ldap", s.handleAdminTestDirectory))
}

func (s *Server) handleAdminCreateLocalUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	if err := s.checkSeatAvailable(r.Context()); err != nil {
		WriteError(w, r, newError(http.StatusPaymentRequired, "license.seats_exhausted", "license_error", err.Error()))
		return
	}
	u, err := s.Store.CreateLocalUser(r.Context(), in.Email, in.Name, in.Password, in.Admin)
	if err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	// A password typed by an admin is temporary by definition.
	_ = s.Store.SetPassword(r.Context(), u.ID, in.Password, true)
	s.audit(r, "user_created", "user", u.ID, nil, map[string]any{"email": u.Email, "role": u.Role, "source": "local_admin"})
	WriteJSON(w, http.StatusCreated, u)
}

func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	if _, err := s.Store.UserByID(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.SetPassword(r.Context(), id, in.Password, true); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	_ = s.Sessions.DeleteForUser(r.Context(), id)
	s.audit(r, "user_password_reset", "user", id, nil, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"reset": true, "must_change": true})
}

func (s *Server) handleAdminResetTOTP(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DisableTOTP(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "user_totp_disabled", "user", id, nil, map[string]any{"by": "admin"})
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": false})
}
