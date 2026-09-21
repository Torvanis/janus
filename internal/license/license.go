// Package license defines the Janus license file format shared between
// the license issuer, signing service, and Janus gateway verifier.
//
// Wire format:  JANUS-LICENSE-1.<base64url(payload JSON)>.<base64url(ed25519 sig)>
// The signature covers the raw payload bytes. Verifiers select the public key
// by the payload's key_id, so keys can be rotated with two keys embedded.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const Prefix = "JANUS-LICENSE-1"

// Editions.
const (
	EditionCommunity  = "community"
	EditionBusiness   = "business"
	EditionEnterprise = "enterprise"
)

// Terms.
const (
	TermSubscription = "subscription"
	TermPerpetual    = "perpetual"
	TermTrial        = "trial"
)

// Feature flags a key may carry. The gateway treats an unknown feature as
// harmless so this list can grow without breaking old binaries.
var BusinessFeatures = []string{
	"scim", "ldap", "multi_oidc", "ha", "guardrails_enforce",
	"reports_scheduled", "captures", "audit_export", "model_fallbacks", "email_alerts",
}

var EnterpriseFeatures = append(append([]string{}, BusinessFeatures...),
	"airgap", "white_label", "sla",
)

// Claims is the signed payload.
type Claims struct {
	KeyID            string     `json:"key_id"`
	BillingMode      string     `json:"billing_mode,omitempty"` // absent means legacy live; sandbox uses isolated signer trust
	LicenseID        string     `json:"license_id"`
	Org              string     `json:"org"`
	IssuedTo         string     `json:"issued_to"`
	Edition          string     `json:"edition"`
	Seats            int        `json:"seats"` // 0 = unlimited
	Nodes            int        `json:"nodes"` // 0 = unlimited
	Site             string     `json:"site,omitempty"`
	Features         []string   `json:"features"`
	Issued           time.Time  `json:"issued"`
	Exp              *time.Time `json:"exp,omitempty"` // nil for perpetual
	GraceDays        int        `json:"grace_days"`
	Term             string     `json:"term"`
	MaintenanceUntil string     `json:"maintenance_until,omitempty"` // YYYY-MM-DD, perpetual only
	Offline          bool       `json:"offline,omitempty"`
	SyncURL          string     `json:"sync_url,omitempty"`
	Notes            string     `json:"notes,omitempty"`
}

// Validate checks structural sanity of claims before signing.
func (c *Claims) Validate() error {
	if c.BillingMode != "" && c.BillingMode != "live" && c.BillingMode != "sandbox" {
		return errors.New("license: bad billing_mode")
	}
	switch c.Edition {
	case EditionCommunity, EditionBusiness, EditionEnterprise:
	default:
		return fmt.Errorf("license: bad edition %q", c.Edition)
	}
	switch c.Term {
	case TermSubscription, TermTrial:
		if c.Exp == nil {
			return errors.New("license: subscription/trial requires exp")
		}
	case TermPerpetual:
		if c.MaintenanceUntil == "" {
			return errors.New("license: perpetual requires maintenance_until")
		}
		if _, err := time.Parse("2006-01-02", c.MaintenanceUntil); err != nil {
			return fmt.Errorf("license: maintenance_until: %w", err)
		}
	default:
		return fmt.Errorf("license: bad term %q", c.Term)
	}
	if c.KeyID == "" || c.LicenseID == "" || c.Org == "" {
		return errors.New("license: key_id, license_id and org are required")
	}
	if c.Seats < 0 || c.Nodes < 0 || c.GraceDays < 0 {
		return errors.New("license: negative limits")
	}
	if c.Features == nil {
		c.Features = []string{}
	}
	return nil
}

// Sign produces the license file string.
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	enc := base64.RawURLEncoding
	return Prefix + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

// ErrBadSignature is returned when the signature does not verify.
var ErrBadSignature = errors.New("license: bad signature")

// ErrUnknownKey is returned when no public key matches key_id.
var ErrUnknownKey = errors.New("license: unknown key_id")

// Parse decodes without verifying. Use Verify in production paths.
func Parse(s string) (Claims, []byte, []byte, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return Claims{}, nil, nil, errors.New("license: malformed")
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, nil, nil, fmt.Errorf("license: payload: %w", err)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return Claims{}, nil, nil, fmt.Errorf("license: signature: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, nil, nil, fmt.Errorf("license: claims: %w", err)
	}
	return c, payload, sig, nil
}

// Verify parses and checks the signature against the key selected by key_id.
func Verify(s string, pubs map[string]ed25519.PublicKey) (Claims, error) {
	c, payload, sig, err := Parse(s)
	if err != nil {
		return Claims{}, err
	}
	pub, ok := pubs[c.KeyID]
	if !ok {
		return Claims{}, ErrUnknownKey
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Claims{}, ErrBadSignature
	}
	return c, nil
}

// Status is the runtime state a verifier derives from claims and the clock.
type Status string

const (
	StatusValid    Status = "valid"
	StatusExpiring Status = "expiring" // within 30 days of exp
	StatusGrace    Status = "grace"    // past exp, within grace_days
	StatusExpired  Status = "expired"
)

// StatusAt derives the status at time now.
func (c *Claims) StatusAt(now time.Time) Status {
	if c.Term == TermPerpetual || c.Exp == nil {
		return StatusValid
	}
	exp := *c.Exp
	switch {
	case now.Before(exp.Add(-30 * 24 * time.Hour)):
		return StatusValid
	case now.Before(exp):
		return StatusExpiring
	case now.Before(exp.Add(time.Duration(c.GraceDays) * 24 * time.Hour)):
		return StatusGrace
	default:
		return StatusExpired
	}
}
