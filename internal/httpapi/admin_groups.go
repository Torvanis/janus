package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// --- Admin groups -------------------------------------------------------------
//
// Admin capability composes from three independent, OR'd sources, layered so
// that withdrawing one never collapses the others:
//
//  1. JANUS_BOOTSTRAP_ADMIN_EMAILS — environment, the failsafe. It cannot be
//     revoked from the UI and survives any database change.
//  2. An explicit per-user grant (the role column), made on Admin → People.
//  3. Membership of an admin group — JANUS_ADMIN_GROUPS from the environment
//     unioned with the groups managed here.
//
// Removing a group therefore does not demote a user who also holds an explicit
// grant, and revoking an explicit grant does not demote a bootstrap admin.
// Group-granted access (admin_via_group) is taken away as soon as a user's
// groups no longer include an admin group — see reevaluateGroupAdmins.

func (s *Server) handleListAdminGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Store.ListAdminGroups(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The environment list is reported alongside so the UI can show the full
	// effective set and mark the env entries as not editable here — otherwise
	// an admin sees a group granting access with no explanation of where it
	// came from, and no way to act on it.
	WriteJSON(w, http.StatusOK, map[string]any{
		"admin_groups":          groups,
		"env_admin_groups":      s.Config.AdminGroups,
		"bootstrap_admin_count": len(s.Config.BootstrapAdminEmails),
	})
}

func (s *Server) handleAddAdminGroup(w http.ResponseWriter, r *http.Request) {
	actor := UserFrom(r.Context())
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	group, err := s.Store.AddAdminGroup(r.Context(), body.Name, actorID(actor))
	if err != nil {
		var vErr *store.ValidationError
		if errors.As(err, &vErr) {
			WriteError(w, r, ErrInvalidRequest(vErr.Message).WithParam(vErr.Field))
			return
		}
		WriteError(w, r, err)
		return
	}
	s.audit(r, "admin_group_added", "admin_group", group.Name, nil, map[string]any{"name": group.Name})
	WriteJSON(w, http.StatusCreated, map[string]any{"admin_group": group})
}

func (s *Server) handleRemoveAdminGroup(w http.ResponseWriter, r *http.Request) {
	name, err := url.PathUnescape(chi.URLParam(r, "name"))
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("That group name could not be decoded.").WithParam("name"))
		return
	}
	if err := s.Store.RemoveAdminGroup(r.Context(), name); err != nil {
		WriteError(w, r, err)
		return
	}
	// Explicit and bootstrap grants are untouched (see store.RemoveAdminGroup);
	// people who were administrators only through this group lose it now.
	revoked := s.reevaluateGroupAdmins(r.Context())
	s.audit(r, "admin_group_removed", "admin_group", name, map[string]any{"name": name}, map[string]any{
		"name": name, "admins_revoked": len(revoked),
	})
	WriteJSON(w, http.StatusOK, map[string]any{"removed": name, "admins_revoked": len(revoked)})
}

// reevaluateGroupAdmins takes group-granted administrator access away from
// everyone whose current groups no longer include an administrator group
// (environment, Administration-managed, any identity provider's or the
// directory's admin groups). It runs whenever admin groups or group
// membership change, so removal applies at once instead of at the next
// sign-in. Granting still happens only at sign-in. A failure is logged, not
// returned: the change that triggered it already succeeded.
func (s *Server) reevaluateGroupAdmins(ctx context.Context) []string {
	set := map[string]struct{}{}
	add := func(names []string) {
		for _, n := range names {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				set[n] = struct{}{}
			}
		}
	}
	add(s.Config.AdminGroups)
	stored, err := s.Store.AdminGroupNames(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "re-evaluate admin groups: read admin groups", "error", err.Error())
		return nil
	}
	for n := range stored {
		set[n] = struct{}{}
	}
	providers, err := s.Store.ListIdentityProviders(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "re-evaluate admin groups: read identity providers", "error", err.Error())
		return nil
	}
	for _, p := range providers {
		add(p.AdminGroups)
	}
	if d, err := s.Store.Directory(ctx); err == nil {
		add(d.AdminGroups)
	} else if !errors.Is(err, store.ErrNoDirectory) {
		s.Logger.WarnContext(ctx, "re-evaluate admin groups: read directory", "error", err.Error())
		return nil
	}
	revoked, err := s.Store.RevokeStaleAdminViaGroup(ctx, set)
	if err != nil {
		s.Logger.WarnContext(ctx, "re-evaluate admin groups", "error", err.Error())
	}
	for _, id := range revoked {
		if err := s.Store.AppendAudit(ctx, &store.AuditEntry{
			ActorUserID: auditSystemActor, ActorLabel: auditSystemActor,
			Action: "user_role_changed", ResourceType: "user", ResourceID: id,
			OldValue: encodeAuditValue(map[string]any{"admin_via_group": true}),
			NewValue: encodeAuditValue(map[string]any{"admin_via_group": false, "reason": auditReasonAdminGroup}),
		}); err != nil {
			s.Logger.WarnContext(ctx, "audit admin group revocation", "error", err.Error())
		}
	}
	if len(revoked) > 0 {
		s.InvalidateTokenCache()
	}
	return revoked
}
