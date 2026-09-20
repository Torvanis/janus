package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a throwaway embedded database with all migrations applied.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "janus-test.db")
	s, err := Open(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedCatalogModels registers model names in the catalog so they survive the
// model-breakdown validity filter: BreakdownUsage("model") only returns model
// names that exist in the catalog.
func seedCatalogModels(t *testing.T, s *Store, names ...string) {
	t.Helper()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "catalog-"+NewID(), "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create catalog upstream: %v", err)
	}
	for _, name := range names {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed catalog model %q: %v", name, err)
		}
	}
}

// TestLedgerUpsertIsAtomicOnFreshWindow locks the AddLedger/SetLedger upsert
// contract (regression: an UPDATE→SELECT→INSERT sequence let two concurrent
// writers on a brand-new (quota_id, window_start) both miss the UPDATE and
// race the INSERT — the loser's delta was silently dropped until the next
// reconcile). Every concurrent delta must land and the running total must be
// exact.
func TestLedgerUpsertIsAtomicOnFreshWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	window := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const writers = 16
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			_, err := s.AddLedger(ctx, "q-concurrent", window, 3)
			errs <- err
		}()
	}
	for i := 0; i < writers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent AddLedger on a fresh window: %v", err)
		}
	}
	v, err := s.LedgerValue(ctx, "q-concurrent", window)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if v != writers*3 {
		t.Fatalf("ledger total = %d after %d concurrent +3 deltas, want %d (a delta was dropped)", v, writers, writers*3)
	}

	// AddLedger returns the post-increment value.
	if v, err := s.AddLedger(ctx, "q-concurrent", window, 2); err != nil || v != writers*3+2 {
		t.Fatalf("AddLedger returned (%d, %v), want (%d, nil)", v, err, writers*3+2)
	}

	// SetLedger must work on both fresh and existing windows.
	if err := s.SetLedger(ctx, "q-set", window, 7); err != nil {
		t.Fatalf("SetLedger fresh window: %v", err)
	}
	if err := s.SetLedger(ctx, "q-set", window, 11); err != nil {
		t.Fatalf("SetLedger existing window: %v", err)
	}
	if v, err := s.LedgerValue(ctx, "q-set", window); err != nil || v != 11 {
		t.Fatalf("ledger after SetLedger = (%d, %v), want (11, nil)", v, err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read applied migrations: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(migrations))
	}
	// Re-running must be a no-op, which is what makes multi-replica boot safe.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate run: %v", err)
	}
	again, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("re-read applied migrations: %v", err)
	}
	if len(again) != len(applied) {
		t.Fatalf("migration ledger grew to %d entries on re-run", len(again))
	}
}

func TestUserUpsertIsIdentityKeyed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user, created, err := s.UpsertUserFromIdentity(ctx, "sub-1", "alice@example.com", "Alice", false, false)
	if err != nil || !created {
		t.Fatalf("first sign-in: created=%v err=%v", created, err)
	}
	if user.Role != RoleUser {
		t.Fatalf("new user role = %q, want %q", user.Role, RoleUser)
	}

	// Email changes must not create a second account: the subject is the key.
	again, created, err := s.UpsertUserFromIdentity(ctx, "sub-1", "alice.smith@example.com", "Alice Smith", false, false)
	if err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if created {
		t.Fatal("a returning user must not be created again")
	}
	if again.ID != user.ID {
		t.Fatalf("user id changed from %s to %s across sign-ins", user.ID, again.ID)
	}
	if again.Email != "alice.smith@example.com" {
		t.Fatalf("email was not refreshed on sign-in: %q", again.Email)
	}
}

func TestBootstrapAdminPromotesExistingUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.UpsertUserFromIdentity(ctx, "sub-2", "bob@example.com", "Bob", false, false); err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	promoted, _, err := s.UpsertUserFromIdentity(ctx, "sub-2", "bob@example.com", "Bob", true, false)
	if err != nil {
		t.Fatalf("bootstrap sign-in: %v", err)
	}
	if promoted.Role != RoleAdmin {
		t.Fatalf("bootstrap email did not grant admin, role = %q", promoted.Role)
	}
}

