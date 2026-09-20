package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCipher struct{}

func (fakeCipher) Encrypt(p string) (string, error) { return "enc:" + p, nil }
func (fakeCipher) Decrypt(e string) (string, error) {
	if len(e) < 4 || e[:4] != "enc:" {
		return "", errors.New("bad envelope")
	}
	return e[4:], nil
}

func TestSecgwPolicyValidateDefaultsAndRejections(t *testing.T) {
	p := &SecgwPolicy{Name: "Default", Checks: []SecgwCheck{{Kind: SecgwCheckSecrets}}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	c := p.Checks[0]
	if c.Mode != SecgwModeObserve || c.Fail != SecgwFailClosed || c.Direction != SecgwDirectionIngress {
		t.Fatalf("defaults not applied: %+v", c)
	}

	// The editor round-trips every kind, so a policy that only blocks
	// secrets still carries a disabled prompt_injection check. That must
	// save on an org with no classifier yet (the exact first-run case).
	off := &SecgwPolicy{Name: "Secrets only", Checks: []SecgwCheck{
		{Kind: SecgwCheckSecrets, Enabled: true, Mode: SecgwModeBlock},
		{Kind: SecgwCheckPromptInjection, Enabled: false},
	}}
	if err := off.Validate(); err != nil {
		t.Fatalf("disabled prompt_injection without classifier must be saveable: %v", err)
	}

	cases := []struct {
		name string
		p    SecgwPolicy
		want string
	}{
		{"no checks", SecgwPolicy{Name: "x"}, "checks"},
		{"bad kind", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: "nope"}}}, "checks[0].kind"},
		{"dup kind", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckPII}, {Kind: SecgwCheckPII}}}, "checks[1].kind"},
		{"redact injection", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckPromptInjection, Mode: SecgwModeRedact, ClassifierModelID: "m"}}}, "checks[0].mode"},
		{"redact shape", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckShape, Mode: SecgwModeRedact}}}, "checks[0].mode"},
		{"injection no classifier", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckPromptInjection, Enabled: true}}}, "checks[0].classifier_model_id"},
		{"classifier on deterministic", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckSecrets, ClassifierModelID: "m"}}}, "checks[0].classifier_model_id"},
		{"shape on egress", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckShape, Direction: SecgwDirectionEgress}}}, "checks[0].direction"},
		{"bad fail", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckSecrets, Fail: "maybe"}}}, "checks[0].fail"},
		{"bad name", SecgwPolicy{Name: "!!", Checks: []SecgwCheck{{Kind: SecgwCheckSecrets}}}, "name"},
		{"content_safety no classifier", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true}}}, "checks[0].classifier_model_id"},
		{"content_safety redact", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Mode: SecgwModeRedact, ClassifierModelID: "m"}}}, "checks[0].mode"},
		// v1 ruling: egress content safety is observe-only. Blocking on
		// egress or both must be refused at save time, with the reason.
		{"content_safety block egress", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Mode: SecgwModeBlock, Direction: SecgwDirectionEgress, ClassifierModelID: "m"}}}, "checks[0].mode"},
		{"content_safety block both", SecgwPolicy{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Mode: SecgwModeBlock, Direction: SecgwDirectionBoth, ClassifierModelID: "m"}}}, "checks[0].mode"},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != tc.want {
			t.Errorf("%s: want ValidationError on %q, got %v", tc.name, tc.want, err)
		}
	}
	// What egress content safety CAN do: observe on egress/both, block on ingress.
	for _, ok := range []SecgwPolicy{
		{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true, Mode: SecgwModeObserve, Direction: SecgwDirectionBoth, ClassifierModelID: "m"}}},
		{Name: "x", Checks: []SecgwCheck{{Kind: SecgwCheckContentSafety, Enabled: true, Mode: SecgwModeBlock, Direction: SecgwDirectionIngress, ClassifierModelID: "m"}}},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("valid content_safety policy rejected: %v", err)
		}
	}
}

