package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SettingSecurityPolicy holds the instance's security policy (sessions,
// sign-in banner, lockout, passwords, audit forwarding, smart cards) as one
// JSON document, edited in Administration → Settings → Security.
const SettingSecurityPolicy = "security.policy"

// securityPolicyMigration adds what the policy needs: the source address and
// outcome on audit rows (authentication events are audited now), password
// history and age, a per-email lockout table shared by local and directory
// sign-in, and the smart-card identity a person can be matched by.
var securityPolicyMigration = migration{name: "0073_security_policy", stmt: []string{
	`ALTER TABLE audit_log ADD COLUMN source_ip TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE audit_log ADD COLUMN outcome TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_audit_action_created ON audit_log(action, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_ip ON audit_log(source_ip, created_at)`,
	`ALTER TABLE local_credential ADD COLUMN password_history TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE local_credential ADD COLUMN password_changed_at TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS sign_in_lockout (
		login TEXT PRIMARY KEY,
		failed INTEGER NOT NULL DEFAULT 0,
		first_failed_at TEXT NOT NULL DEFAULT '',
		locked_until TEXT NOT NULL DEFAULT '',
		locked_at TEXT NOT NULL DEFAULT ''
	)`,
	`ALTER TABLE app_user ADD COLUMN pki_identity TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_app_user_pki ON app_user(pki_identity)`,
}, down: []string{
	`DROP TABLE sign_in_lockout`,
	`DROP INDEX IF EXISTS idx_audit_action_created`,
	`DROP INDEX IF EXISTS idx_audit_ip`,
	`DROP INDEX IF EXISTS idx_app_user_pki`,
}}

// SecurityPolicy is the instance security policy. Zero values never mean
// "weakest": DefaultSecurityPolicy is the starting point and Normalize fills
// anything left out.
type SecurityPolicy struct {
	Sessions  SessionPolicy   `json:"sessions"`
	Banner    BannerPolicy    `json:"banner"`
	Lockout   LockoutPolicy   `json:"lockout"`
	Passwords PasswordPolicy  `json:"passwords"`
	Syslog    SyslogPolicy    `json:"syslog"`
	SmartCard SmartCardPolicy `json:"smart_card"`
}

// SessionPolicy bounds browser sessions, in minutes.
type SessionPolicy struct {
	IdleMinutesUser  int `json:"idle_minutes_user"`
	IdleMinutesAdmin int `json:"idle_minutes_admin"`
	AbsoluteMinutes  int `json:"absolute_minutes"`
	// MaxPerUser caps concurrent sessions per person; 0 = no cap. At the
	// cap a new sign-in ends the person's oldest session.
	MaxPerUser int `json:"max_per_user"`
}

// BannerPolicy is the notice shown, and acknowledged, before sign-in.
type BannerPolicy struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	// Button is the acknowledgment button's label.
	Button string `json:"button"`
}

// LockoutPolicy locks a sign-in name after repeated failures.
type LockoutPolicy struct {
	MaxFailures   int `json:"max_failures"`
	WindowMinutes int `json:"window_minutes"`
	// LockMinutes is how long the lock lasts; 0 = until an administrator
	// unlocks the account.
	LockMinutes int `json:"lock_minutes"`
}

// PasswordPolicy applies to local accounts.
type PasswordPolicy struct {
	MinLength      int  `json:"min_length"`
	RequireUpper   bool `json:"require_upper"`
	RequireLower   bool `json:"require_lower"`
	RequireDigit   bool `json:"require_digit"`
	RequireSpecial bool `json:"require_special"`
	// MinChangedChars: a new password must differ from the current one in
	// at least this many characters (self-service changes; 0 = off).
	MinChangedChars int `json:"min_changed_chars"`
	// History: a new password may not match any of the last N (0 = off).
	History int `json:"history"`
	// MinAgeHours: a password cannot be changed again sooner (0 = off;
	// administrators' resets are exempt).
	MinAgeHours int `json:"min_age_hours"`
	// MaxAgeDays: after this a password must be changed at sign-in (0 = never).
	MaxAgeDays int `json:"max_age_days"`
}

