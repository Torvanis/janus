package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// putRawChatSettings stores a Chat settings document as written by a
// release that still had the per-model media answers (saving through the
// store would drop them).
func putRawChatSettings(t *testing.T, h *harness, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `INSERT INTO system_setting (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		store.SettingChat, string(raw), store.FormatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	h.server.invalidateChatAdmin()
}

func adminFact(t *testing.T, h *harness, modelID, key string) (any, bool) {
	t.Helper()
	m, err := h.store.ModelByID(context.Background(), modelID)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := m.CapabilityState().Admin[key]
	return f.Value, ok
}

func systemAudits(t *testing.T, h *harness) []*store.AuditEntry {
	t.Helper()
	entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.AuditEntry
	for _, e := range entries {
		if e.ActorLabel == chatMediaMigrationActor {
			out = append(out, e)
		}
	}
	return out
}

// The old Admin → Chat → Models answers become the models' administrator
// capability values once: yes/no carried over, an existing administrator
// value kept, every change logged and audited as a system change, the old
// fields dropped from the stored document, and a second run changes
// nothing.
func TestChatMediaSettingsMoveToModelCapabilities(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, decided := h.reasoningModel(t, "already-decided", false)
	_, measured := h.reasoningModel(t, "measured-model", false)
	h.record(t, decided.ID, capability.SourceAdmin, map[string]any{adapter.CapInVideo: false})
	h.record(t, measured.ID, capability.SourceProbe, map[string]any{adapter.CapInImage: false})
	putRawChatSettings(t, h, map[string]any{
		"enabled": true, "temporary_ttl_hours": 24,
		"models": map[string]any{
			"test-model":      map[string]any{"images": "yes", "audio": "no", "category": "fast", "order": 3},
			"already-decided": map[string]any{"video": "yes", "images": "yes"},
			"measured-model":  map[string]any{"images": "yes"},
			"gone-model":      map[string]any{"images": "no"},
			"plain-setting":   map[string]any{"hidden": true},
		},
	})

	h.server.MigrateChatMediaSettings(ctx)

	check := func(id, key string, want any) {
		t.Helper()
		got, ok := adminFact(t, h, id, key)
		if !ok || got != want {
			t.Errorf("%s %s = %v (set %v), want %v", id, key, got, ok, want)
		}
	}
	check(h.model.ID, adapter.CapInImage, true)
	check(h.model.ID, adapter.CapInAudio, false)
	if _, ok := adminFact(t, h, h.model.ID, adapter.CapInVideo); ok {
		t.Error("video had no old answer but was set")
	}
	check(decided.ID, adapter.CapInVideo, false) // the administrator's value is kept
	check(decided.ID, adapter.CapInImage, true)
	check(measured.ID, adapter.CapInImage, true) // admin beats the measured no
	if !h.server.chatVision(ctx, nil, "test-model") || h.server.chatTakes(ctx, nil, "test-model", store.ChatAttachmentAudio) {
		t.Error("Chat does not follow the moved values")
	}

	cfg, err := h.store.ChatAdminSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range cfg.Models {
		if m.HasLegacyMedia() {
			t.Errorf("%s still carries an old answer: %+v", name, m)
		}
	}
	if m := cfg.Models["test-model"]; m.Category != "fast" || m.Order != 3 {
		t.Errorf("the rest of the model's Chat setting was lost: %+v", m)
	}
	if !cfg.Models["plain-setting"].Hidden {
		t.Error("an unrelated model setting was lost")
	}
	var raw string
	if err := h.store.DB().QueryRowContext(ctx, `SELECT value FROM system_setting WHERE key = ?`, store.SettingChat).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"images"`, `"audio"`, `"video"`} {
		if strings.Contains(raw, field) {
			t.Errorf("stored document still has %s: %s", field, raw)
		}
	}

	audits := systemAudits(t, h)
	byAction := map[string]int{}
	for _, e := range audits {
		byAction[e.Action]++
		if e.Action == "model_capabilities_updated" && e.ResourceID == decided.ID && strings.Contains(e.NewValue, `"input.video":true`) {
			t.Error("audit says the kept administrator value was replaced")
		}
	}
	// test-model, already-decided (images only) and measured-model.
	if byAction["model_capabilities_updated"] != 3 || byAction["chat_settings_updated"] != 1 {
		t.Fatalf("system audits = %v", byAction)
	}

	// Idempotent: nothing left to move, nothing changes, nothing audited.
	h.record(t, h.model.ID, capability.SourceAdmin, map[string]any{adapter.CapInImage: nil})
	h.server.MigrateChatMediaSettings(ctx)
	if _, ok := adminFact(t, h, h.model.ID, adapter.CapInImage); ok {
		t.Error("a second run put back a value the administrator cleared")
	}
	if n := len(systemAudits(t, h)); n != len(audits) {
		t.Errorf("a second run audited %d more changes", n-len(audits))
	}
}

// A managed model's old answer goes to the model it serves.
func TestChatMediaSettingsOnAManagedModelGoToItsTarget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateManagedModel(ctx, "house-model", "", h.model.ID, h.user.ID); err != nil {
		t.Fatal(err)
	}
	putRawChatSettings(t, h, map[string]any{"enabled": true, "temporary_ttl_hours": 24,
		"models": map[string]any{"house-model": map[string]any{"video": "yes"}}})
	h.server.MigrateChatMediaSettings(ctx)
	if v, ok := adminFact(t, h, h.model.ID, adapter.CapInVideo); !ok || v != true {
		t.Fatalf("target input.video = %v (%v)", v, ok)
	}
	if !h.server.chatTakes(ctx, nil, "house-model", store.ChatAttachmentVideo) {
		t.Error("Chat through the managed name does not see the moved value")
	}
}