func TestSecgwPolicyCRUDAndBindings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := &SecgwPolicy{Name: "Corporate Default", Enabled: true, Mandatory: true, Checks: []SecgwCheck{
		{Kind: SecgwCheckSecrets, Mode: SecgwModeBlock, Direction: SecgwDirectionBoth},
		{Kind: SecgwCheckPII, Mode: SecgwModeRedact},
	}}
	if err := s.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateSecgwPolicy(ctx, &SecgwPolicy{Name: "corporate default", Checks: p.Checks}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	got, err := s.SecgwPolicyByID(ctx, p.ID)
	if err != nil || got.Name != p.Name || len(got.Checks) != 2 || !got.Mandatory || got.Checks[0].Mode != SecgwModeBlock {
		t.Fatalf("round trip: %+v %v", got, err)
	}

	b := &SecgwBinding{PolicyID: p.ID, ScopeType: SecgwScopeOrg}
	if err := s.CreateSecgwBinding(ctx, b); err != nil {
		t.Fatalf("bind org: %v", err)
	}
	if err := s.CreateSecgwBinding(ctx, &SecgwBinding{PolicyID: p.ID, ScopeType: SecgwScopeOrg}); !errors.Is(err, ErrSecgwDuplicateBinding) {
		t.Fatalf("second org binding: want ErrSecgwDuplicateBinding, got %v", err)
	}
	if err := s.CreateSecgwBinding(ctx, &SecgwBinding{PolicyID: p.ID, ScopeType: SecgwScopeGroup}); err == nil {
		t.Fatal("group binding without scope_id accepted")
	}
	if err := s.CreateSecgwBinding(ctx, &SecgwBinding{PolicyID: "missing", ScopeType: SecgwScopeOrg}); err == nil {
		t.Fatal("binding to missing policy accepted")
	}
	if err := s.DeleteSecgwPolicy(ctx, p.ID); !errors.Is(err, ErrSecgwPolicyInUse) {
		t.Fatalf("delete bound policy: want ErrSecgwPolicyInUse, got %v", err)
	}

	snap, err := s.LoadSecgwSnapshot(ctx)
	if err != nil || snap.Empty() || len(snap.Bindings) != 1 || snap.Policies[p.ID] == nil {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	list, err := s.ListSecgwBindings(ctx)
	if err != nil || len(list) != 1 || list[0].PolicyName != p.Name || list[0].ScopeName != "Everyone" {
		t.Fatalf("list bindings: %+v %v", list, err)
	}

	p.Name = "Corporate Default v2"
	p.Checks[0].Mode = SecgwModeObserve
	if err := s.UpdateSecgwPolicy(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.SecgwPolicyByID(ctx, p.ID)
	if got.Name != "Corporate Default v2" || got.Checks[0].Mode != SecgwModeObserve || got.BindingCount != 1 {
		t.Fatalf("update not persisted: %+v", got)
	}

	if err := s.DeleteSecgwBinding(ctx, b.ID); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if err := s.DeleteSecgwBinding(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.DeleteSecgwPolicy(ctx, p.ID); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	if _, err := s.SecgwPolicyByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("policy still present: %v", err)
	}
}

func TestSecgwTermListsEncryptedAtRest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := fakeCipher{}

	l := &SecgwTermList{Name: "Codenames", Terms: []string{"Project Halberd", " Project Halberd ", "", "Orion"}, Allow: []string{"orion nebula"}}
	if err := s.CreateSecgwTermList(ctx, l, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.TermCount != 2 {
		t.Fatalf("dedupe/trim: want 2 terms, got %d", l.TermCount)
	}
	if err := s.CreateSecgwTermList(ctx, &SecgwTermList{Name: "NoCipher", Terms: []string{"x"}}, nil); err == nil {
		t.Fatal("term list without cipher accepted")
	}

	// The metadata listing must never carry terms.
	metas, err := s.ListSecgwTermLists(ctx)
	if err != nil || len(metas) != 1 || metas[0].Terms != nil || metas[0].TermCount != 2 {
		t.Fatalf("list: %+v %v", metas, err)
	}
	// Raw column is an envelope, not plaintext.
	var raw string
	if err := s.queryRow(ctx, `SELECT terms_encrypted FROM secgw_term_list WHERE id = ?`, l.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw[:4] != "enc:" {
		t.Fatalf("terms stored in the clear: %q", raw)
	}
	full, err := s.SecgwTermListByID(ctx, l.ID, c, true)
	if err != nil || len(full.Terms) != 2 || full.Terms[0] != "Project Halberd" || len(full.Allow) != 1 {
		t.Fatalf("decrypt: %+v %v", full, err)
	}
	loaded, err := s.LoadSecgwTermLists(ctx, c)
	if err != nil || len(loaded) != 1 || len(loaded[0].Terms) != 2 {
		t.Fatalf("load: %+v %v", loaded, err)
	}

	bad := &SecgwTermList{Name: "Regex", MatchMode: SecgwTermRegex, Terms: []string{"("}}
	if err := s.CreateSecgwTermList(ctx, bad, c); err == nil {
		t.Fatal("invalid regex accepted")
	}

	l.Terms = []string{"Halberd"}
	if err := s.UpdateSecgwTermList(ctx, l, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	full, _ = s.SecgwTermListByID(ctx, l.ID, c, true)
	if len(full.Terms) != 1 || full.Terms[0] != "Halberd" {
		t.Fatalf("update not persisted: %+v", full)
	}
	if err := s.DeleteSecgwTermList(ctx, l.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestSecgwViolationsHashOnlyUnlessCaptured(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := fakeCipher{}

	secret := &SecgwViolation{RequestID: "req-1", Kind: string(SecgwCheckSecrets), RuleID: "aws-access-key-id", Severity: "high",
		Direction: SecgwDirectionIngress, Action: SecgwActionBlocked, MatchHash: "abc", PolicyID: "p"}
	inj := &SecgwViolation{RequestID: "req-2", Kind: string(SecgwCheckPromptInjection), RuleID: "prompt_guard", Severity: "high",
		Direction: SecgwDirectionIngress, Action: SecgwActionObserved, MatchHash: "def", PolicyID: "p", ClassifierScore: 0.97}
	inj.SetMatchTextEnvelope("enc:ignore previous instructions")
	if err := s.InsertSecgwViolations(ctx, []*SecgwViolation{secret, inj}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.ListSecgwViolations(ctx, SecgwViolationFilter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %d %v", len(got), err)
	}
	for _, v := range got {
		if v.MatchText != "" {
			t.Fatalf("list leaked match text: %+v", v)
		}
	}
	byKind, _ := s.ListSecgwViolations(ctx, SecgwViolationFilter{Kind: string(SecgwCheckSecrets)})
	if len(byKind) != 1 || byKind[0].HasMatchText {
		t.Fatalf("secrets violation must carry no text: %+v", byKind)
	}
	one, err := s.SecgwViolationByID(ctx, inj.ID, c)
	if err != nil || one.MatchText != "ignore previous instructions" || one.ClassifierScore != 0.97 {
		t.Fatalf("read with cipher: %+v %v", one, err)
	}
	none, _ := s.SecgwViolationByID(ctx, secret.ID, c)
	if none.HasMatchText || none.MatchText != "" {
		t.Fatalf("secret text should be absent: %+v", none)
	}

	counts, err := s.SecgwViolationCounts(ctx, time.Now().Add(-time.Hour))
	if err != nil || counts[string(SecgwCheckSecrets)][SecgwActionBlocked] != 1 || counts[string(SecgwCheckPromptInjection)][SecgwActionObserved] != 1 {
		t.Fatalf("counts: %+v %v", counts, err)
	}

	n, err := s.PruneSecgwViolations(ctx, string(SecgwCheckSecrets), SecgwRetention{MaxCount: 0, MaxAgeHours: 1})
	if err != nil || n != 0 {
		t.Fatalf("prune fresh rows: %d %v", n, err)
	}
	n, err = s.PruneSecgwViolations(ctx, string(SecgwCheckPromptInjection), SecgwRetention{MaxCount: 0, MaxAgeHours: 0})
	if err != nil || n != 0 {
		t.Fatalf("prune with no bounds must be a no-op: %d %v", n, err)
	}
}

func TestSetModelClassifierRoleExcludesFromServableAndRefusesGranted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "guard-up", "openai_compatible", "http://guard", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDiscoveredModel(ctx, up.ID, "prompt-guard", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := s.ListModels(ctx, ModelFilter{})
	m := models[0]
	_ = s.SetModelStatus(ctx, m.ID, ModelEnabled)

	if _, err := s.ModelByName(ctx, "prompt-guard"); err != nil {
		t.Fatalf("resolvable before role: %v", err)
	}
	if err := s.SetModelClassifierRole(ctx, m.ID, "bogus"); err == nil {
		t.Fatal("bogus role accepted")
	}
	if err := s.SetModelClassifierRole(ctx, m.ID, ClassifierRoleTextClassification); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := s.ModelByName(ctx, "prompt-guard"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("classifier must not resolve on the proxy path: %v", err)
	}
	servable, _ := s.ListModels(ctx, ModelFilter{Servable: true})
	if len(servable) != 0 {
		t.Fatalf("classifier listed as servable: %+v", servable)
	}
	only, _ := s.ListModels(ctx, ModelFilter{ClassifierOnly: true})
	if len(only) != 1 || only[0].ClassifierRole != ClassifierRoleTextClassification {
		t.Fatalf("classifier picker: %+v", only)
	}
	if err := s.SetModelClassifierRole(ctx, m.ID, ""); err != nil {
		t.Fatalf("clear role: %v", err)
	}
	if _, err := s.CreateGrant(ctx, m.ID, ModelKindModel, GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelClassifierRole(ctx, m.ID, ClassifierRoleTextClassification); err == nil {
		t.Fatal("granted model accepted as classifier")
	}
}
