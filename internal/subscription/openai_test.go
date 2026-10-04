package subscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func newFakeOpenAI(t *testing.T, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &OpenAI{ClientID: "c", UserCodeURL: srv.URL + "/usercode", DevicePoll: srv.URL + "/devicetoken",
		TokenURL: srv.URL + "/oauth/token", InferenceURL: srv.URL + "/codex"}
}

// Full device flow: user code, pending polls, then code+verifier exchanged
// for tokens with PKCE at the token endpoint.
func TestOpenAIDeviceFlow(t *testing.T) {
	access := fakeJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(),
		"https://api.openai.com/profile": map[string]any{"email": "me@example.com"},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_user_id": "user-1", "chatgpt_account_id": "ws-1", "chatgpt_plan_type": "plus"}})
	polls := 0
	var exchanged url.Values
	o := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/usercode":
			if !strings.Contains(string(body), `"client_id":"c"`) {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"device_auth_id":"da-1","user_code":"WXYZ-1234","interval":"5"}`))
		case "/devicetoken":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if !strings.Contains(string(body), `"device_auth_id":"da-1"`) || !strings.Contains(string(body), `"user_code":"WXYZ-1234"`) {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"authorization_code":"ac-1","code_verifier":"ver-1"}`))
		case "/oauth/token":
			exchanged, _ = url.ParseQuery(string(body))
			_, _ = w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"rt-1","id_token":"x"}`))
		default:
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	da, err := o.StartDeviceAuthorization(ctx, nil)
	if err != nil || da.UserCode != "WXYZ-1234" || da.Interval != 5*time.Second || !strings.Contains(da.VerificationURI, "auth.openai.com/codex/device") {
		t.Fatalf("start: %+v %v", da, err)
	}
	if _, err := o.PollDeviceAuthorization(ctx, nil, da.DeviceCode); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("first poll: %v", err)
	}
	tok, err := o.PollDeviceAuthorization(ctx, nil, da.DeviceCode)
	if err != nil || tok.AccessToken != access || tok.RefreshToken != "rt-1" {
		t.Fatalf("poll: %+v %v", tok, err)
	}
	if exchanged.Get("grant_type") != "authorization_code" || exchanged.Get("code") != "ac-1" || exchanged.Get("code_verifier") != "ver-1" ||
		exchanged.Get("redirect_uri") != openaiRedirectURI || exchanged.Get("client_id") != "c" {
		t.Fatalf("exchange form: %v", exchanged)
	}
	if d := time.Until(tok.ExpiresAt); d < 90*time.Minute || d > 3*time.Hour {
		t.Fatalf("expiry from JWT exp: %v", d)
	}
	acct, err := o.Account(ctx, nil, access)
	if err != nil || acct.Subject != "user-1:ws-1" || acct.Email != "me@example.com" || acct.Name != "ChatGPT Plus" {
		t.Fatalf("account: %+v %v", acct, err)
	}
}

func TestOpenAIRefreshReuseNeedsReconnect(t *testing.T) {
	o := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_reused","message":"already used"}}`))
	})
	_, err := o.Refresh(context.Background(), nil, "rt")
	var re *ReauthError
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "another app") {
		t.Fatalf("reused refresh: %v", err)
	}
}

// The catalog is read with the plan's headers, hidden models dropped,
// ordered by priority; an empty answer at the newest client version falls
// back to the ungated one.
func TestOpenAIModelsCatalog(t *testing.T) {
	access := fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1"}})
	var versions []string
	o := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/models" || r.Header.Get("Authorization") != "Bearer "+access || r.Header.Get("ChatGPT-Account-ID") != "ws-1" {
			w.WriteHeader(401)
			return
		}
		v := r.URL.Query().Get("client_version")
		versions = append(versions, v)
		if v != "0.0.0" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5-mini","priority":2},{"slug":"internal","visibility":"hide","priority":0},{"slug":"gpt-5.5","priority":1}]}`))
	})
	models, err := o.Models(context.Background(), nil, access)
	if err != nil || strings.Join(models, ",") != "gpt-5.5,gpt-5.5-mini" || len(versions) != 2 {
		t.Fatalf("models=%v versions=%v err=%v", models, versions, err)
	}
	o2 := newFakeOpenAI(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	var re *ReauthError
	if _, err := o2.Models(context.Background(), nil, access); !errors.As(err, &re) {
		t.Fatalf("forbidden catalog: %v", err)
	}
}

func TestOpenAIRegisteredAndOffByDefault(t *testing.T) {
	p, ok := Get("openai")
	if !ok || p.AdapterType() != "openai_codex" || p.AdminNote() == "" {
		t.Fatalf("openai provider: %v %v", p, ok)
	}
}