// TestMigrationAdminViaGroupIsAdditive proves migration 0011 is additive with
// a safe default: user rows written before the column existed keep scanning
// cleanly, backfill admin_via_group to false, and no pre-existing admin gains
// or loses anything by applying the migration.
func TestMigrationAdminViaGroupIsAdditive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre-0011.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite database: %v", err)
	}
	s := &Store{db: db, dialect: DialectSQLite}
	t.Cleanup(func() { _ = s.Close() })

	// Recreate the world as it was before 0011: apply and record every
	// migration up to (but excluding) admin_via_group, exactly as an old
	// binary would have left the database.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create migration ledger: %v", err)
	}
	for _, m := range migrations {
		if m.name == "0011_admin_via_group" {
			break
		}
		for _, stmt := range m.stmt {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("apply pre-existing migration %s: %v", m.name, err)
			}
		}
		if err := s.exec(ctx, `INSERT INTO schema_migration (name, applied_at) VALUES (?, ?)`, m.name, FormatTime(nowUTC())); err != nil {
			t.Fatalf("record pre-existing migration %s: %v", m.name, err)
		}
	}
	// Legacy rows written by a binary that did not know the column.
	for _, row := range []struct{ id, sub, email, role string }{
		{"u-legacy-admin", "sub-legacy-admin", "legacy-admin@example.com", RoleAdmin},
		{"u-legacy-user", "sub-legacy-user", "legacy-user@example.com", RoleUser},
	} {
		if err := s.exec(ctx,
			`INSERT INTO app_user (id, auth_provider_id, email, name, role, is_active, timezone, locale, created_at, last_login_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			row.id, row.sub, row.email, "Legacy", row.role, 1, "", "", FormatTime(nowUTC()), FormatTime(nowUTC())); err != nil {
			t.Fatalf("insert legacy user %s: %v", row.id, err)
		}
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("apply outstanding migrations on a legacy database: %v", err)
	}
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read applied migrations: %v", err)
	}
	found := false
	for _, name := range applied {
		if name == "0011_admin_via_group" {
			found = true
		}
	}
	if !found {
		t.Fatal("0011_admin_via_group was not recorded in the migration ledger")
	}

	admin, err := s.UserByAuthID(ctx, "sub-legacy-admin")
	if err != nil {
		t.Fatalf("scan legacy admin row through the new column set: %v", err)
	}
	if admin.AdminViaGroup {
		t.Error("legacy rows must backfill admin_via_group to false")
	}
	if admin.Role != RoleAdmin || !admin.IsAdmin() {
		t.Errorf("the migration demoted a pre-existing admin: role=%q admin=%v", admin.Role, admin.IsAdmin())
	}
	regular, err := s.UserByAuthID(ctx, "sub-legacy-user")
	if err != nil {
		t.Fatalf("scan legacy user row: %v", err)
	}
	if regular.Role != RoleUser || regular.AdminViaGroup || regular.IsAdmin() {
		t.Errorf("the migration changed a pre-existing non-admin: role=%q via_group=%v admin=%v",
			regular.Role, regular.AdminViaGroup, regular.IsAdmin())
	}
}

// TestUpsertUserThreeSourceAdminMatrix locks the three-source role resolution
// contract: bootstrap promotes role and is sticky, explicit grants are sticky,
// the group signal lives in admin_via_group and is re-evaluated on every
// sign-in (leaving the group revokes it), and no pre-existing admin role is
// ever auto-demoted by a sign-in.
func TestUpsertUserThreeSourceAdminMatrix(t *testing.T) {
	type signIn struct{ bootstrap, group bool }
	cases := []struct {
		name string
		// signIns[0] is the first login; explicitRole (if set) is written via
		// the admin UI path (UpdateUser) after it; signIns[1:] follow.
		signIns      []signIn
		explicitRole string
		wantRole     string
		wantViaGroup bool
		wantAdmin    bool
	}{
		{"no source stays user", []signIn{{false, false}, {false, false}}, "", RoleUser, false, false},
		{"bootstrap promotes role on first login", []signIn{{true, false}}, "", RoleAdmin, false, true},
		{"group grants capability without touching role", []signIn{{false, true}}, "", RoleUser, true, true},
		{"bootstrap and group compose", []signIn{{true, true}}, "", RoleAdmin, true, true},
		{"group gained on a later sign-in", []signIn{{false, false}, {false, true}}, "", RoleUser, true, true},
		{"leaving the group revokes group-granted admin", []signIn{{false, true}, {false, false}}, "", RoleUser, false, false},
		{"bootstrap admin sticks after leaving the bootstrap list", []signIn{{true, false}, {false, false}}, "", RoleAdmin, false, true},
		{"bootstrap admin sticks after losing every signal", []signIn{{true, true}, {false, false}}, "", RoleAdmin, false, true},
		{"explicit grant sticks across signal-less sign-ins", []signIn{{false, false}, {false, false}}, RoleAdmin, RoleAdmin, false, true},
		{"explicit grant overrides leaving the group", []signIn{{false, true}, {false, false}}, RoleAdmin, RoleAdmin, false, true},
		{"explicit demotion holds but group still grants capability", []signIn{{true, true}, {false, true}}, RoleUser, RoleUser, true, true},
		{"non-admin roles are never touched by sign-in", []signIn{{false, true}, {false, false}}, RoleTeamLead, RoleTeamLead, false, false},
	}
	s := newTestStore(t)
	ctx := context.Background()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authID := "sub-matrix-" + string(rune('a'+i))
			email := authID + "@example.com"
			user, created, err := s.UpsertUserFromIdentity(ctx, authID, email, "Matrix", tc.signIns[0].bootstrap, tc.signIns[0].group)
			if err != nil {
				t.Fatalf("first sign-in: %v", err)
			}
			if !created {
				t.Fatal("first sign-in must create the user")
			}
			if tc.explicitRole != "" {
				if err := s.UpdateUser(ctx, user.ID, tc.explicitRole, true); err != nil {
					t.Fatalf("apply explicit role %q: %v", tc.explicitRole, err)
				}
			}
			for n, si := range tc.signIns[1:] {
				var again *User
				again, created, err = s.UpsertUserFromIdentity(ctx, authID, email, "Matrix", si.bootstrap, si.group)
				if err != nil {
					t.Fatalf("sign-in %d: %v", n+2, err)
				}
				if created {
					t.Fatalf("sign-in %d re-created an existing user", n+2)
				}
				if again.ID != user.ID {
					t.Fatalf("sign-in %d changed the user id from %s to %s", n+2, user.ID, again.ID)
				}
				user = again
			}
			// The returned struct and a fresh read must agree: the state
			// round-trips through the database, not through struct mutation.
			stored, err := s.UserByAuthID(ctx, authID)
			if err != nil {
				t.Fatalf("re-read user: %v", err)
			}
			for label, u := range map[string]*User{"returned": user, "stored": stored} {
				if u.Role != tc.wantRole {
					t.Errorf("%s role = %q, want %q", label, u.Role, tc.wantRole)
				}
				if u.AdminViaGroup != tc.wantViaGroup {
					t.Errorf("%s admin_via_group = %v, want %v", label, u.AdminViaGroup, tc.wantViaGroup)
				}
				if u.IsAdmin() != tc.wantAdmin {
					t.Errorf("%s IsAdmin() = %v, want %v", label, u.IsAdmin(), tc.wantAdmin)
				}
			}
		})
	}
}

// TestUpsertUserGroupSignalIsIdempotent proves that repeating the same sign-in
// leaves the row byte-for-byte stable on the admin surface: same role, same
// group flag, same id, no re-creation.
func TestUpsertUserGroupSignalIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, created, err := s.UpsertUserFromIdentity(ctx, "sub-idem", "idem@example.com", "Idem", false, true)
	if err != nil || !created {
		t.Fatalf("first sign-in: created=%v err=%v", created, err)
	}
	for i := 0; i < 3; i++ {
		again, created, err := s.UpsertUserFromIdentity(ctx, "sub-idem", "idem@example.com", "Idem", false, true)
		if err != nil {
			t.Fatalf("repeat sign-in %d: %v", i, err)
		}
		if created || again.ID != first.ID || again.Role != first.Role || again.AdminViaGroup != first.AdminViaGroup {
			t.Fatalf("repeat sign-in %d drifted: created=%v id=%s role=%q via_group=%v",
				i, created, again.ID, again.Role, again.AdminViaGroup)
		}
	}
}

func TestIDPGroupSyncReplacesOnlyProviderGroups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-3", "carol@example.com", "Carol", false, false)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	inApp, err := s.CreateGroup(ctx, "platform-owners")
	if err != nil {
		t.Fatalf("create in-app group: %v", err)
	}
	if err := s.SetGroupMembers(ctx, inApp.ID, []string{user.ID}); err != nil {
		t.Fatalf("set in-app members: %v", err)
	}
	if err := s.ReplaceIDPGroupsForUser(ctx, user.ID, []string{"engineers", "all-staff"}); err != nil {
		t.Fatalf("first group sync: %v", err)
	}
	names, err := s.GroupNamesForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("read groups: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("after sync user has groups %v, want 3", names)
	}

	// Second sign-in drops "engineers" upstream; the in-app group must survive.
	if err := s.ReplaceIDPGroupsForUser(ctx, user.ID, []string{"all-staff"}); err != nil {
		t.Fatalf("second group sync: %v", err)
	}
	names, err = s.GroupNamesForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("re-read groups: %v", err)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if found["engineers"] {
		t.Error("a group removed upstream should be dropped at next sign-in")
	}
	if !found["all-staff"] || !found["platform-owners"] {
		t.Errorf("group sync lost groups it should keep: %v", names)
	}
}

func TestGrantEvaluationComposesWithOr(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	up, err := s.CreateUpstream(ctx, "openai", "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	for _, name := range []string{"model-direct", "model-group", "model-all", "model-none"} {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed model %s: %v", name, err)
		}
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	byName := map[string]*Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-4", "dave@example.com", "Dave", false, false)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	group, err := s.EnsureGroup(ctx, "engineers", true)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	if _, err := s.CreateGrant(ctx, byName["model-direct"].ID, ModelKindModel, GranteeUser, user.ID); err != nil {
		t.Fatalf("direct grant: %v", err)
	}
	if _, err := s.CreateGrant(ctx, byName["model-group"].ID, ModelKindModel, GranteeGroup, group.ID); err != nil {
		t.Fatalf("group grant: %v", err)
	}
	if _, err := s.CreateGrant(ctx, byName["model-all"].ID, ModelKindModel, GranteeAllUsers, ""); err != nil {
		t.Fatalf("all-users grant: %v", err)
	}

	granted, err := s.GrantedModelIDs(ctx, user.ID, []string{group.ID})
	if err != nil {
		t.Fatalf("evaluate grants: %v", err)
	}
	if len(granted) != 3 {
		t.Fatalf("user can reach %d models, want 3", len(granted))
	}
	if _, ok := granted[byName["model-none"].ID]; ok {
		t.Error("an ungranted model must not be reachable")
	}
	if source := granted[byName["model-direct"].ID]; source != "direct" {
		t.Errorf("direct grant source = %q, want %q", source, "direct")
	}

	// A user in no groups keeps only the all-users grant.
	lonely, err := s.GrantedModelIDs(ctx, "nobody", nil)
	if err != nil {
		t.Fatalf("evaluate grants for ungrouped user: %v", err)
	}
	if len(lonely) != 1 {
		t.Fatalf("ungrouped user can reach %d models, want 1 (all-users only)", len(lonely))
	}
}

func TestTokenDigestLookupAndRevocation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-5", "erin@example.com", "Erin", false, false)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	token, plaintext, err := s.CreateToken(ctx, user.ID, "laptop")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if len(plaintext) < 20 || plaintext[:6] != TokenPrefix {
		t.Fatalf("token value %q does not look like a janus credential", plaintext)
	}
	found, err := s.TokenByDigest(ctx, HashToken(plaintext))
	if err != nil {
		t.Fatalf("look up token by digest: %v", err)
	}
	if found.ID != token.ID {
		t.Fatalf("digest lookup returned token %s, want %s", found.ID, token.ID)
	}
	// The plaintext must not be recoverable from the listing.
	listed, err := s.ListTokens(ctx, user.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list tokens: %v (%d rows)", err, len(listed))
	}
	if err := s.RevokeToken(ctx, token.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	revoked, err := s.TokenByDigest(ctx, HashToken(plaintext))
	if err != nil {
		t.Fatalf("look up revoked token: %v", err)
	}
	if !revoked.Revoked() {
		t.Fatal("token should report as revoked after revocation")
	}
}

// TestAggregateUsageZeroCostEvents locks the store contract local-only mode
// (JANUS_LOCAL_ONLY) relies on: events recorded with CostNano == 0 aggregate
// exactly like any others — request and token totals sum normally and the
// cost total is plain zero, with no special-casing anywhere in the store.
func TestAggregateUsageZeroCostEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: "u-local", TokenID: "tok-local",
			ModelName: "gpt-4o", Modality: "chat", TokensIn: 100, TokensOut: 40, CostNano: 0, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert zero-cost usage event: %v", err)
		}
	}

	totals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u-local"})
	if err != nil {
		t.Fatalf("aggregate usage: %v", err)
	}
	if totals.Requests != 3 || totals.TokensIn != 300 || totals.TokensOut != 120 {
		t.Fatalf("totals = %+v, want 3 requests / 300 tokens in / 120 tokens out", totals)
	}
	if totals.CostNano != 0 {
		t.Fatalf("cost total = %d nano-USD, want 0 for zero-cost (local-only) events", totals.CostNano)
	}
}

// TestUsageScopeTokenID is the documented store-level regression: UsageScope must
// be able to scope aggregates and breakdowns to a single token, not just to a
// user (the per-token dashboard used to fall back to user-wide numbers).
func TestUsageScopeTokenID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedCatalogModels(t, s, "gpt-4o", "claude")

	seed := func(tokenID, model string, n int, tokensIn, costNano int64) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := s.InsertUsageEvent(ctx, &UsageEvent{
				CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: "u1", TokenID: tokenID,
				ModelName: model, Modality: "chat", TokensIn: tokensIn, CostNano: costNano, HTTPStatus: 200,
			}); err != nil {
				t.Fatalf("insert usage event: %v", err)
			}
		}
	}
	seed("tok-a", "gpt-4o", 3, 100, 1000)
	seed("tok-b", "claude", 2, 10, 100)

	totals, err := s.AggregateUsage(ctx, UsageScope{TokenID: "tok-a"})
	if err != nil {
		t.Fatalf("aggregate token scope: %v", err)
	}
	if totals.Requests != 3 || totals.TokensIn != 300 || totals.CostNano != 3000 {
		t.Fatalf("token-scoped totals = %+v, want 3 requests / 300 tokens in / 3000 nano-USD", totals)
	}

	byModel, err := s.BreakdownUsage(ctx, UsageScope{TokenID: "tok-a"}, "model")
	if err != nil {
		t.Fatalf("breakdown token scope: %v", err)
	}
	if len(byModel) != 1 || byModel[0].Key != "gpt-4o" {
		t.Fatalf("token-scoped model breakdown = %+v, want exactly gpt-4o", byModel)
	}

	// User scope still spans both tokens.
	userTotals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u1"})
	if err != nil {
		t.Fatalf("aggregate user scope: %v", err)
	}
	if userTotals.Requests != 5 {
		t.Fatalf("user-scoped requests = %d, want 5", userTotals.Requests)
	}
}

// TestCacheWriteTokenAggregation locks the aggregate half of the canonical
// cache-hit-rate contract: AggregateUsage, BreakdownUsage, and UsageSeries all
// report summed tokens_cache_write_5m / tokens_cache_write_1h alongside
// tokens_cached (legacy rows contribute zero), without disturbing any
// pre-existing field, so every consumer can compute
// tokens_cached / (tokens_in + tokens_cache_write_5m + tokens_cache_write_1h).
func TestCacheWriteTokenAggregation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Register the models so the breakdown's catalog-validity filter keeps
	// their rows (BreakdownUsage drops names absent from the catalog).
	seedCatalogModels(t, s, "gpt-4o", "claude", "legacy-model")

	// The model breakdown filters rows against the model catalog (invalid
	// model names must never reach usage graphs), so the models this test
	// reports on have to exist in the catalog. AggregateUsage is unfiltered;
	// only BreakdownUsage(dimension="model") needs the seed.
	up, err := s.CreateUpstream(ctx, "openai", "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	for _, name := range []string{"gpt-4o", "claude", "legacy-model"} {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	insert := func(model string, in, cached, cacheWrite5m, cacheWrite1h int64) {
		t.Helper()
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: now.Add(-time.Minute), UserID: "u1", ModelName: model, Modality: "chat",
			TokensIn: in, TokensOut: 5, TokensCached: cached,
			TokensCacheWrite5m: cacheWrite5m, TokensCacheWrite1h: cacheWrite1h,
			CostNano: 1000, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}
	insert("gpt-4o", 950, 50, 100, 0)
	insert("claude", 1000, 200, 25, 75)
	// A legacy-shaped event (recorded before cache-write tracking): all cache
	// fields zero. It must contribute zeros, never NULL-poison the sums.
	insert("legacy-model", 10, 0, 0, 0)

	totals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u1"})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if totals.TokensCacheWrite5m != 125 || totals.TokensCacheWrite1h != 75 {
		t.Fatalf("cache-write sums = %d/%d, want 125/75", totals.TokensCacheWrite5m, totals.TokensCacheWrite1h)
	}
	// Pre-existing fields keep their values.
	if totals.TokensIn != 1960 || totals.TokensOut != 15 || totals.TokensCached != 250 ||
		totals.CostNano != 3000 || totals.Requests != 3 || totals.ErrorCount != 0 {
		t.Fatalf("existing totals disturbed: %+v", totals)
	}

	byModel, err := s.BreakdownUsage(ctx, UsageScope{UserID: "u1"}, "model")
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	rows := make(map[string]Totals, len(byModel))
	for _, b := range byModel {
		rows[b.Key] = b.Totals
	}
	if got := rows["claude"]; got.TokensCacheWrite5m != 25 || got.TokensCacheWrite1h != 75 || got.TokensCached != 200 {
		t.Fatalf("claude breakdown = %+v, want cache 200 / writes 25+75", got)
	}
	if got := rows["gpt-4o"]; got.TokensCacheWrite5m != 100 || got.TokensCacheWrite1h != 0 {
		t.Fatalf("gpt-4o breakdown = %+v, want writes 100+0", got)
	}
	if got := rows["legacy-model"]; got.TokensCacheWrite5m != 0 || got.TokensCacheWrite1h != 0 || got.TokensCached != 0 {
		t.Fatalf("legacy breakdown = %+v, want all-zero cache fields", got)
	}

	series, err := s.UsageSeries(ctx, UsageScope{UserID: "u1", End: now}, time.Hour, 2)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("series has %d buckets, want 2", len(series))
	}
	last := series[1].Totals
	if last.TokensCacheWrite5m != 125 || last.TokensCacheWrite1h != 75 || last.TokensCached != 250 {
		t.Fatalf("series bucket totals = %+v, want cache 250 / writes 125+75", last)
	}
	if last.TokensIn != 1960 || last.Requests != 3 {
		t.Fatalf("series bucket existing fields disturbed: %+v", last)
	}

	// The JSON contract: stable snake_case names next to tokens_cached.
	blob, err := json.Marshal(totals)
	if err != nil {
		t.Fatalf("marshal totals: %v", err)
	}
	for _, key := range []string{`"tokens_cache_write_5m":125`, `"tokens_cache_write_1h":75`, `"tokens_cached":250`} {
		if !strings.Contains(string(blob), key) {
			t.Fatalf("totals JSON %s missing %s", blob, key)
		}
	}
}

func TestUsageAggregationAndRetention(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedCatalogModels(t, s, "gpt-4o-mini", "claude")

	for i := 0; i < 3; i++ {
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: "u1", ModelName: "gpt-4o-mini",
			Modality: "chat", TokensIn: 100, TokensOut: 50, CostNano: 1000, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{
		CreatedAt: now, UserID: "u2", ModelName: "claude", Modality: "chat",
		TokensIn: 10, TokensOut: 5, CostNano: 100, HTTPStatus: 429, ErrorCode: "policy.quota_exceeded",
	}); err != nil {
		t.Fatalf("insert other user event: %v", err)
	}

	totals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u1", Start: now.Add(-time.Hour), End: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if totals.Requests != 3 || totals.TokensIn != 300 || totals.CostNano != 3000 {
		t.Fatalf("totals = %+v, want 3 requests / 300 tokens in / 3000 nano-USD", totals)
	}

	errTotals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u2"})
	if err != nil {
		t.Fatalf("aggregate errors: %v", err)
	}
	if errTotals.ErrorCount != 1 {
		t.Fatalf("error count = %d, want 1", errTotals.ErrorCount)
	}

	byModel, err := s.BreakdownUsage(ctx, UsageScope{}, "model")
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(byModel) != 2 {
		t.Fatalf("model breakdown has %d rows, want 2", len(byModel))
	}

	sum, err := s.SumMetricSince(ctx, "user", "u1", "", MetricTokensIn, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("sum metric: %v", err)
	}
	if sum != 300 {
		t.Fatalf("quota metric sum = %d, want 300", sum)
	}

	deleted, err := s.PurgeUsageEvents(ctx, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("purge removed %d rows, want the older events", deleted)
	}
}

// TestBreakdownUsageMetricOrdering locks the leaderboard ranking contract:
// BreakdownUsage defaults to ordering by SUM(tokens_out) DESC so top-users
// charts rank by token volume, while "cost" and "requests" remain available
// as explicit metrics for spend-oriented surfaces.
func TestBreakdownUsageMetricOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := func(userID string, n int, tokensOut, costNano int64) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := s.InsertUsageEvent(ctx, &UsageEvent{
				CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: userID, TokenID: "tok-" + userID,
				ModelName: "gpt-4o", Modality: "chat", TokensIn: 10, TokensOut: tokensOut, CostNano: costNano, HTTPStatus: 200,
			}); err != nil {
				t.Fatalf("insert usage event: %v", err)
			}
		}
	}
	// u-tokens emits the most output tokens, u-spend costs the most, and
	// u-requests makes the most calls — each metric has a distinct winner.
	seed("u-tokens", 2, 5000, 10)
	seed("u-spend", 3, 100, 90000)
	seed("u-requests", 5, 10, 1)

	keys := func(rows []Breakdown) []string {
		out := make([]string, 0, len(rows))
		for _, b := range rows {
			out = append(out, b.Key)
		}
		return out
	}

	// Default (metric omitted) ranks by SUM(tokens_out) DESC.
	byDefault, err := s.BreakdownUsage(ctx, UsageScope{}, "user")
	if err != nil {
		t.Fatalf("breakdown default metric: %v", err)
	}
	if got := keys(byDefault); len(got) != 3 || got[0] != "u-tokens" || got[1] != "u-spend" || got[2] != "u-requests" {
		t.Fatalf("default ordering = %v, want [u-tokens u-spend u-requests] (tokens_out DESC)", got)
	}

	// Explicit tokens_out matches the default.
	byTokensOut, err := s.BreakdownUsage(ctx, UsageScope{}, "user", BreakdownMetricTokensOut)
	if err != nil {
		t.Fatalf("breakdown tokens_out: %v", err)
	}
	if got := keys(byTokensOut); got[0] != "u-tokens" {
		t.Fatalf("tokens_out ordering = %v, want u-tokens first", got)
	}

	// Cost ranks by SUM(cost_nanousd) DESC.
	byCost, err := s.BreakdownUsage(ctx, UsageScope{}, "user", BreakdownMetricCost)
	if err != nil {
		t.Fatalf("breakdown cost: %v", err)
	}
	if got := keys(byCost); got[0] != "u-spend" {
		t.Fatalf("cost ordering = %v, want u-spend first", got)
	}

	// Requests ranks by COUNT(*) DESC.
	byRequests, err := s.BreakdownUsage(ctx, UsageScope{}, "user", BreakdownMetricRequests)
	if err != nil {
		t.Fatalf("breakdown requests: %v", err)
	}
	if got := keys(byRequests); got[0] != "u-requests" {
		t.Fatalf("requests ordering = %v, want u-requests first", got)
	}

	// An unknown metric is refused, never silently reinterpreted.
	if _, err := s.BreakdownUsage(ctx, UsageScope{}, "user", "latency"); err == nil {
		t.Fatal("unsupported breakdown metric should return an error")
	}
}

// TestBreakdownUsageModelFiltersInvalidNames locks the model-graph validity
// contract: BreakdownUsage("model") returns only model names that exist in
// the model catalog (native names and display-name aliases both count), while
// names recorded from requests with invalid model identifiers are logged at
// warn level with context and excluded from the results.
func TestBreakdownUsageModelFiltersInvalidNames(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Catalog: one plain model and one carrying a display-name alias (usage
	// events store PublicName(), so the alias is what lands in usage_event).
	up, err := s.CreateUpstream(ctx, "openai", "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "gpt-4o", []string{"chat"}); err != nil {
		t.Fatalf("seed gpt-4o: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "anthropic-fable", []string{"chat"}); err != nil {
		t.Fatalf("seed anthropic-fable: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{Search: "anthropic-fable"})
	if err != nil || len(models) != 1 {
		t.Fatalf("find anthropic-fable: %v (%d rows)", err, len(models))
	}
	if err := s.SetModelDisplayName(ctx, models[0].ID, "fable-5"); err != nil {
		t.Fatalf("set display name: %v", err)
	}

	// Capture the store's diagnostic log output.
	var logs bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	insert := func(model string) {
		t.Helper()
		if err := s.InsertUsageEvent(ctx, &UsageEvent{
			CreatedAt: now.Add(-time.Minute), UserID: "u1", TokenID: "tok-1", ModelName: model,
			Modality: "chat", TokensIn: 10, CostNano: 100, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event for %q: %v", model, err)
		}
	}
	insert("gpt-4o")               // valid: native catalog name
	insert("fable-5")              // valid: display-name alias
	insert("gpt-5-o-hallucinated") // invalid: never existed in the catalog
	insert("")                     // invalid: request failed before model resolution

	byModel, err := s.BreakdownUsage(ctx, UsageScope{UserID: "u1"}, "model")
	if err != nil {
		t.Fatalf("model breakdown: %v", err)
	}
	got := map[string]bool{}
	for _, b := range byModel {
		got[b.Key] = true
	}
	if len(byModel) != 2 || !got["gpt-4o"] || !got["fable-5"] {
		t.Fatalf("model breakdown = %+v, want exactly gpt-4o and Fable 5 (invalid names filtered)", byModel)
	}

	logged := logs.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("dropped model names must be logged at warn level: %s", logged)
	}
	if !strings.Contains(logged, "gpt-5-o-hallucinated") {
		t.Errorf("the invalid model name was dropped without being logged: %s", logged)
	}
	if !strings.Contains(logged, "user_id=u1") {
		t.Errorf("the dropped-model log carries no user context: %s", logged)
	}
	if !strings.Contains(logged, "time=") {
		t.Errorf("the dropped-model log carries no timestamp: %s", logged)
	}
	if strings.Contains(logged, "model_name=gpt-4o") || strings.Contains(logged, `model_name="fable-5"`) {
		t.Errorf("valid catalog models must not be logged as dropped: %s", logged)
	}

	// Other dimensions pass through unfiltered — the raw names stay queryable
	// (they are logged data, just never graphed as models).
	byModality, err := s.BreakdownUsage(ctx, UsageScope{UserID: "u1"}, "modality")
	if err != nil {
		t.Fatalf("modality breakdown: %v", err)
	}
	if len(byModality) != 1 || byModality[0].Totals.Requests != 4 {
		t.Fatalf("modality breakdown = %+v, want one chat row covering all 4 events", byModality)
	}
}

func TestFeatureFlagsDefaultOff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flags, err := s.FeatureFlags(ctx)
	if err != nil {
		t.Fatalf("read flags: %v", err)
	}
	if flags["leaderboards_enabled"] {
		t.Fatal("leaderboards must default to off")
	}
	if err := s.SetFeatureFlag(ctx, "leaderboards_enabled", true); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	flags, err = s.FeatureFlags(ctx)
	if err != nil {
		t.Fatalf("re-read flags: %v", err)
	}
	if !flags["leaderboards_enabled"] {
		t.Fatal("flag change did not persist")
	}
	if err := s.SetFeatureFlag(ctx, "not_a_real_flag", true); err == nil {
		t.Fatal("unknown flags must be rejected")
	}
}

// spend_emphasis picks the default dashboard metric: off = usage emphasis
// (graphs open on Tokens), on = spend emphasis (graphs open on Spend). The
// shipped default is usage emphasis, matching the repo convention that flags
// default off. This covers the full store round trip: registered default,
// SetFeatureFlag persistence in both directions, and FeatureFlags merge
// (the same paths PATCH /api/v1/admin/features and /api/v1/me rely on).
func TestSpendEmphasisFlagDefaultsOffAndRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flags, err := s.FeatureFlags(ctx)
	if err != nil {
		t.Fatalf("read flags: %v", err)
	}
	enabled, ok := flags["spend_emphasis"]
	if !ok {
		t.Fatal("spend_emphasis must be a registered feature flag")
	}
	if enabled {
		t.Fatal("spend_emphasis must default to off (usage emphasis)")
	}
	if err := s.SetFeatureFlag(ctx, "spend_emphasis", true); err != nil {
		t.Fatalf("set spend_emphasis: %v", err)
	}
	flags, err = s.FeatureFlags(ctx)
	if err != nil {
		t.Fatalf("re-read flags: %v", err)
	}
	if !flags["spend_emphasis"] {
		t.Fatal("spend_emphasis change did not persist")
	}
	if err := s.SetFeatureFlag(ctx, "spend_emphasis", false); err != nil {
		t.Fatalf("clear spend_emphasis: %v", err)
	}
	flags, err = s.FeatureFlags(ctx)
	if err != nil {
		t.Fatalf("re-read flags after clear: %v", err)
	}
	if flags["spend_emphasis"] {
		t.Fatal("spend_emphasis must round-trip back to off")
	}
}

// TestUpstreamTimeoutOverridesRoundTrip covers the persistence contract behind
// PATCH /api/v1/admin/system/upstream-timeouts: nothing stored reads as the
// zero set (every hop inherits its environment default), partial sets are
// representable, saves upsert in place, the row's updated_at is surfaced, and
// storing an all-zero set removes the row — so "reverted" and "never set" are
// indistinguishable, and a reopened database sees the saved values.
func TestUpstreamTimeoutOverridesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus-timeouts.db")
	s, err := Open(context.Background(), "sqlite://"+path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()

	got, err := s.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		t.Fatalf("read overrides on a fresh database: %v", err)
	}
	if !got.IsZero() || !got.UpdatedAt.IsZero() {
		t.Fatalf("fresh database must report no overrides, got %+v", got)
	}

	// A partial set: only TTFB is overridden; connect and total keep their
	// environment defaults.
	if err := s.SetUpstreamTimeoutOverrides(ctx, UpstreamTimeoutOverrides{TTFBSeconds: 120}); err != nil {
		t.Fatalf("set ttfb override: %v", err)
	}
	got, err = s.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		t.Fatalf("read overrides: %v", err)
	}
	if got.ConnectSeconds != 0 || got.TTFBSeconds != 120 || got.TotalSeconds != 0 {
		t.Fatalf("overrides = %+v, want only ttfb=120", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("a stored override must carry the row's updated_at")
	}

	// Saving again upserts rather than duplicating or failing on the key.
	if err := s.SetUpstreamTimeoutOverrides(ctx, UpstreamTimeoutOverrides{ConnectSeconds: 15, TTFBSeconds: 300, TotalSeconds: 900}); err != nil {
		t.Fatalf("replace overrides: %v", err)
	}

	// Survives a restart: reopen the same file and read it back.
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err = reopened.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		t.Fatalf("read overrides after reopen: %v", err)
	}
	if got.ConnectSeconds != 15 || got.TTFBSeconds != 300 || got.TotalSeconds != 900 {
		t.Fatalf("overrides after reopen = %+v, want 15/300/900", got)
	}

	// Storing the zero set deletes the row.
	if err := reopened.SetUpstreamTimeoutOverrides(ctx, UpstreamTimeoutOverrides{}); err != nil {
		t.Fatalf("clear via zero set: %v", err)
	}
	got, err = reopened.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if !got.IsZero() || !got.UpdatedAt.IsZero() {
		t.Fatalf("zero set must remove the row entirely, got %+v", got)
	}
	var rows int
	if err := reopened.queryRow(ctx, `SELECT COUNT(*) FROM system_setting WHERE key = ?`, SettingUpstreamTimeouts).Scan(&rows); err != nil {
		t.Fatalf("count setting rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("system_setting still holds %d upstream_timeouts row(s) after clear", rows)
	}

	// ClearUpstreamTimeoutOverrides is the explicit form of the same revert.
	if err := reopened.SetUpstreamTimeoutOverrides(ctx, UpstreamTimeoutOverrides{TotalSeconds: 1200}); err != nil {
		t.Fatalf("set total override: %v", err)
	}
	if err := reopened.ClearUpstreamTimeoutOverrides(ctx); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	if got, _ = reopened.UpstreamTimeoutOverrides(ctx); !got.IsZero() {
		t.Fatalf("clear must remove every override, got %+v", got)
	}
}

// TestUpstreamTimeoutOverridesValidation locks the store-level guard rails:
// negative values and values past the 24h ceiling are refused before anything
// is written.
func TestUpstreamTimeoutOverridesValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for name, bad := range map[string]UpstreamTimeoutOverrides{
		"negative connect":  {ConnectSeconds: -1},
		"negative ttfb":     {TTFBSeconds: -30},
		"negative total":    {TotalSeconds: -600},
		"ttfb past ceiling": {TTFBSeconds: MaxUpstreamTimeoutSeconds + 1},
	} {
		if err := s.SetUpstreamTimeoutOverrides(ctx, bad); err == nil {
			t.Errorf("%s: SetUpstreamTimeoutOverrides(%+v) must fail", name, bad)
		}
	}
	got, err := s.UpstreamTimeoutOverrides(ctx)
	if err != nil {
		t.Fatalf("read overrides: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("rejected writes must leave nothing behind, got %+v", got)
	}
	// The ceiling itself is allowed.
	if err := s.SetUpstreamTimeoutOverrides(ctx, UpstreamTimeoutOverrides{TotalSeconds: MaxUpstreamTimeoutSeconds}); err != nil {
		t.Fatalf("the 24h ceiling must be accepted: %v", err)
	}
}

func TestRateCardVersioningKeepsHistoricalRates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "openai", "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "gpt-4o", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil || len(models) != 1 {
		t.Fatalf("list models: %v", err)
	}
	model := models[0]

	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)
	if err := s.SetModelRates(ctx, model.ID, RateCard{RateInNano: 1000, RateOutNano: 2000, RateCachedNano: 100, EffectiveFrom: old}); err != nil {
		t.Fatalf("set original rates: %v", err)
	}
	if err := s.SetModelRates(ctx, model.ID, RateCard{RateInNano: 5000, RateOutNano: 9000, RateCachedNano: 500, EffectiveFrom: recent}); err != nil {
		t.Fatalf("set new rates: %v", err)
	}

	rc, err := s.RateCardAt(ctx, model.ID, old.Add(time.Hour))
	if err != nil {
		t.Fatalf("historical rate lookup: %v", err)
	}
	if rc.RateInNano != 1000 || rc.RateOutNano != 2000 {
		t.Fatalf("historical rates = (%d,%d), want the rate in force at the time (1000,2000)", rc.RateInNano, rc.RateOutNano)
	}

	rc, err = s.RateCardAt(ctx, model.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("current rate lookup: %v", err)
	}
	if rc.RateInNano != 5000 || rc.RateOutNano != 9000 {
		t.Fatalf("current rates = (%d,%d), want (5000,9000)", rc.RateInNano, rc.RateOutNano)
	}
}

func TestStaleModelsAreFlaggedNotDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "ollama", "ollama", "http://localhost:11434", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	for _, name := range []string{"llama3", "mistral"} {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if _, err := s.MarkStaleModels(ctx, up.ID, []string{"llama3"}); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("stale models were deleted; %d rows remain, want 2", len(models))
	}
	for _, m := range models {
		if m.Name == "mistral" && m.Status != ModelStale {
			t.Fatalf("vanished model status = %q, want %q", m.Status, ModelStale)
		}
	}
}

// seedEnabledModel discovers a model on the given upstream, flips it to
// enabled, and returns the persisted row.
func seedEnabledModel(t *testing.T, s *Store, upstreamID, name string) *Model {
	t.Helper()
	ctx := context.Background()
	if _, err := s.UpsertDiscoveredModel(ctx, upstreamID, name, []string{"chat"}); err != nil {
		t.Fatalf("seed model %s: %v", name, err)
	}
	m := modelNamed(t, s, name)
	if err := s.SetModelStatus(ctx, m.ID, ModelEnabled); err != nil {
		t.Fatalf("enable model %s: %v", name, err)
	}
	m.Status = ModelEnabled
	return m
}

// modelNamed loads one model row by its native name across all statuses.
func modelNamed(t *testing.T, s *Store, name string) *Model {
	t.Helper()
	models, err := s.ListModels(context.Background(), ModelFilter{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q not found", name)
	return nil
}

// TestMigrationModelDisplayName pins the 0012 migration: a fresh database
// carries the column, new rows default to the empty string (never NULL), and
// PublicName falls back to the native name.
func TestMigrationModelDisplayName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("read applied migrations: %v", err)
	}
	found := false
	for _, name := range applied {
		if name == "0012_model_display_name" {
			found = true
		}
	}
	if !found {
		t.Fatalf("0012_model_display_name missing from ledger: %v", applied)
	}

	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")
	if m.DisplayName != "" {
		t.Fatalf("freshly discovered model display_name = %q, want empty", m.DisplayName)
	}
	if got := m.PublicName(); got != "anthropic/fable-5-20260101" {
		t.Fatalf("PublicName with no rename = %q, want the native name", got)
	}
}

// TestModelDisplayNameSetClearRoundTrip locks the rename write path: set
// persists, whitespace is trimmed, empty clears, and unknown ids report
// ErrNotFound.
func TestModelDisplayNameSetClearRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")

	// Surrounding whitespace is trimmed and capitals are folded: the stored
	// value is the routable form a caller can actually send.
	if err := s.SetModelDisplayName(ctx, m.ID, "  Fable-5  "); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	got, err := s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload model: %v", err)
	}
	if got.DisplayName != "fable-5" {
		t.Fatalf("display name = %q, want trimmed %q", got.DisplayName, "fable-5")
	}
	if got.PublicName() != "fable-5" {
		t.Fatalf("PublicName = %q, want the display name", got.PublicName())
	}

	// Clearing (empty or whitespace-only) reverts to the native name.
	if err := s.SetModelDisplayName(ctx, m.ID, "   "); err != nil {
		t.Fatalf("clear display name: %v", err)
	}
	got, err = s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload cleared model: %v", err)
	}
	if got.DisplayName != "" {
		t.Fatalf("display name after clear = %q, want empty", got.DisplayName)
	}
	if got.PublicName() != "anthropic/fable-5-20260101" {
		t.Fatalf("PublicName after clear = %q, want the native name", got.PublicName())
	}

	if err := s.SetModelDisplayName(ctx, "no-such-id", "Anything"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename of unknown model = %v, want ErrNotFound", err)
	}
}

// TestModelByNameResolvesDisplayNameFirst is the routing acceptance:
// "fable-5" resolves the model natively named anthropic/fable-5-20260101,
// the native name keeps resolving, the upstream/display form works, and a
// disabled model's display name never routes.
func TestModelByNameResolvesDisplayNameFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")
	if err := s.SetModelDisplayName(ctx, m.ID, "fable-5"); err != nil {
		t.Fatalf("set display name: %v", err)
	}

	byDisplay, err := s.ModelByName(ctx, "fable-5")
	if err != nil {
		t.Fatalf("resolve by display name: %v", err)
	}
	if byDisplay.ID != m.ID {
		t.Fatalf("display name resolved model %s, want %s", byDisplay.ID, m.ID)
	}

	byNative, err := s.ModelByName(ctx, "anthropic/fable-5-20260101")
	if err != nil {
		t.Fatalf("resolve by native name after rename: %v", err)
	}
	if byNative.ID != m.ID {
		t.Fatalf("native name resolved model %s, want %s (renames must not break existing clients)", byNative.ID, m.ID)
	}

	// upstream/display disambiguation form, matched case-insensitively like
	// the upstream/native form always has been.
	byQualified, err := s.ModelByName(ctx, "Anthropic/fable-5")
	if err != nil {
		t.Fatalf("resolve by upstream/display form: %v", err)
	}
	if byQualified.ID != m.ID {
		t.Fatalf("upstream/display form resolved model %s, want %s", byQualified.ID, m.ID)
	}

	// A disabled model's display name must not participate in routing.
	if err := s.SetModelStatus(ctx, m.ID, ModelDisabled); err != nil {
		t.Fatalf("disable model: %v", err)
	}
	if _, err := s.ModelByName(ctx, "fable-5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled model resolved by display name: %v, want ErrNotFound", err)
	}
}

// TestModelDisplayNameUniqueness locks the validation contract: a display
// name may not collide with another ENABLED model's native name or display
// name (exact match); disabled models never block; re-asserting a model's own
// display name is a no-op success.
func TestModelDisplayNameUniqueness(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	a := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")
	b := seedEnabledModel(t, s, up.ID, "anthropic/quill-2")
	if err := s.SetModelDisplayName(ctx, a.ID, "fable-5"); err != nil {
		t.Fatalf("rename model a: %v", err)
	}

	assertValidationErr := func(err error, when string) {
		t.Helper()
		var v *ValidationError
		if !errors.As(err, &v) {
			t.Fatalf("%s returned %v, want *ValidationError", when, err)
		}
		if v.Field != "display_name" {
			t.Fatalf("%s ValidationError field = %q, want display_name", when, v.Field)
		}
	}

	// Collision with another enabled model's display name.
	assertValidationErr(s.SetModelDisplayName(ctx, b.ID, "fable-5"), "duplicate display name")
	// Collision with another enabled model's native name.
	assertValidationErr(s.SetModelDisplayName(ctx, b.ID, "anthropic/fable-5-20260101"), "display name shadowing a native name")
	// The losing write must not have persisted.
	if got, err := s.ModelByID(ctx, b.ID); err != nil || got.DisplayName != "" {
		t.Fatalf("model b display name = (%q, %v) after rejected writes, want empty", got.DisplayName, err)
	}

	// Re-asserting a model's own current display name is fine.
	if err := s.SetModelDisplayName(ctx, a.ID, "fable-5"); err != nil {
		t.Fatalf("re-assert own display name: %v", err)
	}

	// Disabled models do not participate in routing and must not block.
	if err := s.SetModelStatus(ctx, a.ID, ModelDisabled); err != nil {
		t.Fatalf("disable model a: %v", err)
	}
	if err := s.SetModelDisplayName(ctx, b.ID, "fable-5"); err != nil {
		t.Fatalf("rename against a disabled model's display name: %v", err)
	}
}

// TestUpsertDiscoveredModelPreservesDisplayName pins that re-discovery only
// refreshes discovered_at/modalities/status and never clobbers an admin
// rename.
func TestUpsertDiscoveredModelPreservesDisplayName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")
	if err := s.SetModelDisplayName(ctx, m.ID, "fable-5"); err != nil {
		t.Fatalf("set display name: %v", err)
	}

	created, err := s.UpsertDiscoveredModel(ctx, up.ID, "anthropic/fable-5-20260101", []string{"chat", "embeddings"})
	if err != nil {
		t.Fatalf("re-discover model: %v", err)
	}
	if created {
		t.Fatal("re-discovery must update the existing row, not create a new one")
	}
	got, err := s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload model: %v", err)
	}
	if got.DisplayName != "fable-5" {
		t.Fatalf("display name after re-discovery = %q, want %q (rename must survive)", got.DisplayName, "fable-5")
	}
	if len(got.Modalities) != 2 {
		t.Fatalf("modalities after re-discovery = %v, want the refreshed pair", got.Modalities)
	}

	// A model that went stale and comes back keeps its rename too.
	if _, err := s.MarkStaleModels(ctx, up.ID, nil); err != nil {
		t.Fatalf("mark all stale: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "anthropic/fable-5-20260101", []string{"chat"}); err != nil {
		t.Fatalf("re-discover stale model: %v", err)
	}
	got, err = s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload revived model: %v", err)
	}
	if got.DisplayName != "fable-5" {
		t.Fatalf("display name after stale revival = %q, want %q", got.DisplayName, "fable-5")
	}
}

// TestModelFilterSearchMatchesDisplayName pins that the admin listing search
// matches either the native name or the display name, case-insensitively.
func TestModelFilterSearchMatchesDisplayName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "anthropic/fable-5-20260101")
	seedEnabledModel(t, s, up.ID, "anthropic/quill-2")
	if err := s.SetModelDisplayName(ctx, m.ID, "fable-5"); err != nil {
		t.Fatalf("set display name: %v", err)
	}

	cases := []struct {
		search string
		want   int
	}{
		{"FABLE-5", 1},       // display name, case-insensitive
		{"fable-5-2026", 1},  // native name fragment
		{"quill", 1},         // untouched model still searchable
		{"anthropic/", 2},    // native prefix spans both
		{"no-such-model", 0}, // miss
	}
	for _, tc := range cases {
		got, err := s.ListModels(ctx, ModelFilter{Search: tc.search})
		if err != nil {
			t.Fatalf("search %q: %v", tc.search, err)
		}
		if len(got) != tc.want {
			t.Fatalf("search %q matched %d models, want %d", tc.search, len(got), tc.want)
		}
	}
}

// TestSetModelRatesHighRateRoundTrip is the migration-0013 regression: money
// values above 2^31-1 nano-USD (~$2.147/MTok) must survive a write/read
// round-trip exactly. Fable-class output rates are $50/MTok = 50e9 nano-USD;
// before 0013 that overflowed the 32-bit INTEGER columns on PostgreSQL and
// surfaced as a 500 when saving the rate card.
func TestSetModelRatesHighRateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const (
		rateIn     int64 = 10_000_000_000 // $10 / MTok
		rateOut    int64 = 50_000_000_000 // $50 / MTok — past the int4 limit
		rateCached int64 = 1_000_000_000  // $1 / MTok
	)

	up, err := s.CreateUpstream(ctx, "anthropic", "anthropic", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "claude-fable-5", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil || len(models) != 1 {
		t.Fatalf("list models: %v (%d rows)", err, len(models))
	}
	model := models[0]

	effective := time.Now().UTC().Add(-time.Hour)
	if err := s.SetModelRates(ctx, model.ID, RateCard{RateInNano: rateIn, RateOutNano: rateOut, RateCachedNano: rateCached, EffectiveFrom: effective}); err != nil {
		t.Fatalf("SetModelRates with a $50/MTok rate must not fail: %v", err)
	}

	loaded, err := s.ModelByID(ctx, model.ID)
	if err != nil {
		t.Fatalf("read model back: %v", err)
	}
	if loaded.RateInNano != rateIn || loaded.RateOutNano != rateOut || loaded.RateCachedNano != rateCached {
		t.Fatalf("model rates round-tripped as (%d,%d,%d), want (%d,%d,%d)",
			loaded.RateInNano, loaded.RateOutNano, loaded.RateCachedNano, rateIn, rateOut, rateCached)
	}

	// The versioned rate card must hold the exact values too.
	rc, err := s.RateCardAt(ctx, model.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("rate card lookup: %v", err)
	}
	if rc.RateInNano != rateIn || rc.RateOutNano != rateOut || rc.RateCachedNano != rateCached {
		t.Fatalf("rate card at now = (%d,%d,%d), want (%d,%d,%d)", rc.RateInNano, rc.RateOutNano, rc.RateCachedNano, rateIn, rateOut, rateCached)
	}

	// usage_event.cost_nanousd has the same exposure: 1M output tokens at
	// $50/MTok costs 50e9 nano-USD and must aggregate back exactly.
	const cost int64 = 50_000_000_000
	if err := s.InsertUsageEvent(ctx, &UsageEvent{
		CreatedAt: time.Now().UTC(), UserID: "u-fable", ModelID: model.ID, ModelName: model.Name,
		Modality: "chat", TokensIn: 1_000_000, TokensOut: 1_000_000, CostNano: cost, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert high-cost usage event: %v", err)
	}
	totals, err := s.AggregateUsage(ctx, UsageScope{UserID: "u-fable"})
	if err != nil {
		t.Fatalf("aggregate usage: %v", err)
	}
	if totals.CostNano != cost {
		t.Fatalf("usage cost round-tripped as %d nano-USD, want %d", totals.CostNano, cost)
	}

	// quota.limit_value and quota_ledger.current_value share the exposure:
	// a $5,000 spend cap is 5e12 nano-USD.
	const bigLimit int64 = 5_000_000_000_000
	q := &Quota{SubjectType: "user", SubjectID: "u-fable", Metric: MetricCostUSD, Limit: bigLimit, Window: "calendar_month"}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatalf("create quota with a >int4 limit: %v", err)
	}
	qLoaded, err := s.QuotaByID(ctx, q.ID)
	if err != nil {
		t.Fatalf("read quota back: %v", err)
	}
	if qLoaded.Limit != bigLimit {
		t.Fatalf("quota limit round-tripped as %d, want %d", qLoaded.Limit, bigLimit)
	}
	window := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := s.SetLedger(ctx, q.ID, window, bigLimit-1); err != nil {
		t.Fatalf("set ledger with a >int4 value: %v", err)
	}
	if v, err := s.LedgerValue(ctx, q.ID, window); err != nil || v != bigLimit-1 {
		t.Fatalf("ledger value round-tripped as (%d, %v), want (%d, nil)", v, err, bigLimit-1)
	}
}

// widened lists every (table, column) pair migration 0013 must convert to
// BIGINT on PostgreSQL: the rate columns on model and rate_card_version plus
// the same-exposure money/counter columns.
var widened = []struct{ table, column string }{
	{"model", "rate_in_nanousd"},
	{"model", "rate_out_nanousd"},
	{"model", "rate_cached_nanousd"},
	{"rate_card_version", "rate_in_nanousd"},
	{"rate_card_version", "rate_out_nanousd"},
	{"rate_card_version", "rate_cached_nanousd"},
	{"usage_event", "cost_nanousd"},
	{"quota", "limit_value"},
	{"quota_ledger", "current_value"},
}

// added lists every column migration 0013 introduces, all NOT NULL DEFAULT 0.
var added = []struct{ table, column string }{
	{"model", "rate_cache_write_5m_nanousd"},
	{"model", "rate_cache_write_1h_nanousd"},
	{"rate_card_version", "rate_cache_write_5m_nanousd"},
	{"rate_card_version", "rate_cache_write_1h_nanousd"},
	{"usage_event", "tokens_cache_write_5m"},
	{"usage_event", "tokens_cache_write_1h"},
}

func findMigration(t *testing.T, name string) *migration {
	t.Helper()
	for i := range migrations {
		if migrations[i].name == name {
			return &migrations[i]
		}
	}
	t.Fatalf("migration %s is not registered in internal/store/schema.go", name)
	return nil
}

// hasStatement reports whether any statement in stmts targets the table and
// contains every fragment (whitespace-normalised, case-sensitive SQL as
// written in schema.go).
func hasStatement(stmts []string, table string, fragments ...string) bool {
	for _, stmt := range stmts {
		norm := strings.Join(strings.Fields(stmt), " ")
		if !strings.Contains(norm, "ALTER TABLE "+table+" ") {
			continue
		}
		ok := true
		for _, f := range fragments {
			if !strings.Contains(norm, f) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestMigration0013DDL is the DDL-level proof for the PostgreSQL dialect: the
// pod cannot run a live Postgres (final audit does), so this asserts the exact
// SQL migration 0013 emits — TYPE BIGINT for every widened money column, and
// BIGINT NOT NULL DEFAULT 0 for every added cache-write column — plus a
// complete, symmetric down migration.
func TestMigration0013DDL(t *testing.T) {
	m := findMigration(t, "0013_bigint_money_cache_write_rates")

	for _, w := range widened {
		t.Run("widen/"+w.table+"."+w.column, func(t *testing.T) {
			if !hasStatement(m.pgStmt, w.table, "ALTER COLUMN "+w.column+" TYPE BIGINT") {
				t.Errorf("pgStmt is missing `ALTER TABLE %s ALTER COLUMN %s TYPE BIGINT`", w.table, w.column)
			}
			if !hasStatement(m.pgDown, w.table, "ALTER COLUMN "+w.column+" TYPE INTEGER") {
				t.Errorf("pgDown is missing the (lossy) narrowing of %s.%s back to INTEGER", w.table, w.column)
			}
		})
	}
	for _, a := range added {
		t.Run("add/"+a.table+"."+a.column, func(t *testing.T) {
			if !hasStatement(m.stmt, a.table, "ADD COLUMN "+a.column+" BIGINT NOT NULL DEFAULT 0") {
				t.Errorf("stmt is missing `ALTER TABLE %s ADD COLUMN %s BIGINT NOT NULL DEFAULT 0`", a.table, a.column)
			}
			if !hasStatement(m.down, a.table, "DROP COLUMN "+a.column) {
				t.Errorf("down is missing `ALTER TABLE %s DROP COLUMN %s`", a.table, a.column)
			}
		})
	}

	// The widening must never appear in the shared statements: SQLite does
	// not support ALTER COLUMN and would fail the whole migration.
	for _, stmt := range m.stmt {
		if strings.Contains(stmt, "ALTER COLUMN") {
			t.Errorf("shared stmt contains dialect-specific DDL that SQLite cannot run: %s", stmt)
		}
	}
}

// TestMigration0013UpDownSQLite proves 0013 applies, rolls back, and
// re-applies on the embedded engine, and that the added columns really exist
// with a 0 default for insert paths that do not name them.
func TestMigration0013UpDownSQLite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const name = "0013_bigint_money_cache_write_rates"

	columnPresent := func(table, column string) bool {
		// Selecting the column compiles only when it exists; no rows needed.
		rows, err := s.query(ctx, `SELECT `+column+` FROM `+table+` LIMIT 1`)
		if err != nil {
			return false
		}
		defer func() { _ = rows.Close() }()
		return rows.Err() == nil
	}
	appliedNames := func() map[string]bool {
		names, err := s.AppliedMigrations(ctx)
		if err != nil {
			t.Fatalf("read applied migrations: %v", err)
		}
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		return set
	}

	// Up: newTestStore already migrated; the columns and ledger entry exist.
	if !appliedNames()[name] {
		t.Fatalf("migration %s not recorded after Open", name)
	}
	for _, a := range added {
		if !columnPresent(a.table, a.column) {
			t.Fatalf("after migrate, column %s.%s is missing", a.table, a.column)
		}
	}

	// The added columns default to 0 on insert paths that predate them.
	up, err := s.CreateUpstream(ctx, "local", "openai_compatible", "http://127.0.0.1:1", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "self-hosted", []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil || len(models) != 1 {
		t.Fatalf("list models: %v (%d rows)", err, len(models))
	}
	if err := s.SetModelRates(ctx, models[0].ID, RateCard{EffectiveFrom: time.Now().UTC()}); err != nil {
		t.Fatalf("write rate card version: %v", err)
	}
	var w5m, w1h int64
	if err := s.queryRow(ctx, `SELECT rate_cache_write_5m_nanousd, rate_cache_write_1h_nanousd FROM rate_card_version LIMIT 1`).Scan(&w5m, &w1h); err != nil {
		t.Fatalf("read cache-write defaults: %v", err)
	}
	if w5m != 0 || w1h != 0 {
		t.Fatalf("cache-write columns default to (%d,%d), want (0,0)", w5m, w1h)
	}

	// Down: the added columns disappear and the ledger entry is removed.
	if err := s.RollbackMigration(ctx, name); err != nil {
		t.Fatalf("rollback %s: %v", name, err)
	}
	if appliedNames()[name] {
		t.Fatalf("migration %s still recorded after rollback", name)
	}
	for _, a := range added {
		if columnPresent(a.table, a.column) {
			t.Fatalf("after rollback, column %s.%s still exists", a.table, a.column)
		}
	}

	// Rollback guards: double rollback and forward-only migrations refuse.
	if err := s.RollbackMigration(ctx, name); err == nil {
		t.Fatal("rolling back an unapplied migration must fail")
	}
	if err := s.RollbackMigration(ctx, "0001_core_identity"); err == nil {
		t.Fatal("rolling back a forward-only migration must fail")
	}
	if err := s.RollbackMigration(ctx, "9999_never_shipped"); err == nil {
		t.Fatal("rolling back an unknown migration must fail")
	}

	// Up again: Migrate re-applies 0013 and the schema is whole.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	if !appliedNames()[name] {
		t.Fatalf("migration %s not re-recorded after re-migrate", name)
	}
	for _, a := range added {
		if !columnPresent(a.table, a.column) {
			t.Fatalf("after re-migrate, column %s.%s is missing", a.table, a.column)
		}
	}
}

// seedRateModel creates an upstream with one discovered model and returns the
// model, ready for rate-card writes.
func seedRateModel(t *testing.T, s *Store, upstream, model string) *Model {
	t.Helper()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, upstream, "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, model, []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil || len(models) != 1 {
		t.Fatalf("list models: %v (%d rows)", err, len(models))
	}
	return models[0]
}

// TestRateCardCacheWriteRoundTrip proves the two cache-write dimensions
// persist exactly on both the model row and the versioned rate card,
// including the boundary values 0 (self-hosted, explicitly free) and
// 50e9 nano-USD ($50/MTok, past the old int4 limit).
func TestRateCardCacheWriteRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		w5m, w1h   int64
		in, out, c int64
	}{
		{name: "explicit zero", w5m: 0, w1h: 0, in: 0, out: 0, c: 0},
		{name: "fable-class high rates", w5m: 50_000_000_000, w1h: 25_000_000_000, in: 10_000_000_000, out: 50_000_000_000, c: 1_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			model := seedRateModel(t, s, "anthropic", "claude-fable-5")

			effective := time.Now().UTC().Add(-time.Hour)
			card := RateCard{
				RateInNano: tc.in, RateOutNano: tc.out, RateCachedNano: tc.c,
				RateCacheWrite5mNano: tc.w5m, RateCacheWrite1hNano: tc.w1h,
				EffectiveFrom: effective,
			}
			if err := s.SetModelRates(ctx, model.ID, card); err != nil {
				t.Fatalf("SetModelRates: %v", err)
			}

			loaded, err := s.ModelByID(ctx, model.ID)
			if err != nil {
				t.Fatalf("read model back: %v", err)
			}
			if loaded.RateCacheWrite5mNano != tc.w5m || loaded.RateCacheWrite1hNano != tc.w1h {
				t.Fatalf("model cache-write rates = (%d,%d), want (%d,%d)",
					loaded.RateCacheWrite5mNano, loaded.RateCacheWrite1hNano, tc.w5m, tc.w1h)
			}
			if loaded.RateInNano != tc.in || loaded.RateOutNano != tc.out || loaded.RateCachedNano != tc.c {
				t.Fatalf("model rates = (%d,%d,%d), want (%d,%d,%d)",
					loaded.RateInNano, loaded.RateOutNano, loaded.RateCachedNano, tc.in, tc.out, tc.c)
			}

			rc, err := s.RateCardAt(ctx, model.ID, time.Now().UTC())
			if err != nil {
				t.Fatalf("rate card lookup: %v", err)
			}
			if rc.RateCacheWrite5mNano != tc.w5m || rc.RateCacheWrite1hNano != tc.w1h {
				t.Fatalf("rate card cache-write rates = (%d,%d), want (%d,%d)",
					rc.RateCacheWrite5mNano, rc.RateCacheWrite1hNano, tc.w5m, tc.w1h)
			}
			if rc.RateInNano != tc.in || rc.RateOutNano != tc.out || rc.RateCachedNano != tc.c {
				t.Fatalf("rate card = (%d,%d,%d), want (%d,%d,%d)",
					rc.RateInNano, rc.RateOutNano, rc.RateCachedNano, tc.in, tc.out, tc.c)
			}
			if !rc.EffectiveFrom.Equal(effective.Truncate(time.Second)) && !rc.EffectiveFrom.Equal(effective) {
				t.Fatalf("rate card effective_from = %v, want %v", rc.EffectiveFrom, effective)
			}
		})
	}
}

// TestRateCardVersioningCacheWrite proves cache-write rates version like the
// other three dimensions: an old effective_from resolves the old prices, the
// new one resolves the new prices, and a future timestamp gets the latest.
func TestRateCardVersioningCacheWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := seedRateModel(t, s, "anthropic", "claude-opus-5")

	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)
	if err := s.SetModelRates(ctx, model.ID, RateCard{
		RateInNano: 1000, RateOutNano: 2000, RateCachedNano: 100,
		RateCacheWrite5mNano: 1250, RateCacheWrite1hNano: 2500,
		EffectiveFrom: old,
	}); err != nil {
		t.Fatalf("set original rates: %v", err)
	}
	if err := s.SetModelRates(ctx, model.ID, RateCard{
		RateInNano: 5000, RateOutNano: 9000, RateCachedNano: 500,
		RateCacheWrite5mNano: 6250, RateCacheWrite1hNano: 12500,
		EffectiveFrom: recent,
	}); err != nil {
		t.Fatalf("set new rates: %v", err)
	}

	checks := []struct {
		name     string
		at       time.Time
		w5m, w1h int64
	}{
		{"between versions resolves the old card", old.Add(time.Hour), 1250, 2500},
		{"after the new version resolves the new card", time.Now().UTC(), 6250, 12500},
		{"future timestamps resolve the latest card", time.Now().UTC().Add(24 * time.Hour), 6250, 12500},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			rc, err := s.RateCardAt(ctx, model.ID, c.at)
			if err != nil {
				t.Fatalf("rate card lookup at %v: %v", c.at, err)
			}
			if rc.RateCacheWrite5mNano != c.w5m || rc.RateCacheWrite1hNano != c.w1h {
				t.Fatalf("cache-write rates at %v = (%d,%d), want (%d,%d)",
					c.at, rc.RateCacheWrite5mNano, rc.RateCacheWrite1hNano, c.w5m, c.w1h)
			}
		})
	}
}

// TestRateCardAtLegacyVersionReadsZeroCacheWrite simulates a rate_card_version
// row written before migration 0013 (no cache-write columns named on insert):
// RateCardAt must resolve it with both cache-write rates at 0 — exactly what
// was billed at the time — while the three legacy dimensions come back intact.
func TestRateCardAtLegacyVersionReadsZeroCacheWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := seedRateModel(t, s, "openai", "gpt-4o")

	effective := time.Now().UTC().Add(-24 * time.Hour)
	if err := s.exec(ctx,
		`INSERT INTO rate_card_version (id, model_id, rate_in_nanousd, rate_out_nanousd, rate_cached_nanousd, effective_from, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		NewID(), model.ID, 3000, 15000, 300, FormatTime(effective), FormatTime(effective)); err != nil {
		t.Fatalf("insert legacy rate card version: %v", err)
	}

	rc, err := s.RateCardAt(ctx, model.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("rate card lookup: %v", err)
	}
	if rc.RateInNano != 3000 || rc.RateOutNano != 15000 || rc.RateCachedNano != 300 {
		t.Fatalf("legacy dimensions = (%d,%d,%d), want (3000,15000,300)", rc.RateInNano, rc.RateOutNano, rc.RateCachedNano)
	}
	if rc.RateCacheWrite5mNano != 0 || rc.RateCacheWrite1hNano != 0 {
		t.Fatalf("pre-0013 cache-write rates = (%d,%d), want (0,0)", rc.RateCacheWrite5mNano, rc.RateCacheWrite1hNano)
	}
}

// TestSetModelRatesRejectsNegative proves every dimension is validated and a
// rejected write leaves no partial version behind.
func TestSetModelRatesRejectsNegative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := seedRateModel(t, s, "openai", "gpt-4o")

	base := RateCard{RateInNano: 1, RateOutNano: 1, RateCachedNano: 1, RateCacheWrite5mNano: 1, RateCacheWrite1hNano: 1, EffectiveFrom: time.Now().UTC()}
	mutations := []struct {
		name   string
		mutate func(*RateCard)
	}{
		{"rate_in", func(rc *RateCard) { rc.RateInNano = -1 }},
		{"rate_out", func(rc *RateCard) { rc.RateOutNano = -1 }},
		{"rate_cached", func(rc *RateCard) { rc.RateCachedNano = -1 }},
		{"rate_cache_write_5m", func(rc *RateCard) { rc.RateCacheWrite5mNano = -1 }},
		{"rate_cache_write_1h", func(rc *RateCard) { rc.RateCacheWrite1hNano = -1 }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			rc := base
			m.mutate(&rc)
			if err := s.SetModelRates(ctx, model.ID, rc); !errors.Is(err, ErrNegativeRate) {
				t.Fatalf("SetModelRates with negative %s = %v, want ErrNegativeRate", m.name, err)
			}
		})
	}
	var versions int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM rate_card_version WHERE model_id = ?`, model.ID).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 0 {
		t.Fatalf("rejected writes left %d rate card versions behind, want 0", versions)
	}
}

// TestSetModelRatesUnknownModel proves pricing a model that does not exist
// fails with ErrNotFound and writes nothing.
func TestSetModelRatesUnknownModel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	err := s.SetModelRates(ctx, "no-such-model", RateCard{RateInNano: 1000, EffectiveFrom: time.Now().UTC()})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetModelRates on a missing model = %v, want ErrNotFound", err)
	}
	var versions int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM rate_card_version`).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 0 {
		t.Fatalf("missing-model write left %d rate card versions behind, want 0", versions)
	}
}

