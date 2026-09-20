package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// newEnabledModel creates an upstream and one enabled, priced model, returning
// the model. It is the fixture both feature suites below build on.
func newEnabledModel(t *testing.T, s *Store, name string) *Model {
	t.Helper()
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "up-"+NewID(), "openai_compatible", "https://api.example.com", "", "")
	if err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
		t.Fatalf("discover model: %v", err)
	}
	models, err := s.ListModels(ctx, ModelFilter{UpstreamID: up.ID})
	if err != nil || len(models) == 0 {
		t.Fatalf("load discovered model: %v", err)
	}
	m := models[0]
	if err := s.SetModelStatus(ctx, m.ID, ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	reloaded, err := s.ModelByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload model: %v", err)
	}
	return reloaded
}

// --- Service tokens ----------------------------------------------------------

// TestServiceTokenLifecycle locks the credential lifecycle: a minted token is
// usable, a revoked one is not, and an expired one is distinguishable from a
// revoked one so operators can tell "someone turned this off" from "this aged
// out".
func TestServiceTokenLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tok, plaintext, err := s.CreateServiceToken(ctx, "nightly-summariser", "batch job", "", time.Time{})
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}
	if !hasPrefix(plaintext, ServiceTokenPrefix) {
		t.Errorf("plaintext %q does not carry the service prefix %q", plaintext, ServiceTokenPrefix)
	}
	now := time.Now().UTC()
	if !tok.Usable(now) || tok.Status(now) != "active" {
		t.Errorf("fresh token should be active, got %q", tok.Status(now))
	}

	// The digest resolves; the plaintext is not recoverable from storage.
	found, err := s.ServiceTokenByDigest(ctx, HashToken(plaintext))
	if err != nil {
		t.Fatalf("resolve by digest: %v", err)
	}
	if found.ID != tok.ID {
		t.Errorf("digest resolved to %q, want %q", found.ID, tok.ID)
	}

	if err := s.RevokeServiceToken(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revoked, err := s.ServiceTokenByID(ctx, tok.ID)
	if err != nil {
		t.Fatalf("reload revoked: %v", err)
	}
	if revoked.Usable(time.Now().UTC()) {
		t.Error("a revoked token must not be usable")
	}
	if got := revoked.Status(time.Now().UTC()); got != "revoked" {
		t.Errorf("status = %q, want revoked", got)
	}

	// Expiry is a separate state from revocation.
	expired, _, err := s.CreateServiceToken(ctx, "short-lived", "", "", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("create expiring token: %v", err)
	}
	future := time.Now().UTC().Add(2 * time.Hour)
	if expired.Usable(future) {
		t.Error("a token past its expiry must not be usable")
	}
	if got := expired.Status(future); got != "expired" {
		t.Errorf("status = %q, want expired", got)
	}
}

// TestServiceTokenNamesMustBeUnique locks the reporting-key constraint: usage
// is attributed by NAME, so two tokens sharing one would make a usage report
// ambiguous.
func TestServiceTokenNamesMustBeUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.CreateServiceToken(ctx, "shared", "", "", time.Time{}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	_, _, err := s.CreateServiceToken(ctx, "shared", "", "", time.Time{})
	if !errors.Is(err, ErrServiceTokenNameTaken) {
		t.Fatalf("duplicate name error = %v, want ErrServiceTokenNameTaken", err)
	}
}

// TestServiceTokenJSONOmitsZeroTimestamps locks the wire contract that cost the
// user-token type a bug once already: Go's zero time encodes as a truthy
// string, so clients badge active credentials as revoked. Both optional
// timestamps must serialise as "".
func TestServiceTokenJSONOmitsZeroTimestamps(t *testing.T) {
	tok := ServiceToken{ID: "id", Name: "svc"}
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"revoked_at", "expires_at", "last_used_at"} {
		if got := decoded[field]; got != "" {
			t.Errorf("%s = %v, want empty string", field, got)
		}
	}
	if decoded["status"] != "active" {
		t.Errorf("status = %v, want active", decoded["status"])
	}
}

