package license

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Trusted signing keys, keyed by key_id. Two slots so a key rotation ships in a
// release BEFORE the signer starts issuing with the new id; never remove the
// previous key in the same release that adds a new one.
//
// Public verification keys only. Private signing material is never included
// in the gateway. Nothing here can mint a license.
var trustedKeys = map[string]string{
	"2026-09": "uNnWQesfzK9aCMo7y5kQ+tftj5DETajlLB4cICCwCxY=",
}

// TrustedKeys decodes the embedded public keys. Tests may pass their own map
// to NewManager; production code always uses this.
func TrustedKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(trustedKeys))
	for id, b64 := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("license: malformed embedded public key " + id)
		}
		out[id] = ed25519.PublicKey(raw)
	}
	return out
}

// Community limits applied when no key is installed. They match a Community
// key exactly so "no key" and "community key" are one code path.
const (
	CommunitySeats = 25
	CommunityNodes = 1
	// SkewTolerance is how far the local clock may be behind `issued` before
	// the key is treated as not-yet-valid. A bad clock must never block traffic,
	// so the only effect of skew beyond this is a warning in the panel.
	SkewTolerance = 24 * time.Hour
	recheckEvery  = time.Hour
)

// State is what the rest of the gateway consumes. It is a snapshot: cheap to
// copy, safe to hand to templates and JSON.
type State struct {
	// Installed is false when no key was found anywhere.
	Installed bool `json:"installed"`
	// Source is "file", "database" or "" (none).
	Source string `json:"source,omitempty"`
	// Edition is community/business/enterprise; community when no key.
	Edition string `json:"edition"`
	// Status is valid/expiring/grace/expired/invalid. A missing key reports
	// "valid" with edition community — Community is a real edition, not an error.
	Status Status `json:"status"`
	// Error explains an invalid key (bad signature, unknown key_id, tampered).
	Error string `json:"error,omitempty"`
	// Claims are the verified claims; nil when not installed or invalid.
	Claims *Claims `json:"claims,omitempty"`
	// Seats/Nodes are the effective limits (0 = unlimited).
	Seats int `json:"seats"`
	Nodes int `json:"nodes"`
	// Features are the effective feature flags (Community has none).
	Features []string `json:"features"`
	// ClockSkew is set when the key's issued time is ahead of the local clock
	// by more than SkewTolerance. Informational only.
	ClockSkew bool `json:"clock_skew,omitempty"`
	// CheckedAt is when this snapshot was computed.
	CheckedAt time.Time `json:"checked_at"`
	// ExpiresAt / GraceUntil are surfaced for banners (nil for perpetual/none).
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	GraceUntil *time.Time `json:"grace_until,omitempty"`
}

// Restricted reports whether CREATE operations are blocked. Everything already
// configured keeps working regardless (RULING: never disrupt work).
func (s State) Restricted() bool {
	return s.Status == StatusExpired || s.Status == StatusInvalid
}

// Has reports whether a gated feature is licensed.
func (s State) Has(feature string) bool {
	for _, f := range s.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// StatusInvalid is a gateway-side status for keys that fail verification.
// The shared contract only derives valid/expiring/grace/expired from claims.
const StatusInvalid Status = "invalid"

// Store is the persistence the manager needs. The gateway's store satisfies it
// with two system_setting keys; tests use an in-memory map.
type Store interface {
	LicenseKey(ctx context.Context) (string, error) // "" when none
	PutLicenseKey(ctx context.Context, key string) error
	DeleteLicenseKey(ctx context.Context) error
}

// Manager loads the key, verifies it, caches the state and re-checks hourly.
type Manager struct {
	pubs  map[string]ed25519.PublicKey
	file  string
	store Store
	log   *slog.Logger
	now   func() time.Time

	mu    sync.RWMutex
	state State
}

// NewManager builds a manager. file may be "" (database only). pubs nil means
// the embedded keys.
func NewManager(file string, store Store, pubs map[string]ed25519.PublicKey, log *slog.Logger) *Manager {
	if pubs == nil {
		pubs = TrustedKeys()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{pubs: pubs, file: file, store: store, log: log, now: time.Now}
}

// State returns the cached snapshot. Safe for concurrent use.
func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Refresh reloads the key from file (preferred) or the database and recomputes
// the state. It never returns an error for a bad key — that is a State, not a
// failure — only for I/O problems.
func (m *Manager) Refresh(ctx context.Context) error {
	raw, source, err := m.load(ctx)
	if err != nil {
		return err
	}
	st := m.evaluate(raw, source)
	m.mu.Lock()
	prev := m.state
	m.state = st
	m.mu.Unlock()
	if prev.Status != st.Status || prev.Edition != st.Edition || prev.Installed != st.Installed {
		m.log.Info("license state", "edition", st.Edition, "status", st.Status, "source", st.Source, "seats", st.Seats, "nodes", st.Nodes, "error", st.Error)
	}
	return nil
}

// Run refreshes hourly until ctx is done. Call after an initial Refresh.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(recheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Refresh(ctx); err != nil {
				m.log.Warn("license refresh", "error", err.Error())
			}
		}
	}
}

