package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// auditSystemActor labels audit entries produced by the gateway itself rather
// than a signed-in principal (for example group-driven role changes applied
// during sign-in).
const auditSystemActor = "system"

// auditReasonAdminGroup is the recorded cause for role changes driven by
// JANUS_ADMIN_GROUPS membership evaluation at sign-in.
const auditReasonAdminGroup = "admin_group_membership"

// handleLogin starts the sign-in flow. With an identity provider configured it
// redirects to the provider; in evaluation mode it signs the caller in locally.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	redirectTo := safeRedirect(r.URL.Query().Get("redirect_uri"))

	if s.Config.DevAuthEnabled {
		email := strings.TrimSpace(r.URL.Query().Get("email"))
		if email == "" {
			http.Redirect(w, r, "/auth/signed-out?reason=missing_email", http.StatusFound)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			name = deriveName(email)
		}
		groups := []string{}
		if g := r.URL.Query().Get("groups"); g != "" {
			groups = strings.Split(g, ",")
		}
		s.completeSignIn(w, r, auth.DevIdentity(email, name, groups), redirectTo)
		return
	}

	if s.OIDC == nil {
		s.renderLoginError(w, r, "Sign-in is not configured. Set JANUS_OIDC_PROVIDER_URL, JANUS_OIDC_CLIENT_ID, and JANUS_OIDC_CLIENT_SECRET.")
		return
	}
	target, err := s.OIDC.AuthCodeURL(r.Context(), redirectTo)
	if err != nil {
		s.renderLoginError(w, r, "Sign-in could not be started. Try again.")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleCallback completes the OIDC exchange and establishes a session.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		s.renderLoginError(w, r, "Sign-in is not configured.")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		detail := r.URL.Query().Get("error_description")
		if detail == "" {
			detail = errParam
		}
		s.renderLoginError(w, r, "Your identity provider refused the sign-in: "+detail)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		s.renderLoginError(w, r, "The sign-in response was incomplete. Start again from the sign-in page.")
		return
	}
	identity, redirectTo, err := s.OIDC.Exchange(r.Context(), code, state)
	if err != nil {
		s.Logger.WarnContext(r.Context(), "oidc exchange failed", "error", err.Error())
		s.renderLoginError(w, r, err.Error())
		return
	}
	s.completeSignIn(w, r, identity, redirectTo)
}

// completeSignIn applies the first-login/every-login contract and starts a session.
func (s *Server) completeSignIn(w http.ResponseWriter, r *http.Request, identity *auth.Identity, redirectTo string) {
	s.completeSignInWith(w, r, identity, redirectTo, nil)
}

