package subscription

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// OpenAI connects a ChatGPT Plus / Pro / Business plan. The plan's
// inference surface is chatgpt.com/backend-api/codex, the endpoint OpenAI's
// Codex agents use; it speaks the Responses API only, so the openai_codex
// adapter translates Chat Completions to and from it.
//
// Sign-in is OpenAI's device flow for Codex: the user enters a short code at
// auth.openai.com/codex/device, Janus polls for an authorization code plus
// PKCE verifier, then exchanges them at the standard token endpoint. The
// OAuth client is OpenAI's public Codex client, the same one third-party
// Codex-compatible agents use; JANUS_OPENAI_OAUTH_CLIENT_ID overrides it.
const (
	openaiDefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	openaiIssuer          = "https://auth.openai.com"
	openaiUserCodeURL     = openaiIssuer + "/api/accounts/deviceauth/usercode"
	openaiDevicePollURL   = openaiIssuer + "/api/accounts/deviceauth/token"
	openaiVerifyURL       = openaiIssuer + "/codex/device"
	openaiTokenURL        = openaiIssuer + "/oauth/token"
	openaiRedirectURI     = openaiIssuer + "/deviceauth/callback"
	openaiInferenceURL    = "https://chatgpt.com/backend-api/codex"
	// The model catalog is gated by client version; the newest version
	// sees every model, the 0.0.0 sentinel is the fallback for accounts
	// the gate answers with an empty list.
	openaiCatalogVersions = "99.0.0,0.0.0"
	// openaiDeviceCodeTTL is how long OpenAI honours a device code.
	openaiDeviceCodeTTL = 15 * time.Minute
)

// OpenAI is the ChatGPT plan provider. Endpoint fields exist for tests;
// zero values mean the real OpenAI endpoints.
type OpenAI struct {
	ClientID     string
	UserCodeURL  string
	DevicePoll   string
	TokenURL     string
	InferenceURL string
}

func init() { Register(&OpenAI{}) }

// ID implements Provider.
func (o *OpenAI) ID() string { return "openai" }

// DisplayName implements Provider.
func (o *OpenAI) DisplayName() string { return "OpenAI ChatGPT (Plus / Pro)" }

// Description implements Provider.
func (o *OpenAI) Description() string {
	return "Use your own ChatGPT Plus, Pro or Business plan. You sign in on auth.openai.com; Janus never sees your OpenAI password."
}

// AdminNote implements Provider.
func (o *OpenAI) AdminNote() string {
	return "OpenAI lets ChatGPT plans sign in to third-party Codex-compatible agents. Janus uses OpenAI's public Codex sign-in client and the plan's usage limits apply; turn this off if OpenAI changes those terms."
}

// AdapterType implements Provider.
func (o *OpenAI) AdapterType() string { return adapter.CodexAdapterType }

// InferenceBaseURL implements Provider.
func (o *OpenAI) InferenceBaseURL() string { return pick(o.InferenceURL, openaiInferenceURL) }

func (o *OpenAI) clientID() string {
	if o.ClientID != "" {
		return o.ClientID
	}
	if v := strings.TrimSpace(os.Getenv("JANUS_OPENAI_OAUTH_CLIENT_ID")); v != "" {
		return v
	}
	return openaiDefaultClientID
}

// openaiDeviceCode packs OpenAI's two-part device handle into the single
// opaque device code the Provider interface stores.
func openaiDeviceCode(authID, userCode string) string { return authID + "|" + userCode }

func splitOpenAIDeviceCode(code string) (authID, userCode string, ok bool) {
	authID, userCode, ok = strings.Cut(code, "|")
	return authID, userCode, ok && authID != "" && userCode != ""
}

