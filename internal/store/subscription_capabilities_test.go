package store

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/capability"
)

// Each person's plan record is their own: one connection's measurement and
// setting never show on another connection, even for the same vendor and
// model; a reconnect of the same vendor account keeps the record and a
// different account starts clean.
func TestSubscriptionCapabilitiesPerConnection(t *testing.T) {
	s := newMetadataTestStore(t)
	ctx := context.Background()
	alice, err := s.CreateLocalUser(ctx, "alice-plancap@example.test", "Alice", "correct horse battery staple 9", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateLocalUser(ctx, "bob-plancap@example.test", "Bob", "correct horse battery staple 9", false)
	if err != nil {
		t.Fatal(err)
	}
	connect := func(userID, subject string) *SubscriptionConnection {
		t.Helper()
		c, err := s.UpsertSubscription(ctx, SubscriptionUpsert{UserID: userID, Provider: "openai", AccountSubject: subject,
			Name: "ChatGPT Plus", Models: []string{"gpt-6.1-sol", "gpt-5.5"}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a := connect(alice.ID, "acct-a")
	b := connect(bob.ID, "acct-b")

	now := time.Now().UTC()
	if _, err := s.UpdateSubscriptionCapabilities(ctx, a.ID, func(rec capability.Record) (bool, error) {
		st := rec["gpt-6.1-sol"]
		st.Probe = capability.Facts{"input.image": {Value: true, At: now, Note: "it named the color of a test image"}}
		if err := st.SetAdmin(map[string]any{"input.audio": false}, now); err != nil {
			return false, err
		}
		rec["gpt-6.1-sol"] = st
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscriptionReportedCapabilities(ctx, b.ID, map[string]capability.Facts{
		"gpt-6.1-sol": {"input.image": {Value: false, At: now}},
	}); err != nil {
		t.Fatal(err)
	}

	gotA, _ := s.SubscriptionByID(ctx, a.ID)
	gotB, _ := s.SubscriptionByID(ctx, b.ID)
	ra := gotA.Capabilities["gpt-6.1-sol"].Resolve()
	rb := gotB.Capabilities["gpt-6.1-sol"].Resolve()
	if ra["input.image"].Value != true || ra["input.image"].Source != capability.SourceProbe || ra["input.audio"].Source != capability.SourceAdmin {
		t.Fatalf("alice's record: %#v", ra)
	}
	if rb["input.image"].Value != false || rb["input.image"].Source != capability.SourceProvider || len(rb) != 1 {
		t.Fatalf("bob's record holds only what his vendor list said: %#v", rb)
	}

	// The vendor list for alice now reports images; her measurement and
	// setting stay, the report sits under them.
	if err := s.SetSubscriptionReportedCapabilities(ctx, a.ID, map[string]capability.Facts{
		"gpt-6.1-sol": {"input.image": {Value: true, At: now}, "input.video": {Value: false, At: now}},
	}); err != nil {
		t.Fatal(err)
	}
	gotA, _ = s.SubscriptionByID(ctx, a.ID)
	st := gotA.Capabilities["gpt-6.1-sol"]
	if st.Probe["input.image"].Value != true || st.Admin["input.audio"].Value != false || st.Provider["input.video"].Value != false {
		t.Fatalf("reported refresh must keep the owner's layers: %#v", st)
	}
	// A list that no longer reports the model drops only the report.
	if err := s.SetSubscriptionReportedCapabilities(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	gotA, _ = s.SubscriptionByID(ctx, a.ID)
	if st := gotA.Capabilities["gpt-6.1-sol"]; st.Provider != nil || st.Admin == nil || st.Probe == nil {
		t.Fatalf("withdrawn report: %#v", st)
	}

	// Reconnecting the same account keeps the record.
	connect(alice.ID, "acct-a")
	gotA, _ = s.SubscriptionByID(ctx, a.ID)
	if gotA.Capabilities["gpt-6.1-sol"].Admin == nil {
		t.Fatalf("a reconnect of the same account lost the owner's settings: %#v", gotA.Capabilities)
	}
	// A different vendor account on the same provider starts clean: plans
	// differ per account.
	connect(alice.ID, "acct-a2")
	gotA, _ = s.SubscriptionByID(ctx, a.ID)
	if len(gotA.Capabilities) != 0 {
		t.Fatalf("another account inherited the record: %#v", gotA.Capabilities)
	}
	gotB, _ = s.SubscriptionByID(ctx, b.ID)
	if gotB.Capabilities["gpt-6.1-sol"].Provider["input.image"].Value != false {
		t.Fatalf("bob's record changed: %#v", gotB.Capabilities)
	}
}

// A claim that nobody finished reads as failed after its TTL, so a model is
// never shown as being measured forever.
func TestSubscriptionCapabilitiesSettleAbandonedMeasure(t *testing.T) {
	s := newMetadataTestStore(t)
	ctx := context.Background()
	u, err := s.CreateLocalUser(ctx, "carol-plancap@example.test", "Carol", "correct horse battery staple 9", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.UpsertSubscription(ctx, SubscriptionUpsert{UserID: u.ID, Provider: "xai", AccountSubject: "x", Models: []string{"grok-4.3"}})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour)
	if _, err := s.UpdateSubscriptionCapabilities(ctx, c.ID, func(rec capability.Record) (bool, error) {
		st := rec["grok-4.3"]
		st.Claim("manual", old)
		rec["grok-4.3"] = st
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.SubscriptionByID(ctx, c.ID)
	if p := got.Capabilities["grok-4.3"].Probed; p == nil || p.Status != capability.ProbeFailed {
		t.Fatalf("abandoned claim: %#v", p)
	}
	// An unparseable column is an empty record, not a failed read.
	if err := s.exec(ctx, `UPDATE subscription_connection SET capabilities_json = 'not json' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SubscriptionByID(ctx, c.ID); err != nil || len(got.Capabilities) != 0 {
		t.Fatalf("broken column: %v %#v", err, got)
	}
}