// TestListRequestsTokenSizeFiltersAndSort proves the request log can be
// narrowed by strict token-size thresholds on each direction independently
// and ordered by either token column in both directions.
func TestListRequestsTokenSizeFiltersAndSort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, pair := range [][2]int64{{500, 50}, {1000, 20_000}, {15_000, 900}, {600_000, 120_000}} {
		ev := &UsageEvent{
			UserID: "u1", ModelName: "m", Modality: "chat", HTTPStatus: 200,
			TokensIn: pair[0], TokensOut: pair[1], CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := s.InsertUsageEvent(ctx, ev); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n := func(v int64) *int64 { return &v }

	// Greater-than is strict: the 1,000-token row is excluded.
	events, total, err := s.ListRequests(ctx, RequestFilter{UserID: "u1", TokensInGT: n(1000)})
	if err != nil {
		t.Fatalf("list tokens_in > 1000: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("tokens_in > 1000 matched %d rows (total %d), want 2", len(events), total)
	}
	for _, e := range events {
		if e.TokensIn <= 1000 {
			t.Fatalf("tokens_in > 1000 returned row with tokens_in=%d", e.TokensIn)
		}
	}
	// Smaller-than on the other direction, independent of tokens_in.
	events, total, err = s.ListRequests(ctx, RequestFilter{UserID: "u1", TokensOutLT: n(1000)})
	if err != nil {
		t.Fatalf("list tokens_out < 1000: %v", err)
	}
	if total != 2 {
		t.Fatalf("tokens_out < 1000 total = %d, want 2", total)
	}
	for _, e := range events {
		if e.TokensOut >= 1000 {
			t.Fatalf("tokens_out < 1000 returned row with tokens_out=%d", e.TokensOut)
		}
	}
	// Both directions combine with AND.
	_, total, err = s.ListRequests(ctx, RequestFilter{UserID: "u1", TokensInGT: n(10_000), TokensOutGT: n(100_000)})
	if err != nil {
		t.Fatalf("list combined: %v", err)
	}
	if total != 1 {
		t.Fatalf("combined thresholds total = %d, want 1", total)
	}

	// Sorting by each token column, descending by default and `_asc` reversed.
	for _, tc := range []struct {
		sort  string
		pick  func(*UsageEvent) int64
		first int64
	}{
		{"tokens_in", func(e *UsageEvent) int64 { return e.TokensIn }, 600_000},
		{"tokens_in_asc", func(e *UsageEvent) int64 { return e.TokensIn }, 500},
		{"tokens_out", func(e *UsageEvent) int64 { return e.TokensOut }, 120_000},
		{"tokens_out_asc", func(e *UsageEvent) int64 { return e.TokensOut }, 50},
	} {
		events, _, err := s.ListRequests(ctx, RequestFilter{UserID: "u1", Sort: tc.sort})
		if err != nil {
			t.Fatalf("list sort=%s: %v", tc.sort, err)
		}
		if len(events) != 4 || tc.pick(events[0]) != tc.first {
			t.Fatalf("sort=%s first row = %d, want %d", tc.sort, tc.pick(events[0]), tc.first)
		}
	}
}

// TestUsageEventCacheWriteTokensRoundTrip proves the two cache-write token
// counts (migration 0013) persist exactly as written and survive both read
// paths: the request-log query and a direct column select. These counts are
// what historical repricing of cache-write rates depends on.
func TestUsageEventCacheWriteTokensRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ev := &UsageEvent{
		UserID: "u1", ModelName: "claude-fable-5", Modality: "chat",
		TokensIn: 1000, TokensOut: 200, TokensCached: 300,
		TokensCacheWrite5m: 100, TokensCacheWrite1h: 50,
		CostNano: 12_345, HTTPStatus: 200,
	}
	if err := s.InsertUsageEvent(ctx, ev); err != nil {
		t.Fatalf("insert usage event with cache-write tokens: %v", err)
	}
	// A legacy-shaped event (no cache-write fields set) must persist zeros.
	legacy := &UsageEvent{UserID: "u1", ModelName: "gpt-4o", Modality: "chat", TokensIn: 10, HTTPStatus: 200}
	if err := s.InsertUsageEvent(ctx, legacy); err != nil {
		t.Fatalf("insert legacy usage event: %v", err)
	}

	events, total, err := s.ListRequests(ctx, RequestFilter{UserID: "u1"})
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("listed %d events (total %d), want 2", len(events), total)
	}
	byID := map[string]*UsageEvent{}
	for _, e := range events {
		byID[e.ID] = e
	}
	got, ok := byID[ev.ID]
	if !ok {
		t.Fatalf("cache-write event %s missing from request log", ev.ID)
	}
	if got.TokensCacheWrite5m != 100 || got.TokensCacheWrite1h != 50 {
		t.Fatalf("read-back cache-write tokens = (%d, %d), want (100, 50)", got.TokensCacheWrite5m, got.TokensCacheWrite1h)
	}
	if got.TokensIn != 1000 || got.TokensOut != 200 || got.TokensCached != 300 || got.CostNano != 12_345 {
		t.Fatalf("read-back event mutated other fields: %+v", got)
	}
	if l := byID[legacy.ID]; l == nil || l.TokensCacheWrite5m != 0 || l.TokensCacheWrite1h != 0 {
		t.Fatalf("legacy event cache-write tokens = %+v, want zeros", l)
	}

	// Straight from the table: the columns themselves hold the exact values.
	var w5m, w1h int64
	if err := s.queryRow(ctx, `SELECT tokens_cache_write_5m, tokens_cache_write_1h FROM usage_event WHERE id = ?`, ev.ID).
		Scan(&w5m, &w1h); err != nil {
		t.Fatalf("select cache-write columns: %v", err)
	}
	if w5m != 100 || w1h != 50 {
		t.Fatalf("persisted cache-write columns = (%d, %d), want (100, 50)", w5m, w1h)
	}
}

