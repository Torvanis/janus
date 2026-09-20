package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// LDAP directory sign-in (Business: ldap). The login form is the same one
// local accounts use: the password is tried against the local credential
// first, then against the directory. Directory users are provisioned
// just-in-time with auth_provider_id "ldap:<dn>" and go through
// completeSignInWith so the directory's admin groups apply.

func (s *Server) directoryConfig(r *http.Request) (*store.Directory, auth.LDAPConfig, bool) {
	if s.requireFeature("ldap") != nil {
		return nil, auth.LDAPConfig{}, false
	}
	d, err := s.Store.Directory(r.Context())
	if err != nil || !d.Enabled {
		return nil, auth.LDAPConfig{}, false
	}
	pw, err := d.BindPassword(s.Cipher)
	if err != nil {
		s.Logger.WarnContext(r.Context(), "directory bind password unreadable", "error", err.Error())
		return nil, auth.LDAPConfig{}, false
	}
	return d, toLDAPConfig(d, pw), true
}

func toLDAPConfig(d *store.Directory, bindPassword string) auth.LDAPConfig {
	return auth.LDAPConfig{
		URL: d.URL, StartTLS: d.StartTLS, SkipVerify: d.SkipVerify, BindDN: d.BindDN, BindPassword: bindPassword,
		BaseDN: d.BaseDN, UserFilter: d.UserFilter, EmailAttr: d.EmailAttr, NameAttr: d.NameAttr,
		GroupsAttr: d.GroupsAttr, GroupFilter: d.GroupFilter, GroupNameAttr: d.GroupNameAttr, Timeout: 8 * time.Second,
	}
}

// tryDirectoryLogin is called by handleLocalLogin after the local check
// failed with bad credentials. Returns handled=true when it wrote a response.
func (s *Server) tryDirectoryLogin(w http.ResponseWriter, r *http.Request, login, password, redirectTo string) bool {
	d, cfg, ok := s.directoryConfig(r)
	if !ok {
		return false
	}
	lu, err := cfg.Authenticate(r.Context(), login, password)
	switch {
	case errors.Is(err, auth.ErrLDAPBadCredentials):
		return false // fall through to the generic 401
	case errors.Is(err, auth.ErrLDAPUnavailable):
		s.Logger.ErrorContext(r.Context(), "directory unavailable", "error", err.Error())
		WriteError(w, r, newError(http.StatusServiceUnavailable, "directory_unavailable", "auth_error", "The directory could not be reached. Try again, or sign in with a Janus account."))
		return true
	case err != nil:
		s.Logger.ErrorContext(r.Context(), "directory sign-in failed", "error", err.Error())
		WriteError(w, r, ErrInternal())
		return true
	}
	if lu.Email == "" {
		WriteError(w, r, newError(http.StatusForbidden, "sign_in_refused", "auth_error", "Your directory account has no email address; ask an administrator to set one."))
		return true
	}
	identity := &auth.Identity{Subject: "ldap:" + strings.ToLower(lu.DN), Email: lu.Email, EmailVerified: true, Name: lu.Name, Groups: lu.Groups}
	rec := &redirectRecorder{header: http.Header{}}
	s.completeSignInWith(rec, r, identity, safeRedirect(redirectTo), d.AdminGroups)
	for _, c := range rec.cookies {
		http.SetCookie(w, c)
	}
	if loc := rec.header.Get("Location"); strings.HasPrefix(loc, "/auth/login?error=") {
		WriteError(w, r, newError(http.StatusForbidden, "sign_in_refused", "auth_error", unescapeQuery(loc[len("/auth/login?error="):])))
		return true
	}
	WriteJSON(w, http.StatusOK, map[string]any{"signed_in": true, "redirect_to": safeRedirect(redirectTo)})
	return true
}

// ---- admin ----

type directoryBody struct {
	URL           string   `json:"url"`
	StartTLS      bool     `json:"start_tls"`
	SkipVerify    bool     `json:"skip_verify"`
	BindDN        string   `json:"bind_dn"`
	BindPassword  string   `json:"bind_password"`
	BaseDN        string   `json:"base_dn"`
	UserFilter    string   `json:"user_filter"`
	EmailAttr     string   `json:"email_attr"`
	NameAttr      string   `json:"name_attr"`
	GroupsAttr    string   `json:"groups_attr"`
	GroupFilter   string   `json:"group_filter"`
	GroupNameAttr string   `json:"group_name_attr"`
	AdminGroups   []string `json:"admin_groups"`
	Enabled       *bool    `json:"enabled"`
}