// SyslogPolicy forwards every audit record to a syslog server or SIEM.
type SyslogPolicy struct {
	Enabled bool `json:"enabled"`
	// Protocol: udp, tcp or tls.
	Protocol string `json:"protocol"`
	// Address is host:port.
	Address string `json:"address"`
	// Format: rfc5424 (default) or cef.
	Format   string `json:"format"`
	Facility int    `json:"facility"`
	// AppName is the APP-NAME (RFC 5424) / device product (CEF): distinguishes
	// this Janus from other applications' logs. Hostname is the instance.
	AppName string `json:"app_name"`
	// ServerName overrides the TLS server name (default: the host).
	ServerName string `json:"server_name"`
	// ClientCertFile / ClientKeyFile: mounted files for mutual TLS.
	ClientCertFile string `json:"client_cert_file"`
	ClientKeyFile  string `json:"client_key_file"`
}

// SmartCardPolicy is CAC/PIV sign-in with a client certificate.
type SmartCardPolicy struct {
	Enabled bool `json:"enabled"`
	// Source: direct (Janus terminates TLS and asks for the certificate) or
	// proxy (a trusted proxy forwards the verified certificate in Header).
	Source string `json:"source"`
	Header string `json:"header"`
	// HeaderFormat: xfcc (Envoy/Istio), pem (URL-escaped PEM, nginx
	// $ssl_client_escaped_cert) or base64 (DER or PEM, base64).
	HeaderFormat string `json:"header_format"`
	// Match is the order identities are tried: upn, email, edipi.
	Match []string `json:"match"`
	// Provision creates an account for a valid card no person matches;
	// otherwise the sign-in is refused (default).
	Provision bool `json:"provision"`
	// RequireForAdmins: administrators may only sign in with a card.
	RequireForAdmins bool `json:"require_for_admins"`
}

// DoDBannerText is the Standard Mandatory DoD Notice and Consent Banner.
const DoDBannerText = `You are accessing a U.S. Government (USG) Information System (IS) that is provided for USG-authorized use only.

By using this IS (which includes any device attached to this IS), you consent to the following conditions:

-The USG routinely intercepts and monitors communications on this IS for purposes including, but not limited to, penetration testing, COMSEC monitoring, network operations and defense, personnel misconduct (PM), law enforcement (LE), and counterintelligence (CI) investigations.

-At any time, the USG may inspect and seize data stored on this IS.

-Communications using, or data stored on, this IS are not private, are subject to routine monitoring, interception, and search, and may be disclosed or used for any USG-authorized purpose.

-This IS includes security measures (e.g., authentication and access controls) to protect USG interests--not for your personal benefit or privacy.

-Notwithstanding the above, using this IS does not constitute consent to PM, LE or CI investigative searching or monitoring of the content of privileged communications, or work product, related to personal representation or services by attorneys, psychotherapists, or clergy, and their assistants. Such communications and work product are private and confidential. See User Agreement for details.`

// DefaultSecurityPolicy is what an instance runs before an administrator
// changes anything: the behavior Janus had before the policy existed.
func DefaultSecurityPolicy() SecurityPolicy {
	return SecurityPolicy{
		Sessions:  SessionPolicy{IdleMinutesUser: 240, IdleMinutesAdmin: 240, AbsoluteMinutes: 1440},
		Banner:    BannerPolicy{Title: "Notice", Text: "", Button: "I acknowledge"},
		Lockout:   LockoutPolicy{MaxFailures: 10, WindowMinutes: 15, LockMinutes: 15},
		Passwords: PasswordPolicy{MinLength: 12},
		Syslog:    SyslogPolicy{Protocol: "tls", Format: "rfc5424", Facility: 10, AppName: "janus"},
		SmartCard: SmartCardPolicy{Source: "direct", Header: "X-Forwarded-Client-Cert", HeaderFormat: "xfcc", Match: []string{"upn", "email", "edipi"}},
	}
}

