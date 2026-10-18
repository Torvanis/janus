package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSecgwCategoriesSeeded(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	cats, err := s.ListSecgwCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	for _, c := range cats {
		groups[c.Group]++
		if !c.Builtin || !c.Enabled {
			t.Fatalf("%s: built-ins ship enabled", c.Slug)
		}
	}
	if groups[SecgwCategoryGroupSafety] != 14 || groups[SecgwCategoryGroupFamily] != 5 || len(cats) != 19 {
		t.Fatalf("seeded %v, want 14 safety + 5 family", groups)
	}
	if cats[0].Slug != "violent_crimes" || cats[0].Name != "Violent crimes" || !strings.HasPrefix(cats[0].Includes, "Helping with or encouraging violence") {
		t.Fatalf("first safety category = %+v", cats[0])
	}
	if cats[0].LegacyCode != "S1" || cats[0].Tier != "critical" {
		t.Fatalf("violent_crimes legacy=%q tier=%q", cats[0].LegacyCode, cats[0].Tier)
	}
	var low *SecgwCategory
	for i := range cats {
		if cats[i].Slug == "feeling_low" {
			low = &cats[i]
		}
	}
	if low == nil || low.MinConfidence != 0.35 || low.Group != SecgwCategoryGroupFamily || len(low.Examples) != 2 {
		t.Fatalf("feeling_low = %+v", low)
	}
	want := []string{"violent_crimes", "sex_crimes", "child_sexual_exploitation", "indiscriminate_weapons", "self_harm"}
	if got := DefaultSecgwCategorySlugs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("default slugs %v", got)
	}
	// Seeding again (a re-run migration) keeps edits.
	name := "Violence"
	if _, err := s.UpdateSecgwCategory(ctx, cats[0].ID, SecgwCategoryInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range seedSecgwCategoryStatements() {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.SecgwCategoryByID(ctx, cats[0].ID)
	if got.Name != "Violence" {
		t.Fatalf("re-seeding overwrote an admin edit: %q", got.Name)
	}
	reset, err := s.ResetSecgwCategory(ctx, cats[0].ID)
	if err != nil || reset.Name != "Violent crimes" {
		t.Fatalf("reset = %+v %v", reset, err)
	}
}

func TestSecgwCategoryCRUDAndLinks(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	name, inc := "Gaming during class", "Asking about games while in a lesson."
	c, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &name, Includes: &inc})
	if err != nil {
		t.Fatal(err)
	}
	if c.Slug != "gaming_during_class" || c.Group != SecgwCategoryGroupCustom || c.Builtin || c.MinConfidence != DefaultSecgwMinConfidence {
		t.Fatalf("created %+v", c)
	}
	if _, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &name, Includes: &inc}); !isValidation(err, "name") {
		t.Fatalf("duplicate name: %v", err)
	}
	none := "None"
	if _, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &none, Includes: &inc}); !isValidation(err, "name") {
		t.Fatalf("slug none must be refused: %v", err)
	}
	empty := ""
	if _, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &empty}); !isValidation(err, "name") {
		t.Fatalf("empty name: %v", err)
	}
	other := "Other"
	if _, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &other}); !isValidation(err, "includes") {
		t.Fatalf("no description: %v", err)
	}
	bad := 1.5
	if _, err := s.UpdateSecgwCategory(ctx, c.ID, SecgwCategoryInput{MinConfidence: &bad}); !isValidation(err, "min_confidence") {
		t.Fatalf("confidence out of range: %v", err)
	}
	missing := "no_such_insights"
	if _, err := s.UpdateSecgwCategory(ctx, c.ID, SecgwCategoryInput{LinkedSlug: &missing}); !isValidation(err, "linked_slug") {
		t.Fatalf("unknown link: %v", err)
	}
	insights, err := s.ListClassificationCategories(ctx)
	if err != nil || len(insights) == 0 {
		t.Fatalf("insights categories: %v", err)
	}
	link := insights[0].Slug
	if _, err := s.UpdateSecgwCategory(ctx, c.ID, SecgwCategoryInput{LinkedSlug: &link}); err != nil {
		t.Fatal(err)
	}
	m, err := s.SecgwCategoryMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := m[c.Slug]; got == nil || got.LinkedName == "" || got.Includes != insights[0].Description {
		t.Fatalf("linked category must read the insights definition: %+v", got)
	}
	// Built-ins cannot be deleted.
	if err := s.DeleteSecgwCategory(ctx, "scat_violent_crimes"); !isValidation(err, "id") {
		t.Fatalf("delete built-in: %v", err)
	}
	if _, err := s.ResetSecgwCategory(ctx, c.ID); !isValidation(err, "id") {
		t.Fatalf("reset custom: %v", err)
	}
	if err := s.DeleteSecgwCategory(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SecgwCategoryByID(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestSecgwCategoryInUseCannotGo(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	name, inc := "Crypto scams", "Asking how to run a crypto giveaway scam."
	c, err := s.CreateSecgwCategory(ctx, SecgwCategoryInput{Name: &name, Includes: &inc})
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := json.Marshal(map[string]any{"method": SecgwMethodCategories, "safety": []string{c.Slug, "feeling_low"}})
	p := &SecgwPolicy{Name: "Kids", Enabled: true, Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true, Mode: SecgwModeObserve,
		Direction: SecgwDirectionIngress, Options: opts, ClassifierModelID: "m"}}}
	if err := s.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecgwCategory(ctx, c.ID); !isValidation(err, "id") || !strings.Contains(err.Error(), "Kids") {
		t.Fatalf("delete in use must name the policy: %v", err)
	}
	off := false
	if _, err := s.UpdateSecgwCategory(ctx, "scat_feeling_low", SecgwCategoryInput{Enabled: &off}); !isValidation(err, "enabled") || !strings.Contains(err.Error(), "Kids") {
		t.Fatalf("disable in use must name the policy: %v", err)
	}
	// A switched-off check does not hold its categories.
	p.Checks[0].Enabled = false
	if err := s.UpdateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecgwCategory(ctx, c.ID); err != nil {
		t.Fatalf("delete after the check was switched off: %v", err)
	}
	// A legacy check's S-codes do not hold categories.
	if _, err := s.UpdateSecgwCategory(ctx, "scat_elections", SecgwCategoryInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.SecgwCategoryMap(ctx)
	if m["elections"] != nil {
		t.Fatal("disabled category in the snapshot map")
	}
}

func TestSecgwViolationCategoryFilterAndTop(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(rule string) *SecgwViolation {
		return &SecgwViolation{Kind: string(SecgwCheckContentSafety), RuleID: rule, Severity: "high", Direction: SecgwDirectionIngress,
			Action: SecgwActionObserved, MatchHash: "h", CreatedAt: now}
	}
	if err := s.InsertSecgwViolations(ctx, []*SecgwViolation{
		mk("feeling_low"), mk("homework_shortcut,feeling_low"), mk("not_for_children"), mk("S9"), mk("feeling_lowish"),
		{Kind: string(SecgwCheckSecrets), RuleID: "feeling_low", Severity: "high", Direction: "ingress", Action: "observed", MatchHash: "h", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSecgwViolations(ctx, SecgwViolationFilter{Category: "feeling_low"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("category filter matched %d, want 2 (whole list elements, content safety only)", len(got))
	}
	for _, v := range got {
		if v.RuleID == "homework_shortcut,feeling_low" && strings.Join(v.RuleNames, "|") != "Homework done for them|Feeling low or unsafe" {
			t.Fatalf("rule names = %v", v.RuleNames)
		}
	}
	got, _ = s.ListSecgwViolations(ctx, SecgwViolationFilter{Category: "indiscriminate_weapons"})
	if len(got) != 1 || got[0].RuleID != "S9" || strings.Join(got[0].RuleNames, ",") != "Indiscriminate weapons" {
		t.Fatalf("a legacy code must match its safety category, by name: %+v", got)
	}
	top, err := s.SecgwTopCategories(ctx, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) == 0 || top[0].Slug != "feeling_low" || top[0].Count != 2 || top[0].Name != "Feeling low or unsafe" {
		t.Fatalf("top = %+v", top)
	}
	found := false
	for _, r := range top {
		if r.Slug == "indiscriminate_weapons" && r.Name == "Indiscriminate weapons" {
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy S9 must be counted under its category name: %+v", top)
	}
}

func isValidation(err error, field string) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Field == field
}

func TestSecgwCategoriesCheckMayBlockRequestsOnBothDirections(t *testing.T) {
	cat := json.RawMessage(`{"method":"categories","safety":["feeling_low"]}`)
	ok := SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true, Mode: SecgwModeBlock, Direction: SecgwDirectionBoth, ClassifierModelID: "m", Options: cat}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("categories block on both (requests blocked, replies observed) refused: %v", err)
	}
	bad := SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true, Mode: SecgwModeBlock, Direction: SecgwDirectionEgress, ClassifierModelID: "m", Options: cat}}}
	if err := bad.Validate(); !isValidation(err, "checks[0].mode") {
		t.Fatalf("block on replies only must still be refused: %v", err)
	}
}