// TestMigration0014DDL pins the shipped DDL for the context-window column:
// a shared ADD COLUMN with INTEGER affinity (64-bit on SQLite, int4 on
// PostgreSQL — ample for token counts), NOT NULL DEFAULT 0 so pre-existing
// rows read back "unknown", and a symmetric down migration. No
// dialect-specific statements are needed, so pgStmt/pgDown must stay empty.
func TestMigration0014DDL(t *testing.T) {
	m := findMigration(t, "0014_model_context_window")

	if !hasStatement(m.stmt, "model", "ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0") {
		t.Error("stmt is missing `ALTER TABLE model ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0`")
	}
	if !hasStatement(m.down, "model", "DROP COLUMN context_window") {
		t.Error("down is missing `ALTER TABLE model DROP COLUMN context_window`")
	}
	if len(m.pgStmt) != 0 || len(m.pgDown) != 0 {
		t.Errorf("0014 must be dialect-shared DDL only; got %d pgStmt / %d pgDown statements", len(m.pgStmt), len(m.pgDown))
	}
	// SQLite cannot run ALTER COLUMN; the shared statements must never carry it.
	for _, stmt := range m.stmt {
		if strings.Contains(stmt, "ALTER COLUMN") {
			t.Errorf("shared stmt contains dialect-specific DDL that SQLite cannot run: %s", stmt)
		}
	}
}