// StartDeviceAuthorization implements Provider.
func (o *OpenAI) StartDeviceAuthorization(ctx context.Context, client *http.Client) (*DeviceAuthorization, error) {
	var out struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"`
	}
	status, body, err := postJSON(ctx, client, pick(o.UserCodeURL, openaiUserCodeURL), map[string]string{"client_id": o.clientID()}, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusTooManyRequests {
		return nil, fmt.Errorf("OpenAI is rate-limiting sign-in requests; wait a minute and try again")
	}
	if status != http.StatusOK || out.DeviceAuthID == "" || out.UserCode == "" {
		return nil, fmt.Errorf("OpenAI device sign-in failed (HTTP %d): %s", status, oauthDetail(body))
	}
	interval := 5
	var n json.Number
	if json.Unmarshal(bytes.Trim(out.Interval, `"`), &n) == nil {
		if v, err := n.Int64(); err == nil && v > 0 {
			interval = int(v)
		}
	}
	if interval < 3 {
		interval = 3
	}
	return &DeviceAuthorization{
		DeviceCode: openaiDeviceCode(out.DeviceAuthID, out.UserCode), UserCode: out.UserCode,
		VerificationURI: openaiVerifyURL, VerificationURIComplete: openaiVerifyURL,
		ExpiresIn: openaiDeviceCodeTTL, Interval: time.Duration(interval) * time.Second,
	}, nil
}

// PollDeviceAuthorization implements Provider. OpenAI answers 403/404 while
// the user has not finished, then an authorization code + PKCE verifier that
// is exchanged for tokens.
func (o *OpenAI) PollDeviceAuthorization(ctx context.Context, client *http.Client, deviceCode string) (*Tokens, error) {
	authID, userCode, ok := splitOpenAIDeviceCode(deviceCode)
	if !ok {
		return nil, ErrExpired
	}
	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	status, body, err := postJSON(ctx, client, pick(o.DevicePoll, openaiDevicePollURL),
		map[string]string{"device_auth_id": authID, "user_code": userCode}, &code)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusNotFound:
		return nil, ErrAuthorizationPending
	case http.StatusTooManyRequests:
		return nil, ErrSlowDown
	case http.StatusGone, http.StatusBadRequest:
		return nil, ErrExpired
	default:
		return nil, fmt.Errorf("OpenAI sign-in polling failed (HTTP %d): %s", status, oauthDetail(body))
	}
	if code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return nil, fmt.Errorf("OpenAI sign-in returned no authorization code")
	}
	tok, status, body, err := o.token(ctx, client, url.Values{
		"grant_type": {"authorization_code"}, "code": {code.AuthorizationCode},
		"redirect_uri": {openaiRedirectURI}, "client_id": {o.clientID()}, "code_verifier": {code.CodeVerifier},
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("OpenAI token exchange failed (HTTP %d): %s", status, oauthDetail(body))
	}
	return tok, nil
}

// Refresh implements Provider. OpenAI rotates refresh tokens, and a
// refresh token another client already used is refused as reused.
func (o *OpenAI) Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Tokens, error) {
	tok, status, body, err := o.token(ctx, client, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {o.clientID()},
	})
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusOK:
		if tok.RefreshToken == "" {
			tok.RefreshToken = refreshToken
		}
		return tok, nil
	case status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("OpenAI is rate-limiting token refresh; retry shortly")
	case oauthErrorCode(body) == "refresh_token_reused":
		return nil, &ReauthError{Reason: "OpenAI says this sign-in was already used by another app. Connect the subscription again."}
	case status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, &ReauthError{Reason: "OpenAI no longer accepts this sign-in (" + oauthDetail(body) + "). Connect the subscription again."}
	}
	return nil, fmt.Errorf("OpenAI token refresh failed (HTTP %d): %s", status, oauthDetail(body))
}

func (o *OpenAI) token(ctx context.Context, client *http.Client, form url.Values) (*Tokens, int, []byte, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	status, body, err := postForm(ctx, client, pick(o.TokenURL, openaiTokenURL), form, &out)
	if err != nil {
		return nil, 0, nil, err
	}
	if status != http.StatusOK {
		return nil, status, body, nil
	}
	if out.AccessToken == "" {
		return nil, status, body, fmt.Errorf("OpenAI token response carried no access_token")
	}
	expires := time.Duration(out.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = jwtLifetime(out.AccessToken)
	}
	return &Tokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: time.Now().UTC().Add(expires), Scope: out.Scope}, status, body, nil
}

