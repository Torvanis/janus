package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPolicyStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenMigratedSQLite(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSecurityPolicyDefaultsKeepOldBehavior(t *testing.T) {
	d := DefaultSecurityPolicy()
	if d.Sessions.IdleMinutesUser != 240 || d.Sessions.AbsoluteMinutes != 1440 || d.Lockout.MaxFailures != 10 || d.Lockout.LockMinutes != 15 ||
		d.Passwords.MinLength != 12 || d.Banner.Enabled || d.Syslog.Enabled || d.SmartCard.Enabled {
		t.Fatalf("defaults changed behavior: %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(STIGFindings(d)) == 0 {
		t.Fatal("defaults must be reported as below the STIG")
	}
}

func TestSTIGValuesMeetEveryFindingAndKeepStricter(t *testing.T) {
	p := DefaultSecurityPolicy()
	p.Sessions.IdleMinutesAdmin = 5 // stricter than the STIG: kept
	p.Passwords.MinLength = 20
	s := STIGSecurityPolicy(p)
	if g := STIGFindings(s); len(g) != 0 {
		t.Fatalf("STIG values still have findings: %v", g)
	}
	if s.Sessions.IdleMinutesUser != 15 || s.Sessions.IdleMinutesAdmin != 5 || s.Passwords.MinLength != 20 || s.Lockout.MaxFailures != 3 ||
		s.Lockout.LockMinutes != 0 || !s.Banner.Enabled || !strings.Contains(s.Banner.Text, "U.S. Government (USG) Information System") ||
		s.Passwords.History != 5 || s.Passwords.MaxAgeDays != 180 || s.Passwords.MinAgeHours != 24 || s.Sessions.MaxPerUser != 3 {
		t.Fatalf("stig = %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityPolicyValidateNamesTheField(t *testing.T) {
	for want, mut := range map[string]func(*SecurityPolicy){
		"idle_minutes_user": func(p *SecurityPolicy) { p.Sessions.IdleMinutesUser = 0 },
		"banner.text":       func(p *SecurityPolicy) { p.Banner.Enabled, p.Banner.Text = true, " " },
		"max_failures":      func(p *SecurityPolicy) { p.Lockout.MaxFailures = 0 },
		"min_length":        func(p *SecurityPolicy) { p.Passwords.MinLength = 6 },
		"min_age_hours":     func(p *SecurityPolicy) { p.Passwords.MinAgeHours, p.Passwords.MaxAgeDays = 48, 1 },
		"syslog.protocol":   func(p *SecurityPolicy) { p.Syslog.Enabled, p.Syslog.Protocol, p.Syslog.Address = true, "http", "x:1" },
		"syslog.address":    func(p *SecurityPolicy) { p.Syslog.Enabled, p.Syslog.Address = true, "nohost" },
		"client_cert_file":  func(p *SecurityPolicy) { p.Syslog.ClientCertFile = "/a" },
		"smart_card.header": func(p *SecurityPolicy) { p.SmartCard.Source, p.SmartCard.Header = "proxy", "" },
		"smart_card.match":  func(p *SecurityPolicy) { p.SmartCard.Match = []string{"fingerprint"} },
	} {
		p := DefaultSecurityPolicy()
		mut(&p)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestSecurityPolicySaveReadAndSTIGFloor(t *testing.T) {
	ctx := context.Background()
	s := newPolicyStore(t)
	p := DefaultSecurityPolicy()
	p.Sessions.IdleMinutesUser = 30
	if err := s.PutSecurityPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := s.SecurityPolicy(ctx).Sessions.IdleMinutesUser; got != 30 {
		t.Fatalf("read back %d", got)
	}
	SetSTIGBaseline(true)
	t.Cleanup(func() { SetSTIGBaseline(false) })
	if got := s.SecurityPolicy(ctx); got.Sessions.IdleMinutesUser != 15 || !got.Banner.Enabled {
		t.Fatalf("STIG floor not applied: %+v", got.Sessions)
	}
	if stored, _ := s.StoredSecurityPolicy(ctx); stored.Sessions.IdleMinutesUser != 30 {
		t.Fatal("the floor must not rewrite what was saved")
	}
}

func TestPasswordPolicyRules(t *testing.T) {
	p := STIGSecurityPolicy(DefaultSecurityPolicy()).Passwords
	for pw, want := range map[string]string{
		"Short1!":                        "15",
		"alllowercase-but-long-1":        "upper-case",
		"ALLUPPERCASE-BUT-LONG-1":        "lower-case",
		"NoDigitsHere-but-Long":          "digit",
		"NoSpecialCharacters123":         "special",
		"Correct-Horse-Battery-Staple-9": "",
	} {
		err := CheckPasswordPolicy(p, pw, "")
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v (want %q)", pw, err, want)
		}
	}
	if err := CheckPasswordPolicy(p, "Correct-Horse-Battery-Staple-0", "Correct-Horse-Battery-Staple-9"); err == nil || !strings.Contains(err.Error(), "at least 8 characters") {
		t.Fatalf("one-character change accepted: %v", err)
	}
	if err := CheckPasswordPolicy(p, "Totally-Different-Phrase-77", "Correct-Horse-Battery-Staple-9"); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordHistoryAgeAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := newPolicyStore(t)
	pol := STIGSecurityPolicy(DefaultSecurityPolicy())
	pol.Passwords.MinAgeHours = 0 // tested separately below
	if err := s.PutSecurityPolicy(ctx, pol); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateLocalUser(ctx, "pw@example.com", "", "First-Password-0001!", false)
	if err != nil {
		t.Fatal(err)
	}
	pws := []string{"Second-Phrase-0002!", "Third-Sentence-0003!", "Fourth-Words-00004!"}
	cur := "First-Password-0001!"
	for _, p := range pws {
		if err := s.ChangeOwnPassword(ctx, u.ID, cur, p); err != nil {
			t.Fatalf("change to %s: %v", p, err)
		}
		cur = p
	}
	// The first password is within the last 5: refused.
	if err := s.ChangeOwnPassword(ctx, u.ID, cur, "First-Password-0001!"); err == nil || !strings.Contains(err.Error(), "used recently") {
		t.Fatalf("reuse accepted: %v", err)
	}
	// An administrator's temporary password is exempt from history.
	if err := s.SetPassword(ctx, u.ID, "First-Password-0001!", true); err != nil {
		t.Fatalf("admin reset: %v", err)
	}

	// Minimum age.
	pol.Passwords.MinAgeHours = 24
	_ = s.PutSecurityPolicy(ctx, pol)
	if err := s.SetPassword(ctx, u.ID, "Fifth-Choice-000005!", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeOwnPassword(ctx, u.ID, "Fifth-Choice-000005!", "Sixth-Option-0000066!"); err == nil || !strings.Contains(err.Error(), "24 hours") {
		t.Fatalf("change inside minimum age accepted: %v", err)
	}

	// Maximum age: a password older than max_age must be changed at sign-in.
	_ = s.exec(ctx, `UPDATE local_credential SET password_changed_at = ? WHERE user_id = ?`, FormatTime(time.Now().Add(-200*24*time.Hour)), u.ID)
	_, c, err := s.CheckPassword(ctx, "pw@example.com", "Fifth-Choice-000005!")
	if err != nil || !c.MustChange || !c.Expired {
		t.Fatalf("expired password: err=%v cred=%+v", err, c)
	}
}

func TestLockoutWindowAndAdminUnlock(t *testing.T) {
	ctx := context.Background()
	s := newPolicyStore(t)
	p := LockoutPolicy{MaxFailures: 3, WindowMinutes: 15, LockMinutes: 0}
	for i := 1; i <= 3; i++ {
		locked, err := s.RecordSignInFailure(ctx, "Bob@Example.com", p)
		if err != nil || locked != (i == 3) {
			t.Fatalf("attempt %d: locked=%v err=%v", i, locked, err)
		}
	}
	if locked, until := s.SignInLocked(ctx, "bob@example.com"); !locked || !until.IsZero() {
		t.Fatalf("must stay locked until an administrator unlocks: %v %v", locked, until)
	}
	if msg := LockedMessage(time.Time{}); !strings.Contains(msg, "administrator") {
		t.Fatal(msg)
	}
	if l, _ := s.LockedSignIns(ctx); len(l) != 1 || !l[0].UntilUnlocked || l[0].Login != "bob@example.com" {
		t.Fatalf("list = %+v", l)
	}
	// A success does not clear an active lock.
	_ = s.ClearSignInFailures(ctx, "bob@example.com")
	if locked, _ := s.SignInLocked(ctx, "bob@example.com"); !locked {
		t.Fatal("ClearSignInFailures removed a lock")
	}
	if was, _ := s.UnlockSignIn(ctx, "bob@example.com"); !was {
		t.Fatal("unlock did not report the lock")
	}
	if locked, _ := s.SignInLocked(ctx, "bob@example.com"); locked {
		t.Fatal("still locked")
	}

	// Failures outside the window start over; a timed lock expires.
	timed := LockoutPolicy{MaxFailures: 2, WindowMinutes: 15, LockMinutes: 1}
	_, _ = s.RecordSignInFailure(ctx, "carol@example.com", timed)
	_ = s.exec(ctx, `UPDATE sign_in_lockout SET first_failed_at = ? WHERE login = ?`, FormatTime(time.Now().Add(-20*time.Minute)), "carol@example.com")
	if locked, _ := s.RecordSignInFailure(ctx, "carol@example.com", timed); locked {
		t.Fatal("old failure counted")
	}
	if locked, _ := s.RecordSignInFailure(ctx, "carol@example.com", timed); !locked {
		t.Fatal("second failure in window did not lock")
	}
	_ = s.exec(ctx, `UPDATE sign_in_lockout SET locked_until = ? WHERE login = ?`, FormatTime(time.Now().Add(-time.Second)), "carol@example.com")
	if locked, _ := s.SignInLocked(ctx, "carol@example.com"); locked {
		t.Fatal("timed lock did not expire")
	}
}

func TestAuditFiltersSortAndForwarder(t *testing.T) {
	ctx := context.Background()
	s := newPolicyStore(t)
	var got []AuditEntry
	SetAuditForwarder(func(e AuditEntry) { got = append(got, e) })
	t.Cleanup(func() { SetAuditForwarder(nil) })
	add := func(action, actor, ip, outcome string) {
		if err := s.AppendAudit(ctx, &AuditEntry{Action: action, ActorLabel: actor, ResourceType: "session", SourceIP: ip, Outcome: outcome}); err != nil {
			t.Fatal(err)
		}
	}
	add("sign_in", "zed@example.com", "10.0.0.1", "")
	add("sign_in_failed", "amy@example.com", "10.0.0.2", AuditFailure)
	add("access_denied", "amy@example.com", "10.0.0.2", AuditFailure)
	add("upstream_created", "admin@example.com", "10.0.0.9", "")
	if len(got) != 4 || got[0].Outcome != AuditSuccess {
		t.Fatalf("forwarded %d: %+v", len(got), got)
	}
	count := func(f AuditFilter) int {
		_, n, err := s.ListAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(AuditFilter{Category: AuditCategoryAuthentication}); n != 2 {
		t.Fatalf("authentication = %d", n)
	}
	if n := count(AuditFilter{Category: AuditCategoryAccess}); n != 1 {
		t.Fatalf("access = %d", n)
	}
	if n := count(AuditFilter{Category: AuditCategoryAdministration}); n != 1 {
		t.Fatalf("administration = %d", n)
	}
	if n := count(AuditFilter{Outcome: AuditFailure}); n != 2 {
		t.Fatalf("failures = %d", n)
	}
	if n := count(AuditFilter{IP: "10.0.0.2"}); n != 2 {
		t.Fatalf("ip = %d", n)
	}
	if n := count(AuditFilter{Query: "AMY@"}); n != 2 {
		t.Fatalf("query = %d", n)
	}
	if n := count(AuditFilter{Query: "100%_"}); n != 0 {
		t.Fatalf("LIKE wildcards not escaped: %d", n)
	}
	rows, _, _ := s.ListAudit(ctx, AuditFilter{Sort: "actor", Asc: true})
	if rows[0].ActorLabel != "admin@example.com" || rows[len(rows)-1].ActorLabel != "zed@example.com" {
		t.Fatalf("sort by actor: %s … %s", rows[0].ActorLabel, rows[len(rows)-1].ActorLabel)
	}
	// Inside a request transaction the record is forwarded only on commit.
	got = nil
	txctx, finish, err := s.BeginRequestTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendAudit(txctx, &AuditEntry{Action: "rolled_back", ResourceType: "x"})
	_ = finish(false)
	txctx, finish, _ = s.BeginRequestTx(ctx)
	_ = s.AppendAudit(txctx, &AuditEntry{Action: "committed", ResourceType: "x"})
	if len(got) != 0 {
		t.Fatal("forwarded before commit")
	}
	_ = finish(true)
	if len(got) != 1 || got[0].Action != "committed" {
		t.Fatalf("after commit: %+v", got)
	}
}
