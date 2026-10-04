package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// xAI publishes subscription sign-in for third-party agents (SuperGrok and
// X Premium+ plans; https://x.ai/news/grok-opencode). The device-code grant
// against auth.x.ai is the flow xAI documents for headless hosts, and the
// resulting bearer token is accepted by the standard api.x.ai/v1 surface.
//
// The OAuth client below is the public client xAI's own CLI and the
// third-party integrations it has announced use; there is no self-service
// registration yet. JANUS_XAI_OAUTH_CLIENT_ID overrides it the moment xAI
// issues Janus its own client.
const (
	xaiDefaultClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiIssuer          = "https://auth.x.ai"
	xaiDeviceURL       = xaiIssuer + "/oauth2/device/code"
	xaiTokenURL        = xaiIssuer + "/oauth2/token"
	xaiUserinfoURL     = xaiIssuer + "/oauth2/userinfo"
	xaiRevokeURL       = xaiIssuer + "/oauth2/revoke"
	xaiScope           = "openid profile email offline_access grok-cli:access api:access"
	xaiInferenceURL    = "https://api.x.ai/v1"
	deviceGrantType    = "urn:ietf:params:oauth:grant-type:device_code"
)

// XAI is the SuperGrok / X Premium+ provider. The endpoint fields exist so
// tests can point it at a fake authorization server; zero values mean the
// real xAI endpoints.
type XAI struct {
	ClientID     string
	DeviceURL    string
	TokenURL     string
	UserinfoURL  string
	RevokeURL    string
	InferenceURL string
}

func init() { Register(&XAI{}) }

// ID implements Provider.
func (x *XAI) ID() string { return "xai" }

// DisplayName implements Provider.
func (x *XAI) DisplayName() string { return "xAI Grok (SuperGrok / X Premium+)" }

// Description implements Provider.
func (x *XAI) Description() string {
	return "Use your own SuperGrok or X Premium+ plan. You sign in on accounts.x.ai; Janus never sees your xAI password."
}

// AdminNote implements Provider.
func (x *XAI) AdminNote() string {
	return "xAI permits approved third-party agents to use SuperGrok and X Premium+ sign-in. Janus uses xAI's public device-sign-in client until xAI issues it its own; turn this off if xAI withdraws that permission."
}

// AdapterType implements Provider: api.x.ai speaks the OpenAI wire format.
func (x *XAI) AdapterType() string { return "openai_compatible" }

// InferenceBaseURL implements Provider.
func (x *XAI) InferenceBaseURL() string { return pick(x.InferenceURL, xaiInferenceURL) }

func (x *XAI) clientID() string {
	if x.ClientID != "" {
		return x.ClientID
	}
	if v := strings.TrimSpace(os.Getenv("JANUS_XAI_OAUTH_CLIENT_ID")); v != "" {
		return v
	}
	return xaiDefaultClientID
}

// StartDeviceAuthorization implements Provider.
func (x *XAI) StartDeviceAuthorization(ctx context.Context, client *http.Client) (*DeviceAuthorization, error) {
	var out struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, body, err := postForm(ctx, client, pick(x.DeviceURL, xaiDeviceURL), url.Values{
		"client_id": {x.clientID()}, "scope": {xaiScope},
	}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("xAI device authorization failed (HTTP %d): %s", status, oauthDetail(body))
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 900
	}
	if out.VerificationURIComplete == "" {
		out.VerificationURIComplete = out.VerificationURI
	}
	return &DeviceAuthorization{
		DeviceCode: out.DeviceCode, UserCode: out.UserCode,
		VerificationURI: out.VerificationURI, VerificationURIComplete: out.VerificationURIComplete,
		ExpiresIn: time.Duration(out.ExpiresIn) * time.Second, Interval: time.Duration(out.Interval) * time.Second,
	}, nil
}

// PollDeviceAuthorization implements Provider.
func (x *XAI) PollDeviceAuthorization(ctx context.Context, client *http.Client, deviceCode string) (*Tokens, error) {
	tok, status, body, err := x.token(ctx, client, url.Values{
		"grant_type": {deviceGrantType}, "client_id": {x.clientID()}, "device_code": {deviceCode},
	})
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		return tok, nil
	}
	switch oauthErrorCode(body) {
	case "authorization_pending":
		return nil, ErrAuthorizationPending
	case "slow_down":
		return nil, ErrSlowDown
	case "expired_token", "invalid_grant":
		return nil, ErrExpired
	case "access_denied":
		return nil, ErrDenied
	}
	return nil, fmt.Errorf("xAI token exchange failed (HTTP %d): %s", status, oauthDetail(body))
}