// completeSignInWith is completeSignIn plus per-provider admin groups (from
// an admin-configured OIDC provider), OR-ed with the global ones.
func (s *Server) completeSignInWith(w http.ResponseWriter, r *http.Request, identity *auth.Identity, redirectTo string, providerAdminGroups []string) {
	ctx := r.Context()
	// Administrator capability composes from three independent sources (OR):
	// the bootstrap email list (sticky role promotion), an explicit grant from
	// the admin UI (sticky role), and IdP admin-group membership (re-evaluated
	// on every sign-in). The dev-auth `groups` query parameter flows through
	// identity.Groups, so evaluation deployments follow the same rules.
	bootstrap := s.isBootstrapAdmin(identity.Email)
	groupAdmin := s.isAdminGroupMember(ctx, identity.Groups) || groupsIntersect(identity.Groups, providerAdminGroups)

	// Preserve existing immutable subject bindings. Only a genuinely unknown
	// subject may claim a directory identity before just-in-time provisioning.
	before, err := s.Store.UserByAuthID(ctx, identity.Subject)
	if errors.Is(err, store.ErrNotFound) {
		before, err = s.Store.ClaimSCIMIdentity(ctx, identity.Subject, identity.Email, identity.Name, identity.EmailVerified)
		if errors.Is(err, store.ErrNotFound) {
			err = nil // No directory match: ordinary first-login provisioning.
		}
	}
	if err != nil {
		s.Logger.ErrorContext(ctx, "link directory identity on sign-in", "error", err.Error())
		s.renderLoginError(w, r, "Your account could not be linked to your directory identity. Contact a Janus administrator to check your directory account and verified email, then try again.")
		return
	}
	if before != nil && !before.IsActive {
		s.renderLoginError(w, r, "Your account has been disabled. Contact a Janus administrator to restore access.")
		return
	}

	// Seat limit (RULING): a NEW user cannot complete first sign-in when the
	// licensed seat count is already used; existing users are never affected.
	// Admins always get in so the person who can fix the license can sign in.
	if before == nil && !bootstrap && !groupAdmin {
		if err := s.checkSeatAvailable(ctx); err != nil {
			s.Logger.WarnContext(ctx, "seat limit reached; first sign-in refused", "email", identity.Email)
			s.renderLoginError(w, r, err.Error())
			return
		}
	}

	user, created, err := s.Store.UpsertUserFromIdentity(ctx, identity.Subject, identity.Email, identity.Name, bootstrap, groupAdmin)
	if err != nil {
		s.Logger.ErrorContext(ctx, "provision user on sign-in", "error", err.Error())
		s.renderLoginError(w, r, "Your account could not be prepared. Contact a Janus administrator.")
		return
	}
	if !user.IsActive {
		s.renderLoginError(w, r, "Your account has been disabled. Contact a Janus administrator to restore access.")
		return
	}
	// IdP groups are replaced wholesale on every sign-in; in-app groups are left
	// untouched so admin-curated membership survives.
	if err := s.Store.ReplaceIDPGroupsForUser(ctx, user.ID, identity.Groups); err != nil {
		s.Logger.ErrorContext(ctx, "synchronise identity-provider groups", "error", err.Error())
		s.renderLoginError(w, r, "Your directory groups could not be synchronised. Try signing in again; if this continues, contact a Janus administrator.")
		return
	}
	switch {
	case created:
		// First login: one entry records the account and the capability it
		// arrived with, including a first-login group promotion.
		if err := s.Store.AppendAudit(ctx, &store.AuditEntry{
			ActorUserID: user.ID, ActorLabel: user.Email, Action: "user_created",
			ResourceType: "user", ResourceID: user.ID,
			NewValue: encodeAuditValue(map[string]any{
				"email": user.Email, "role": effectiveRole(user), "admin_via_group": user.AdminViaGroup,
			}),
		}); err != nil {
			s.Logger.ErrorContext(ctx, "audit user creation", "error", err.Error())
		}
	case before != nil && before.AdminViaGroup != user.AdminViaGroup && before.IsAdmin() != user.IsAdmin():
		// The group signal flipped AND it changed what the user can actually
		// do (an explicit role=admin grant or a bootstrap promotion absorbs
		// the flip without any capability change — no entry then).
		if err := s.Store.AppendAudit(ctx, &store.AuditEntry{
			ActorUserID: auditSystemActor, ActorLabel: auditSystemActor,
			Action: "user_role_changed", ResourceType: "user", ResourceID: user.ID,
			OldValue: encodeAuditValue(map[string]any{"role": effectiveRole(before)}),
			NewValue: encodeAuditValue(map[string]any{"role": effectiveRole(user), "reason": auditReasonAdminGroup}),
		}); err != nil {
			s.Logger.ErrorContext(ctx, "audit group-driven role change", "error", err.Error())
		}
	}
	session, err := s.Sessions.Create(ctx, user.ID)
	if err != nil {
		s.renderLoginError(w, r, "Your session could not be created. Try again.")
		return
	}
	auth.SetSessionCookies(w, session, s.Config.CookieSecure, s.Config.SessionTTL)
	http.Redirect(w, r, safeRedirect(redirectTo), http.StatusFound)
}

func (s *Server) isBootstrapAdmin(email string) bool {
	target := strings.ToLower(strings.TrimSpace(email))
	for _, candidate := range s.Config.BootstrapAdminEmails {
		if candidate == target {
			return true
		}
	}
	return false
}

