// Package subscription is the extension point for personal provider
// subscriptions: a user signs in to their OWN plan at a vendor (SuperGrok,
// …) and Janus routes that user's traffic through it.
//
// A vendor is added by implementing Provider in its own file and calling
// Register from init. Nothing in the proxy, the store, or the SPA needs to
// change: the proxy asks the registry for the provider named in the model
// prefix (my/<provider>/<model>), the store keeps opaque encrypted tokens,
// and the SPA renders whatever GET /api/v1/me/subscriptions lists.
//
// Every provider uses the OAuth 2.0 device authorization grant (RFC 8628).
// It is the only flow that works for a server-side gateway without the vendor
// registering a Janus-specific redirect URI: the user opens the vendor's own
// sign-in page, approves, and Janus polls for the result. Janus never sees
// the user's vendor password.
//
// Invariant: a connection belongs to exactly one Janus user and is only ever
// used for that user's own requests. There is no sharing, pooling, or
// fallback to another user's connection anywhere in this package or its
// callers.
package subscription

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelPrefix introduces a personal-subscription model name on the proxy
// surface: my/<provider>/<native model>. The prefix keeps personal models
// from ever colliding with (or silently shadowing) an organization upstream
// that serves the same native name.
const ModelPrefix = "my/"

// DeviceAuthorization is the vendor's answer to a device-code request.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration
}

// Tokens is a credential set returned by the vendor's token endpoint.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scope        string
}

// Account identifies the vendor account the tokens belong to.
type Account struct {
	Subject string
	Email   string
	Name    string
	// Meta is small per-connection routing state the provider needs later
	// (e.g. the account's own API host). Stored with the connection and
	// handed back through ConnectionBaseURL; never secrets.
	Meta map[string]string
}

// PlanUsage is the vendor's own view of how much of the plan is used, when
// the vendor publishes one. Janus shows it next to its own request counts.
type PlanUsage struct {
	Plan    string        `json:"plan,omitempty"`
	Windows []UsageWindow `json:"windows"`
	// Notes are short extra facts ("Credits balance: $4.20").
	Notes []string `json:"notes,omitempty"`
}

// UsageWindow is one limit: either a percentage (UsedPercent) or a count
// (Used of Limit). Unlimited windows carry no numbers.
type UsageWindow struct {
	Label       string    `json:"label"`
	UsedPercent float64   `json:"used_percent"`
	Used        *float64  `json:"used,omitempty"`
	Limit       *float64  `json:"limit,omitempty"`
	Unlimited   bool      `json:"unlimited,omitempty"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// UsageReporter is implemented by providers that publish plan usage.
type UsageReporter interface {
	Usage(ctx context.Context, client *http.Client, accessToken string, meta map[string]string) (*PlanUsage, error)
}

// KeyAuth is implemented by providers that connect with a key the user
// creates at the vendor and pastes into Janus, instead of a device sign-in.
// Their device methods are never called.
type KeyAuth interface {
	// KeyHelp tells the user where to create the key.
	KeyHelp() (instructions, url string)
	// VerifyKey checks the key against the vendor and reads the account.
	VerifyKey(ctx context.Context, client *http.Client, key string) (*Account, error)
}

// ConnectionRouter is implemented by providers whose inference host depends
// on the account (GitHub Copilot routes individual, business and enterprise
// accounts to different hosts).
type ConnectionRouter interface {
	ConnectionBaseURL(meta map[string]string) string
}

// ErrNotSupported is returned by device methods of key-based providers.
var ErrNotSupported = errors.New("not supported by this provider")

// Poll outcomes that are not failures. The caller keeps polling on
// ErrAuthorizationPending and backs off on ErrSlowDown.
var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("slow down")
	ErrExpired              = errors.New("device code expired")
	ErrDenied               = errors.New("access denied")
)

// ReauthError means the stored credential can no longer be used and the
// user must connect again (refresh token revoked, account downgraded, …).
type ReauthError struct{ Reason string }

func (e *ReauthError) Error() string { return e.Reason }

// Provider is one vendor whose consumer subscription can be connected.
type Provider interface {
	// ID is the stable identifier stored on connection rows and used in
	// the model prefix (my/<ID>/…). Lowercase, no slashes.
	ID() string
	// DisplayName is shown to users ("xAI (SuperGrok / X Premium+)").
	DisplayName() string
	// Description is one line of help text for the connect card.
	Description() string
	// AdminNote is shown to administrators beside the provider's switch:
	// what the vendor's terms say about third-party use, so the switch can
	// be turned off if they change.
	AdminNote() string
	// AdapterType names the internal/adapter implementation that speaks
	// the vendor's inference API with a bearer token.
	AdapterType() string
	// InferenceBaseURL is the base URL the adapter is pointed at.
	InferenceBaseURL() string

	StartDeviceAuthorization(ctx context.Context, client *http.Client) (*DeviceAuthorization, error)
	// PollDeviceAuthorization exchanges a device code for tokens. It
	// returns ErrAuthorizationPending / ErrSlowDown / ErrExpired /
	// ErrDenied for the RFC 8628 non-success outcomes.
	PollDeviceAuthorization(ctx context.Context, client *http.Client, deviceCode string) (*Tokens, error)
	// Refresh trades a refresh token for a fresh access token. Vendors
	// that rotate refresh tokens return the new one; otherwise the
	// original is echoed back. A *ReauthError means reconnect.
	Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Tokens, error)
	// Account reads who the tokens belong to.
	Account(ctx context.Context, client *http.Client, accessToken string) (*Account, error)
	// Models lists the native model names the subscription can call.
	Models(ctx context.Context, client *http.Client, accessToken string) ([]string, error)
	// Revoke best-effort revokes a refresh token at the vendor when the
	// user disconnects. Failure must not block the local disconnect.
	Revoke(ctx context.Context, client *http.Client, refreshToken string) error
}

var (
	mu        sync.RWMutex
	providers = map[string]Provider{}
)

// Register adds a provider. Called from each provider file's init; a
// duplicate ID is a programming error.
func Register(p Provider) {
	mu.Lock()
	defer mu.Unlock()
	id := p.ID()
	if id == "" || strings.ContainsAny(id, "/ ") || id != strings.ToLower(id) {
		panic(fmt.Sprintf("subscription: invalid provider id %q", id))
	}
	if _, dup := providers[id]; dup {
		panic(fmt.Sprintf("subscription: provider %q registered twice", id))
	}
	providers[id] = p
}

// Get returns the provider with the given ID.
func Get(id string) (Provider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := providers[id]
	return p, ok
}

// All returns every registered provider ordered by ID.
func All() []Provider {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// IsPersonalModel reports whether a requested model name addresses a
// personal subscription.
func IsPersonalModel(name string) bool { return strings.HasPrefix(name, ModelPrefix) }

// ParseModel splits my/<provider>/<model>. ok is false for anything else,
// including an empty provider or model segment.
func ParseModel(name string) (providerID, model string, ok bool) {
	rest, found := strings.CutPrefix(name, ModelPrefix)
	if !found {
		return "", "", false
	}
	providerID, model, found = strings.Cut(rest, "/")
	if !found || providerID == "" || model == "" {
		return "", "", false
	}
	return providerID, model, true
}

// ModelName builds the caller-facing name for a native model.
func ModelName(providerID, model string) string {
	return ModelPrefix + providerID + "/" + model
}
