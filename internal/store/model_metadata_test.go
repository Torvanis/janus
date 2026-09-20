package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func metadataModel(t *testing.T, s *Store) *Model {
	t.Helper()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "metadata", "openai_compatible", "https://api.openai.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpsertDiscoveredModel(ctx, up.ID, "gpt-6-astra", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ModelByUpstreamAndName(ctx, up.ID, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestMetadataMigrationPreservesLegacy(t *testing.T) {
	// Build an actual pre-feature database, then apply the new migration twice.
	all := migrations
	var old []migration
	for _, m := range all {
		if m.name != "0026_model_metadata" {
			old = append(old, m)
		}
	}
	migrations = old
	s := newMetadataTestStore(t)
	migrations = all
	defer func() { migrations = all }()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "legacy", "openai_compatible", "https://api.openai.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpsertDiscoveredModel(ctx, up.ID, "legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{UpstreamID: up.ID}
	if err = s.queryRow(ctx, `SELECT id FROM model WHERE name='legacy'`).Scan(&m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE model SET context_window=8192,rate_in_nanousd=42,rate_out_nanousd=0,rate_cached_nanousd=3,rate_cache_write_5m_nanousd=4,rate_cache_write_1h_nanousd=5,rate_effective_from=? WHERE id=?`, FormatTime(time.Now()), m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, m.UpstreamID, "unknown", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"free", "unversioned"} {
		if _, err := s.UpsertDiscoveredModel(ctx, m.UpstreamID, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.exec(ctx, `UPDATE model SET rate_effective_from=? WHERE name='free'`, FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE model SET rate_in_nanousd=3000000000 WHERE name='unversioned'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"free", "unversioned"} {
		row, err := s.ModelByUpstreamAndName(ctx, m.UpstreamID, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range metadataFields[1:] {
			f := row.Metadata[key]
			if name == "free" {
				if f.Override == nil || *f.Override != 0 {
					t.Fatalf("explicit free legacy %s lost", key)
				}
			} else if key == "rate_in_nanousd" {
				if f.Override == nil || *f.Override != 3000000000 {
					t.Fatal("nonzero legacy rate without timestamp lost")
				}
			} else if f.Override != nil {
				t.Fatalf("unknown unversioned %s pinned", key)
			}
		}
	}
	var raw string
	if err := s.queryRow(ctx, `SELECT metadata_json FROM model WHERE id=?`, m.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Overrides map[string]*int64 `json:"overrides"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]int64{"context_window": 8192, "rate_in_nanousd": 42, "rate_out_nanousd": 0, "rate_cached_nanousd": 3, "rate_cache_write_5m_nanousd": 4, "rate_cache_write_1h_nanousd": 5} {
		if state.Overrides[k] == nil || *state.Overrides[k] != v {
			t.Errorf("legacy %s=%v want %d", k, state.Overrides[k], v)
		}
	}
	if err := s.queryRow(ctx, `SELECT metadata_json FROM model WHERE name='unknown'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	state.Overrides = nil
	_ = json.Unmarshal([]byte(raw), &state)
	for k, v := range state.Overrides {
		if v != nil {
			t.Errorf("unknown legacy field %s became override", k)
		}
	}
}

func TestMetadataPrecedenceAndAtomicPersistence(t *testing.T) {
	s := newMetadataTestStore(t)
	m := metadataModel(t, s)
	ctx := context.Background()
	n := func(v int64) *int64 { return &v }
	refresh := func(contextTokens, input int64) {
		t.Helper()
		if err := s.RefreshModelMetadata(ctx, m.ID, AutomaticMetadata{Upstream: map[string]int64{"context_window": contextTokens, "rate_in_nanousd": input}, Reference: map[string]int64{"context_window": 1050000, "rate_in_nanousd": 7, "rate_out_nanousd": 8}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(field, source string, want *int64) {
		t.Helper()
		got, err := s.ModelByID(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		f := got.Metadata[field]
		if f.Source != source || (f.Value == nil) != (want == nil) || (want != nil && f.Value != nil && *f.Value != *want) {
			t.Fatalf("%s=%+v want %s %v", field, f, source, want)
		}
	}
	refresh(100, 0)
	check("rate_in_nanousd", "upstream", n(0))
	check("rate_out_nanousd", "reference", n(8))
	check("rate_cached_nanousd", "unknown", nil)
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"context_window": n(200), "rate_in_nanousd": n(20), "rate_cached_nanousd": n(0)}}); err != nil {
		t.Fatal(err)
	}
	refresh(300, 30)
	check("context_window", "admin", n(200))
	check("rate_in_nanousd", "admin", n(20))
	check("rate_cached_nanousd", "admin", n(0))
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"context_window": nil}}); err != nil {
		t.Fatal(err)
	}
	check("context_window", "upstream", n(300))
	check("rate_in_nanousd", "admin", n(20))
	if err := s.PatchModel(ctx, m.ID, ModelPatch{Overrides: map[string]*int64{"rate_in_nanousd": nil}}); err != nil {
		t.Fatal(err)
	}
	check("rate_in_nanousd", "upstream", n(30))
	check("rate_cached_nanousd", "admin", n(0))
	if err := s.RefreshModelMetadata(ctx, m.ID, AutomaticMetadata{Reference: map[string]int64{"context_window": 1050000}}); err != nil {
		t.Fatal(err)
	}
	check("context_window", "reference", n(1050000))
	check("rate_in_nanousd", "unknown", nil)
	// A late persistence failure rolls back alias, override and rate history.
	before, _ := s.ModelByID(ctx, m.ID)
	var versions int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM rate_card_version WHERE model_id=?`, m.ID).Scan(&versions)
	trigger := `CREATE TRIGGER reject_metadata BEFORE INSERT ON rate_card_version BEGIN SELECT RAISE(ABORT,'test rejection'); END`
	if s.dialect == DialectPostgres {
		if err := s.exec(ctx, `CREATE FUNCTION reject_metadata() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test rejection'; END $$`); err != nil {
			t.Fatal(err)
		}
		trigger = `CREATE TRIGGER reject_metadata BEFORE INSERT ON rate_card_version FOR EACH ROW EXECUTE FUNCTION reject_metadata()`
	}
	if err := s.exec(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	alias := "new-alias"
	if err := s.PatchModel(ctx, m.ID, ModelPatch{DisplayName: &alias, Overrides: map[string]*int64{"rate_in_nanousd": n(99)}}); err == nil {
		t.Fatal("expected persistence failure")
	}
	after, _ := s.ModelByID(ctx, m.ID)
	if after.DisplayName != before.DisplayName || after.RateInNano != before.RateInNano {
		t.Fatal("partial metadata write")
	}
	var count int
	_ = s.queryRow(ctx, `SELECT COUNT(*) FROM rate_card_version WHERE model_id=?`, m.ID).Scan(&count)
	if count != versions {
		t.Fatal("history changed after failure")
	}
}

func TestMetadataMigrationIsAtomic(t *testing.T) {
	all := migrations
	defer func() { migrations = all }()
	// Target the metadata migration by identity, not whichever feature was added last.
	metadataIndex := -1
	for i, m := range all {
		if m.name == "0026_model_metadata" {
			metadataIndex = i
			break
		}
	}
	if metadataIndex < 0 {
		t.Fatal("metadata migration missing")
	}
	migrations = all[:metadataIndex]
	s := newMetadataTestStore(t)
	ctx := context.Background()
	broken := all[metadataIndex]
	broken.stmt = append(append([]string{}, broken.stmt...), `SELECT missing_metadata_column FROM model`)
	migrations = append(append([]migration{}, migrations...), broken)
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("expected migration failure")
	}
	rows, err := s.query(ctx, `SELECT metadata_json FROM model`)
	if err == nil {
		_ = rows.Close()
		t.Fatal("failed migration left metadata schema partially installed")
	}
	migrations = all
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
}