// TestMigration0014UpDownSQLite proves 0014 applies, rolls back, and
// re-applies on the embedded engine, and that rows created without naming the
// column — i.e. every row that predates the migration — read back 0.
func TestMigration0014UpDownSQLite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const name = "0014_model_context_window"

	columnPresent := func() bool {
		rows, err := s.query(ctx, `SELECT context_window FROM model LIMIT 1`)
		if err != nil {
			return false
		}
		defer func() { _ = rows.Close() }()
		return rows.Err() == nil
	}
	appliedNames := func() map[string]bool {
		names, err := s.AppliedMigrations(ctx)
		if err != nil {
			t.Fatalf("read applied migrations: %v", err)
		}
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		return set
	}

	// Up: newTestStore already migrated; column and ledger entry exist.
	if !appliedNames()[name] {
		t.Fatalf("migration %s not recorded after Open", name)
	}
	if !columnPresent() {
		t.Fatal("after migrate, column model.context_window is missing")
	}

	// Insert paths that do not name the column get the 0 = "unknown" default.
	model := seedRateModel(t, s, "openai", "some-model")
	if model.ContextWindow != 0 {
		t.Fatalf("freshly discovered model context_window = %d, want 0 (unknown)", model.ContextWindow)
	}

	// Down: the column disappears and the ledger entry is removed.
	if err := s.RollbackMigration(ctx, name); err != nil {
		t.Fatalf("rollback %s: %v", name, err)
	}
	if appliedNames()[name] {
		t.Fatalf("migration %s still recorded after rollback", name)
	}
	if columnPresent() {
		t.Fatal("after rollback, column model.context_window still exists")
	}

	// Up again: Migrate re-applies 0014 and the schema is whole.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	if !appliedNames()[name] {
		t.Fatalf("migration %s not re-recorded after re-migrate", name)
	}
	if !columnPresent() {
		t.Fatal("after re-migrate, column model.context_window is missing")
	}
	// The pre-rollback row reads back 0 — exactly the pre-migration state.
	loaded, err := s.ModelByID(ctx, model.ID)
	if err != nil {
		t.Fatalf("read model after re-migrate: %v", err)
	}
	if loaded.ContextWindow != 0 {
		t.Fatalf("pre-migration row context_window = %d, want 0", loaded.ContextWindow)
	}
}

