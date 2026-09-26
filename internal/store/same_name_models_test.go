package store

import (
	"context"
	"testing"
	"time"
)

// The same model name served by two upstreams is two catalog entries, and a
// name lookup must see both — the freshest discovery first — so the proxy can
// route to whichever copy the caller is granted. Before this, only the first
// copy in upstream-name order was ever considered.
func TestModelsByNameReturnsEverySameNameCopyFreshestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	older := newEnabledModel(t, s, "shared-model")
	time.Sleep(5 * time.Millisecond)
	newer := newEnabledModel(t, s, "shared-model")
	if older.UpstreamID == newer.UpstreamID {
		t.Fatal("fixture: the two copies must live on different upstreams")
	}

	got, err := s.ModelsByName(ctx, "SHARED-model")
	if err != nil {
		t.Fatalf("ModelsByName: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ModelsByName returned %d models, want both copies", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("order = [%s %s], want newest discovery first [%s %s]", got[0].ID, got[1].ID, newer.ID, older.ID)
	}

	resolved, err := s.ResolveModelForRequest(ctx, "shared-model")
	if err != nil {
		t.Fatalf("ResolveModelForRequest: %v", err)
	}
	if resolved.Model.ID != newer.ID || len(resolved.Alternatives) != 1 || resolved.Alternatives[0].ID != older.ID {
		t.Fatalf("resolved = %s alternatives=%d, want %s with the older copy as the alternative", resolved.Model.ID, len(resolved.Alternatives), newer.ID)
	}

	// A copy that stops being served drops out of the candidate list.
	if err := s.SetModelStatus(ctx, newer.ID, ModelDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, err = s.ModelsByName(ctx, "shared-model")
	if err != nil || len(got) != 1 || got[0].ID != older.ID {
		t.Fatalf("after disabling the newer copy: %v %v, want only the older copy", got, err)
	}
}

// Grants listed for same-named models must say which upstream each is on,
// or an admin cannot tell which copy a grant covers.
func TestListGrantsNamesTheUpstream(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m := newEnabledModel(t, s, "shared-model")
	if _, err := s.CreateGrant(ctx, m.ID, ModelKindModel, GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	grants, err := s.ListGrants(ctx, "")
	if err != nil || len(grants) != 1 {
		t.Fatalf("ListGrants = %v %v", grants, err)
	}
	if grants[0].UpstreamName == "" || grants[0].UpstreamName != m.UpstreamName {
		t.Fatalf("grant upstream = %q, want %q", grants[0].UpstreamName, m.UpstreamName)
	}
}
