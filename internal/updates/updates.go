// Package updates is the opt-in version check against janusedge.com.
//
// It is OFF by default (JANUS_UPDATE_CHECK=true enables it) and hard-off
// under JANUS_OFFLINE=true. When on, once a day it sends exactly three
// things — the running version, the edition, and this gateway's instance id —
// and receives the latest release. It never sends users, models, usage or
// hostnames, and it never changes anything: the result is shown under
// Admin → System and nowhere else.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultEndpoint is the product API on janusedge.com.
const DefaultEndpoint = "https://janusedge.com/api/v1/updates/check"

// Interval between checks. Daily is plenty: releases are monthly-ish and the
// point is to surface an advisory, not to poll.
const Interval = 24 * time.Hour

// Result is what the last check returned, shaped for the admin UI.
type Result struct {
	// Enabled reflects config: false when opt-out or offline. The UI shows
	// "not enabled" rather than a stale/empty result.
	Enabled bool `json:"enabled"`
	// Offline is true when JANUS_OFFLINE forced the check off.
	Offline bool `json:"offline"`
	// CheckedAt is zero until the first successful or failed attempt.
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	Error           string     `json:"error,omitempty"`
	Current         string     `json:"current"`
	Latest          string     `json:"latest,omitempty"`
	Published       string     `json:"published,omitempty"`
	NotesURL        string     `json:"notes_url,omitempty"`
	Advisory        string     `json:"advisory,omitempty"`
	MinSupported    string     `json:"min_supported,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	// Unsupported: the running version is below min_supported — the vendor
	// no longer ships fixes for it. Advisory only; nothing is blocked.
	Unsupported bool `json:"unsupported"`
}

// Checker runs the periodic check and caches the last Result.
type Checker struct {
	endpoint   string
	version    string
	edition    func() string
	instanceID string
	enabled    bool
	offline    bool
	client     *http.Client
	logger     *slog.Logger

	mu   sync.RWMutex
	last Result
}

// Options configure a Checker. Edition is a func because the license can
// change at runtime (a key installed from the UI).
type Options struct {
	Endpoint   string
	Version    string
	Edition    func() string
	InstanceID string
	Enabled    bool
	Offline    bool
	Logger     *slog.Logger
}

// New builds a Checker. It does not start anything.
func New(o Options) *Checker {
	if o.Endpoint == "" {
		o.Endpoint = DefaultEndpoint
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Edition == nil {
		o.Edition = func() string { return "" }
	}
	c := &Checker{
		endpoint:   o.Endpoint,
		version:    o.Version,
		edition:    o.Edition,
		instanceID: o.InstanceID,
		enabled:    o.Enabled && !o.Offline,
		offline:    o.Offline,
		client:     &http.Client{Timeout: 15 * time.Second},
		logger:     o.Logger,
	}
	c.last = Result{Enabled: c.enabled, Offline: c.offline, Current: c.version}
	return c
}

// Last returns the cached result (never blocks on the network).
func (c *Checker) Last() Result {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Enabled reports whether checks run at all.
func (c *Checker) Enabled() bool { return c.enabled }

// Run performs a check now and then every Interval until ctx ends. It is a
// no-op when the check is disabled, so callers can start it unconditionally.
func (c *Checker) Run(ctx context.Context) {
	if !c.enabled {
		return
	}
	c.Check(ctx)
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Check(ctx)
		}
	}
}

// Check performs one check and records the outcome. Failures are recorded
// and logged at debug: an unreachable vendor is normal on many networks and
// must never be noisy or affect the gateway.
func (c *Checker) Check(ctx context.Context) Result {
	if !c.enabled {
		return c.Last()
	}
	now := time.Now().UTC()
	res := Result{Enabled: true, Current: c.version, CheckedAt: &now}
	if err := c.fetch(ctx, &res); err != nil {
		res.Error = err.Error()
		c.logger.Debug("update check failed", "error", err)
	} else if res.UpdateAvailable {
		c.logger.Info("update available", "current", c.version, "latest", res.Latest, "notes", res.NotesURL)
	}
	c.mu.Lock()
	c.last = res
	c.mu.Unlock()
	return res
}

func (c *Checker) fetch(ctx context.Context, res *Result) error {
	q := url.Values{}
	q.Set("version", c.version)
	if ed := c.edition(); ed != "" {
		q.Set("edition", ed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "janus-edge/"+c.version)
	req.Header.Set("Accept", "application/json")
	if c.instanceID != "" {
		req.Header.Set("X-Janus-Instance", c.instanceID)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update check: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Latest          *string `json:"latest"`
		Published       string  `json:"published"`
		NotesURL        string  `json:"notes_url"`
		Advisory        string  `json:"advisory"`
		MinSupported    string  `json:"min_supported"`
		UpdateAvailable bool    `json:"update_available"`
		Unsupported     bool    `json:"unsupported"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&body); err != nil {
		return fmt.Errorf("update check: bad response: %w", err)
	}
	if body.Latest != nil {
		res.Latest = *body.Latest
	}
	res.Published = body.Published
	res.NotesURL = body.NotesURL
	res.Advisory = body.Advisory
	res.MinSupported = body.MinSupported
	res.UpdateAvailable = body.UpdateAvailable
	res.Unsupported = body.Unsupported
	return nil
}
