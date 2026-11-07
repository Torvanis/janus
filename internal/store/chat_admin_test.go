package store

import (
	"context"
	"testing"
)

// Personal plans are fixed-price: $0 in Janus, but reportable. The
// billing_source report dimension separates them by vendor.
func TestReportBillingSourceSeparatesPersonalPlans(t *testing.T) {
	for _, c := range []struct{ sub, model, want string }{
		{"", "gpt-5", "organization"},
		{"conn-1", "my/xai/grok-4.7", "plan:xai"},
		{"conn-2", "my/openai/gpt-6.1-sol", "plan:openai"},
		{"conn-3", "odd", "plan"},
	} {
		if got := reportBillingSource(c.sub, c.model); got != c.want {
			t.Errorf("%q %q = %q, want %q", c.sub, c.model, got, c.want)
		}
	}
}

// Defaults keep pre-admin behavior; a saved document round-trips; a
// restriction needs someone in it.
func TestChatAdminSettingsDefaultsAndValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	got, err := s.ChatAdminSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Access.Mode != ChatAccessEveryone || !got.SharesEnabled || !got.ExportEnabled || !got.PersonalPlans || got.TemporaryTTLHours != 24 {
		t.Fatalf("defaults = %+v", got)
	}
	got.Access = ChatAccess{Mode: ChatAccessRestricted}
	if err := s.SetChatAdminSettings(ctx, got); err == nil {
		t.Fatal("restricted with nobody accepted")
	}
	got.Access = ChatAccess{Mode: ChatAccessRestricted, Groups: []string{"g1", "g1", " "}}
	got.Models = map[string]ChatModelSetting{"a": {Category: "Fast", Recommended: true}, "b": {}}
	if err := s.SetChatAdminSettings(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, _ := s.ChatAdminSettings(ctx)
	if len(back.Access.Groups) != 1 || back.Models["a"].Category != "fast" || len(back.Models) != 1 {
		t.Fatalf("round trip = %+v", back)
	}
}
