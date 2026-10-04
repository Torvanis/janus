package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// Copilot connects a GitHub Copilot plan (Free, Pro, Pro+, Business,
// Enterprise). GitHub supports third-party tools using a Copilot
// subscription through a normal GitHub OAuth app; Janus ships its own
// ("Janus", registered by the Janus vendor) so no administrator has to
// register anything. JANUS_GITHUB_OAUTH_CLIENT_ID lets an organisation that
// insists on its own OAuth app use that instead.
//
// Sign-in is GitHub's device flow. The OAuth app does not expire user
// tokens, so there is no refresh token: the token is valid until the user
// revokes it on GitHub or leaves it unused for a year, and the background
// check notices either.
//
// Copilot's model endpoint speaks Chat Completions natively. Each account
// is served from its own host (api.individual / api.business /
// api.enterprise.githubcopilot.com), read from copilot_internal/user at
// connect time.
const (
	githubDefaultClientID = "Ov23liBCkw78ysVOJPdC"
	githubDeviceURL       = "https://github.com/login/device/code"
	githubTokenURL        = "https://github.com/login/oauth/access_token"
	githubAPIURL          = "https://api.github.com"
	copilotDefaultAPI     = "https://api.individual.githubcopilot.com"
	// read:user is enough for Copilot inference and the account lookup.
	githubScope = "read:user"
)

// Copilot is the GitHub Copilot provider. Endpoint fields exist for tests;
// zero values mean the real GitHub endpoints.
type Copilot struct {
	ClientID  string
	DeviceURL string
	TokenURL  string
	APIURL    string
	// CopilotAPI, when set, replaces the per-account inference host (tests).
	CopilotAPI string
}

func init() { Register(&Copilot{}) }

// ID implements Provider.
func (c *Copilot) ID() string { return "copilot" }

// DisplayName implements Provider.
func (c *Copilot) DisplayName() string { return "GitHub Copilot" }

// Description implements Provider.
func (c *Copilot) Description() string {
	return "Use your own GitHub Copilot plan (Free, Pro, Pro+, Business or Enterprise). You sign in on github.com; Janus never sees your GitHub password."
}

// AdminNote implements Provider.
func (c *Copilot) AdminNote() string {
	return "GitHub supports third-party tools signing in with a Copilot subscription through an OAuth app; Janus uses its own registered app, so nothing needs to be set up. GitHub announces this for paid plans; Copilot Free accounts also work. Organizations that restrict OAuth apps must approve \"Janus\" once on GitHub."
}

// AdapterType implements Provider.
// Copilot serves the OpenAI routes without a /v1 segment; the
// github_copilot adapter strips it from the client's path.
func (c *Copilot) AdapterType() string { return adapter.CopilotAdapterType }

// InferenceBaseURL implements Provider: the individual-plan host, used only
// when a connection has no recorded host.
func (c *Copilot) InferenceBaseURL() string { return c.apiHost(nil) }

// ConnectionBaseURL implements ConnectionRouter.
func (c *Copilot) ConnectionBaseURL(meta map[string]string) string { return c.apiHost(meta) }

// apiHost is the account's Copilot host. Only GitHub's own Copilot hosts are
// accepted, so a tampered row cannot point the token elsewhere.
func (c *Copilot) apiHost(meta map[string]string) string {
	if c.CopilotAPI != "" {
		return strings.TrimRight(c.CopilotAPI, "/")
	}
	if api := meta["api"]; validCopilotAPI(api) {
		return strings.TrimRight(api, "/")
	}
	return copilotDefaultAPI
}

func validCopilotAPI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".githubcopilot.com")
}

func (c *Copilot) clientID() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	if v := strings.TrimSpace(os.Getenv("JANUS_GITHUB_OAUTH_CLIENT_ID")); v != "" {
		return v
	}
	return githubDefaultClientID
}

// StartDeviceAuthorization implements Provider.
func (c *Copilot) StartDeviceAuthorization(ctx context.Context, client *http.Client) (*DeviceAuthorization, error) {
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	status, body, err := postForm(ctx, client, pick(c.DeviceURL, githubDeviceURL),
		url.Values{"client_id": {c.clientID()}, "scope": {githubScope}}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out.DeviceCode == "" {
		if code, _ := oauthError(body); code == "device_flow_disabled" {
			return nil, fmt.Errorf("the GitHub OAuth app does not have device flow enabled")
		}
		return nil, fmt.Errorf("GitHub device sign-in failed (HTTP %d): %s", status, oauthDetail(body))
	}
	if out.Interval < 5 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 900
	}
	return &DeviceAuthorization{
		DeviceCode: out.DeviceCode, UserCode: out.UserCode,
		VerificationURI: pick(out.VerificationURI, "https://github.com/login/device"),
		// GitHub has no prefilled-code link; the user types the code.
		VerificationURIComplete: pick(out.VerificationURI, "https://github.com/login/device"),
		ExpiresIn:               time.Duration(out.ExpiresIn) * time.Second, Interval: time.Duration(out.Interval) * time.Second,
	}, nil
}

