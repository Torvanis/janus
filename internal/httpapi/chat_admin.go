package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Janus Chat administration (Admin → Chat) and the per-user gate.
//
// Editions (user ruling): Chat administration is Community; restricting WHO
// may use Chat (groups / teams / people) is Business. It is gated on the
// EDITION (like performance mode), not on a new key feature, so every
// existing Business key carries it without reissuing. An expired or missing
// Business key never locks people out: a saved restriction simply stops
// applying (fail open to "everyone"), matching the "expiry never disrupts
// work" rule — turning Chat OFF for the instance is always honoured.

// chatSettingsCache keeps one read of the admin settings per few seconds:
// every chat request consults them.
type chatSettingsCache struct {
	at  time.Time
	val store.ChatAdminSettings
}

const chatSettingsTTL = 5 * time.Second

func (s *Server) chatAdmin(ctx context.Context) store.ChatAdminSettings {
	s.chatAdminMu.Lock()
	c := s.chatAdminCache
	s.chatAdminMu.Unlock()
	if c != nil && time.Since(c.at) < chatSettingsTTL {
		return c.val
	}
	v, err := s.Store.ChatAdminSettings(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "read chat settings", "error", err.Error())
		if c != nil {
			return c.val
		}
		return store.DefaultChatAdminSettings()
	}
	s.chatAdminMu.Lock()
	s.chatAdminCache = &chatSettingsCache{at: time.Now(), val: v}
	s.chatAdminMu.Unlock()
	return v
}

func (s *Server) invalidateChatAdmin() {
	s.chatAdminMu.Lock()
	s.chatAdminCache = nil
	s.chatAdminMu.Unlock()
}

// chatAccessLicensed reports whether the edition includes Chat access
// restrictions (Business or above, even past its grace period). It gates
// SAVING a new or changed restriction only: a saved restriction always
// applies, so neither expiry nor a downgrade opens Chat to everyone.
func (s *Server) chatAccessLicensed() bool {
	st := s.licenseState()
	return st.Edition == license.EditionBusiness || st.Edition == license.EditionEnterprise
}

// chatAllowedFor reports whether user may use Chat, and the message to show
// when not. Admins always may (they configure it).
func (s *Server) chatAllowedFor(ctx context.Context, user *store.User) (bool, string) {
	if !s.chatEnabled() || user == nil {
		return false, ""
	}
	cfg := s.chatAdmin(ctx)
	if !cfg.Enabled {
		return false, "Janus Chat is turned off for this instance."
	}
	if cfg.Access.Mode != store.ChatAccessRestricted || user.IsAdmin() {
		return true, ""
	}
	msg := cfg.Access.Message
	if msg == "" {
		msg = "Janus Chat isn't available to your account. Ask an administrator for access."
	}
	if slices.Contains(cfg.Access.Users, user.ID) {
		return true, ""
	}
	if len(cfg.Access.Groups) > 0 {
		groups, err := s.Store.GroupIDsForUser(ctx, user.ID)
		if err == nil {
			for _, g := range groups {
				if slices.Contains(cfg.Access.Groups, g) {
					return true, ""
				}
			}
		}
	}
	for _, team := range cfg.Access.Teams {
		if s.Store.ValidateTeamMember(ctx, team, user.ID) == nil {
			return true, ""
		}
	}
	return false, msg
}

// chatOrgInstructions leads every chat's system message.
func (s *Server) chatOrgInstructions(ctx context.Context) string {
	return strings.TrimSpace(s.chatAdmin(ctx).OrgInstructions)
}

// chatTemporaryTTL is the admin-set lifetime of an idle temporary chat.
func (s *Server) chatTemporaryTTL(ctx context.Context) time.Duration {
	h := s.chatAdmin(ctx).TemporaryTTLHours
	if h <= 0 {
		return store.ChatTemporaryTTL
	}
	return time.Duration(h) * time.Hour
}

