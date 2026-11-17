package store

import (
	"context"
	"testing"
)

// Migration 0086 turns Chat on for an install that saved it off during the
// preview, changing only the top-level switch (run with
// JANUS_ROLLUP_TEST_DATABASE_URL for Postgres).
func TestChatGAMigrationTurnsChatOnOnce(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	apply := func() {
		t.Helper()
		for _, stmt := range chatGAMigration.stmt {
			if err := s.exec(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	registered := false
	for _, m := range migrations {
		registered = registered || m.name == chatGAMigration.name
	}
	if !registered {
		t.Fatal("0086_chat_generally_available is not in the migration list")
	}

	// Never saved: no row is created, and the default is on.
	apply()
	if _, _, err := s.systemSetting(ctx, SettingChat); err != ErrNotFound {
		t.Fatalf("migration created a chat setting: %v", err)
	}
	if cfg, err := s.ChatAdminSettings(ctx); err != nil || !cfg.Enabled {
		t.Fatalf("default = %+v %v", cfg.Enabled, err)
	}

	// Saved off during the preview, with other switches off too.
	off := DefaultChatAdminSettings()
	off.Enabled = false
	off.SharesEnabled = false
	off.Search.Enabled = false
	off.OrgInstructions = `Say "enabled":false never`
	off.StarterPrompts = []string{"hello"}
	if err := s.SetChatAdminSettings(ctx, off); err != nil {
		t.Fatal(err)
	}
	apply()
	got, err := s.ChatAdminSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.SharesEnabled || got.Search.Enabled || got.OrgInstructions != off.OrgInstructions ||
		len(got.StarterPrompts) != 1 || got.StarterPrompts[0] != "hello" {
		t.Fatalf("after migration = %+v", got)
	}

	// Re-applying leaves an "on" document byte for byte.
	before, _, _ := s.systemSetting(ctx, SettingChat)
	apply()
	after, _, _ := s.systemSetting(ctx, SettingChat)
	if before != after {
		t.Fatalf("an on document changed:\n%s\n%s", before, after)
	}
}
