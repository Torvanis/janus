package subscription

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeXAI(t *testing.T, handler http.HandlerFunc) *XAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &XAI{
		ClientID: "c", DeviceURL: srv.URL + "/device", TokenURL: srv.URL + "/token",
		UserinfoURL: srv.URL + "/userinfo", RevokeURL: srv.URL + "/revoke", InferenceURL: srv.URL + "/v1",
	}
}

func TestXAIPollMapsRFC8628Errors(t *testing.T) {
	for code, want := range map[string]error{
		"authorization_pending": ErrAuthorizationPending,
		"slow_down":             ErrSlowDown,
		"expired_token":         ErrExpired,
		"access_denied":         ErrDenied,
	} {
		x := fakeXAI(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
		})
		if _, err := x.PollDeviceAuthorization(context.Background(), nil, "d"); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", code, err, want)
		}
	}
}

func TestXAIRefreshKeepsRefreshTokenWhenNotRotated(t *testing.T) {
	x := fakeXAI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" || r.Form.Get("client_id") != "c" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"a2","expires_in":60}`))
	})
	tok, err := x.Refresh(context.Background(), nil, "r1")
	if err != nil || tok.AccessToken != "a2" || tok.RefreshToken != "r1" {
		t.Fatalf("refresh: %+v %v", tok, err)
	}
}

func TestXAIRefreshReauthCases(t *testing.T) {
	for status, fragment := range map[int]string{
		http.StatusBadRequest: "Connect the subscription again",
		http.StatusForbidden:  "plan may not include API",
	} {
		x := fakeXAI(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token=SECRET revoked"}`))
		})
		_, err := x.Refresh(context.Background(), nil, "r1")
		var reauth *ReauthError
		if !errors.As(err, &reauth) || !strings.Contains(reauth.Reason, fragment) {
			t.Errorf("status %d: got %v", status, err)
		}
	}
}

func TestOAuthDetailNeverEchoesUnknownBodies(t *testing.T) {
	if got := oauthDetail([]byte(`{"access_token":"leak"}`)); strings.Contains(got, "leak") {
		t.Fatalf("leaked body: %q", got)
	}
	if got := oauthDetail([]byte(`<html>token=leak</html>`)); strings.Contains(got, "leak") {
		t.Fatalf("leaked body: %q", got)
	}
}

func TestRegistry(t *testing.T) {
	p, ok := Get("xai")
	if !ok || p.AdapterType() != "openai_compatible" || !strings.HasPrefix(p.InferenceBaseURL(), "https://") {
		t.Fatalf("xai provider: %v %v", p, ok)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	Register(&XAI{})
}
