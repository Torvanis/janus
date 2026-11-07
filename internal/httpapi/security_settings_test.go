package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/pki"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

func setPolicy(t *testing.T, h *harness, mut func(*store.SecurityPolicy)) {
	t.Helper()
	p := store.DefaultSecurityPolicy()
	mut(&p)
	if err := h.store.PutSecurityPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func auditRows(t *testing.T, h *harness, f store.AuditFilter) []*store.AuditEntry {
	t.Helper()
	f.Limit = 500
	rows, _, err := h.store.ListAudit(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func hasAudit(rows []*store.AuditEntry, action, outcome string, contains ...string) bool {
	for _, r := range rows {
		if r.Action != action || (outcome != "" && r.Outcome != outcome) {
			continue
		}
		ok := true
		for _, c := range contains {
			if !strings.Contains(r.NewValue+r.ActorLabel+r.SourceIP, c) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The banner must be acknowledged before any sign-in: the local form and a
// redirecting method (dev / OIDC) are refused without it, and the
// acknowledgment is audited and single-use.
func TestBannerGatesEverySignIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateLocalUser(ctx, "alice@example.com", "Alice", "alice-password-12", false); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Banner = store.BannerPolicy{Enabled: true, Title: "Notice", Text: store.DoDBannerText, Button: "I agree"}
	})
	get := httptest.NewRecorder()
	h.handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/auth/banner", nil))
	if m := decode(t, get); m["enabled"] != true || m["acknowledged"] != false || !strings.Contains(m["text"].(string), "USG-authorized use only") {
		t.Fatalf("banner: %v", m)
	}
	login := func(cookies []*http.Cookie) *httptest.ResponseRecorder {
		return h.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12"}, cookies)
	}
	if r := login(nil); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), "banner_required") {
		t.Fatalf("sign-in without acknowledgment: %d %s", r.Code, r.Body.String())
	}
	// Dev auth (a redirecting method) is refused too. The FIPS edition
	// refuses dev auth altogether, which is also a refusal.
	h.server.Config.DevAuthEnabled = true
	dev := httptest.NewRecorder()
	h.handler.ServeHTTP(dev, httptest.NewRequest(http.MethodGet, "/auth/start?email=x@example.com", nil))
	loc := dev.Header().Get("Location")
	if !strings.Contains(loc, "error=") || (!fipsmode.IsFIPSEdition() && !strings.Contains(loc, "acknowledge")) {
		t.Fatalf("dev sign-in without acknowledgment: %d %s", dev.Code, loc)
	}
	for _, c := range dev.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			t.Fatal("dev sign-in without acknowledgment set a session")
		}
	}

	ack := h.jsonPost("/auth/banner/ack", map[string]any{}, nil)
	consent := ack.Result().Cookies()
	if ack.Code != http.StatusOK || len(consent) != 1 || consent[0].Name != consentCookie || !consent[0].HttpOnly {
		t.Fatalf("ack: %d %v", ack.Code, consent)
	}
	ok := login(consent)
	if ok.Code != http.StatusOK {
		t.Fatalf("sign-in after acknowledgment: %d %s", ok.Code, ok.Body.String())
	}
	cleared := false
	for _, c := range ok.Result().Cookies() {
		if c.Name == consentCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the acknowledgment must be used up by the sign-in")
	}
	// A forged or edited banner invalidates the acknowledgment.
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Banner = store.BannerPolicy{Enabled: true, Title: "Notice", Text: "Different text", Button: "OK"}
	})
	if r := login(consent); r.Code != http.StatusForbidden {
		t.Fatalf("acknowledgment of the old text accepted: %d", r.Code)
	}
	forged := []*http.Cookie{{Name: consentCookie, Value: "9999999999.AAAA"}}
	if r := login(forged); r.Code != http.StatusForbidden {
		t.Fatalf("forged acknowledgment accepted: %d", r.Code)
	}
	rows := auditRows(t, h, store.AuditFilter{Category: store.AuditCategoryAuthentication})
	if !hasAudit(rows, "banner_acknowledged", store.AuditSuccess) || !hasAudit(rows, "sign_in", store.AuditSuccess, "local") {
		t.Fatalf("audit rows: %+v", rows)
	}
}

