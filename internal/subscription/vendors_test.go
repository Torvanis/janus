package subscription

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGitHub serves the device flow, /user, copilot_internal/user and the
// Copilot model list, all on one server.
func fakeGitHub(t *testing.T, copilotStatus int) (*Copilot, *int) {
	t.Helper()
	polls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		authz := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/login/device/code":
			if form.Get("client_id") != "gh-client" || form.Get("scope") != "read:user" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":899,"interval":5}`))
		case "/login/oauth/access_token":
			polls++
			switch {
			case form.Get("device_code") != "dc":
				_, _ = w.Write([]byte(`{"error":"incorrect_device_code"}`))
			case polls == 1:
				// GitHub answers pending with HTTP 200.
				_, _ = w.Write([]byte(`{"error":"authorization_pending","error_description":"pending"}`))
			default:
				_, _ = w.Write([]byte(`{"access_token":"gho_live","token_type":"bearer","scope":"read:user"}`))
			}
		case "/user":
			if authz != "token gho_live" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"id":75057989,"login":"octo","name":"","email":null}`))
		case "/copilot_internal/user":
			if authz != "token gho_live" {
				w.WriteHeader(401)
				return
			}
			if copilotStatus != 200 {
				w.WriteHeader(copilotStatus)
				return
			}
			_, _ = w.Write([]byte(`{"login":"octo","access_type_sku":"free_limited_copilot","copilot_plan":"individual",
				"quota_reset_date_utc":"2026-10-01T00:00:00Z",
				"endpoints":{"api":"` + srv.URL + `"},
				"quota_snapshots":{"chat":{"entitlement":200,"remaining":150,"percent_remaining":75,"unlimited":false},
				"completions":{"entitlement":2000,"remaining":1998,"unlimited":false},
				"premium_interactions":{"entitlement":0,"remaining":0,"has_quota":false,"unlimited":false}}}`))
		case "/models":
			if authz != "Bearer gho_live" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o","capabilities":{"type":"chat"}},{"id":"gpt-4o","capabilities":{"type":"chat"}},
				{"id":"text-embedding-3","capabilities":{"type":"embeddings"}},{"id":"o-blocked","capabilities":{"type":"chat"},"policy":{"state":"disabled"}},
				{"id":"claude-x","capabilities":{"type":"chat"},"policy":{"state":"enabled"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &Copilot{ClientID: "gh-client", DeviceURL: srv.URL + "/login/device/code", TokenURL: srv.URL + "/login/oauth/access_token", APIURL: srv.URL}, &polls
}

func TestCopilotDeviceFlowAccountModelsUsage(t *testing.T) {
	c, _ := fakeGitHub(t, 200)
	ctx := context.Background()
	auth, err := c.StartDeviceAuthorization(ctx, nil)
	if err != nil || auth.UserCode != "ABCD-1234" || auth.DeviceCode != "dc" {
		t.Fatalf("start: %+v %v", auth, err)
	}
	if _, err := c.PollDeviceAuthorization(ctx, nil, "dc"); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("first poll: want pending, got %v", err)
	}
	tok, err := c.PollDeviceAuthorization(ctx, nil, "dc")
	if err != nil || tok.AccessToken != "gho_live" || !tok.ExpiresAt.IsZero() || tok.RefreshToken != "" {
		t.Fatalf("second poll: %+v %v", tok, err)
	}
	if _, err := c.PollDeviceAuthorization(ctx, nil, "other"); !errors.Is(err, ErrExpired) {
		t.Fatalf("bad device code: %v", err)
	}
	acct, err := c.Account(ctx, nil, "gho_live")
	// The fake's endpoint is plain http, which the host allow-list refuses,
	// so no api host is recorded; login stands in for the hidden email.
	if err != nil || acct.Subject != "75057989" || acct.Email != "octo" || acct.Name != "octo" || acct.Meta["plan"] != "Copilot Free" || acct.Meta["api"] != "" {
		t.Fatalf("account: %+v %v", acct, err)
	}
	c.CopilotAPI = c.APIURL // point inference/models at the fake
	models, err := c.Models(ctx, nil, "gho_live")
	if err != nil || strings.Join(models, ",") != "claude-x,gpt-4o" {
		t.Fatalf("models (chat only, deduped, policy-disabled dropped): %v %v", models, err)
	}
	u, err := c.Usage(ctx, nil, "gho_live", nil)
	// Premium requests are not part of the Free plan: omitted, not "100% used".
	if err != nil || u.Plan != "Copilot Free" || len(u.Windows) != 2 {
		t.Fatalf("usage: %+v %v", u, err)
	}
	chat, comp := u.Windows[0], u.Windows[1]
	if comp.Label != "Code completions" || *comp.Used != 2 || chat.Label != "Chat requests" || *chat.Used != 50 || *chat.Limit != 200 || chat.UsedPercent != 25 ||
		chat.ResetsAt.IsZero() {
		t.Fatalf("usage windows: %+v %+v", chat, comp)
	}
}

func TestCopilotAccountWithoutCopilotNeedsAction(t *testing.T) {
	c, _ := fakeGitHub(t, http.StatusNotFound)
	_, err := c.Account(context.Background(), nil, "gho_live")
	var reauth *ReauthError
	if !errors.As(err, &reauth) || !strings.Contains(reauth.Reason, "no Copilot access") {
		t.Fatalf("want reauth about Copilot access, got %v", err)
	}
	if _, err := c.Account(context.Background(), nil, "gho_dead"); err == nil {
		t.Fatal("dead token accepted")
	}
}

func TestCopilotHostAllowList(t *testing.T) {
	c := &Copilot{}
	for raw, want := range map[string]string{
		"https://api.business.githubcopilot.com":         "https://api.business.githubcopilot.com",
		"https://api.individual.githubcopilot.com/":      "https://api.individual.githubcopilot.com",
		"https://evil.example.com":                       copilotDefaultAPI,
		"http://api.business.githubcopilot.com":          copilotDefaultAPI,
		"https://githubcopilot.com.evil.com":             copilotDefaultAPI,
		"https://api.githubcopilot.com/v1":               copilotDefaultAPI,
		"https://user:pw@api.business.githubcopilot.com": copilotDefaultAPI,
		"": copilotDefaultAPI,
	} {
		if got := c.ConnectionBaseURL(map[string]string{"api": raw}); got != want {
			t.Errorf("%q → %q, want %q", raw, got, want)
		}
	}
}

func TestMistralKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer good-key-0123456789abcd" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"devstral-latest","capabilities":{"completion_chat":true}},
			{"id":"mistral-embed","capabilities":{"completion_chat":false}},
			{"id":"old-model","deprecation":"2026-01-01","capabilities":{"completion_chat":true}},
			{"id":"codestral-latest","capabilities":{"completion_chat":true}}]}`))
	}))
	defer srv.Close()
	m := &Mistral{APIURL: srv.URL + "/v1"}
	ctx := context.Background()
	// Surrounding whitespace from a paste is ignored.
	acct, err := m.VerifyKey(ctx, nil, "  good-key-0123456789abcd\n")
	if err != nil || acct.Email != "Vibe key …abcd" || !strings.HasPrefix(acct.Subject, "key:") || strings.Contains(acct.Subject, "good-key") {
		t.Fatalf("verify: %+v %v", acct, err)
	}
	models, _ := m.Models(ctx, nil, "good-key-0123456789abcd")
	if strings.Join(models, ",") != "codestral-latest,devstral-latest" {
		t.Fatalf("models: %v", models)
	}
	var reauth *ReauthError
	if _, err := m.VerifyKey(ctx, nil, "bad-key-0123456789abcdef"); !errors.As(err, &reauth) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := m.VerifyKey(ctx, nil, "short"); err == nil {
		t.Fatal("short key accepted")
	}
	var _ KeyAuth = m
	if _, err := m.StartDeviceAuthorization(ctx, nil); !errors.Is(err, ErrNotSupported) {
		t.Fatal("mistral should not offer device sign-in")
	}
}