// PollDeviceAuthorization implements Provider. GitHub answers pending and
// error states with HTTP 200 and an "error" field.
func (c *Copilot) PollDeviceAuthorization(ctx context.Context, client *http.Client, deviceCode string) (*Tokens, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	status, body, err := postForm(ctx, client, pick(c.TokenURL, githubTokenURL), url.Values{
		"client_id": {c.clientID()}, "device_code": {deviceCode}, "grant_type": {deviceGrantType},
	}, &out)
	if err != nil {
		return nil, err
	}
	switch code, _ := oauthError(body); code {
	case "":
	case "authorization_pending":
		return nil, ErrAuthorizationPending
	case "slow_down":
		return nil, ErrSlowDown
	case "expired_token", "incorrect_device_code", "bad_verification_code":
		return nil, ErrExpired
	case "access_denied":
		return nil, ErrDenied
	default:
		return nil, fmt.Errorf("GitHub sign-in failed: %s", oauthDetail(body))
	}
	if status != http.StatusOK || out.AccessToken == "" {
		return nil, fmt.Errorf("GitHub token exchange failed (HTTP %d): %s", status, oauthDetail(body))
	}
	tok := &Tokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Scope: out.Scope}
	if out.ExpiresIn > 0 {
		// Only when an organisation's own OAuth app has token expiry on.
		tok.ExpiresAt = time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// Refresh implements Provider. The Janus OAuth app issues non-expiring
// tokens, so this only runs for an override app with expiry enabled, which
// would need a client secret Janus does not hold.
func (c *Copilot) Refresh(context.Context, *http.Client, string) (*Tokens, error) {
	return nil, &ReauthError{Reason: "GitHub sign-in expired. Connect the subscription again."}
}

// copilotUser is the part of api.github.com/copilot_internal/user Janus
// reads: which plan, which host, and the plan's quotas.
type copilotUser struct {
	Login         string `json:"login"`
	Plan          string `json:"copilot_plan"`
	SKU           string `json:"access_type_sku"`
	ChatEnabled   *bool  `json:"chat_enabled"`
	QuotaResetUTC string `json:"quota_reset_date_utc"`
	QuotaReset    string `json:"quota_reset_date"`
	Endpoints     struct {
		API string `json:"api"`
	} `json:"endpoints"`
	Snapshots map[string]struct {
		Entitlement      *float64 `json:"entitlement"`
		Remaining        *float64 `json:"remaining"`
		PercentRemaining *float64 `json:"percent_remaining"`
		Unlimited        bool     `json:"unlimited"`
		HasQuota         *bool    `json:"has_quota"`
	} `json:"quota_snapshots"`
}

func (c *Copilot) copilotUser(ctx context.Context, client *http.Client, token string) (*copilotUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pick(c.APIURL, githubAPIURL)+"/copilot_internal/user", nil)
	if err != nil {
		return nil, err
	}
	githubHeaders(req, token)
	var out copilotUser
	status, body, err := do(client, req, &out)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return &out, nil
	case http.StatusUnauthorized:
		return nil, &ReauthError{Reason: "GitHub no longer accepts this sign-in (it was revoked or unused for a year). Connect the subscription again."}
	case http.StatusForbidden, http.StatusNotFound:
		return nil, &ReauthError{Reason: "This GitHub account has no Copilot access. Turn on Copilot (Free works) at github.com/settings/copilot, or ask your organization to approve the Janus app."}
	}
	return nil, fmt.Errorf("GitHub Copilot lookup failed (HTTP %d): %s", status, oauthDetail(body))
}

func githubHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// Account implements Provider. It also proves the account has Copilot and
// records which Copilot host serves it.
func (c *Copilot) Account(ctx context.Context, client *http.Client, accessToken string) (*Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pick(c.APIURL, githubAPIURL)+"/user", nil)
	if err != nil {
		return nil, err
	}
	githubHeaders(req, accessToken)
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	status, body, err := do(client, req, &u)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || u.ID == 0 {
		return nil, fmt.Errorf("GitHub account lookup failed (HTTP %d): %s", status, oauthDetail(body))
	}
	cu, err := c.copilotUser(ctx, client, accessToken)
	if err != nil {
		return nil, err
	}
	meta := map[string]string{"plan": copilotPlanName(cu)}
	if validCopilotAPI(cu.Endpoints.API) {
		meta["api"] = strings.TrimRight(cu.Endpoints.API, "/")
	}
	email := u.Email
	if email == "" {
		email = u.Login // GitHub hides private emails; the login identifies the account.
	}
	return &Account{Subject: fmt.Sprint(u.ID), Email: email, Name: pick(u.Name, u.Login), Meta: meta}, nil
}

