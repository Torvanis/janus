package store

import (
	"context"
	"testing"
)

// A user who signed in through OIDC first must be recognized by SCIM
// provisioning through the immutable externalId (the OIDC subject), even when
// the SCIM userName is not the user's email address.
func TestSCIMProvisioningBindsExistingSubjectByExternalID(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	u, _, err := s.UpsertUserFromIdentity(ctx, "oidc:casey-subject", "Casey.Sample@Example.org", "Casey Sample", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleAdmin {
		t.Fatalf("fixture expected bootstrap admin, got %s", u.Role)
	}
	r, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{
		"userName":    "csample",
		"externalId":  "oidc:casey-subject",
		"displayName": "Casey Sample",
		"active":      true,
		"emails":      []any{map[string]any{"value": "casey.sample@example.org", "primary": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r["id"].(string); got != u.ID {
		t.Fatalf("SCIM provisioning created a duplicate identity: got %s want %s", got, u.ID)
	}
	after, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != RoleAdmin || !after.IsActive {
		t.Fatalf("binding altered role or lifecycle: %+v", after)
	}
	if after.AuthID != "oidc:casey-subject" {
		t.Fatalf("binding must not replace the sign-in subject: %s", after.AuthID)
	}
	users, _, err := s.ListUsers(ctx, UserFilter{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, x := range users {
		if x.Name == "Casey Sample" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one Casey Sample, found %d", count)
	}
	// Directory deactivation must now govern the original account.
	if _, err = s.SaveSCIMResource(ctx, "Users", u.ID, map[string]any{"userName": "csample", "externalId": "oidc:casey-subject", "active": false}); err != nil {
		t.Fatal(err)
	}
	after, err = s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.IsActive {
		t.Fatal("directory deactivation did not disable the bound account")
	}
}

// externalId pointing at one existing user while the userName email matches a
// different user is ambiguous and must be rejected, not silently linked.
func TestSCIMProvisioningRejectsSubjectEmailIdentityConflict(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	a, _, err := s.UpsertUserFromIdentity(ctx, "oidc:subject-a", "a@example.com", "A", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.UpsertUserFromIdentity(ctx, "oidc:subject-b", "b@example.com", "B", false, false); err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "b@example.com", "externalId": "oidc:subject-a"})
	if err == nil {
		t.Fatal("conflicting subject and email identities were linked")
	}
	// Neither account may be silently mutated by the rejected write.
	after, err := s.UserByID(ctx, a.ID)
	if err != nil || after.Email != "a@example.com" {
		t.Fatalf("rejected provisioning mutated account: %+v %v", after, err)
	}
}

// A subject already managed by another SCIM record must not be claimable again.
func TestSCIMProvisioningSubjectAlreadyManagedConflicts(t *testing.T) {
	s := scimTestStore(t)
	ctx := context.Background()
	if _, _, err := s.UpsertUserFromIdentity(ctx, "oidc:managed", "managed@example.com", "Managed", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "managed@example.com", "externalId": "oidc:managed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSCIMResource(ctx, "Users", "", map[string]any{"userName": "managed2", "externalId": "oidc:managed"}); err == nil {
		t.Fatal("second SCIM record claimed an already managed subject")
	}
}