// Install verifies a pasted key and, if it parses and verifies, stores it in
// the database and refreshes. The file, if configured and present, still wins
// on the next refresh — Install reports that so the UI can say so.
func (m *Manager) Install(ctx context.Context, raw string) (State, error) {
	raw = strings.TrimSpace(raw)
	if _, err := Verify(raw, m.pubs); err != nil {
		return m.State(), err
	}
	if err := m.store.PutLicenseKey(ctx, raw); err != nil {
		return m.State(), err
	}
	if err := m.Refresh(ctx); err != nil {
		return m.State(), err
	}
	return m.State(), nil
}

// Remove deletes the database copy and refreshes.
func (m *Manager) Remove(ctx context.Context) (State, error) {
	if err := m.store.DeleteLicenseKey(ctx); err != nil {
		return m.State(), err
	}
	if err := m.Refresh(ctx); err != nil {
		return m.State(), err
	}
	return m.State(), nil
}

func (m *Manager) load(ctx context.Context) (raw, source string, err error) {
	if m.file != "" {
		b, ferr := os.ReadFile(m.file)
		switch {
		case ferr == nil && strings.TrimSpace(string(b)) != "":
			return strings.TrimSpace(string(b)), "file", nil
		case ferr != nil && !errors.Is(ferr, os.ErrNotExist):
			return "", "", fmt.Errorf("read license file %s: %w", m.file, ferr)
		}
	}
	if m.store != nil {
		k, serr := m.store.LicenseKey(ctx)
		if serr != nil {
			return "", "", serr
		}
		if strings.TrimSpace(k) != "" {
			return strings.TrimSpace(k), "database", nil
		}
	}
	return "", "", nil
}

func (m *Manager) evaluate(raw, source string) State {
	now := m.now()
	st := State{CheckedAt: now, Edition: EditionCommunity, Status: StatusValid, Seats: CommunitySeats, Nodes: CommunityNodes, Features: []string{}}
	if raw == "" {
		return st
	}
	st.Installed = true
	st.Source = source
	c, err := Verify(raw, m.pubs)
	if err != nil {
		// An invalid key is worse than no key only in that we tell the admin.
		// Limits stay at Community; creation is blocked so a tampered key can't
		// be used to sneak past the seat limit while looking "installed".
		st.Status = StatusInvalid
		st.Error = err.Error()
		return st
	}
	st.Claims = &c
	st.Edition = c.Edition
	st.Seats = c.Seats
	st.Nodes = c.Nodes
	st.Features = append([]string{}, c.Features...)
	if st.Features == nil {
		st.Features = []string{}
	}
	if c.Edition == EditionCommunity {
		// A Community key documents who the user is but never lifts the limits.
		st.Seats = CommunitySeats
		st.Nodes = CommunityNodes
		st.Features = []string{}
	}
	if c.Issued.After(now.Add(SkewTolerance)) {
		st.ClockSkew = true
	}
	st.Status = c.StatusAt(now)
	if c.Exp != nil {
		e := *c.Exp
		st.ExpiresAt = &e
		g := e.Add(time.Duration(c.GraceDays) * 24 * time.Hour)
		st.GraceUntil = &g
	}
	return st
}

// CheckMaintenance enforces the perpetual-key rule: a build published after
// maintenance_until must refuse to run BEFORE migrations so the previous
// version restarts cleanly. buildDate is the release date baked into the
// binary (YYYY-MM-DD); "" (dev builds) never refuses. Returns nil when fine.
func CheckMaintenance(c *Claims, buildDate string) error {
	if c == nil || c.Term != TermPerpetual || c.MaintenanceUntil == "" || buildDate == "" {
		return nil
	}
	mu, err1 := time.Parse("2006-01-02", c.MaintenanceUntil)
	bd, err2 := time.Parse("2006-01-02", buildDate)
	if err1 != nil || err2 != nil {
		return nil
	}
	if bd.After(mu) {
		return fmt.Errorf("license %s (perpetual) covers releases up to %s; this build is dated %s. Run a release from on or before %s, or renew maintenance at https://janusedge.com/portal",
			c.LicenseID, c.MaintenanceUntil, buildDate, c.MaintenanceUntil)
	}
	return nil
}