// Lockout follows the policy for local and directory accounts alike, is
// audited with the source address, and an administrator unlocks it.
func TestLockoutPolicyAndAdminUnlock(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	if _, err := h.store.CreateLocalUser(ctx, "alice@example.com", "Alice", "alice-password-12", false); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Lockout = store.LockoutPolicy{MaxFailures: 3, WindowMinutes: 15, LockMinutes: 0}
	})
	try := func(pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(`{"email":"alice@example.com","password":"`+pw+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.44:5555"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	for i := 1; i <= 2; i++ {
		if r := try("wrong-password-1"); r.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.Code)
		}
	}
	if r := try("wrong-password-1"); r.Code != http.StatusLocked || !strings.Contains(r.Body.String(), "administrator to unlock") {
		t.Fatalf("third failure: %d %s", r.Code, r.Body.String())
	}
	if r := try("alice-password-12"); r.Code != http.StatusLocked {
		t.Fatalf("right password while locked: %d", r.Code)
	}
	rows := auditRows(t, h, store.AuditFilter{IP: "192.0.2.44"})
	if !hasAudit(rows, "account_locked", store.AuditFailure) || !hasAudit(rows, "sign_in_failed", store.AuditFailure, "bad_credentials", "alice@example.com") {
		t.Fatalf("lockout audit: %+v", rows)
	}
	list := decode(t, h.do(http.MethodGet, "/api/v1/admin/security/lockouts", nil))
	if ls, _ := list["lockouts"].([]any); len(ls) != 1 {
		t.Fatalf("lockouts: %v", list)
	}
	if r := h.do(http.MethodPost, "/api/v1/admin/security/lockouts/unlock", map[string]any{"login": "Alice@Example.com"}); r.Code != http.StatusOK {
		t.Fatalf("unlock: %d %s", r.Code, r.Body.String())
	}
	if r := try("alice-password-12"); r.Code != http.StatusOK {
		t.Fatalf("after unlock: %d %s", r.Code, r.Body.String())
	}
	if !hasAudit(auditRows(t, h, store.AuditFilter{}), "account_unlocked", "") {
		t.Fatal("unlock not audited")
	}
}

func TestDirectorySignInCountsTowardLockout(t *testing.T) {
	u := ldapURL(t)
	h := newHarness(t)
	promote(t, h)
	bizLicense(t, h)
	if rec := h.do(http.MethodPut, "/api/v1/admin/directory", fixtureDirectory(u)); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Lockout = store.LockoutPolicy{MaxFailures: 2, WindowMinutes: 15, LockMinutes: 5}
	})
	login := func(pw string) *httptest.ResponseRecorder {
		return h.jsonPost("/auth/local/login", map[string]string{"email": "ada@example.com", "password": pw}, nil)
	}
	_ = login("nope")
	if r := login("nope"); r.Code != http.StatusLocked || !strings.Contains(r.Body.String(), "minute") {
		t.Fatalf("directory lockout: %d %s", r.Code, r.Body.String())
	}
	if r := login("pw-ada"); r.Code != http.StatusLocked {
		t.Fatalf("directory password while locked: %d", r.Code)
	}
}

