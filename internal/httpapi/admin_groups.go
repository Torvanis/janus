package httpapi

import (
	"errors"
	"net/http"
	"net/url"

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
	// Deliberately no demotion: see store.RemoveAdminGroup. The audit entry
	// records that fact so a reviewer is not left wondering why existing
	// admins kept their access.
	s.audit(r, "admin_group_removed", "admin_group", name, map[string]any{"name": name}, map[string]any{
		"name": name, "existing_admins_retained": true,
	})
	WriteJSON(w, http.StatusOK, map[string]any{"removed": name})
}