// STIGSecurityPolicy is p raised to the Application Security and
// Development STIG values. Settings already stricter are kept.
func STIGSecurityPolicy(p SecurityPolicy) SecurityPolicy {
	s := &p.Sessions
	s.IdleMinutesUser = minPos(s.IdleMinutesUser, 15)
	s.IdleMinutesAdmin = minPos(s.IdleMinutesAdmin, 10)
	s.AbsoluteMinutes = minPos(s.AbsoluteMinutes, 1440)
	if s.MaxPerUser == 0 {
		s.MaxPerUser = 3
	}
	if !p.Banner.Enabled || strings.TrimSpace(p.Banner.Text) == "" {
		p.Banner = BannerPolicy{Enabled: true, Title: "U.S. Government Information System", Text: DoDBannerText, Button: firstNonEmpty(p.Banner.Button, "I acknowledge")}
	}
	l := &p.Lockout
	l.MaxFailures = minPos(l.MaxFailures, 3)
	l.WindowMinutes = max(l.WindowMinutes, 15)
	l.LockMinutes = 0 // until an administrator unlocks
	pw := &p.Passwords
	pw.MinLength = max(pw.MinLength, 15)
	pw.RequireUpper, pw.RequireLower, pw.RequireDigit, pw.RequireSpecial = true, true, true, true
	pw.MinChangedChars = max(pw.MinChangedChars, 8)
	pw.History = max(pw.History, 5)
	pw.MinAgeHours = max(pw.MinAgeHours, 24)
	pw.MaxAgeDays = minPos(pw.MaxAgeDays, 180)
	return p
}

// STIGFindings lists where p is weaker than the STIG values (empty: none).
// Syslog and smart cards are not checked: they need a destination and a
// card infrastructure the policy cannot invent.
func STIGFindings(p SecurityPolicy) []string {
	var out []string
	add := func(bad bool, msg string) {
		if bad {
			out = append(out, msg)
		}
	}
	s := p.Sessions
	add(s.IdleMinutesUser > 15, "sessions: user idle timeout must be 15 minutes or less (APSC-DV-000070)")
	add(s.IdleMinutesAdmin > 10, "sessions: administrator idle timeout must be 10 minutes or less (APSC-DV-000080)")
	add(s.MaxPerUser == 0, "sessions: the number of sessions per person must be limited (APSC-DV-000010)")
	add(!p.Banner.Enabled || strings.TrimSpace(p.Banner.Text) == "", "banner: the Notice and Consent banner must be shown before sign-in (APSC-DV-000520)")
	add(p.Lockout.MaxFailures > 3 || p.Lockout.WindowMinutes < 15, "lockout: lock after 3 failed attempts in 15 minutes (APSC-DV-000500)")
	add(p.Lockout.LockMinutes != 0, "lockout: a locked account must stay locked until an administrator unlocks it (APSC-DV-000510)")
	pw := p.Passwords
	add(pw.MinLength < 15, "passwords: minimum length must be at least 15 (APSC-DV-001680)")
	add(!pw.RequireUpper || !pw.RequireLower || !pw.RequireDigit || !pw.RequireSpecial, "passwords: upper case, lower case, digit and special character must all be required (APSC-DV-001690..001720)")
	add(pw.MinChangedChars < 8, "passwords: a change must alter at least 8 characters (APSC-DV-001730)")
	add(pw.History < 5, "passwords: the last 5 passwords may not be reused (APSC-DV-001780)")
	add(pw.MinAgeHours < 24, "passwords: minimum lifetime must be 24 hours (APSC-DV-001760)")
	add(pw.MaxAgeDays == 0 || pw.MaxAgeDays > 180, "passwords: maximum lifetime must be 180 days or less (APSC-DV-001770)")
	return out
}