// TestAllUsersGrantDoesNotReachServiceTokens is the central access-control
// guarantee of the feature: broadening access for PEOPLE must never silently
// hand the same model to every unattended integration.
func TestAllUsersGrantDoesNotReachServiceTokens(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := newEnabledModel(t, s, "gpt-4o-mini")
	svc, _, err := s.CreateServiceToken(ctx, "website-bot", "", "", time.Time{})
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}

	if _, err := s.CreateGrant(ctx, model.ID, ModelKindModel, GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant all users: %v", err)
	}
	granted, err := s.GrantedModelIDsForServiceToken(ctx, svc.ID)
	if err != nil {
		t.Fatalf("resolve service token grants: %v", err)
	}
	if _, ok := granted[model.ID]; ok {
		t.Fatal("an all_users grant must NOT reach a service token")
	}

	// The explicit grant does reach it.
	if _, err := s.CreateGrant(ctx, model.ID, ModelKindModel, GranteeServiceToken, svc.ID); err != nil {
		t.Fatalf("grant service token: %v", err)
	}
	granted, err = s.GrantedModelIDsForServiceToken(ctx, svc.ID)
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if _, ok := granted[model.ID]; !ok {
		t.Fatal("an explicit service_token grant must reach the token")
	}
}

// TestAllServiceTokensGrantReachesEveryToken covers the collective grantee,
// including tokens created after the grant was written.
func TestAllServiceTokensGrantReachesEveryToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := newEnabledModel(t, s, "claude-sonnet")

	if _, err := s.CreateGrant(ctx, model.ID, ModelKindModel, GranteeAllServiceTokens, ""); err != nil {
		t.Fatalf("grant all service tokens: %v", err)
	}
	// Created AFTER the grant: a collective grant is a standing rule, not a
	// snapshot of the tokens that existed when it was written.
	later, _, err := s.CreateServiceToken(ctx, "created-later", "", "", time.Time{})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	granted, err := s.GrantedModelIDsForServiceToken(ctx, later.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := granted[model.ID]; !ok {
		t.Fatal("all_service_tokens must reach a token created after the grant")
	}
}

