package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func scimTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStore(t)
}
func TestSCIMUsersProvisionAndClaim(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	r, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "Alice@Example.com", "displayName": "Alice", "active": false, "id": "forged", "urn:example:custom": map[string]any{"costCenter": "42"}})
	if err != nil {
		t.Fatal(err)
	}
	id := r["id"].(string)
	if id == "forged" {
		t.Fatal("client ID trusted")
	}
	u, err := s.UserByID(ctx, id)
	if err != nil || u.AuthID != "scim:"+id || u.IsActive || u.Role != RoleUser {
		t.Fatalf("user %+v %v", u, err)
	}
	if _, err = s.ClaimSCIMIdentity(ctx, "oidc:alice", "alice@example.com", "Alice", false); err == nil {
		t.Fatal("unverified identity claimed")
	}
	u, err = s.ClaimSCIMIdentity(ctx, "oidc:alice", "alice@example.com", "Alice", true)
	if err != nil || u.ID != id || u.IsActive {
		t.Fatalf("claim %+v %v", u, err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "ALICE@example.com"}); err == nil {
		t.Fatal("duplicate username accepted")
	}
	got, err := s.SCIMResource(ctx, "Users", id)
	if err != nil || got["urn:example:custom"] == nil {
		t.Fatal("extension lost", err)
	}
	all, err := s.ListSCIMResources(ctx, "Users")
	if err != nil || len(all) != 1 {
		t.Fatal("listing", err)
	}
	if err = s.DeleteSCIMResource(ctx, "Users", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SCIMResource(ctx, "Users", id); err == nil {
		t.Fatal("tombstone visible")
	}
}
func TestSCIMGroupsAuthoritativeRemoval(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	u, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "group@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	uid := u["id"].(string)
	members := []any{map[string]any{"value": uid}}
	g, err := s.SaveSCIMResource(ctx, "Groups", "", map[string]any{"displayName": "Directory", "members": members})
	if err != nil {
		t.Fatal(err)
	}
	gid := g["id"].(string)
	team, err := s.CreateTeam(ctx, "mapped", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamGroupMappings(ctx, team.ID, []string{gid}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMemberRole(ctx, team.ID, uid, "leader"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Groups", gid, map[string]any{"displayName": "Broken", "members": []any{map[string]any{"value": "missing"}}}); err == nil {
		t.Fatal("invalid member accepted")
	}
	if _, err = s.GroupByName(ctx, "Directory"); err != nil {
		t.Fatal("failed save changed group", err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Groups", gid, map[string]any{"displayName": "Renamed"}); err != nil {
		t.Fatal("directory removal must override last-leader guard", err)
	}
	if _, err = s.TeamRole(ctx, team.ID, uid); err == nil {
		t.Fatal("removed directory leader retains access")
	}
	if _, err = s.SaveSCIMResource(ctx, "Groups", gid, map[string]any{"displayName": "Renamed", "members": members}); err != nil {
		t.Fatal(err)
	}
	if err = s.AddTeamMember(ctx, team.ID, uid, "moderator", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSCIMResource(ctx, "Groups", gid); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TeamRole(ctx, team.ID, uid); err != nil {
		t.Fatal("manual membership removed", err)
	}
	var n int
	if err = s.queryRow(ctx, `SELECT COUNT(*) FROM team_membership_source WHERE source_type='group' AND source_id=?`, gid).Scan(&n); err != nil || n != 0 {
		t.Fatal("group source remains", err)
	}
}
func TestSCIMDeactivationRevokesCredentialsAndGroupSources(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	existing, _, err := s.UpsertUserFromIdentity(ctx, "oidc:existing", "existing@example.com", "Old", true, false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "EXISTING@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	uid := u["id"].(string)
	if uid != existing.ID {
		t.Fatal("existing identity not linked")
	}
	token, _, err := s.CreateToken(ctx, uid, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InsertWebSession(ctx, &WebSession{ID: "session", UserID: uid, CreatedAt: time.Now(), AbsoluteEnds: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	g, err := s.SaveSCIMResource(ctx, "Groups", "", map[string]any{"displayName": "Deactivation", "members": []any{map[string]any{"value": uid}}})
	if err != nil {
		t.Fatal(err)
	}
	gid := g["id"].(string)
	team, err := s.CreateTeam(ctx, "disabled", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamGroupMappings(ctx, team.ID, []string{gid}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMemberRole(ctx, team.ID, uid, "leader"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Users", uid, map[string]any{"userName": "existing@example.com", "active": false, "roles": []any{"superadmin"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserByID(ctx, uid)
	if err != nil || got.IsActive || got.AuthID != existing.AuthID || got.Role != RoleAdmin {
		t.Fatalf("identity changed %+v %v", got, err)
	}
	var revoked string
	if err = s.queryRow(ctx, `SELECT revoked_at FROM api_token WHERE id=?`, token.ID).Scan(&revoked); err != nil || revoked == "" {
		t.Fatal("token not revoked", err)
	}
	if _, err = s.WebSessionByID(ctx, "session"); err == nil {
		t.Fatal("session remains")
	}
	if _, err = s.TeamRole(ctx, team.ID, uid); err == nil {
		t.Fatal("disabled user retains group source")
	}
}
func TestSCIMValidationAndRollback(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	for _, subject := range []string{"one", "two"} {
		if _, _, err := s.UpsertUserFromIdentity(ctx, subject, "duplicate@example.com", subject, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "DUPLICATE@example.com"}); err == nil {
		t.Fatal("ambiguous existing identities linked")
	}
	// A database failure after materialization must roll back the real user too.
	if err := s.exec(ctx, `CREATE TRIGGER scim_test_failure BEFORE INSERT ON scim_resource BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "rollback@example.com"}); err == nil {
		t.Fatal("injected failure ignored")
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM app_user WHERE email='rollback@example.com'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial identity committed", err)
	}
	if err := s.exec(ctx, `DROP TRIGGER scim_test_failure`); err != nil {
		t.Fatal(err)
	}
	u, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "tombstone@example.com", "externalId": "directory-key", "password": "must-not-persist"})
	if err != nil {
		t.Fatal(err)
	}
	id := u["id"].(string)
	if _, ok := u["password"]; ok {
		t.Fatal("password reflected")
	}
	if err = s.DeleteSCIMResource(ctx, "Users", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "new@example.com", "externalId": "directory-key"}); err == nil {
		t.Fatal("tombstone external ID reused")
	}
	claimed, err := s.ClaimSCIMIdentity(ctx, "oidc:tombstone", "tombstone@example.com", "Name", true)
	if err != nil || claimed.IsActive {
		t.Fatalf("deleted identity bypass %+v %v", claimed, err)
	}
	if _, err = s.ClaimSCIMIdentity(ctx, "oidc:attacker", "tombstone@example.com", "Name", true); err == nil {
		t.Fatal("bound identity stolen")
	}
}
func TestSCIMKnownAttributeTypes(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	for _, attr := range []string{"displayName", "name"} {
		r := map[string]any{"userName": "bad@example.com", attr: 42}
		if _, err := s.SaveSCIMResource(ctx, "Users", "", r); err == nil {
			t.Fatalf("invalid %s accepted", attr)
		}
	}
}
func TestSCIMRejectsMalformedEmailAfterPrimary(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	_, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "bad@example.com", "emails": []any{map[string]any{"value": "bad@example.com", "primary": true}, 42}})
	if err == nil {
		t.Fatal("malformed trailing email accepted")
	}
}
func TestSCIMClaimByTrustedExternalSubject(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	r, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "directory-only-name", "externalId": "issuer|immutable-subject", "active": false})
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.ClaimSCIMIdentity(ctx, "issuer|immutable-subject", "email@example.com", "Name", false)
	if err != nil || u.ID != r["id"] || u.IsActive {
		t.Fatalf("external claim %+v %v", u, err)
	}
	// Subsequent immutable-subject lookup must not depend on an email claim.
	u, err = s.ClaimSCIMIdentity(ctx, "issuer|immutable-subject", "directory-only-name", "Name", false)
	if err != nil || u.IsActive {
		t.Fatalf("bound subject %+v %v", u, err)
	}
	r2, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "another@example.com", "externalId": "issuer|other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimSCIMIdentity(ctx, "issuer|other", "directory-only-name", "Name", true); err == nil {
		t.Fatal("conflicting directory links accepted")
	}
	still, err := s.UserByID(ctx, r2["id"].(string))
	if err != nil || still.AuthID != "scim:"+still.ID {
		t.Fatal("conflicting claim mutated identity", err)
	}
}
func TestSCIMTokenLifecycle(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	token, secret, err := s.CreateSCIMToken(ctx, "directory", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "scim_") || len(secret) < 48 {
		t.Fatal("insufficient token entropy")
	}
	var digest string
	if err = s.queryRow(ctx, `SELECT token_digest FROM scim_token WHERE id=?`, token.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != HashToken(secret) {
		t.Fatal("credential not hashed")
	}
	found, err := s.AuthenticateSCIMToken(ctx, secret)
	if err != nil || found.LastUsedAt.IsZero() {
		t.Fatalf("authentication: %v", err)
	}
	listed, err := s.ListSCIMTokens(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatal(err)
	}
	b, _ := json.Marshal(listed)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), digest) {
		t.Fatal("credential disclosure")
	}
	if err = s.RevokeSCIMToken(ctx, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSCIMToken(ctx, secret); err == nil {
		t.Fatal("revoked accepted")
	}
	tok, sec, err := s.CreateSCIMToken(ctx, "expires", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.exec(ctx, `UPDATE scim_token SET expires_at=? WHERE id=?`, FormatTime(time.Now().Add(-time.Hour)), tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSCIMToken(ctx, sec); err == nil {
		t.Fatal("expired accepted")
	}
	if _, _, err = s.CreateSCIMToken(ctx, "bad", time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("past expiry accepted")
	}
}