// chatModelAllowed refuses models the admin hid from Chat, and personal
// plans when the admin turned them off for Chat.
func (s *Server) chatModelAllowed(ctx context.Context, model string) error {
	cfg := s.chatAdmin(ctx)
	if subscription.IsPersonalModel(model) {
		if !cfg.PersonalPlans {
			return ErrForbidden("Personal plans are turned off for Janus Chat on this instance; pick one of the organization's models.")
		}
		return nil
	}
	if cfg.Models[model].Hidden {
		return ErrForbidden("That model isn't offered in Janus Chat; pick another one.")
	}
	return nil
}

// --- Picker metadata (GET /api/v1/chat/catalog) ---------------------------

type chatCatalogModel struct {
	Name        string `json:"name"`
	ChatName    string `json:"chat_name,omitempty"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Order       int    `json:"order,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
	// Tools are the Chat tools this model's provider runs that the admin
	// has not turned off (each chat can still turn them off).
	Tools []string `json:"tools,omitempty"`
	// Assist are the Janus-run tools the model may call: create_file,
	// create_video, create_music, memory (on by default).
	Assist []string `json:"assist,omitempty"`
}

// handleChatCatalog returns what the picker needs beyond /api/v1/models:
// per-model chat names/descriptions (from the grants that apply in the
// current "Working under" context), categories, order, recommendations,
// hidden models, the default, starter prompts and which features are on.
func (s *Server) handleChatCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	cfg := s.chatAdmin(ctx)
	groupIDs, teamIDs, err := s.principalContext(ctx, user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	subject := store.GrantSubject{UserID: user.ID, GroupIDs: groupIDs}
	if len(teamIDs) > 0 {
		subject.TeamID = teamIDs[0]
	}
	labels, err := s.Store.ChatLabelsFor(ctx, subject)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	catalog, err := s.grantedCatalog(ctx, user, nil)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	models := []chatCatalogModel{}
	hidden := []string{}
	// create_image is listed only when this person has an image model.
	imageModels := s.chatImageModels(ctx, user)
	hasImage := len(imageModels) > 0
	assist := s.chatAssistFor(ctx, user, nil, "", false)
	hasVideo, hasMusic := len(s.chatMediaModels(ctx, user, store.ChatToolVideo)) > 0, len(s.chatMediaModels(ctx, user, store.ChatToolMusic)) > 0
	toolsFor := func(name string) []string {
		t := s.chatToolsFor(ctx, nil, name)
		if !hasImage || s.chatImageModel(ctx, name) || chatPlanImageModel(name) {
			t = slices.DeleteFunc(t, func(id string) bool { return id == store.ChatToolImage })
		}
		return t
	}
	// assistFor: the Janus-run assistant tools a chat model may call
	// (files, video, music, memory), listed apart from the provider's own.
	assistFor := func(name string) []string {
		if s.chatImageModel(ctx, name) || chatPlanImageModel(name) {
			return nil
		}
		var t []string
		if assist.File {
			t = append(t, store.ChatToolFile)
		}
		if hasVideo && !slices.Contains(cfg.ToolsOff, store.ChatToolVideo) {
			t = append(t, store.ChatToolVideo)
		}
		if hasMusic && !slices.Contains(cfg.ToolsOff, store.ChatToolMusic) {
			t = append(t, store.ChatToolMusic)
		}
		if assist.Memory {
			t = append(t, store.ChatToolMemory)
		}
		// Janus reads pages for models whose provider does not.
		if assist.ReadPage && !s.chatModelTools(ctx, name)[store.ChatToolWebFetch] {
			t = append(t, store.ChatToolWebFetch)
		}
		return t
	}
	// Child Safety: models that make what this child's age range may not
	// (pictures, music) are hidden, like an administrator's hidden model.
	child := s.childProfileFor(ctx, user)
	childOff := func(name string) bool {
		if child == nil {
			return false
		}
		if !familyMediaAllowed(child, familyMediaImage) && (s.chatImageModel(ctx, name) || chatPlanImageModel(name)) {
			return true
		}
		return !familyMediaAllowed(child, familyMediaMusic) && chatMusicModelName(name)
	}
	add := func(id, name string) {
		set := cfg.Models[name]
		if set.Hidden || childOff(name) {
			hidden = append(hidden, name)
			return
		}
		l := labels[id]
		models = append(models, chatCatalogModel{Name: name, ChatName: l.Name, Description: l.Description,
			Category: set.Category, Order: set.Order, Recommended: set.Recommended, Tools: toolsFor(name), Assist: assistFor(name)})
	}
	for _, m := range catalog.Models {
		add(m.ID, m.PublicName())
	}
	for _, mm := range catalog.Managed {
		add(mm.ID, mm.Name)
	}
	// The caller's personal plan models: tools only (no admin curation).
	if cfg.PersonalPlans {
		if plans, err := s.personalModelNames(ctx, user); err == nil {
			for _, name := range plans {
				if t, a := toolsFor(name), assistFor(name); len(t) > 0 || len(a) > 0 {
					models = append(models, chatCatalogModel{Name: name, Tools: t, Assist: a})
				}
			}
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"models":          models,
		"hidden":          hidden,
		"default_model":   cfg.DefaultModel,
		"starter_prompts": cfg.StarterPrompts,
		"categories":      store.ChatCategories,
		"tools":           chatToolCatalog,
		// The image models create_image can use for this person (their
		// plan's first, then granted), and the admin's default.
		"image_models":        imageModels,
		"default_image_model": cfg.ImageModel,
		// Video and music models create_video / create_music can use.
		"video_models": s.chatMediaModels(ctx, user, store.ChatToolVideo),
		"music_models": s.chatMediaModels(ctx, user, store.ChatToolMusic),
		"features": map[string]bool{
			"shares":         cfg.SharesEnabled,
			"export":         cfg.ExportEnabled,
			"personal_plans": cfg.PersonalPlans,
		},
		// How long an idle temporary chat is kept (the toggle says so).
		"temporary_ttl_hours": cfg.TemporaryTTLHours,
		// The media tools Child Safety keeps from this person (image,
		// video, music, transcription), so Chat can say why they are gone.
		"child_media_off": childMediaOffList(child),
	})
}

