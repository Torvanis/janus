package license

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// SyncEndpoint is deliberately not configurable, including through signed URLs.
const SyncEndpoint = "https://janusedge.com/api/v1/license/sync"
const syncTTL = 6 * time.Hour

// Subscription is advisory billing metadata, never an entitlement grant.
type Subscription struct {
	SchemaVersion     int        `json:"schema_version"`
	LicenseID         string     `json:"license_id"`
	BillingMode       string     `json:"billing_mode"`
	Status            string     `json:"status"`
	AutoRenew         bool       `json:"auto_renew"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	PaidThrough       *time.Time `json:"paid_through,omitempty"`
	VerifiedAt        time.Time  `json:"verified_at"`
	FreshUntil        time.Time  `json:"fresh_until"`
	Revision          int64      `json:"revision"`
}

// UnmarshalJSON rejects incomplete known-version metadata rather than treating
// omitted booleans or revision as positive billing evidence. Unknown versions
// remain conservative and may accompany a valid signed renewal.
func (s *Subscription) UnmarshalJSON(b []byte) error {
	type plain Subscription
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	if value.SchemaVersion == 1 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			return err
		}
		for _, key := range []string{"license_id", "billing_mode", "status", "auto_renew", "cancel_at_period_end", "verified_at", "fresh_until", "revision"} {
			v, ok := fields[key]
			if !ok || string(v) == "null" {
				return errors.New("incomplete subscription metadata")
			}
		}
	}
	*s = Subscription(value)
	return nil
}

// RenewalNotice is safe for non-administrators: it contains no billing identity.
type RenewalNotice struct {
	SuppressExpiring bool       `json:"suppress_expiring"`
	Reason           string     `json:"reason"`
	FreshUntil       *time.Time `json:"fresh_until,omitempty"`
}

// SyncState is the credential-free administrator projection.
type SyncState struct {
	Enabled           bool          `json:"enabled"`
	HasToken          bool          `json:"has_token"`
	ManagedByEnv      bool          `json:"enabled_managed"`
	TokenManagedByEnv bool          `json:"token_managed"`
	Configurable      bool          `json:"configurable"`
	Mode              string        `json:"mode"`
	Health            string        `json:"health"`
	LastAttemptAt     *time.Time    `json:"last_attempt_at,omitempty"`
	LastSuccessAt     *time.Time    `json:"last_success_at,omitempty"`
	FreshUntil        *time.Time    `json:"fresh_until,omitempty"`
	NextRetryAt       *time.Time    `json:"next_retry_at,omitempty"`
	ErrorCode         string        `json:"error_code,omitempty"`
	Subscription      *Subscription `json:"subscription,omitempty"`
}

type syncRecord struct {
	Generation        string        `json:"generation"`
	Enabled           bool          `json:"enabled"`
	Token             string        `json:"token,omitempty"` // AES-GCM envelope, not plaintext
	LeaseUntil        time.Time     `json:"lease_until"`
	LastAttempt       *time.Time    `json:"last_attempt,omitempty"`
	LastSuccess       *time.Time    `json:"last_success,omitempty"`
	Next              time.Time     `json:"next"`
	Error             string        `json:"error,omitempty"`
	Revoked           bool          `json:"revoked,omitempty"`
	RevocationBarrier time.Time     `json:"revocation_barrier,omitempty"`
	RecoveryAllowed   bool          `json:"recovery_allowed,omitempty"`
	Failures          int           `json:"failures"`
	Subscription      *Subscription `json:"subscription,omitempty"`
	Revision          int64         `json:"revision"`
	MetadataDigest    string        `json:"metadata_digest,omitempty"`
	MetadataVerified  time.Time     `json:"metadata_verified"`
	ObservedKey       string        `json:"observed_key,omitempty"`
	RuntimeBinding    string        `json:"runtime_binding,omitempty"`
}

// SyncStore supplies shared atomic state; no lease lives only in process memory.
type SyncStore interface {
	LicenseSyncSnapshot(context.Context) (string, string, error)
	CompareAndSwapLicenseSync(context.Context, string, string, string, *string) (bool, error)
}

// SecretCipher is the existing gateway encryption-at-rest interface.
type SecretCipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

// SyncOptions permits hermetic tests to inject a clock and transport, never an
// alternate endpoint or an insecure production trust switch. Enabled nil means
// database/UI authority; explicit false and true pin the runtime setting.
type SyncOptions struct {
	Now       func() time.Time
	Transport http.RoundTripper
	Offline   bool
	Enabled   *bool
	Token     string
	TokenSet  bool
}

// SyncEngine independently renews signed licenses; application updates are unrelated.
type SyncEngine struct {
	manager *Manager
	store   SyncStore
	cipher  SecretCipher
	options SyncOptions
	client  *http.Client
	wake    chan struct{}
}

// NewSync builds the opt-in consumer. Production callers use default TLS trust.
func NewSync(m *Manager, s SyncStore, c SecretCipher, o SyncOptions) *SyncEngine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Enabled != nil {
		v := *o.Enabled
		o.Enabled = &v
	}
	return &SyncEngine{manager: m, store: s, cipher: c, options: o, wake: make(chan struct{}, 1), client: &http.Client{Timeout: 15 * time.Second, Transport: o.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}
}

func fingerprint(s string) string        { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (e *SyncEngine) tokenManaged() bool { return e.options.TokenSet || e.options.Token != "" }
func (e *SyncEngine) binding() string {
	enabled := "database"
	if e.options.Enabled != nil {
		if *e.options.Enabled {
			enabled = "true"
		} else {
			enabled = "false"
		}
	}
	source := "database"
	if e.tokenManaged() {
		source = "environment"
	}
	return fingerprint(enabled + ":" + source + ":" + e.options.Token)
}
func (e *SyncEngine) enabled(r syncRecord) bool {
	if e.options.Enabled != nil {
		return *e.options.Enabled
	}
	return r.Enabled
}
func (e *SyncEngine) read(ctx context.Context) (syncRecord, string, string, error) {
	raw, key, err := e.store.LicenseSyncSnapshot(ctx)
	if err != nil {
		return syncRecord{}, "", "", err
	}
	var r syncRecord
	err = json.Unmarshal([]byte(raw), &r)
	return r, raw, key, err
}
func (e *SyncEngine) cas(ctx context.Context, raw, key string, r syncRecord, renewed *string) (bool, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	return e.store.CompareAndSwapLicenseSync(ctx, raw, key, string(b), renewed)
}

// Configure stores a UI configuration. Blank token preserves; explicit clear
// removes it. Environment-pinned values cannot be silently overridden by UI.
func (e *SyncEngine) Configure(ctx context.Context, enabled bool, token string, clear bool) error {
	return e.ConfigurePatch(ctx, &enabled, token, clear)
}

// ConfigurePatch preserves the current explicit enabled choice when omitted.
// Token-only provisioning never opts a deployment in.
func (e *SyncEngine) ConfigurePatch(ctx context.Context, requested *bool, token string, clear bool) error {
	token = strings.TrimSpace(token)
	if clear && token != "" {
		return errors.New("token and clear_token are mutually exclusive")
	}
	if len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return errors.New("invalid sync token")
	}
	if e.options.Enabled != nil && requested != nil && *requested != *e.options.Enabled {
		return errors.New("enabled is managed by JANUS_LICENSE_SYNC")
	}
	if e.tokenManaged() && (token != "" || clear) {
		return errors.New("token is managed by JANUS_LICENSE_SYNC_TOKEN")
	}
	if requested != nil && *requested && e.manager.file != "" {
		return errors.New("file-backed sync is unsupported; remove JANUS_LICENSE_FILE and deliberately install the key in the managed database first")
	}
	for attempt := 0; attempt < 5; attempt++ {
		r, raw, key, err := e.read(ctx)
		if err != nil {
			return errors.New("sync storage unavailable")
		}
		enabled := r.Enabled
		if requested != nil {
			enabled = *requested
		}
		envelope := r.Token
		if clear {
			envelope = ""
		}
		if token != "" {
			if strings.Contains(token, ".") {
				// Raw provisioning can precede installation. A copied full credential
				// must be bound to the installed verified license before storing it.
				licenseID := ""
				if strings.Contains(token, ".") {
					claims, verifyErr := Verify(key, e.manager.pubs)
					if verifyErr != nil {
						return errors.New("install a valid license before configuring a full credential")
					}
					licenseID = claims.LicenseID
				}
				var valid bool
				token, valid = normalizeSyncToken(token, licenseID)
				if !valid {
					return errors.New("invalid sync token or license identity")
				}
			}
			if e.cipher == nil {
				return errors.New("credential encryption unavailable")
			}
			envelope, err = e.cipher.Encrypt(token)
			if err != nil {
				return errors.New("credential encryption failed")
			}
		}
		if requested != nil && *requested && !clear && ((e.tokenManaged() && e.options.Token == "") || (!e.tokenManaged() && envelope == "")) {
			return errors.New("sync token required")
		}
		// Preserve anti-replay high water across toggles and credential rotation.
		next := syncRecord{Generation: fingerprint(raw + e.options.Now().String() + envelope), Enabled: enabled, Token: envelope, Revision: r.Revision, MetadataDigest: r.MetadataDigest, MetadataVerified: r.MetadataVerified, Revoked: r.Revoked, RevocationBarrier: r.RevocationBarrier, RecoveryAllowed: r.RecoveryAllowed}
		ok, err := e.cas(ctx, raw, key, next, nil)
		if err != nil {
			return errors.New("sync storage unavailable")
		}
		if ok {
			select {
			case e.wake <- struct{}{}:
			default:
			}
			return nil
		}
	}
	return errors.New("sync configuration changed concurrently; retry")
}

func (e *SyncEngine) mode(r syncRecord, c *Claims) string {
	if e.manager.file != "" {
		return "file"
	}
	if e.options.Offline || (c != nil && c.Offline) {
		return "offline"
	}
	if e.enabled(r) {
		return "online"
	}
	return "manual"
}

// View reads persisted state without making a network request. Failure is
// conservative; a stale observation never hides a warning after a restart.
func (e *SyncEngine) View(ctx context.Context) (SyncState, RenewalNotice) {
	st := SyncState{Mode: "manual", Health: "error", ErrorCode: "storage", ManagedByEnv: e.options.Enabled != nil, TokenManagedByEnv: e.tokenManaged(), Configurable: e.options.Enabled == nil}
	notice := RenewalNotice{Reason: "unknown"}
	r, _, key, err := e.read(ctx)
	if err != nil {
		return st, notice
	}
	local := e.manager.evaluate(key, "database")
	c := local.Claims
	st.Enabled = e.enabled(r)
	st.HasToken = r.Token != ""
	if e.tokenManaged() {
		st.HasToken = e.options.Token != ""
	}
	st.Mode = e.mode(r, c)
	st.LastAttemptAt = r.LastAttempt
	st.LastSuccessAt = r.LastSuccess
	st.ErrorCode = r.Error
	st.Subscription = r.Subscription
	if !r.Next.IsZero() {
		n := r.Next
		st.NextRetryAt = &n
	}
	st.Health = "never"
	notice.Reason = "never"
	if r.Revoked {
		st.Health, st.ErrorCode, notice.Reason = "error", "revoked", "revoked"
		return st, notice
	}
	if st.Mode != "online" {
		st.Health = "disabled"
		notice.Reason = st.Mode
		return st, notice
	}
	if r.RuntimeBinding != e.binding() || r.ObservedKey != fingerprint(key) {
		notice.Reason = "changed"
		return st, notice
	}
	if r.Error != "" {
		st.Health = "error"
		notice.Reason = r.Error
		return st, notice
	}
	now := e.options.Now()
	if r.LastSuccess == nil {
		return st, notice
	}
	deadline := r.LastSuccess.Add(syncTTL)
	if r.Subscription != nil && r.Subscription.FreshUntil.Before(deadline) {
		deadline = r.Subscription.FreshUntil
	}
	st.FreshUntil = &deadline
	if now.Before(*r.LastSuccess) || !now.Before(deadline) {
		st.Health = "stale"
		notice.Reason = "stale"
		return st, notice
	}
	st.Health = "healthy"
	notice.Reason = "unknown_subscription"
	if c == nil || (c.StatusAt(now) != StatusValid && c.StatusAt(now) != StatusExpiring) {
		notice.Reason = "license_status"
		return st, notice
	}
	if c.Term != TermSubscription || c.Issued.After(now.Add(5*time.Minute)) {
		return st, notice
	}
	sub := r.Subscription
	if !validMetadata(sub, *c, now) {
		return st, notice
	}
	if sub.Status != "active" || !sub.AutoRenew || sub.CancelAtPeriodEnd {
		notice.Reason = "subscription_attention"
		return st, notice
	}
	notice = RenewalNotice{SuppressExpiring: true, Reason: "auto_renew", FreshUntil: &deadline}
	return st, notice
}

func billingMode(c Claims) string {
	if c.BillingMode == "" {
		return "live"
	}
	return c.BillingMode
}
func validMetadata(s *Subscription, c Claims, now time.Time) bool {
	return s != nil && s.SchemaVersion == 1 && metadataConsistent(s, c, now) && s.PaidThrough != nil && now.Before(s.FreshUntil)
}

// Missing paid coverage and stale billing health cannot hide warnings, but do
// not revoke the independently signed entitlement delivered alongside them.
func metadataConsistent(s *Subscription, c Claims, now time.Time) bool {
	return s.Revision >= 0 && s.LicenseID == c.LicenseID && (s.BillingMode == "live" || s.BillingMode == "sandbox") && s.BillingMode == billingMode(c) && (s.PaidThrough == nil || (c.Exp != nil && s.PaidThrough.Equal(*c.Exp))) && !s.VerifiedAt.IsZero() && !s.VerifiedAt.After(now.Add(5*time.Minute)) && s.FreshUntil.Equal(s.VerifiedAt.Add(syncTTL))
}

// Sync claims a persisted lease and commits renewed key + observation together.
// force bypasses schedule, never a live lease. Errors are fixed codes only.
func (e *SyncEngine) Sync(ctx context.Context, force bool) error {
	r, raw, key, err := e.read(ctx)
	if err != nil {
		return errors.New("storage")
	}
	c, err := Verify(key, e.manager.pubs)
	if err != nil {
		return errors.New("license_invalid")
	}
	if c.Validate() != nil {
		return errors.New("license_invalid")
	}
	if e.mode(r, &c) != "online" {
		return errors.New("sync_disabled")
	}
	now := e.options.Now()
	if now.Before(r.LeaseUntil) || (!force && now.Before(r.Next)) {
		return nil
	}
	r.LeaseUntil = now.Add(time.Minute)
	r.LastAttempt = &now
	r.Error = "in_progress"
	ok, err := e.cas(ctx, raw, key, r, nil)
	if err != nil {
		return errors.New("storage")
	}
	if !ok {
		return nil
	}
	claimed, _ := json.Marshal(r)
	renewed, sub, code := e.fetch(ctx, key, c, &r)
	completed := e.options.Now()
	r.LeaseUntil = time.Time{}
	r.Error = code
	if code == "revoked" {
		r.Revoked = true
	}
	r.RuntimeBinding = e.binding()
	r.ObservedKey = fingerprint(key)
	if code == "" {
		r.Revoked = false
		r.LastSuccess = &completed
		r.Failures = 0
		r.Next = completed.Add(time.Hour)
		r.Subscription = sub
		if sub != nil && sub.SchemaVersion == 1 {
			r.Revision = sub.Revision
			b, _ := json.Marshal(sub)
			r.MetadataDigest = fingerprint(string(b))
			r.MetadataVerified = sub.VerifiedAt
		}
		r.ObservedKey = fingerprint(renewed)
	} else {
		r.Failures++
		delay := time.Minute
		for i := 1; i < r.Failures && delay < time.Hour; i++ {
			delay *= 2
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		r.Next = completed.Add(delay)
	}
	// A canceled HTTP request must still durably record failure, bounded separately.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var install *string
	if code == "" {
		install = &renewed
	}
	ok, err = e.cas(persist, string(claimed), key, r, install)
	if err != nil {
		return errors.New("storage")
	}
	if !ok {
		return errors.New("configuration_changed")
	}
	if code != "" {
		return errors.New(code)
	}
	if err = e.manager.Refresh(persist); err != nil {
		return errors.New("local_refresh")
	}
	return nil
}

func (e *SyncEngine) fetch(ctx context.Context, key string, c Claims, r *syncRecord) (string, *Subscription, string) {
	if c.SyncURL != "" && c.SyncURL != SyncEndpoint {
		return "", nil, "unsafe_endpoint"
	}
	token := e.options.Token
	if !e.tokenManaged() {
		if e.cipher == nil {
			return "", nil, "credential"
		}
		var err error
		token, err = e.cipher.Decrypt(r.Token)
		if err != nil {
			return "", nil, "credential"
		}
	}
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return "", nil, "credential"
	}
	var credentialOK bool
	token, credentialOK = normalizeSyncToken(token, c.LicenseID)
	if !credentialOK {
		return "", nil, "credential"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SyncEndpoint, strings.NewReader("{}"))
	if err != nil {
		return "", nil, "request"
	}
	req.Header.Set("Authorization", "Bearer "+c.LicenseID+"."+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return "", nil, "network"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 401, 403:
			return "", nil, "credentials_rejected"
		case 429:
			return "", nil, "rate_limited"
		default:
			return "", nil, "upstream"
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil || len(b) > 128*1024 {
		return "", nil, "response_size"
	}
	var result struct {
		License      string        `json:"license"`
		LicenseID    string        `json:"license_id"`
		Status       string        `json:"status"`
		RevokedAt    *time.Time    `json:"revoked_at"`
		Subscription *Subscription `json:"subscription"`
	}
	if json.Unmarshal(b, &result) != nil {
		return "", nil, "response_invalid"
	}
	if result.Status == "revoked" {
		if result.LicenseID != c.LicenseID {
			return "", nil, "identity_changed"
		}
		// A successfully repaired tombstone retains its barrier. Replayed old
		// revocation (or legacy metadata without ordering evidence) cannot
		// establish a new permanent tombstone on the replacement key.
		if !r.Revoked && !r.RevocationBarrier.IsZero() && (result.RevokedAt == nil || !result.RevokedAt.After(r.RevocationBarrier)) {
			return "", nil, "revision_regression"
		}
		// The local receipt barrier prevents replay of any pre-observation key.
		// Legacy/malformed freshness cannot authorize automatic recovery.
		if !r.Revoked {
			r.RevocationBarrier = e.options.Now()
			if c.Issued.After(r.RevocationBarrier) {
				r.RevocationBarrier = c.Issued
			}
			r.RecoveryAllowed = result.RevokedAt != nil && !result.RevokedAt.IsZero() && !result.RevokedAt.After(e.options.Now().Add(5*time.Minute))
		}
		if result.RevokedAt == nil || result.RevokedAt.IsZero() || result.RevokedAt.After(e.options.Now().Add(5*time.Minute)) {
			r.RecoveryAllowed = false
		} else if result.RevokedAt.After(r.RevocationBarrier) {
			r.RevocationBarrier = *result.RevokedAt
		}
		// A known authenticated revocation generation participates in replay
		// fencing too; do not compare repair only to the last active response.
		if sub := result.Subscription; sub != nil && sub.SchemaVersion == 1 {
			encoded, _ := json.Marshal(sub)
			if !metadataConsistent(sub, c, e.options.Now()) || sub.Revision < r.Revision || sub.VerifiedAt.Before(r.MetadataVerified) || (sub.Revision == r.Revision && r.MetadataDigest != "" && fingerprint(string(encoded)) != r.MetadataDigest) {
				r.RecoveryAllowed = false
			} else {
				r.Revision, r.MetadataVerified, r.MetadataDigest = sub.Revision, sub.VerifiedAt, fingerprint(string(encoded))
			}
		}
		return "", nil, "revoked"
	}
	switch result.Status {
	case "active", "grace", "expired":
	default:
		return "", nil, "response_invalid"
	}
	next, err := Verify(result.License, e.manager.pubs)
	if err != nil || next.Validate() != nil {
		return "", nil, "signature_invalid"
	}
	if next.LicenseID != c.LicenseID || result.LicenseID != c.LicenseID || next.Edition != c.Edition || next.Org != c.Org || next.Term != c.Term || next.Offline != c.Offline || billingMode(next) != billingMode(c) || (billingMode(next) != "live" && billingMode(next) != "sandbox") {
		return "", nil, "identity_changed"
	}
	// KeyID is not identity: trusted production signer rotation remains supported.
	if next.Issued.Before(c.Issued) || next.Issued.After(e.options.Now().Add(5*time.Minute)) || (c.Exp != nil && (next.Exp == nil || next.Exp.Before(*c.Exp))) || next.GraceDays < c.GraceDays {
		return "", nil, "key_regression"
	}
	if next.SyncURL != "" && next.SyncURL != SyncEndpoint {
		return "", nil, "unsafe_endpoint"
	}
	sub := result.Subscription
	if sub != nil && sub.SchemaVersion == 1 {
		if sub.Revision < 0 || sub.Revision < r.Revision || sub.VerifiedAt.Before(r.MetadataVerified) {
			return "", nil, "revision_regression"
		}
		encoded, _ := json.Marshal(sub)
		if sub.Revision == r.Revision && r.MetadataDigest != "" && fingerprint(string(encoded)) != r.MetadataDigest {
			return "", nil, "revision_regression"
		}
		// Unknown/new schemas can be installed, but never suppress; malformed known
		// metadata cannot renew local observation freshness or overwrite prior state.
		if sub.SchemaVersion == 1 && !metadataConsistent(sub, next, e.options.Now()) {
			return "", nil, "metadata_invalid"
		}
	}
	if r.Revoked && (!r.RecoveryAllowed || r.RevocationBarrier.IsZero() || !next.Issued.After(r.RevocationBarrier) || !validMetadata(sub, next, e.options.Now()) || sub.Revision <= r.Revision || !sub.VerifiedAt.After(r.MetadataVerified) || !sub.VerifiedAt.After(r.RevocationBarrier) || sub.Status != "active" || !sub.AutoRenew || sub.CancelAtPeriodEnd || (next.StatusAt(e.options.Now()) != StatusValid && next.StatusAt(e.options.Now()) != StatusExpiring)) {
		return "", nil, "revoked"
	}
	return result.License, sub, ""
}

// normalizeSyncToken accepts the portal's raw token or an exact same-license
// credential. Never accept arbitrary Authorization headers or foreign identities.
func normalizeSyncToken(token, licenseID string) (string, bool) {
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n\t ") {
		return "", false
	}
	if prefix, suffix, full := strings.Cut(token, "."); full {
		if prefix != licenseID || suffix == "" || strings.Contains(suffix, ".") {
			return "", false
		}
		token = suffix
	}
	return token, true
}

// Run polls immediately at startup, on UI changes, and for persisted retries.
func (e *SyncEngine) Run(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = e.Sync(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-e.wake:
		}
	}
}