func (b directoryBody) validate() (store.Directory, *APIError) {
	u, err := url.Parse(strings.TrimSpace(b.URL))
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
		return store.Directory{}, ErrInvalidRequest("url must be ldap://host:389 or ldaps://host:636")
	}
	if strings.TrimSpace(b.BaseDN) == "" {
		return store.Directory{}, ErrInvalidRequest("base_dn is required")
	}
	if f := strings.TrimSpace(b.UserFilter); f != "" && !strings.Contains(f, "{login}") {
		return store.Directory{}, ErrInvalidRequest("user_filter must contain {login}")
	}
	if b.GroupsAttr == "" && strings.TrimSpace(b.GroupFilter) != "" && !strings.Contains(b.GroupFilter, "{dn}") {
		return store.Directory{}, ErrInvalidRequest("group_filter must contain {dn}")
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	return store.Directory{
		URL: u.String(), StartTLS: b.StartTLS, SkipVerify: b.SkipVerify, BindDN: strings.TrimSpace(b.BindDN), BaseDN: strings.TrimSpace(b.BaseDN),
		UserFilter: strings.TrimSpace(b.UserFilter), EmailAttr: strings.TrimSpace(b.EmailAttr), NameAttr: strings.TrimSpace(b.NameAttr),
		GroupsAttr: strings.TrimSpace(b.GroupsAttr), GroupFilter: strings.TrimSpace(b.GroupFilter), GroupNameAttr: strings.TrimSpace(b.GroupNameAttr),
		AdminGroups: b.AdminGroups, Enabled: enabled,
	}, nil
}

func directoryView(d *store.Directory) map[string]any {
	return map[string]any{
		"url": d.URL, "start_tls": d.StartTLS, "skip_verify": d.SkipVerify, "bind_dn": d.BindDN, "has_bind_password": d.BindPasswordEnc != "",
		"base_dn": d.BaseDN, "user_filter": d.UserFilter, "email_attr": d.EmailAttr, "name_attr": d.NameAttr,
		"groups_attr": d.GroupsAttr, "group_filter": d.GroupFilter, "group_name_attr": d.GroupNameAttr,
		"admin_groups": d.AdminGroups, "enabled": d.Enabled, "updated_at": d.UpdatedAt,
	}
}

func (s *Server) handleAdminGetDirectory(w http.ResponseWriter, r *http.Request) {
	licensed := s.requireFeature("ldap") == nil
	d, err := s.Store.Directory(r.Context())
	if errors.Is(err, store.ErrNoDirectory) {
		WriteJSON(w, http.StatusOK, map[string]any{"directory": nil, "licensed": licensed})
		return
	}
	if err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"directory": directoryView(d), "licensed": licensed})
}

func (s *Server) handleAdminPutDirectory(w http.ResponseWriter, r *http.Request) {
	var body directoryBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	d, apiErr := body.validate()
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	prev, _ := s.Store.Directory(r.Context())
	saved, err := s.Store.SaveDirectory(r.Context(), s.Cipher, d, body.BindPassword)
	if err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	var before any
	if prev != nil {
		before = map[string]any{"url": prev.URL, "enabled": prev.Enabled}
	}
	s.audit(r, "directory.update", "directory", "ldap", before, map[string]any{"url": saved.URL, "enabled": saved.Enabled})
	WriteJSON(w, http.StatusOK, directoryView(saved))
}

func (s *Server) handleAdminDeleteDirectory(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Directory(r.Context()); errors.Is(err, store.ErrNoDirectory) {
		WriteError(w, r, ErrNotFoundf("directory"))
		return
	}
	if err := s.Store.DeleteDirectory(r.Context()); err != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	s.audit(r, "directory.delete", "directory", "ldap", nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminTestDirectory binds with the candidate settings and, if a
// login is given, resolves that user (DN, email, groups) — no password
// needed, so the admin can verify filters and group mapping before saving.
// A blank bind_password reuses the stored one, like PUT.
func (s *Server) handleAdminTestDirectory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		directoryBody
		Login string `json:"login"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteError(w, r, ErrInvalidRequest("invalid JSON body"))
		return
	}
	d, apiErr := body.directoryBody.validate()
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	pw := body.BindPassword
	if pw == "" {
		if prev, err := s.Store.Directory(r.Context()); err == nil {
			pw, _ = prev.BindPassword(s.Cipher)
		}
	}
	u, err := toLDAPConfig(&d, pw).Lookup(r.Context(), body.Login)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, auth.ErrLDAPBadCredentials) {
			msg = "bind succeeded, but no single user matched user_filter for that login"
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
		return
	}
	out := map[string]any{"ok": true}
	if body.Login != "" {
		admin := groupsIntersect(u.Groups, d.AdminGroups)
		out["user"] = map[string]any{"dn": u.DN, "email": u.Email, "name": u.Name, "groups": u.Groups, "would_be_admin": admin}
	}
	WriteJSON(w, http.StatusOK, out)
}
