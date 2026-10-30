package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestChildSafetyProfilesAndCoverage(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	cfg, err := s.SecgwFamily(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.ClassifierModelID != "" || cfg.Ask != SecgwAskTogether || len(cfg.Profiles) != 3 {
		t.Fatalf("seeded config %+v", cfg)
	}
	little, kids, teens := cfg.Profile(ChildProfileLittle), cfg.Profile(ChildProfileKids), cfg.Profile(ChildProfileTeens)
	if little == nil || kids == nil || teens == nil || !little.Builtin {
		t.Fatalf("built-in age ranges %+v", cfg.Profiles)
	}
	// Age-appropriate defaults.
	if little.Outcome("not_for_children") != ChildOutcomeBlock || kids.Outcome("not_for_children") != ChildOutcomeAnswer ||
		teens.Outcome(ChildCategoryFeelingLow) != ChildOutcomeAnswer || kids.Outcome(ChildCategoryFeelingLow) != ChildOutcomeBlock {
		t.Fatalf("defaults little=%v kids=%v teens=%v", little.Categories, kids.Categories, teens.Categories)
	}
	if !kids.Tutoring() || !strings.Contains(TutorRule(), "NEVER give the final answer") || !strings.Contains(TutorRule(), "even after they try") {
		t.Fatalf("tutor rule %q", TutorRule())
	}
	// Every category the check can ask has a shipped definition.
	for _, slug := range append(kids.Asks(), ChildCategoryFinalAnswer) {
		if _, ok := ChildCategory(slug); !ok {
			t.Fatalf("no built-in %s", slug)
		}
	}

	kid, _, _ := s.UpsertUserFromIdentity(ctx, "kid", "kid@example.com", "Kid", false, false)
	teen, _, _ := s.UpsertUserFromIdentity(ctx, "teen", "teen@example.com", "Teen", false, false)
	both, _, _ := s.UpsertUserFromIdentity(ctx, "both", "both@example.com", "Both", false, false)
	adult, _, _ := s.UpsertUserFromIdentity(ctx, "adult", "adult@example.com", "Adult", false, false)
	teensTeam, _ := s.CreateTeam(ctx, "Teens", "")
	strictTeam, _ := s.CreateTeam(ctx, "Strict", "")
	for _, m := range []struct{ team, user string }{{teensTeam.ID, teen.ID}, {teensTeam.ID, both.ID}, {strictTeam.ID, both.ID}, {teensTeam.ID, kid.ID}} {
		if err := s.AddTeamMember(ctx, m.team, m.user, "member", "manual", ""); err != nil {
			t.Fatal(err)
		}
	}
	// A custom range copied from Kids: homework blocked, a model blocked,
	// edited instructions.
	custom := ChildProfile{Name: "Kids, strict", Categories: map[string]ChildCategorySetting{
		ChildCategoryHomeworkShortcut: {Outcome: ChildOutcomeBlock},
		"personal_info":               {Outcome: ChildOutcomeAnswer, Instructions: "Our own privacy words."},
	}, BlockedModels: []string{"my/xai/grok-4.7", " my/xai/grok-4.7 "}}
	profiles := []ChildProfile{*little, *kids, *teens, custom}
	in := SecgwFamilyInput{Enabled: true, ClassifierModelID: "m1", Profiles: profiles, Assignments: []SecgwFamilyAssignment{
		{ScopeType: SecgwFamilyScopeUser, ScopeID: kid.ID, ProfileID: ChildProfileLittle},
		{ScopeType: SecgwFamilyScopeTeam, ScopeID: teensTeam.ID, ProfileID: ChildProfileTeens},
	}}
	if err := s.SetSecgwFamily(ctx, in, "admin"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.SecgwFamily(ctx)
	var saved *ChildProfile
	for _, p := range cfg.Profiles {
		if p.Name == "Kids, strict" {
			saved = p
		}
	}
	if saved == nil || saved.Builtin || saved.Outcome(ChildCategoryHomeworkShortcut) != ChildOutcomeBlock || saved.InstructionsFor("personal_info") != "Our own privacy words." ||
		len(saved.BlockedModels) != 1 || saved.InstructionsFor("bypass_controls") != ChildBuiltin(ChildProfileKids).Categories["bypass_controls"] {
		t.Fatalf("custom range %+v", saved)
	}
	// Assign the strict team to the custom range.
	in.Profiles = nil
	in.Assignments = append(in.Assignments, SecgwFamilyAssignment{ScopeType: SecgwFamilyScopeTeam, ScopeID: strictTeam.ID, ProfileID: saved.ID})
	if err := s.SetSecgwFamily(ctx, in, "admin"); err != nil {
		t.Fatal(err)
	}
	snap, err := s.LoadSecgwSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := snap.Family
	// Direct assignment beats the team (kid is also in Teens).
	if p := f.Profile(kid.ID); p == nil || p.ID != ChildProfileLittle {
		t.Fatalf("kid profile %+v", p)
	}
	if p := f.Profile(teen.ID); p == nil || p.ID != ChildProfileTeens {
		t.Fatalf("teen profile %+v", p)
	}
	// Two teams: strictest per category, blocked models unioned.
	p := f.Profile(both.ID)
	if p == nil || p.Outcome(ChildCategoryHomeworkShortcut) != ChildOutcomeBlock || p.Outcome(ChildCategoryFeelingLow) != ChildOutcomeBlock ||
		!p.Blocks("my/xai/grok-4.7") || p.Outcome(ChildCategoryHomeworkLearning) != ChildOutcomeAnswer {
		t.Fatalf("merged profile %+v", p)
	}
	if f.Covers(adult.ID) {
		t.Fatal("adult covered")
	}
	// Bad input.
	var ve *ValidationError
	for _, bad := range []SecgwFamilyInput{
		{Ask: "sometimes"},
		{Assignments: []SecgwFamilyAssignment{{ScopeType: "group", ScopeID: "x"}}},
		{Assignments: []SecgwFamilyAssignment{{ScopeType: SecgwFamilyScopeUser, ScopeID: "nobody"}}},
		{Assignments: []SecgwFamilyAssignment{{ScopeType: SecgwFamilyScopeUser, ScopeID: kid.ID, ProfileID: "cs_nope"}}},
		{Profiles: []ChildProfile{*little, *kids}}, // removes a built-in
		{Profiles: []ChildProfile{*little, *kids, *teens, {Name: "x", Categories: map[string]ChildCategorySetting{"elections": {Outcome: "block"}}}}},
		{Profiles: []ChildProfile{*little, *kids, *teens, {Name: "x", Categories: map[string]ChildCategorySetting{"personal_info": {Outcome: "sometimes"}}}}},
		{Profiles: []ChildProfile{*little, *kids, *teens, {Name: " "}}},
	} {
		if err := s.SetSecgwFamily(ctx, bad, "admin"); !errors.As(err, &ve) {
			t.Fatalf("want validation error for %+v, got %v", bad, err)
		}
	}
	// Off = covers no one.
	in.Enabled = false
	if err := s.SetSecgwFamily(ctx, in, "admin"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SecgwFamilyCovers(ctx, kid.ID); ok {
		t.Fatal("disabled Child Safety still covers")
	}
	// Violations are labeled.
	if err := s.InsertSecgwViolations(ctx, []*SecgwViolation{{RequestID: "r1", UserID: kid.ID, PolicyID: SecgwFamilyPolicyID, Kind: "content_safety",
		RuleID: "feeling_low", Severity: "high", Direction: "ingress", Action: SecgwActionSupported, ClassifierScore: 0.5}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSecgwViolations(ctx, []*SecgwViolation{{RequestID: "r2", UserID: kid.ID, PolicyID: SecgwFamilyPolicyID, Kind: "content_safety",
		RuleID: ChildCategoryFinalAnswer, Severity: "high", Direction: "egress", Action: SecgwActionStreamCut, ClassifierScore: 0.9}}); err != nil {
		t.Fatal(err)
	}
	vs, err := s.ListSecgwViolations(ctx, SecgwViolationFilter{PolicyID: SecgwFamilyPolicyID})
	if err != nil || len(vs) != 2 || vs[0].PolicyName != SecgwFamilyPolicyName {
		t.Fatalf("violations %+v err %v", vs, err)
	}
	// Child Safety's own categories are named, not shown as slugs.
	for _, v := range vs {
		if v.RuleID == ChildCategoryFinalAnswer && (len(v.RuleNames) != 1 || v.RuleNames[0] != "Gives the answer away") {
			t.Fatalf("rule names %v", v.RuleNames)
		}
	}
}

// Existing Family safety assignments land on the Kids range.
func TestChildSafetyMigrationKeepsAssignments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	kid, _, _ := s.UpsertUserFromIdentity(ctx, "kid", "kid@example.com", "Kid", false, false)
	if err := s.RollbackMigration(ctx, "0067_child_safety"); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `INSERT INTO secgw_family_assignment (scope_type, scope_id, created_by, created_at) VALUES ('user', ?, '', '2026-10-20T00:00:00Z')`, kid.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(ctx, `UPDATE secgw_family SET enabled = 1, classifier_model_id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := s.LoadSecgwFamilySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := snap.Profile(kid.ID); p == nil || p.ID != ChildProfileKids {
		t.Fatalf("migrated assignment %+v", p)
	}
}