// --- Admin endpoints ------------------------------------------------------

type chatAdminView struct {
	store.ChatAdminSettings
	// AccessLicensed is false on Community: a restriction can be drafted
	// but is not saved/applied.
	AccessLicensed bool `json:"access_licensed"`
	// PreviewOn mirrors JANUS_CHAT_PREVIEW (the hard switch above Enabled).
	PreviewOn bool `json:"preview_on"`
	// SearchKeysSet reports which search services have a stored key (the
	// keys themselves are never sent). Read-only.
	SearchKeysSet map[string]bool `json:"search_keys_set"`
	// SearchKey sets (non-empty) the API key of search.provider on PUT;
	// SearchKeyClear removes it. Write-only.
	SearchKey      string `json:"search_key,omitempty"`
	SearchKeyClear bool   `json:"search_key_clear,omitempty"`
}

// adminChatView is the settings as the console sees them: keys hidden.
func (s *Server) adminChatView(cfg store.ChatAdminSettings) chatAdminView {
	set := map[string]bool{}
	for _, p := range store.SearchProviders {
		if store.SearchProviderNeedsKey(p) {
			set[p] = cfg.Search.KeySet(p)
		}
	}
	cfg.Search.Keys = nil
	return chatAdminView{ChatAdminSettings: cfg, AccessLicensed: s.chatAccessLicensed(), PreviewOn: s.chatEnabled(), SearchKeysSet: set}
}

func (s *Server) handleAdminChatGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.ChatAdminSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.adminChatView(cfg))
}