// TestModelContextWindowRoundTrip proves a stored context window survives
// every model read path (ModelByID, ListModels, ModelByName), that 0 clears
// it back to "unknown", and that invalid writes are rejected: a negative
// count as a *ValidationError on field "context_window" (a 400, never a 500)
// and an unknown model id as ErrNotFound.
func TestModelContextWindowRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := seedRateModel(t, s, "anthropic", "claude-sonnet-5")

	const window int64 = 200_000
	if err := s.SetModelContextWindow(ctx, model.ID, window); err != nil {
		t.Fatalf("SetModelContextWindow: %v", err)
	}

	byID, err := s.ModelByID(ctx, model.ID)
	if err != nil {
		t.Fatalf("ModelByID: %v", err)
	}
	if byID.ContextWindow != window {
		t.Fatalf("ModelByID context_window = %d, want %d", byID.ContextWindow, window)
	}

	listed, err := s.ListModels(ctx, ModelFilter{})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListModels: %v (%d rows)", err, len(listed))
	}
	if listed[0].ContextWindow != window {
		t.Fatalf("ListModels context_window = %d, want %d", listed[0].ContextWindow, window)
	}

	// ModelByName only resolves enabled models; price and enable first.
	if err := s.SetModelRates(ctx, model.ID, RateCard{RateInNano: 1, RateOutNano: 2, EffectiveFrom: time.Now().UTC()}); err != nil {
		t.Fatalf("price model: %v", err)
	}
	if err := s.SetModelStatus(ctx, model.ID, ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	byName, err := s.ModelByName(ctx, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ModelByName: %v", err)
	}
	if byName.ContextWindow != window {
		t.Fatalf("ModelByName context_window = %d, want %d", byName.ContextWindow, window)
	}

	// Zero is a valid write: it clears the value back to "unknown".
	if err := s.SetModelContextWindow(ctx, model.ID, 0); err != nil {
		t.Fatalf("clear context window: %v", err)
	}
	if cleared, _ := s.ModelByID(ctx, model.ID); cleared.ContextWindow != 0 {
		t.Fatalf("cleared context_window = %d, want 0", cleared.ContextWindow)
	}

	// Negative values are refused as a field-specific validation error.
	err = s.SetModelContextWindow(ctx, model.ID, -1)
	var vErr *ValidationError
	if !errors.As(err, &vErr) || vErr.Field != "context_window" {
		t.Fatalf("negative write error = %v, want *ValidationError on context_window", err)
	}
	if after, _ := s.ModelByID(ctx, model.ID); after.ContextWindow != 0 {
		t.Fatalf("rejected write mutated context_window to %d", after.ContextWindow)
	}

	// Unknown models surface as ErrNotFound so the API returns a 404.
	if err := s.SetModelContextWindow(ctx, "no-such-model", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown model error = %v, want ErrNotFound", err)
	}
}