func TestOpenAIUsageWindows(t *testing.T) {
	o := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{
			"primary_window":{"used_percent":12.5,"limit_window_seconds":604800,"reset_at":1790000000},
			"secondary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_at":"2026-10-01T00:00:00Z"}},
			"credits":{"has_credits":true,"unlimited":false,"balance":"4.2"}}`))
	})
	u, err := o.Usage(context.Background(), nil, "tok", nil)
	if err != nil || u.Plan != "ChatGPT Plus" || len(u.Windows) != 2 {
		t.Fatalf("usage: %+v %v", u, err)
	}
	// Labelled by window length, not slot.
	if u.Windows[0].Label != "Weekly limit" || u.Windows[1].Label != "5-hour limit" || u.Windows[0].ResetsAt.Unix() != 1790000000 || u.Windows[1].ResetsAt.IsZero() {
		t.Fatalf("windows: %+v", u.Windows)
	}
	if len(u.Notes) != 1 || u.Notes[0] != "Credits balance: $4.20" {
		t.Fatalf("notes: %v", u.Notes)
	}
	var reauth *ReauthError
	if _, err := o.Usage(context.Background(), nil, "dead", nil); !errors.As(err, &reauth) {
		t.Fatalf("dead token: %v", err)
	}
}

func TestNewProvidersRegistered(t *testing.T) {
	for _, id := range []string{"copilot", "mistral"} {
		p, ok := Get(id)
		if !ok || p.AdminNote() == "" || (id == "mistral") != (p.AdapterType() == "openai_compatible") {
			t.Fatalf("%s: %v %v", id, p, ok)
		}
	}
	if (&Copilot{}).clientID() != "Ov23liBCkw78ysVOJPdC" {
		t.Fatal("copilot must default to the Janus GitHub OAuth app")
	}
}