// TestServiceTokenGrantsDoNotLeakToUsers is the mirror of the all_users test:
// a user must not inherit access from an integration credential.
func TestServiceTokenGrantsDoNotLeakToUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	model := newEnabledModel(t, s, "gpt-4o")
	user, _, err := s.UpsertUserFromIdentity(ctx, "auth-1", "person@example.com", "Person", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.CreateGrant(ctx, model.ID, ModelKindModel, GranteeAllServiceTokens, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	granted, err := s.GrantedModelIDs(ctx, user.ID, nil)
	if err != nil {
		t.Fatalf("resolve user grants: %v", err)
	}
	if _, ok := granted[model.ID]; ok {
		t.Fatal("an all_service_tokens grant must NOT reach a human user")
	}
}

// TestServiceTokenUsageExcludedFromUserReports is the reporting guarantee the
// whole feature turns on: service-token traffic counts toward org-wide totals
// but never appears in a leaderboard of people.
func TestServiceTokenUsageExcludedFromUserReports(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCatalogModels(t, s, "gpt-4o")
	user, _, err := s.UpsertUserFromIdentity(ctx, "auth-2", "human@example.com", "Human", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc, _, err := s.CreateServiceToken(ctx, "reporting-bot", "", "", time.Time{})
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}

	// 100 human tokens, 900 integration tokens.
	if err := s.InsertUsageEvent(ctx, &UsageEvent{
		UserID: user.ID, ModelName: "gpt-4o", Modality: "chat", TokensOut: 100, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert user event: %v", err)
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{
		ServiceTokenID: svc.ID, ModelName: "gpt-4o", Modality: "chat", TokensOut: 900, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert service event: %v", err)
	}

	// Org-wide totals include BOTH.
	orgTotals, err := s.AggregateUsage(ctx, UsageScope{})
	if err != nil {
		t.Fatalf("org totals: %v", err)
	}
	if orgTotals.TokensOut != 1000 {
		t.Errorf("org-wide tokens_out = %d, want 1000 (service tokens must be counted)", orgTotals.TokensOut)
	}
	if orgTotals.Requests != 2 {
		t.Errorf("org-wide requests = %d, want 2", orgTotals.Requests)
	}

	// The top-users breakdown excludes service tokens — enforced by the
	// dimension itself, with no principal filter passed by the caller.
	byUser, err := s.BreakdownUsage(ctx, UsageScope{}, "user")
	if err != nil {
		t.Fatalf("user breakdown: %v", err)
	}
	for _, b := range byUser {
		if b.Key == svc.ID {
			t.Fatal("service token appeared as a row in the top-users breakdown")
		}
	}
	var humanTotal int64
	for _, b := range byUser {
		humanTotal += b.Totals.TokensOut
	}
	if humanTotal != 100 {
		t.Errorf("top-users tokens_out = %d, want 100 (service-token traffic must be excluded)", humanTotal)
	}

	// The service-token breakdown shows it, labelled by name.
	bySvc, err := s.BreakdownUsage(ctx, UsageScope{}, "service_token")
	if err != nil {
		t.Fatalf("service token breakdown: %v", err)
	}
	if len(bySvc) != 1 || bySvc[0].Key != svc.ID {
		t.Fatalf("service token breakdown = %+v, want one row for %q", bySvc, svc.ID)
	}
	if bySvc[0].Label != "reporting-bot" {
		t.Errorf("label = %q, want the token name", bySvc[0].Label)
	}
	if bySvc[0].Totals.TokensOut != 900 {
		t.Errorf("service token tokens_out = %d, want 900", bySvc[0].Totals.TokensOut)
	}

	// A user-scoped aggregate never picks up integration traffic.
	userTotals, err := s.AggregateUsage(ctx, UsageScope{UserID: user.ID, Principal: PrincipalUsers})
	if err != nil {
		t.Fatalf("user totals: %v", err)
	}
	if userTotals.TokensOut != 100 {
		t.Errorf("user-scoped tokens_out = %d, want 100", userTotals.TokensOut)
	}
}

// TestServiceTokenQuotasAreNotInherited locks the deliberate decision that an
// integration credential picks up no quota by proximity: only rules written
// directly against it apply.
func TestServiceTokenQuotasAreNotInherited(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	svc, _, err := s.CreateServiceToken(ctx, "bounded-bot", "", "", time.Time{})
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}
	// A user-subject rule must not apply to the token.
	if err := s.CreateQuota(ctx, &Quota{
		SubjectType: "user", SubjectID: "someone", Metric: MetricRequests,
		Limit: 10, Window: WindowDaily,
	}); err != nil {
		t.Fatalf("create user quota: %v", err)
	}
	got, err := s.QuotasForServiceToken(ctx, svc.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("service token inherited %d quota(s); it must inherit none", len(got))
	}

	if err := s.CreateQuota(ctx, &Quota{
		SubjectType: "service_token", SubjectID: svc.ID, Metric: MetricRequests,
		Limit: 5, Window: WindowDaily,
	}); err != nil {
		t.Fatalf("create service quota: %v", err)
	}
	got, err = s.QuotasForServiceToken(ctx, svc.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got) != 1 || got[0].Limit != 5 {
		t.Fatalf("service token quotas = %+v, want the one direct rule", got)
	}
	if got[0].SubjectName != "bounded-bot" {
		t.Errorf("subject name = %q, want the token name", got[0].SubjectName)
	}
}

// TestSumMetricSinceScopesServiceTokens proves quota accrual reads the service
// token's own consumption and not some user's.
func TestSumMetricSinceScopesServiceTokens(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	svc, _, err := s.CreateServiceToken(ctx, "metered", "", "", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{ServiceTokenID: svc.ID, TokensIn: 7, HTTPStatus: 200}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertUsageEvent(ctx, &UsageEvent{UserID: "some-user", TokensIn: 500, HTTPStatus: 200}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.SumMetricSince(ctx, "service_token", svc.ID, "", MetricTokensIn, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got != 7 {
		t.Errorf("service token tokens_in = %d, want 7 (user traffic must not leak in)", got)
	}
}

// --- Managed models ----------------------------------------------------------

// TestManagedModelResolvesToTarget covers the core indirection and the
// reporting rule that falls out of it.
func TestManagedModelResolvesToTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "gpt-4o-mini")

	mm, err := s.CreateManagedModel(ctx, "current-best", "the good one", target.ID, "")
	if err != nil {
		t.Fatalf("create managed model: %v", err)
	}
	if !mm.Servable || mm.Broken {
		t.Fatalf("fresh managed model should be servable, got servable=%v broken=%v (%s)", mm.Servable, mm.Broken, mm.BrokenReason)
	}
	// Transparency: the alias publishes what it points at.
	if mm.TargetPublicName != target.PublicName() {
		t.Errorf("target public name = %q, want %q", mm.TargetPublicName, target.PublicName())
	}

	resolved, err := s.ResolveModelForRequest(ctx, "current-best")
	if err != nil {
		t.Fatalf("resolve alias: %v", err)
	}
	if !resolved.ViaManagedModel() {
		t.Fatal("resolution should report that it went through an alias")
	}
	if resolved.Model.ID != target.ID {
		t.Errorf("resolved to model %q, want the target %q", resolved.Model.ID, target.ID)
	}
	if resolved.RequestedName() != "current-best" {
		t.Errorf("requested name = %q, want the alias", resolved.RequestedName())
	}
}

// TestManagedModelRepointChangesResolution is the feature's whole purpose: the
// caller keeps sending one name while the admin swaps what it means.
func TestManagedModelRepointChangesResolution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := newEnabledModel(t, s, "old-model")
	second := newEnabledModel(t, s, "new-model")

	mm, err := s.CreateManagedModel(ctx, "best-coder", "", first.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetManagedModelTarget(ctx, mm.ID, second.ID); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	resolved, err := s.ResolveModelForRequest(ctx, "best-coder")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Model.ID != second.ID {
		t.Errorf("after repoint the alias resolved to %q, want %q", resolved.Model.Name, second.Name)
	}
	// Grants live on the alias, so they survive the swap untouched.
	if _, err := s.CreateGrant(ctx, mm.ID, ModelKindManaged, GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant alias: %v", err)
	}
	if err := s.SetManagedModelTarget(ctx, mm.ID, first.ID); err != nil {
		t.Fatalf("repoint back: %v", err)
	}
	after, err := s.ManagedModelByID(ctx, mm.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.GrantCount != 1 {
		t.Errorf("grant count = %d, want 1 — repointing must not disturb grants", after.GrantCount)
	}
}

// TestManagedModelUsageReportsUnderlyingModel locks the reporting rule the user
// called out explicitly: reports reflect the underlying model, while the alias
// stays visible in its own column.
func TestManagedModelUsageReportsUnderlyingModel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "claude-haiku")
	if _, err := s.CreateManagedModel(ctx, "fast-cheap", "", target.ID, ""); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// The proxy records the TARGET's name in model_name and the alias in
	// requested_model_name; this is that shape.
	if err := s.InsertUsageEvent(ctx, &UsageEvent{
		UserID: "u1", ModelID: target.ID, ModelName: target.PublicName(),
		RequestedModelName: "fast-cheap", Modality: "chat", TokensOut: 42, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	byModel, err := s.BreakdownUsage(ctx, UsageScope{}, "model")
	if err != nil {
		t.Fatalf("model breakdown: %v", err)
	}
	if len(byModel) != 1 {
		t.Fatalf("model breakdown = %+v, want exactly one row", byModel)
	}
	if byModel[0].Key != target.PublicName() {
		t.Errorf("model report shows %q, want the UNDERLYING model %q", byModel[0].Key, target.PublicName())
	}

	// Alias adoption is visible on its own dimension.
	byRequested, err := s.BreakdownUsage(ctx, UsageScope{}, "requested_model")
	if err != nil {
		t.Fatalf("requested model breakdown: %v", err)
	}
	if len(byRequested) != 1 || byRequested[0].Key != "fast-cheap" {
		t.Fatalf("requested-model breakdown = %+v, want one row for the alias", byRequested)
	}
}

// TestManagedModelNameCollisions locks the shared namespace in both directions.
// Without it, "current-best" could name both an alias and a real model and
// request routing would be ambiguous.
func TestManagedModelNameCollisions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "real-model")

	// An alias may not take an existing enabled model's native name.
	if _, err := s.CreateManagedModel(ctx, "real-model", "", target.ID, ""); err == nil {
		t.Fatal("an alias must not be allowed to shadow an enabled model's name")
	}

	if _, err := s.CreateManagedModel(ctx, "current-best", "", target.ID, ""); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	// A second alias may not reuse the name.
	if _, err := s.CreateManagedModel(ctx, "current-best", "", target.ID, ""); !errors.Is(err, ErrManagedModelNameTaken) {
		t.Fatalf("duplicate alias name error = %v, want ErrManagedModelNameTaken", err)
	}
	// And a model rename may not collide with an alias — the reverse
	// direction, which is easy to forget and would silently break routing.
	other := newEnabledModel(t, s, "another-model")
	err := s.SetModelDisplayName(ctx, other.ID, "current-best")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("rename onto an alias name error = %v, want a ValidationError", err)
	}
}

// TestManagedModelBrokenTargetIsReported covers the failure mode the request
// did not mention: what happens when the underlying model goes away. The alias
// must report itself broken rather than failing opaquely.
func TestManagedModelBrokenTargetIsReported(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "disappearing-model")
	mm, err := s.CreateManagedModel(ctx, "points-at-it", "", target.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.SetModelStatus(ctx, target.ID, ModelDisabled); err != nil {
		t.Fatalf("disable target: %v", err)
	}
	reloaded, err := s.ManagedModelByID(ctx, mm.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Broken || reloaded.Servable {
		t.Fatalf("alias with a disabled target should be broken and unservable, got broken=%v servable=%v", reloaded.Broken, reloaded.Servable)
	}
	if reloaded.BrokenReason == "" {
		t.Error("a broken alias must explain why so an admin can repair it")
	}

	// Resolution reports the dedicated error, not a misleading "not found".
	if _, err := s.ResolveModelForRequest(ctx, "points-at-it"); !errors.Is(err, ErrManagedModelBroken) {
		t.Fatalf("resolve error = %v, want ErrManagedModelBroken", err)
	}
}

// TestManagedModelDisabledDoesNotResolve locks that a disabled alias behaves as
// though it does not exist rather than erroring as broken.
func TestManagedModelDisabledDoesNotResolve(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "some-model")
	mm, err := s.CreateManagedModel(ctx, "retired-alias", "", target.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetManagedModelStatus(ctx, mm.ID, ManagedModelDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.ResolveModelForRequest(ctx, "retired-alias"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve error = %v, want ErrNotFound", err)
	}
}

// TestManagedModelTargetMustBeRealModel locks that aliases never chain, so a
// resolution is always exactly one hop and an admin cannot build a cycle.
func TestManagedModelTargetMustBeRealModel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "base-model")
	first, err := s.CreateManagedModel(ctx, "alias-one", "", target.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Pointing an alias at another alias's id must be rejected: that id is
	// not in the model table.
	if _, err := s.CreateManagedModel(ctx, "alias-two", "", first.ID, ""); err == nil {
		t.Fatal("an alias must not be allowed to target another alias")
	}
}

// TestDeleteManagedModelRemovesGrants proves retiring an alias does not leave
// dangling grants in the access matrix.
func TestDeleteManagedModelRemovesGrants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "target-model")
	mm, err := s.CreateManagedModel(ctx, "doomed", "", target.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateGrant(ctx, mm.ID, ModelKindManaged, GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := s.DeleteManagedModel(ctx, mm.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	grants, err := s.ListGrants(ctx, mm.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("alias deletion left %d grant(s) behind", len(grants))
	}
}

// TestManagedModelGrantIsIndependentOfTargetGrant locks that access to an alias
// and access to its target are separate decisions in both directions.
func TestManagedModelGrantIsIndependentOfTargetGrant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := newEnabledModel(t, s, "underlying")
	mm, err := s.CreateManagedModel(ctx, "the-alias", "", target.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	user, _, err := s.UpsertUserFromIdentity(ctx, "auth-3", "u@example.com", "U", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Granting the underlying model does not grant the alias.
	if _, err := s.CreateGrant(ctx, target.ID, ModelKindModel, GranteeUser, user.ID); err != nil {
		t.Fatalf("grant target: %v", err)
	}
	granted, err := s.GrantedModelIDs(ctx, user.ID, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := granted[mm.ID]; ok {
		t.Fatal("a grant on the underlying model must not imply access to an alias pointing at it")
	}
	if _, ok := granted[target.ID]; !ok {
		t.Fatal("the direct grant on the target should be present")
	}
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