// Refresh implements Provider. xAI rotates refresh tokens.
func (x *XAI) Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Tokens, error) {
	tok, status, body, err := x.token(ctx, client, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {x.clientID()}, "refresh_token": {refreshToken},
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
	case status == http.StatusForbidden:
		// xAI gates API use by plan tier: the grant exists but the
		// account is not entitled. Reconnecting will not fix it.
		return nil, &ReauthError{Reason: "xAI refused API access for this account (HTTP 403). The plan may not include API/agent access; check your SuperGrok or X Premium+ tier."}
	case status == http.StatusBadRequest || status == http.StatusUnauthorized:
		return nil, &ReauthError{Reason: "xAI no longer accepts this sign-in (" + oauthDetail(body) + "). Connect the subscription again."}
	}
	return nil, fmt.Errorf("xAI token refresh failed (HTTP %d): %s", status, oauthDetail(body))
}

func (x *XAI) token(ctx context.Context, client *http.Client, form url.Values) (*Tokens, int, []byte, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	status, body, err := postForm(ctx, client, pick(x.TokenURL, xaiTokenURL), form, &out)
	if err != nil {
		return nil, 0, nil, err
	}
	if status != http.StatusOK {
		return nil, status, body, nil
	}
	if out.AccessToken == "" {
		return nil, status, body, fmt.Errorf("xAI token response carried no access_token")
	}
	expires := time.Duration(out.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = time.Hour
	}
	return &Tokens{
		AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: time.Now().UTC().Add(expires), Scope: out.Scope,
	}, status, body, nil
}

// Account implements Provider.
func (x *XAI) Account(ctx context.Context, client *http.Client, accessToken string) (*Account, error) {
	var out struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	status, body, err := getJSON(ctx, client, pick(x.UserinfoURL, xaiUserinfoURL), accessToken, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out.Sub == "" {
		return nil, fmt.Errorf("xAI userinfo failed (HTTP %d): %s", status, oauthDetail(body))
	}
	return &Account{Subject: out.Sub, Email: out.Email, Name: out.Name}, nil
}

// Models implements Provider.
func (x *XAI) Models(ctx context.Context, client *http.Client, accessToken string) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, body, err := getJSON(ctx, client, strings.TrimRight(x.InferenceBaseURL(), "/")+"/models", accessToken, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden {
		return nil, &ReauthError{Reason: "xAI refused API access for this account (HTTP 403). The plan may not include API/agent access."}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("xAI model list failed (HTTP %d): %s", status, oauthDetail(body))
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Revoke implements Provider.
func (x *XAI) Revoke(ctx context.Context, client *http.Client, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	status, body, err := postForm(ctx, client, pick(x.RevokeURL, xaiRevokeURL), url.Values{
		"client_id": {x.clientID()}, "token": {refreshToken}, "token_type_hint": {"refresh_token"},
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("xAI revoke failed (HTTP %d): %s", status, oauthDetail(body))
	}
	return nil
}

// --- shared HTTP helpers ------------------------------------------------------

func pick(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, dst any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return do(client, req, dst)
}

func getJSON(ctx context.Context, client *http.Client, endpoint, bearer string, dst any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	return do(client, req, dst)
}

func do(client *http.Client, req *http.Request, dst any) (int, []byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("contact %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read %s response: %w", req.URL.Host, err)
	}
	if resp.StatusCode == http.StatusOK && dst != nil && len(body) > 0 {
		if err := json.Unmarshal(body, dst); err != nil {
			return resp.StatusCode, body, fmt.Errorf("parse %s response: %w", req.URL.Host, err)
		}
	}
	return resp.StatusCode, body, nil
}

// oauthError reads an OAuth error body in either shape vendors use: the
// RFC 6749 {"error":"code","error_description":"..."} or the nested
// {"error":{"code":"...","message":"..."}}.
func oauthError(body []byte) (code, description string) {
	var e struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error) == 0 {
		return "", ""
	}
	if json.Unmarshal(e.Error, &code) == nil {
		return code, e.Description
	}
	var nested struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Error, &nested) == nil {
		code = nested.Code
		if code == "" {
			code = nested.Type
		}
		return code, nested.Message
	}
	return "", ""
}

func oauthErrorCode(body []byte) string {
	code, _ := oauthError(body)
	return code
}

// oauthDetail renders an OAuth error body for a message without echoing
// anything token-shaped: only the standard error code and description.
func oauthDetail(body []byte) string {
	if code, desc := oauthError(body); code != "" {
		if desc != "" {
			return code + ": " + desc
		}
		return code
	}
	if len(body) == 0 {
		return "empty response"
	}
	return "unexpected response"
}
