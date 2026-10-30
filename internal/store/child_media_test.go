package store

import (
	"context"
	"testing"
)

// Media switches are stored per age range (default all off) and merge
// strictest-wins across teams.
func TestChildSafetyMediaSwitchesStoreAndMerge(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	cfg, err := s.SecgwFamily(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kids := *cfg.Profile(ChildProfileKids)
	if kids.Media != (ChildMedia{}) {
		t.Fatalf("default media = %+v, want all off", kids.Media)
	}
	kids.Media = ChildMedia{Image: true, Transcription: true}
	teens := *cfg.Profile(ChildProfileTeens)
	teens.Media = ChildMedia{Image: true, Video: true, Music: true, Transcription: true}
	in := SecgwFamilyInput{Enabled: true, ClassifierModelID: "m1", Profiles: []ChildProfile{*cfg.Profile(ChildProfileLittle), kids, teens}}
	if err := s.SetSecgwFamily(ctx, in, "admin"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.SecgwFamily(ctx)
	if got := cfg.Profile(ChildProfileKids).Media; got != kids.Media {
		t.Fatalf("stored kids media = %+v", got)
	}
	merged := mergeChildProfiles([]*ChildProfile{cfg.Profile(ChildProfileKids), cfg.Profile(ChildProfileTeens)})
	if merged.Media != (ChildMedia{Image: true, Transcription: true}) {
		t.Fatalf("merged media = %+v, want the strictest", merged.Media)
	}
}

// Deleting (archiving) a team drops its Child Safety assignment.
func TestDeleteTeamDropsChildSafetyAssignment(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, "Kids", "")
	if err != nil {
		t.Fatal(err)
	}
	in := SecgwFamilyInput{Enabled: true, ClassifierModelID: "m1", Assignments: []SecgwFamilyAssignment{
		{ScopeType: SecgwFamilyScopeTeam, ScopeID: team.ID, ProfileID: ChildProfileKids}}}
	if err := s.SetSecgwFamily(ctx, in, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeam(ctx, team.ID); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.SecgwFamily(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Assignments) != 0 {
		t.Fatalf("assignments after team delete = %+v", cfg.Assignments)
	}
}