// TestRoutableNameRules locks the shared naming contract for anything a caller
// addresses in the request body's "model" field. Model renames and managed
// aliases must agree: a name you can save must be a name you can call.
func TestRoutableNameRules(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		valid bool
	}{
		{"gpt-4o-mini", "gpt-4o-mini", true},
		{"GPT-4o-Mini", "gpt-4o-mini", true},   // capitals fold, never rejected
		{"  spaced-out  ", "spaced-out", true}, // surrounding space trimmed
		{"qwen3.8:27b", "qwen3.8:27b", true},   // ollama-style tag
		{"org/model_v2", "org/model_v2", true}, // path + underscore
		{"GPT 4o Mini", "", false},             // interior space: the reported bug
		{"best coder", "", false},
		{"emoji✨", "", false},
		{"", "", false},
	} {
		got, err := NormalizeRoutableName(tc.in, "name", 200)
		if tc.valid && err != nil {
			t.Errorf("NormalizeRoutableName(%q) rejected: %v", tc.in, err)
			continue
		}
		if !tc.valid && err == nil {
			t.Errorf("NormalizeRoutableName(%q) accepted %q, want rejection", tc.in, got)
			continue
		}
		if tc.valid && got != tc.want {
			t.Errorf("NormalizeRoutableName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDisplayNameSavedIsCallable is the regression guard for the reported bug:
// a rename that the API accepts must resolve when a caller sends it, in any
// case. Previously "GPT 4o Mini" saved fine and then never routed.
func TestDisplayNameSavedIsCallable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	up, err := s.CreateUpstream(ctx, "OpenAI", "openai_compatible", "http://x/v1", "enc", "mask")
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "gpt-4o-mini")

	// A spaced name is refused up front rather than accepted-then-broken.
	if err := s.SetModelDisplayName(ctx, m.ID, "GPT 4o Mini"); err == nil {
		t.Fatal("a display name with a space must be rejected, not silently unroutable")
	}

	// The capitalised form is accepted and folded.
	if err := s.SetModelDisplayName(ctx, m.ID, "GPT-4o-Mini"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	got, err := s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.DisplayName != "gpt-4o-mini" {
		t.Errorf("stored display name = %q, want lowercased", got.DisplayName)
	}

	// Every casing a caller might send resolves to the same model.
	for _, probe := range []string{"gpt-4o-mini", "GPT-4o-Mini", "GPT-4O-MINI"} {
		resolved, err := s.ModelByName(ctx, probe)
		if err != nil {
			t.Errorf("resolve %q: %v", probe, err)
			continue
		}
		if resolved.ID != m.ID {
			t.Errorf("resolve %q returned %s, want %s", probe, resolved.ID, m.ID)
		}
	}
}

// TestLegacyDisplayNameWithSpaceStillResolves guards the upgrade path: names
// saved before the rule existed keep routing rather than breaking live traffic.
func TestLegacyDisplayNameWithSpaceStillResolves(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	up, err := s.CreateUpstream(ctx, "Local", "llama_cpp", "http://x/v1", "enc", "mask")
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	m := seedEnabledModel(t, s, up.ID, "qwen3.8-27b-q8")

	// Simulate a row written by an older build, bypassing validation.
	if err := s.exec(ctx, `UPDATE model SET display_name = ? WHERE id = ?`, "Qwen 3.8 27B Q8", m.ID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	resolved, err := s.ModelByName(ctx, "Qwen 3.8 27B Q8")
	if err != nil {
		t.Fatalf("legacy spaced display name must keep resolving: %v", err)
	}
	if resolved.ID != m.ID {
		t.Errorf("resolved %s, want %s", resolved.ID, m.ID)
	}
}

// TestAdminGroupLayering locks the ruling that admin capability composes from
// three INDEPENDENT sources, so withdrawing one never collapses the others:
// bootstrap email list -> explicit per-user grant -> admin group membership.
func TestAdminGroupLayering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.AddAdminGroup(ctx, "  Platform-Admins  ", "u1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	groups, err := s.ListAdminGroups(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "platform-admins" {
		t.Fatalf("groups = %+v, want one entry normalised to lowercase", groups)
	}

	// Re-adding is a no-op, not an error: the desired end state is identical.
	if _, err := s.AddAdminGroup(ctx, "platform-admins", "u2"); err != nil {
		t.Errorf("re-adding an existing group must not error: %v", err)
	}
	groups, _ = s.ListAdminGroups(ctx)
	if len(groups) != 1 {
		t.Errorf("re-adding created a duplicate: %+v", groups)
	}

	// Lookup is case-insensitive on both sides.
	names, err := s.AdminGroupNames(ctx)
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if _, ok := names["platform-admins"]; !ok {
		t.Errorf("AdminGroupNames = %+v, want the lowercased name", names)
	}

	// A user promoted through the group, who ALSO holds an explicit grant.
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub-1", "lead@example.com", "Lead", false, true)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpdateUser(ctx, user.ID, RoleAdmin, true); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Removing the group must NOT demote them: the explicit grant is a
	// separate layer and survives on its own.
	if err := s.RemoveAdminGroup(ctx, "Platform-Admins"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if names, _ := s.AdminGroupNames(ctx); len(names) != 0 {
		t.Errorf("group should be gone, got %+v", names)
	}
	after, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Role != RoleAdmin {
		t.Errorf("role = %q after group removal, want admin retained via the explicit grant", after.Role)
	}
}

// TestAdminGroupNameValidation: a blank name is a field error, not a row that
// silently matches nothing.
func TestAdminGroupNameValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, bad := range []string{"", "   "} {
		if _, err := s.AddAdminGroup(ctx, bad, "u1"); err == nil {
			t.Errorf("AddAdminGroup(%q) must be refused", bad)
		}
	}
}