// isAdminGroupMember reports whether any of the identity's groups grants admin.
//
// Two sources are unioned: JANUS_ADMIN_GROUPS (environment, the failsafe path
// that survives any database change) and the admin_group table (managed from
// the UI by an administrator, so a group can be added without a redeploy).
// Config.AdminGroups is trimmed and lowercased at load time and stored group
// names are lowercased on write, so the comparison is case-insensitive on both
// sides. Both empty means group-based promotion is inert and sign-in behaves
// exactly as it did before the mechanism existed.
//
// A store failure is treated as "no group match" rather than failing the
// sign-in: losing the ability to read one optional promotion source must not
// lock every user out. The bootstrap list and explicit per-user grants are
// unaffected either way.
func (s *Server) isAdminGroupMember(ctx context.Context, groups []string) bool {
	if len(groups) == 0 {
		return false
	}
	stored, err := s.Store.AdminGroupNames(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "read admin groups", "error", err.Error())
		stored = nil
	}
	if len(s.Config.AdminGroups) == 0 && len(stored) == 0 {
		return false
	}
	for _, group := range groups {
		name := strings.ToLower(strings.TrimSpace(group))
		if slices.Contains(s.Config.AdminGroups, name) {
			return true
		}
		if _, ok := stored[name]; ok {
			return true
		}
	}
	return false
}

// effectiveRole is the capability the rest of the gateway enforces: the stored
// role, elevated to admin when the sign-in group signal grants it (see
// store.User.IsAdmin). Audit entries record this effective value so a reviewer
// sees what the user could actually do, not just the sticky role column.
func effectiveRole(u *store.User) string {
	if u.IsAdmin() {
		return store.RoleAdmin
	}
	return u.Role
}

// handleLogout ends the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if session := SessionFrom(r.Context()); session != nil {
		if err := s.Sessions.Delete(r.Context(), session.ID); err != nil {
			s.Logger.WarnContext(r.Context(), "delete session", "error", err.Error())
		}
	}
	auth.ClearSessionCookies(w, s.Config.CookieSecure)
	WriteJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// handleAuthHealth reports identity-provider reachability for the sign-in page
// and readiness probes.
func (s *Server) handleAuthHealth(w http.ResponseWriter, r *http.Request) {
	local := s.localAuthEnabled(r)
	setup := s.needsSetup(r)
	if s.Config.DevAuthEnabled {
		WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "provider": "Local evaluation sign-in", "dev_auth": true, "local_auth": local, "needs_setup": false,
		})
		return
	}
	if s.OIDC == nil {
		if local || setup {
			// No IdP, but local accounts (or first-run setup) work: healthy.
			WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "provider": "Janus accounts", "dev_auth": false, "local_auth": true, "needs_setup": setup})
			return
		}
		WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "provider": s.Config.OIDCProviderURL,
			"detail": "The identity provider is not configured.",
		})
		return
	}
	if !s.OIDC.Healthy(r.Context()) {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "provider": s.providerLabel(),
			"detail": "The identity provider is not responding.", "local_auth": local, "needs_setup": false,
		})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "provider": s.providerLabel(), "dev_auth": false, "local_auth": local, "needs_setup": false})
}

// renderLoginError sends the browser back to the sign-in page carrying a
// human-readable reason. No stack traces or provider internals are exposed.
func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/auth/login?error="+url.QueryEscape(message), http.StatusFound)
}

// safeRedirect keeps post-login redirects inside this origin so the sign-in flow
// cannot be used as an open redirect.
func safeRedirect(target string) string {
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return "/dashboard"
	}
	return target
}

func deriveName(email string) string {
	local := email
	if i := strings.Index(email, "@"); i > 0 {
		local = email[:i]
	}
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(local)
	words := strings.Fields(local)
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	if len(words) == 0 {
		return email
	}
	return strings.Join(words, " ")
}

// principalContext returns the caller's identity plus their group and team ids,
// which nearly every authorisation decision needs.
func (s *Server) principalContext(ctx context.Context, user *store.User) (groupIDs []string, teamIDs []string, err error) {
	groupIDs, err = s.Store.GroupIDsForUser(ctx, user.ID)
	if err != nil {
		return nil, nil, err
	}
	teamID, err := s.selectedTeamID(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	if teamID != "" {
		if err := s.Store.ValidateTeamMember(ctx, teamID, user.ID); err != nil {
			return nil, nil, teamContextError(err)
		}
		teamIDs = []string{teamID}
	}
	return groupIDs, teamIDs, nil
}

func groupsIntersect(have, want []string) bool {
	for _, g := range have {
		for _, w := range want {
			if strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(w)) {
				return true
			}
		}
	}
	return false
}