func minPos(v, limit int) int {
	if v <= 0 || v > limit {
		return limit
	}
	return v
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Validate checks ranges; the message names the field.
func (p *SecurityPolicy) Validate() error {
	s := p.Sessions
	switch {
	case s.IdleMinutesUser < 1 || s.IdleMinutesUser > 10080:
		return errors.New("sessions.idle_minutes_user must be 1 to 10080")
	case s.IdleMinutesAdmin < 1 || s.IdleMinutesAdmin > 10080:
		return errors.New("sessions.idle_minutes_admin must be 1 to 10080")
	case s.AbsoluteMinutes < 5 || s.AbsoluteMinutes > 43200:
		return errors.New("sessions.absolute_minutes must be 5 to 43200")
	case s.MaxPerUser < 0 || s.MaxPerUser > 100:
		return errors.New("sessions.max_per_user must be 0 (no limit) to 100")
	}
	if p.Banner.Enabled && strings.TrimSpace(p.Banner.Text) == "" {
		return errors.New("banner.text is required when the banner is on")
	}
	if len(p.Banner.Text) > 20000 || len(p.Banner.Title) > 200 || len(p.Banner.Button) > 60 {
		return errors.New("banner: title up to 200, text up to 20000, button up to 60 characters")
	}
	l := p.Lockout
	switch {
	case l.MaxFailures < 1 || l.MaxFailures > 100:
		return errors.New("lockout.max_failures must be 1 to 100")
	case l.WindowMinutes < 1 || l.WindowMinutes > 1440:
		return errors.New("lockout.window_minutes must be 1 to 1440")
	case l.LockMinutes < 0 || l.LockMinutes > 10080:
		return errors.New("lockout.lock_minutes must be 0 (until unlocked) to 10080")
	}
	pw := p.Passwords
	switch {
	case pw.MinLength < 8 || pw.MinLength > PasswordMaxLen:
		return fmt.Errorf("passwords.min_length must be 8 to %d", PasswordMaxLen)
	case pw.MinChangedChars < 0 || pw.MinChangedChars > pw.MinLength:
		return errors.New("passwords.min_changed_chars must be 0 to the minimum length")
	case pw.History < 0 || pw.History > 24:
		return errors.New("passwords.history must be 0 to 24")
	case pw.MinAgeHours < 0 || pw.MinAgeHours > 720:
		return errors.New("passwords.min_age_hours must be 0 to 720")
	case pw.MaxAgeDays < 0 || pw.MaxAgeDays > 3650:
		return errors.New("passwords.max_age_days must be 0 (never) to 3650")
	case pw.MaxAgeDays > 0 && pw.MinAgeHours >= pw.MaxAgeDays*24:
		return errors.New("passwords.min_age_hours must be shorter than the maximum age")
	}
	sl := p.Syslog
	if sl.Enabled {
		switch sl.Protocol {
		case "udp", "tcp", "tls":
		default:
			return errors.New("syslog.protocol must be udp, tcp or tls")
		}
		if !strings.Contains(sl.Address, ":") {
			return errors.New("syslog.address must be host:port")
		}
	}
	switch sl.Format {
	case "", "rfc5424", "cef":
	default:
		return errors.New("syslog.format must be rfc5424 or cef")
	}
	if sl.Facility < 0 || sl.Facility > 23 {
		return errors.New("syslog.facility must be 0 to 23")
	}
	if (sl.ClientCertFile == "") != (sl.ClientKeyFile == "") {
		return errors.New("syslog: give both client_cert_file and client_key_file, or neither")
	}
	sc := p.SmartCard
	switch sc.Source {
	case "direct", "proxy":
	default:
		return errors.New("smart_card.source must be direct or proxy")
	}
	switch sc.HeaderFormat {
	case "xfcc", "pem", "base64":
	default:
		return errors.New("smart_card.header_format must be xfcc, pem or base64")
	}
	if sc.Source == "proxy" && strings.TrimSpace(sc.Header) == "" {
		return errors.New("smart_card.header is required when the certificate comes from a proxy")
	}
	if len(sc.Match) == 0 {
		return errors.New("smart_card.match needs at least one of upn, email, edipi")
	}
	for _, m := range sc.Match {
		switch m {
		case "upn", "email", "edipi":
		default:
			return errors.New("smart_card.match entries must be upn, email or edipi")
		}
	}
	return nil
}

// normalize fills fields an older or partial document left empty.
func (p *SecurityPolicy) normalize() {
	d := DefaultSecurityPolicy()
	if p.Sessions.IdleMinutesUser == 0 {
		p.Sessions.IdleMinutesUser = d.Sessions.IdleMinutesUser
	}
	if p.Sessions.IdleMinutesAdmin == 0 {
		p.Sessions.IdleMinutesAdmin = d.Sessions.IdleMinutesAdmin
	}
	if p.Sessions.AbsoluteMinutes == 0 {
		p.Sessions.AbsoluteMinutes = d.Sessions.AbsoluteMinutes
	}
	if p.Banner.Button == "" {
		p.Banner.Button = d.Banner.Button
	}
	if p.Lockout.MaxFailures == 0 {
		p.Lockout = d.Lockout
	}
	if p.Lockout.WindowMinutes == 0 {
		p.Lockout.WindowMinutes = d.Lockout.WindowMinutes
	}
	if p.Passwords.MinLength == 0 {
		p.Passwords.MinLength = d.Passwords.MinLength
	}
	if p.Syslog.Protocol == "" {
		p.Syslog.Protocol = d.Syslog.Protocol
	}
	if p.Syslog.Format == "" {
		p.Syslog.Format = d.Syslog.Format
	}
	if p.Syslog.AppName == "" {
		p.Syslog.AppName = d.Syslog.AppName
	}
	if p.SmartCard.Source == "" {
		p.SmartCard.Source = d.SmartCard.Source
	}
	if p.SmartCard.Header == "" {
		p.SmartCard.Header = d.SmartCard.Header
	}
	if p.SmartCard.HeaderFormat == "" {
		p.SmartCard.HeaderFormat = d.SmartCard.HeaderFormat
	}
	if len(p.SmartCard.Match) == 0 {
		p.SmartCard.Match = d.SmartCard.Match
	}
}

// policyCacheTTL bounds how long another replica's change takes to apply.
const policyCacheTTL = 10 * time.Second

type policyCache struct {
	p  SecurityPolicy
	at time.Time
	ok bool
}

// The cache is per database (every Store derived from one *sql.DB shares
// it); the STIG floor is process-wide.
var (
	secPolicyMu sync.Mutex
	secPolicy   = map[*sql.DB]*policyCache{}
	stigFloor   bool
)

func cacheFor(db *sql.DB) *policyCache {
	c := secPolicy[db]
	if c == nil {
		c = &policyCache{}
		secPolicy[db] = c
	}
	return c
}

// SetSTIGBaseline makes every read raise the policy to the STIG values (the
// STIG profile): an older, weaker saved policy never applies.
func SetSTIGBaseline(on bool) {
	secPolicyMu.Lock()
	stigFloor = on
	for _, c := range secPolicy {
		c.ok = false
	}
	secPolicyMu.Unlock()
}

// STIGBaseline reports whether the STIG profile's floor is in force.
func STIGBaseline() bool {
	secPolicyMu.Lock()
	defer secPolicyMu.Unlock()
	return stigFloor
}

// SecurityPolicy returns the policy in force (cached for policyCacheTTL).
// A read failure keeps the last known policy, or the default.
func (s *Store) SecurityPolicy(ctx context.Context) SecurityPolicy {
	secPolicyMu.Lock()
	c := cacheFor(s.db)
	if c.ok && time.Since(c.at) < policyCacheTTL {
		p := c.p
		secPolicyMu.Unlock()
		return p
	}
	stig := stigFloor
	secPolicyMu.Unlock()
	p, err := s.StoredSecurityPolicy(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "read security policy; keeping the last known one", "error", err.Error())
		secPolicyMu.Lock()
		defer secPolicyMu.Unlock()
		if c.ok {
			return c.p
		}
		p = DefaultSecurityPolicy()
	}
	if stig {
		p = STIGSecurityPolicy(p)
	}
	secPolicyMu.Lock()
	c.p, c.at, c.ok = p, time.Now(), true
	secPolicyMu.Unlock()
	return p
}

