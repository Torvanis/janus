package httpapi

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// chatMediaMigrationActor is who the audit trail says made the move.
const chatMediaMigrationActor = "system"

// MigrateChatMediaSettings moves the old Admin → Chat → Models answers
// ("Sees images", "Hears sound", "Sees video": yes/no per model) into the
// models' capabilities, once, at startup:
//
//   - each yes/no becomes the administrator value of input.image,
//     input.audio or input.video on every model the name reaches (all
//     copies of a model name, whatever their status; a managed model's
//     target), unless that model already has an administrator value for
//     the key, which is kept;
//   - each change is logged and audited as a system change;
//   - the old fields are then dropped from the stored Chat settings.
//
// A document without old answers is left alone, so running it again (or on
// a second replica) changes nothing. It must run before anything else saves
// the Chat settings, because saving drops the old fields.
func (s *Server) MigrateChatMediaSettings(ctx context.Context) {
	if s.Store == nil {
		return
	}
	before, err := s.Store.ChatAdminSettings(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "chat media settings: read chat settings", "error", err.Error())
		return
	}
	names := make([]string, 0, len(before.Models))
	legacy := map[string]map[string]string{}
	for name, m := range before.Models {
		if !m.HasLegacyMedia() {
			continue
		}
		names = append(names, name)
		answers := map[string]string{}
		for key, v := range map[string]string{adapter.CapInImage: m.LegacyImages, adapter.CapInAudio: m.LegacyAudio, adapter.CapInVideo: m.LegacyVideo} {
			if v != "" {
				answers[key] = v
			}
		}
		legacy[name] = answers
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	moved := map[string]any{}
	for _, name := range names {
		edits := map[string]any{}
		for key, v := range legacy[name] {
			switch v {
			case "yes":
				edits[key] = true
			case "no":
				edits[key] = false
			}
		}
		if len(edits) == 0 {
			continue
		}
		targets, err := s.chatMediaMigrationTargets(ctx, name)
		if err != nil {
			// Leave the document as it is; the next start tries again.
			s.Logger.WarnContext(ctx, "chat media settings: find models", "model", name, "error", err.Error())
			return
		}
		if len(targets) == 0 {
			s.Logger.WarnContext(ctx, "chat media settings: no model by that name; the old answer is dropped", "model", name, "answers", legacy[name])
			moved[name] = map[string]any{"answers": legacy[name], "applied_to": []string{}}
			continue
		}
		applied := []string{}
		for _, m := range targets {
			if err := s.migrateChatMediaToModel(ctx, m, edits); err != nil {
				s.Logger.WarnContext(ctx, "chat media settings: set model capabilities", "model", name, "model_id", m.ID, "error", err.Error())
				return
			}
			applied = append(applied, m.ID)
		}
		moved[name] = map[string]any{"answers": legacy[name], "applied_to": applied}
	}
	next := before
	models := map[string]store.ChatModelSetting{}
	for name, m := range before.Models {
		m.LegacyImages, m.LegacyAudio, m.LegacyVideo = "", "", ""
		models[name] = m
	}
	next.Models = models
	if err := s.Store.SetChatAdminSettings(ctx, next); err != nil {
		s.Logger.WarnContext(ctx, "chat media settings: drop the old answers", "error", err.Error())
		return
	}
	s.invalidateChatAdmin()
	entry := &store.AuditEntry{
		ActorLabel: chatMediaMigrationActor, Action: "chat_settings_updated", ResourceType: "chat_settings", ResourceID: store.SettingChat,
		OldValue: encodeAuditValue(map[string]any{"models_media": legacy}),
		NewValue: encodeAuditValue(map[string]any{"models_media": map[string]any{}, "moved_to_model_capabilities": moved}),
	}
	if err := s.Store.AppendAudit(ctx, entry); err != nil {
		s.Logger.WarnContext(ctx, "chat media settings: audit", "error", err.Error())
	}
	s.Logger.InfoContext(ctx, "chat media settings moved to model capabilities", "models", len(names))
}

// chatMediaMigrationTargets are the models an old per-name answer applied
// to: a managed model's current target, else every copy of the name.
func (s *Server) chatMediaMigrationTargets(ctx context.Context, name string) ([]*store.Model, error) {
	if managed, err := s.Store.ManagedModelByName(ctx, name); err == nil {
		if managed.Status != store.ManagedModelEnabled {
			// A turned-off alias: its target is what it would serve.
			if managed.TargetModelID == "" {
				return nil, nil
			}
			m, err := s.Store.ModelByID(ctx, managed.TargetModelID)
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return []*store.Model{m}, nil
		}
		resolved, err := s.Store.ResolveModelForRequest(ctx, name)
		if err != nil || resolved.Model == nil {
			// A broken alias reaches no model.
			return nil, nil
		}
		return []*store.Model{resolved.Model}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	models, err := s.Store.ModelsNamedAnyStatus(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return models, err
}

// migrateChatMediaToModel sets the administrator values the model does not
// have yet, and audits what changed.
func (s *Server) migrateChatMediaToModel(ctx context.Context, m *store.Model, edits map[string]any) error {
	before := adminValues(m.CapabilityState())
	var set, kept []string
	after, err := s.Store.UpdateModelCapabilities(ctx, m.ID, func(state *capability.State) (bool, error) {
		set, kept = nil, nil
		apply := map[string]any{}
		for key, v := range edits {
			if _, ok := state.Admin[key]; ok {
				kept = append(kept, key)
				continue
			}
			apply[key] = v
			set = append(set, key)
		}
		if len(apply) == 0 {
			return false, nil
		}
		return true, state.SetAdmin(apply, time.Now().UTC())
	})
	if err != nil {
		return err
	}
	sort.Strings(set)
	sort.Strings(kept)
	if len(kept) > 0 {
		s.Logger.InfoContext(ctx, "chat media settings: model already has an administrator value; kept it", "model", m.Name, "model_id", m.ID, "keys", kept)
	}
	if len(set) == 0 {
		return nil
	}
	s.InvalidateConfigCache()
	s.Logger.InfoContext(ctx, "chat media settings: set model capabilities from the old Chat setting", "model", m.Name, "model_id", m.ID, "keys", set)
	entry := &store.AuditEntry{
		ActorLabel: chatMediaMigrationActor, Action: "model_capabilities_updated", ResourceType: "model", ResourceID: m.ID,
		OldValue: encodeAuditValue(map[string]any{"model": m.Name, "overrides": before}),
		NewValue: encodeAuditValue(map[string]any{"model": after.Name, "overrides": adminValues(after.CapabilityState()),
			"reason": "moved from Admin → Chat → Models (Sees images / Hears sound / Sees video)"}),
	}
	if err := s.Store.AppendAudit(ctx, entry); err != nil {
		s.Logger.WarnContext(ctx, "chat media settings: audit model capabilities", "model_id", m.ID, "error", err.Error())
	}
	return nil
}