func copilotPlanName(cu *copilotUser) string {
	sku := strings.ToLower(cu.SKU)
	switch {
	case strings.Contains(sku, "free"):
		return "Copilot Free"
	case strings.Contains(sku, "plus"):
		return "Copilot Pro+"
	}
	switch strings.ToLower(cu.Plan) {
	case "business":
		return "Copilot Business"
	case "enterprise":
		return "Copilot Enterprise"
	case "individual":
		return "Copilot Pro"
	}
	return "Copilot"
}

// Models implements Provider.
func (c *Copilot) Models(ctx context.Context, client *http.Client, accessToken string) ([]string, error) {
	infos, err := c.ListModels(ctx, client, accessToken)
	return idsOf(infos), err
}

// ListModels implements ModelLister: the chat models the account's plan
// offers. GitHub's catalog lists the accepted reasoning values under
// capabilities.supports.reasoning_effort; a model without that list takes
// no reasoning setting ("model … does not support reasoning effort").
func (c *Copilot) ListModels(ctx context.Context, client *http.Client, accessToken string) ([]ModelInfo, error) {
	base := c.CopilotAPI
	if base == "" {
		cu, err := c.copilotUser(ctx, client, accessToken)
		if err != nil {
			return nil, err
		}
		base = c.apiHost(map[string]string{"api": cu.Endpoints.API})
	}
	var out struct {
		Data []struct {
			ID           string `json:"id"`
			Capabilities struct {
				Type     string `json:"type"`
				Supports struct {
					ReasoningEffort []string `json:"reasoning_effort"`
				} `json:"supports"`
			} `json:"capabilities"`
			Policy *struct {
				State string `json:"state"`
			} `json:"policy"`
		} `json:"data"`
	}
	status, body, err := getJSON(ctx, client, strings.TrimRight(base, "/")+"/models", accessToken, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, &ReauthError{Reason: "GitHub Copilot no longer accepts this sign-in. Connect the subscription again."}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GitHub Copilot model list failed (HTTP %d): %s", status, oauthDetail(body))
	}
	seen := map[string]bool{}
	models := []ModelInfo{}
	for _, m := range out.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] || m.Capabilities.Type != "chat" {
			continue
		}
		// Models an organization disabled by policy cannot be called.
		if m.Policy != nil && strings.EqualFold(m.Policy.State, "disabled") {
			continue
		}
		seen[id] = true
		models = append(models, ModelInfo{ID: id, Reasoning: ReasoningLevels(m.Capabilities.Supports.ReasoningEffort, "")})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// Usage implements UsageReporter: Copilot's premium-request, chat and
// completion allowances for the current month.
func (c *Copilot) Usage(ctx context.Context, client *http.Client, accessToken string, _ map[string]string) (*PlanUsage, error) {
	cu, err := c.copilotUser(ctx, client, accessToken)
	if err != nil {
		return nil, err
	}
	reset := time.Time{}
	for _, s := range []string{cu.QuotaResetUTC, cu.QuotaReset} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			reset = t.UTC()
			break
		}
		if t, err := time.Parse("2006-01-02", s); err == nil {
			reset = t.UTC()
			break
		}
	}
	u := &PlanUsage{Plan: copilotPlanName(cu), Windows: []UsageWindow{}}
	labels := []struct{ key, label string }{
		{"premium_interactions", "Premium requests"}, {"chat", "Chat requests"}, {"completions", "Code completions"},
	}
	for _, l := range labels {
		s, ok := cu.Snapshots[l.key]
		// A plan without this allowance at all (Copilot Free has no
		// premium requests) reports entitlement 0; that is "not part of
		// the plan", not "100% used".
		if !ok || (!s.Unlimited && ((s.HasQuota != nil && !*s.HasQuota) || (s.Entitlement != nil && *s.Entitlement == 0))) {
			continue
		}
		w := UsageWindow{Label: l.label, ResetsAt: reset, Unlimited: s.Unlimited}
		if !s.Unlimited && s.Entitlement != nil && *s.Entitlement > 0 {
			limit := *s.Entitlement
			used := limit
			if s.Remaining != nil {
				used = limit - *s.Remaining
			}
			if used < 0 {
				used = 0
			}
			w.Used, w.Limit = &used, &limit
			w.UsedPercent = used / limit * 100
		} else if !s.Unlimited && s.PercentRemaining != nil {
			w.UsedPercent = 100 - *s.PercentRemaining
		}
		u.Windows = append(u.Windows, w)
	}
	return u, nil
}

// Revoke implements Provider. Revoking an OAuth app grant needs the app's
// client secret, which Janus does not ship; the user removes the grant on
// GitHub (Settings → Applications) if they want it gone there too.
func (c *Copilot) Revoke(context.Context, *http.Client, string) error { return nil }