// StoredSecurityPolicy is the saved policy (the default when none is saved),
// without the STIG floor.
func (s *Store) StoredSecurityPolicy(ctx context.Context) (SecurityPolicy, error) {
	raw, _, err := s.systemSetting(ctx, SettingSecurityPolicy)
	if errors.Is(err, ErrNotFound) {
		return DefaultSecurityPolicy(), nil
	}
	if err != nil {
		return SecurityPolicy{}, err
	}
	var p SecurityPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return SecurityPolicy{}, fmt.Errorf("decode security policy: %w", err)
	}
	p.normalize()
	return p, nil
}

// PutSecurityPolicy validates and saves p, and drops the cache.
func (s *Store) PutSecurityPolicy(ctx context.Context, p SecurityPolicy) error {
	p.normalize()
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := s.putSystemSetting(ctx, SettingSecurityPolicy, string(raw)); err != nil {
		return err
	}
	s.InvalidateSecurityPolicy()
	// Inside a request transaction the new value is visible to other reads
	// only after the commit: drop the cache again then.
	onCommit(ctx, s.InvalidateSecurityPolicy)
	return nil
}

// InvalidateSecurityPolicy makes this replica's next read go to the
// database (other replicas follow within policyCacheTTL).
func (s *Store) InvalidateSecurityPolicy() {
	secPolicyMu.Lock()
	cacheFor(s.db).ok = false
	secPolicyMu.Unlock()
}