func (s *Server) handleAdminChatPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	before, err := s.Store.ChatAdminSettings(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Accept exactly what GET returns (its read-only fields are ignored), so
	// a client can edit and send the document back.
	view := chatAdminView{ChatAdminSettings: before}
	if err := decodeJSON(r, &view); err != nil {
		WriteError(w, r, err)
		return
	}
	next := view.ChatAdminSettings
	// Which address the deployment applied is Janus's own record, not an
	// editable setting (see ApplyDeploymentChatSearch).
	next.Search.Deployed = before.Search.Deployed
	// Search keys: never round-tripped. Keep the stored ones; apply a new
	// or cleared key for the selected provider.
	next.Search.Keys = map[string]string{}
	for k, v := range before.Search.Keys {
		next.Search.Keys[k] = v
	}
	if p := strings.TrimSpace(strings.ToLower(next.Search.Provider)); p != "" {
		switch key := strings.TrimSpace(view.SearchKey); {
		case view.SearchKeyClear:
			delete(next.Search.Keys, p)
		case key != "":
			if len(key) > 512 {
				WriteError(w, r, ErrInvalidRequest("That API key is too long.").WithParam("search_key"))
				return
			}
			enc, err := s.Cipher.Encrypt(key)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			next.Search.Keys[p] = enc
		}
	}
	// Restricting who may use Chat is Business. Saving any OTHER setting on
	// Community (or an expired key) keeps whatever restriction is stored.
	if next.Access.Mode == store.ChatAccessRestricted && !accessEqual(before.Access, next.Access) {
		if !s.chatAccessLicensed() {
			WriteError(w, r, ErrFeatureNotLicensed("chat_access"))
			return
		}
		if s.licenseState().Restricted() {
			// Past the grace period a saved restriction keeps applying,
			// but a new or changed one waits for a renewed key.
			WriteError(w, r, ErrLicenseRequired("Chat access rule"))
			return
		}
	}
	if err := s.validateChatAccessTargets(ctx, next.Access); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.SetChatAdminSettings(ctx, next); err != nil {
		writeValidation(w, r, err)
		return
	}
	s.invalidateChatAdmin()
	saved, _ := s.Store.ChatAdminSettings(ctx)
	// The audit trail records that a key changed, never the key (even
	// encrypted).
	s.audit(r, "chat_settings_updated", "chat_settings", store.SettingChat, auditChatSettings(before), auditChatSettings(saved))
	WriteJSON(w, http.StatusOK, s.adminChatView(saved))
}

// auditChatSettings is the settings with each search key replaced by
// "set" / absent.
func auditChatSettings(c store.ChatAdminSettings) store.ChatAdminSettings {
	keys := map[string]string{}
	for k, v := range c.Search.Keys {
		if v != "" {
			keys[k] = "set (fingerprint " + keyFingerprint(v) + ")"
		}
	}
	c.Search.Keys = keys
	return c
}

// keyFingerprint distinguishes key changes in the audit log without
// revealing anything about the key.
func keyFingerprint(enc string) string {
	sum := sha256.Sum256([]byte(enc))
	return hex.EncodeToString(sum[:4])
}

func accessEqual(a, b store.ChatAccess) bool {
	return a.Mode == b.Mode && slices.Equal(a.Groups, b.Groups) && slices.Equal(a.Teams, b.Teams) && slices.Equal(a.Users, b.Users)
}

func (s *Server) validateChatAccessTargets(ctx context.Context, a store.ChatAccess) error {
	check := func(kind, granteeType string, ids []string) error {
		for _, id := range ids {
			ok, err := s.Store.GranteeExists(ctx, granteeType, strings.TrimSpace(id))
			if err != nil {
				return err
			}
			if !ok {
				return ErrInvalidRequest("That " + kind + " no longer exists; remove it from Chat access.").WithParam("access")
			}
		}
		return nil
	}
	if err := check("group", store.GranteeGroup, a.Groups); err != nil {
		return err
	}
	if err := check("team", store.GranteeTeam, a.Teams); err != nil {
		return err
	}
	return check("person", store.GranteeUser, a.Users)
}

// handleAdminGrantChatLabel sets a grant's chat name/description.
func (s *Server) handleAdminGrantChatLabel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		ChatName        string `json:"chat_name"`
		ChatDescription string `json:"chat_description"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	g, err := s.Store.SetGrantChatLabel(ctx, id, body.ChatName, body.ChatDescription)
	if err == store.ErrNotFound {
		WriteError(w, r, ErrNotFoundf("That grant"))
		return
	}
	if err != nil {
		writeValidation(w, r, err)
		return
	}
	s.audit(r, "grant_chat_label_updated", "grant", id, nil, map[string]string{"chat_name": g.ChatName, "chat_description": g.ChatDescription})
	WriteJSON(w, http.StatusOK, map[string]any{"grant": g})
}