// Session limits come from the policy: idle per role, the absolute cap,
// background polling does not count as activity, a timeout is audited,
// and the per-person cap ends the oldest session.
func TestSessionPolicyIdleAbsoluteAndCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sessions := auth.NewDBSessionStore(h.store, 24*time.Hour, 24*time.Hour)
	h.server.Sessions = sessions
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.Sessions = store.SessionPolicy{IdleMinutesUser: 15, IdleMinutesAdmin: 10, AbsoluteMinutes: 60, MaxPerUser: 2}
	})
	u, _, _ := h.store.UpsertUserFromIdentity(ctx, "sub-pat", "pat@example.com", "Pat", false, false)
	sess, err := sessions.Create(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	me := func(background bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess.ID})
		if background {
			req.Header.Set(auth.ActivityHeader, "background")
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	r := me(false)
	if r.Code != http.StatusOK {
		t.Fatalf("me: %d", r.Code)
	}
	if s, _ := decode(t, r)["session"].(map[string]any); s["idle_timeout_seconds"] != float64(15*60) {
		t.Fatalf("session info: %v", s)
	}
	// 14 minutes idle, then a background poll: still signed in, but the
	// poll does not reset the clock.
	_ = h.store.TouchWebSession(ctx, sess.ID, time.Now().UTC().Add(-14*time.Minute))
	if r := me(true); r.Code != http.StatusOK {
		t.Fatalf("background poll at 14 min: %d", r.Code)
	}
	ws, _ := h.store.WebSessionByID(ctx, sess.ID)
	if time.Since(ws.LastSeenAt) < 13*time.Minute {
		t.Fatal("a background request counted as activity")
	}
	_ = h.store.TouchWebSession(ctx, sess.ID, time.Now().UTC().Add(-16*time.Minute))
	if r := me(false); r.Code != http.StatusUnauthorized || r.Header().Get("X-Janus-Session-Ended") != "idle" {
		t.Fatalf("idle 16 min: %d %v", r.Code, r.Header())
	}
	if !hasAudit(auditRows(t, h, store.AuditFilter{Action: "session_timeout"}), "session_timeout", "", "idle") {
		t.Fatal("timeout not audited")
	}

	// Administrators: 10 minutes.
	admin, _, _ := h.store.UpsertUserFromIdentity(ctx, "sub-adm", "adm@example.com", "Adm", true, false)
	as, _ := sessions.Create(ctx, admin.ID)
	_ = h.store.TouchWebSession(ctx, as.ID, time.Now().UTC().Add(-11*time.Minute))
	if _, err := sessions.Get(ctx, as.ID); err == nil {
		t.Fatal("administrator idle 11 min still signed in")
	}
	// Absolute: shortened after the session began.
	ls, _ := sessions.Create(ctx, u.ID)
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE web_session SET created_at = ?, last_seen_at = ? WHERE id = ?`,
		store.FormatTime(time.Now().UTC().Add(-61*time.Minute)), store.FormatTime(time.Now().UTC()), ls.ID); err != nil {
		t.Fatal(err)
	}
	var te *auth.TimeoutError
	if _, err := sessions.Get(ctx, ls.ID); err == nil || !asTimeout(err, &te) || te.Reason != "expired" {
		t.Fatalf("absolute limit: %v", err)
	}

	// Cap: a third sign-in ends the oldest (password sign-in works in both
	// editions).
	if _, err := h.store.CreateLocalUser(ctx, "cap@example.com", "Cap", "cap-password-1234", false); err != nil {
		t.Fatal(err)
	}
	signIn := func() *http.Cookie {
		rec := h.jsonPost("/auth/local/login", map[string]any{"email": "cap@example.com", "password": "cap-password-1234"}, nil)
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.SessionCookie {
				return c
			}
		}
		t.Fatalf("no session: %d %s", rec.Code, rec.Body.String())
		return nil
	}
	first := signIn()
	time.Sleep(10 * time.Millisecond)
	signIn()
	time.Sleep(10 * time.Millisecond)
	signIn()
	if _, err := sessions.Get(ctx, first.Value); err == nil {
		t.Fatal("the oldest session survived the cap")
	}
	capUser, _ := h.store.UserByEmail(ctx, "cap@example.com")
	if n, _ := sessions.Count(ctx, capUser.ID); n != 2 {
		t.Fatalf("sessions after cap: %d", n)
	}
	rows := auditRows(t, h, store.AuditFilter{Category: store.AuditCategoryAuthentication})
	if !hasAudit(rows, "session_ended_cap", "") || !hasAudit(rows, "sign_in", "", "concurrent_sessions") {
		t.Fatal("cap / concurrent sign-in not audited")
	}
}

func asTimeout(err error, te **auth.TimeoutError) bool {
	t, ok := err.(*auth.TimeoutError)
	if ok {
		*te = t
	}
	return ok
}

// Not being signed in is not an audit event: no credential, or a session
// cookie that has ended, on any path. A bad API token is.
func TestNotSignedInNotAudited(t *testing.T) {
	h := newHarness(t)
	call := func(path, ip string, cookie, bearer bool) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":1000"
		if cookie {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "no-such-session"})
		}
		if bearer {
			req.Header.Set("Authorization", "Bearer janus_not-a-real-token")
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/admin/users", "/api/v1/dashboard/global"} {
		if code := call(path, "198.51.100.21", false, false); code != http.StatusUnauthorized {
			t.Fatalf("no credential %s: %d", path, code)
		}
		if code := call(path, "198.51.100.22", true, false); code != http.StatusUnauthorized {
			t.Fatalf("dead session %s: %d", path, code)
		}
	}
	for _, ip := range []string{"198.51.100.21", "198.51.100.22"} {
		if rows := auditRows(t, h, store.AuditFilter{Action: "access_denied", IP: ip}); len(rows) != 0 {
			t.Fatalf("not signed in was audited (%s): %+v", ip, rows)
		}
	}
	call("/api/v1/admin/users", "198.51.100.23", false, true)
	rows := auditRows(t, h, store.AuditFilter{Action: "access_denied", IP: "198.51.100.23"})
	if len(rows) != 1 || !strings.Contains(rows[0].NewValue, `"credential":"invalid_token"`) {
		t.Fatalf("bad API token: want 1 invalid_token audit row, got %+v", rows)
	}
}

// Refused requests are audited (rate-limited), and the audit list filters
// and sorts.
func TestAccessDeniedAuditedOncePerMinute(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	plain, _, _ := h.store.UpsertUserFromIdentity(ctx, "sub-plain", "plain@example.com", "Plain", false, false)
	_, tok, err := h.store.CreateToken(ctx, plain.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.RemoteAddr = "198.51.100.7:1000"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Fatalf("non-admin reached admin API: %d", rec.Code)
		}
	}
	rows := auditRows(t, h, store.AuditFilter{Action: "access_denied", IP: "198.51.100.7"})
	if len(rows) != 1 || !strings.Contains(rows[0].NewValue, "/api/v1/admin/users") || rows[0].Outcome != store.AuditFailure {
		t.Fatalf("access_denied rows: %+v", rows)
	}
	promote(t, h)
	res := decode(t, h.do(http.MethodGet, "/api/v1/admin/audit?category=access&outcome=failure&sort=ip&dir=asc", nil))
	if res["total_count"] != float64(1) {
		t.Fatalf("filtered list: %v", res)
	}
}

// The STIG profile refuses weaker settings; Validate errors are 400.
func TestSecurityPolicyAPIAndSTIGRefusal(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	doc := decode(t, h.do(http.MethodGet, "/api/v1/admin/security/policy", nil))
	if gaps, _ := doc["stig_gaps"].([]any); len(gaps) == 0 || doc["stig_profile"] != false {
		t.Fatalf("doc: %v", doc)
	}
	p := store.DefaultSecurityPolicy()
	p.Passwords.MinLength = 4
	if r := h.do(http.MethodPut, "/api/v1/admin/security/policy", p); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "min_length") {
		t.Fatalf("invalid: %d %s", r.Code, r.Body.String())
	}
	store.SetSTIGBaseline(true)
	t.Cleanup(func() { store.SetSTIGBaseline(false) })
	weak := store.DefaultSecurityPolicy()
	if r := h.do(http.MethodPut, "/api/v1/admin/security/policy", weak); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "APSC-DV-000070") {
		t.Fatalf("weak under STIG: %d %s", r.Code, r.Body.String())
	}
	strong := store.STIGSecurityPolicy(store.DefaultSecurityPolicy())
	if r := h.do(http.MethodPut, "/api/v1/admin/security/policy", strong); r.Code != http.StatusOK {
		t.Fatalf("STIG values: %d %s", r.Code, r.Body.String())
	}
	changed := auditRows(t, h, store.AuditFilter{Action: "security_policy_changed"})
	if len(changed) == 0 || changed[0].SourceIP == "" {
		t.Fatalf("policy change not audited with its source address: %+v", changed)
	}
}

// Smart cards: a CAC-shaped certificate from a configured CA signs in the
// matching person, through a trusted proxy header and on a direct TLS
// connection; untrusted CAs, untrusted proxies and unknown cards are
// refused and audited.
func TestSmartCardSignIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	card, err := pki.NewTestCard("DOE.JANE.Q.1234567890", "1234567890@mil", "jane.doe@example.mil")
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := pki.NewTestCard("ROE.RICH.2222222222", "2222222222@mil", "")
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(bundle, card.CAPEM, 0o600)
	restore, err := truststore.Configure(truststore.Options{BundleFile: bundle})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	jane, _, _ := h.store.UpsertUserFromIdentity(ctx, "sub-jane", "jane@example.com", "Jane", false, false)
	if err := h.store.SetPKIIdentity(ctx, jane.ID, "1234567890"); err != nil {
		t.Fatal(err)
	}
	_, proxyNet, _ := net.ParseCIDR("10.9.0.0/16")
	h.server.Config.TrustedProxies = append(h.server.Config.TrustedProxies, proxyNet)
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.SmartCard = store.SmartCardPolicy{Enabled: true, Source: "proxy", Header: "X-Forwarded-Client-Cert", HeaderFormat: "xfcc", Match: []string{"upn", "email", "edipi"}}
	})
	viaProxy := func(c *pki.TestCard, peer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/pki?redirect_uri=/models", nil)
		req.RemoteAddr = peer
		if c != nil {
			req.Header.Set("X-Forwarded-Client-Cert", `Hash=x;Cert="`+url.QueryEscape(string(c.CertPEM))+`"`)
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	sessionCookie := func(r *httptest.ResponseRecorder) bool {
		for _, c := range r.Result().Cookies() {
			if c.Name == auth.SessionCookie && c.Value != "" {
				return true
			}
		}
		return false
	}
	ok := viaProxy(card, "10.9.1.1:443")
	if ok.Code != http.StatusFound || ok.Header().Get("Location") != "/models" || !sessionCookie(ok) {
		t.Fatalf("card via proxy: %d %s", ok.Code, ok.Header().Get("Location"))
	}
	if r := viaProxy(card, "203.0.113.9:443"); sessionCookie(r) || !strings.Contains(r.Header().Get("Location"), "error=") {
		t.Fatal("certificate header from an untrusted address accepted")
	}
	if r := viaProxy(stranger, "10.9.1.1:443"); sessionCookie(r) {
		t.Fatal("card from an unconfigured CA accepted")
	}
	if r := viaProxy(nil, "10.9.1.1:443"); !strings.Contains(r.Header().Get("Location"), "No+smart-card") {
		t.Fatalf("no certificate: %s", r.Header().Get("Location"))
	}
	rows := auditRows(t, h, store.AuditFilter{Category: store.AuditCategoryAuthentication})
	if !hasAudit(rows, "sign_in", store.AuditSuccess, "smart_card", "edipi") || !hasAudit(rows, "sign_in_failed", store.AuditFailure, "untrusted") {
		t.Fatalf("card audit: %+v", rows)
	}

	// Direct: the TLS connection carries the card.
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.SmartCard = store.SmartCardPolicy{Enabled: true, Source: "direct", Header: "X", HeaderFormat: "xfcc", Match: []string{"upn", "edipi"}}
	})
	req := httptest.NewRequest(http.MethodGet, "/auth/pki", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{card.Cert}}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if !sessionCookie(rec) {
		t.Fatalf("card on the TLS connection: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Card required for administrators: a password sign-in is refused.
	setPolicy(t, h, func(p *store.SecurityPolicy) {
		p.SmartCard = store.SmartCardPolicy{Enabled: true, Source: "direct", Header: "X", HeaderFormat: "xfcc", Match: []string{"upn"}, RequireForAdmins: true}
	})
	if _, err := h.store.CreateLocalUser(ctx, "boss@example.com", "Boss", "boss-password-123", true); err != nil {
		t.Fatal(err)
	}
	if r := h.jsonPost("/auth/local/login", map[string]any{"email": "boss@example.com", "password": "boss-password-123"}, nil); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), "smart card") {
		t.Fatalf("admin password sign-in with card required: %d %s", r.Code, r.Body.String())
	}
	// Providers list offers the card.
	pr := httptest.NewRecorder()
	h.handler.ServeHTTP(pr, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
	if !strings.Contains(pr.Body.String(), "/auth/pki") {
		t.Fatalf("providers: %s", pr.Body.String())
	}
}