// CheckPasswordPolicy applies p to a new password. old is the current
// password when the person typed it (self-service change), else "".
func CheckPasswordPolicy(p PasswordPolicy, pw, old string) error {
	n := len([]rune(pw))
	if n < p.MinLength || len(pw) > PasswordMaxLen {
		return fmt.Errorf("password must be %d–%d characters", p.MinLength, PasswordMaxLen)
	}
	var up, lo, di, sp bool
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			up = true
		case unicode.IsLower(r):
			lo = true
		case unicode.IsDigit(r):
			di = true
		case !unicode.IsSpace(r) && (unicode.IsPunct(r) || unicode.IsSymbol(r)):
			sp = true
		}
	}
	var missing []string
	if p.RequireUpper && !up {
		missing = append(missing, "an upper-case letter")
	}
	if p.RequireLower && !lo {
		missing = append(missing, "a lower-case letter")
	}
	if p.RequireDigit && !di {
		missing = append(missing, "a digit")
	}
	if p.RequireSpecial && !sp {
		missing = append(missing, "a special character")
	}
	if len(missing) > 0 {
		return fmt.Errorf("password must include %s", joinAnd(missing))
	}
	if old != "" && p.MinChangedChars > 0 && editDistance(old, pw) < p.MinChangedChars {
		return fmt.Errorf("the new password must change at least %d characters of the current one", p.MinChangedChars)
	}
	return nil
}

func joinAnd(v []string) string {
	switch len(v) {
	case 0:
		return ""
	case 1:
		return v[0]
	}
	return strings.Join(v[:len(v)-1], ", ") + " and " + v[len(v)-1]
}

// editDistance is the Levenshtein distance in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// UserByPKIIdentity finds the person whose smart-card identity (a UPN or a
// DoD ID number, set by an administrator) is id.
func (s *Store) UserByPKIIdentity(ctx context.Context, id string) (*User, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil, ErrNotFound
	}
	var userID string
	err := s.queryRow(ctx, `SELECT id FROM app_user WHERE pki_identity = ? AND deleted_at = '' LIMIT 1`, id).Scan(&userID)
	if err != nil {
		return nil, ErrNotFound
	}
	return s.UserByID(ctx, userID)
}

// PKIIdentity returns a person's smart-card identity ("" when none).
func (s *Store) PKIIdentity(ctx context.Context, userID string) (string, error) {
	var v string
	err := s.queryRow(ctx, `SELECT pki_identity FROM app_user WHERE id = ?`, userID).Scan(&v)
	return v, err
}

// SetPKIIdentity sets (or clears, with "") a person's smart-card identity.
// Two people cannot share one.
func (s *Store) SetPKIIdentity(ctx context.Context, userID, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if id != "" {
		if other, err := s.UserByPKIIdentity(ctx, id); err == nil && other.ID != userID {
			return fmt.Errorf("that card identity already belongs to %s", other.Email)
		}
	}
	return s.exec(ctx, `UPDATE app_user SET pki_identity = ? WHERE id = ?`, id, userID)
}