// jwtLifetime reads exp from an access token, falling back to one hour.
func jwtLifetime(token string) time.Duration {
	var c struct {
		Exp int64 `json:"exp"`
	}
	if jwtClaims(token, &c) && c.Exp > 0 {
		if d := time.Until(time.Unix(c.Exp, 0)); d > 0 {
			return d
		}
	}
	return time.Hour
}

func jwtClaims(token string, dst any) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// Account implements Provider. The ChatGPT access token is a JWT that
// carries the account identity, so no userinfo call is needed.
func (o *OpenAI) Account(_ context.Context, _ *http.Client, accessToken string) (*Account, error) {
	var c struct {
		Sub     string `json:"sub"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			UserID    string `json:"chatgpt_user_id"`
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if !jwtClaims(accessToken, &c) {
		return nil, fmt.Errorf("OpenAI access token is not readable")
	}
	subject := c.Auth.UserID
	if subject == "" {
		subject = c.Sub
	}
	if subject == "" {
		return nil, fmt.Errorf("OpenAI access token names no account")
	}
	// One ChatGPT user can belong to several workspaces; the binding is
	// per user+workspace so each stays attributable.
	if c.Auth.AccountID != "" {
		subject += ":" + c.Auth.AccountID
	}
	name := ""
	if c.Auth.PlanType != "" {
		name = "ChatGPT " + strings.ToUpper(c.Auth.PlanType[:1]) + c.Auth.PlanType[1:]
	}
	return &Account{Subject: subject, Email: c.Profile.Email, Name: name}, nil
}

// Models implements Provider.
func (o *OpenAI) Models(ctx context.Context, client *http.Client, accessToken string) ([]string, error) {
	infos, err := o.ListModels(ctx, client, accessToken)
	return idsOf(infos), err
}

// ListModels implements ModelLister: the plan's own model catalog, hidden
// entries dropped, ordered by OpenAI's priority, with each model's
// reasoning levels and default.
func (o *OpenAI) ListModels(ctx context.Context, client *http.Client, accessToken string) ([]ModelInfo, error) {
	var lastStatus int
	var lastBody []byte
	for _, v := range strings.Split(openaiCatalogVersions, ",") {
		var out struct {
			Models []struct {
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
				Priority   *int   `json:"priority"`
				Levels     []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
				DefaultLevel string `json:"default_reasoning_level"`
			} `json:"models"`
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(o.InferenceBaseURL(), "/")+"/models?client_version="+v, nil)
		if err != nil {
			return nil, err
		}
		for k, val := range adapter.CodexHeaders(accessToken) {
			req.Header.Set(k, val)
		}
		req.Header.Set("Accept", "application/json")
		status, body, err := do(client, req, &out)
		if err != nil {
			return nil, err
		}
		lastStatus, lastBody = status, body
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, &ReauthError{Reason: fmt.Sprintf("OpenAI refused the model list (HTTP %d). The plan may not include Codex access.", status)}
		}
		if status != http.StatusOK || len(out.Models) == 0 {
			continue
		}
		type ranked struct {
			slug string
			rank int
			r    *Reasoning
		}
		rows := []ranked{}
		seen := map[string]bool{}
		for _, m := range out.Models {
			slug := strings.TrimSpace(m.Slug)
			vis := strings.ToLower(strings.TrimSpace(m.Visibility))
			if slug == "" || seen[slug] || vis == "hide" || vis == "hidden" {
				continue
			}
			seen[slug] = true
			rank := 1 << 20
			if m.Priority != nil {
				rank = *m.Priority
			}
			var r *Reasoning
			if m.Levels != nil {
				levels := make([]string, 0, len(m.Levels))
				for _, l := range m.Levels {
					levels = append(levels, l.Effort)
				}
				r = ReasoningLevels(levels, m.DefaultLevel)
			}
			rows = append(rows, ranked{slug, rank, r})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].rank != rows[j].rank {
				return rows[i].rank < rows[j].rank
			}
			return rows[i].slug < rows[j].slug
		})
		infos := make([]ModelInfo, len(rows))
		for i, r := range rows {
			infos[i] = ModelInfo{ID: r.slug, Reasoning: r.r}
		}
		return infos, nil
	}
	return nil, fmt.Errorf("OpenAI model list failed (HTTP %d): %s", lastStatus, oauthDetail(lastBody))
}

// Usage implements UsageReporter: the plan's rolling limits as ChatGPT
// reports them (the 5-hour and weekly windows Codex shows), plus credits.
func (o *OpenAI) Usage(ctx context.Context, client *http.Client, accessToken string, _ map[string]string) (*PlanUsage, error) {
	base := strings.TrimSuffix(strings.TrimRight(o.InferenceBaseURL(), "/"), "/codex")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range adapter.CodexHeaders(accessToken) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	var out struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits struct {
			HasCredits bool     `json:"has_credits"`
			Unlimited  bool     `json:"unlimited"`
			Balance    *float64 `json:"balance,string"`
		} `json:"credits"`
	}
	status, body, err := do(client, req, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, &ReauthError{Reason: "OpenAI no longer accepts this sign-in. Connect the subscription again."}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("OpenAI usage lookup failed (HTTP %d): %s", status, oauthDetail(body))
	}
	u := &PlanUsage{Windows: []UsageWindow{}}
	if out.PlanType != "" {
		u.Plan = "ChatGPT " + strings.ToUpper(out.PlanType[:1]) + out.PlanType[1:]
	}
	for i, w := range []*codexWindow{out.RateLimit.Primary, out.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		u.Windows = append(u.Windows, UsageWindow{Label: w.label(i), UsedPercent: *w.UsedPercent, ResetsAt: w.resetsAt()})
	}
	switch {
	case out.Credits.HasCredits && out.Credits.Unlimited:
		u.Notes = append(u.Notes, "Credits: unlimited")
	case out.Credits.HasCredits && out.Credits.Balance != nil:
		u.Notes = append(u.Notes, fmt.Sprintf("Credits balance: $%.2f", *out.Credits.Balance))
	}
	return u, nil
}

// codexWindow is one of ChatGPT's rolling usage windows. reset_at arrives
// as Unix seconds or an RFC 3339 string depending on the backend version.
type codexWindow struct {
	UsedPercent   *float64        `json:"used_percent"`
	WindowSeconds *int64          `json:"limit_window_seconds"`
	ResetAt       json.RawMessage `json:"reset_at"`
}

// label names a window by its published length, not its position: a plan
// that only has a weekly limit returns it in the primary slot.
func (w *codexWindow) label(position int) string {
	if w.WindowSeconds != nil {
		switch *w.WindowSeconds {
		case 5 * 3600:
			return "5-hour limit"
		case 7 * 24 * 3600:
			return "Weekly limit"
		}
		if h := *w.WindowSeconds / 3600; h > 0 && h < 48 {
			return fmt.Sprintf("%d-hour limit", h)
		}
		if d := *w.WindowSeconds / 86400; d > 0 {
			return fmt.Sprintf("%d-day limit", d)
		}
	}
	if position == 0 {
		return "5-hour limit"
	}
	return "Weekly limit"
}

func (w *codexWindow) resetsAt() time.Time {
	var n float64
	if json.Unmarshal(w.ResetAt, &n) == nil && n > 0 {
		return time.Unix(int64(n), 0).UTC()
	}
	var s string
	if json.Unmarshal(w.ResetAt, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Revoke implements Provider. OpenAI publishes no revocation endpoint for
// the Codex client; the user removes the app from their OpenAI account
// settings, and Janus discards its copy of the tokens.
func (o *OpenAI) Revoke(context.Context, *http.Client, string) error { return nil }

func postJSON(ctx context.Context, client *http.Client, endpoint string, payload, dst any) (int, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return do(client, req, dst)
}
