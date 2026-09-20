package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSCIMEmailVerifiedClaims(t *testing.T) {
	p := &OIDCProvider{cfg: OIDCConfig{Claims: ClaimMapping{Email: "mail"}}}
	for _, tc := range []struct {
		name     string
		email    any
		verified any
		want     bool
	}{
		{"absent", "alice@example.com", nil, false},
		{"false", "alice@example.com", false, false},
		{"string true", "alice@example.com", "true", false},
		{"true", "alice@example.com", true, true},
		{"fallback", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := p.mapClaims(map[string]any{"sub": "subject", "mail": tc.email, "email_verified": tc.verified})
			if id.EmailVerified != tc.want {
				t.Fatalf("EmailVerified = %v, want %v", id.EmailVerified, tc.want)
			}
		})
	}
	if !DevIdentity("alice@example.com", "Alice", nil).EmailVerified {
		t.Fatal("development identity should explicitly bypass email verification")
	}
}

func TestSCIMUserInfoEmailVerifiedDoesNotCarryOver(t *testing.T) {
	for _, verified := range []any{nil, false, "true", true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": "subject", "email": "new@example.com", "email_verified": verified})
		}))
		p := &OIDCProvider{cfg: OIDCConfig{Claims: ClaimMapping{Email: "email"}}, meta: ProviderMetadata{UserInfoURL: srv.URL}, client: srv.Client()}
		id := &Identity{Subject: "subject", Email: "old@example.com", EmailVerified: true}
		p.enrichFromUserInfo(context.Background(), "access", id)
		want, _ := verified.(bool)
		if id.Email != "new@example.com" || id.EmailVerified != want {
			t.Errorf("verification %v: got %+v", verified, id)
		}
		srv.Close()
	}
}

func TestSCIMUserInfoRequiresMatchingSubject(t *testing.T) {
	for _, sub := range []string{"", "other-sub"} {
		t.Run(sub, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"sub": sub, "email": "victim@example.com", "email_verified": true, "groups": []string{"admins"}})
			}))
			defer srv.Close()
			p := &OIDCProvider{cfg: OIDCConfig{Claims: ClaimMapping{Email: "email", Groups: "groups"}}, meta: ProviderMetadata{UserInfoURL: srv.URL}, client: srv.Client()}
			id := &Identity{Subject: "original-sub", Email: "original@example.com"}
			before := *id
			p.enrichFromUserInfo(context.Background(), "access", id)
			if !reflect.DeepEqual(*id, before) {
				t.Fatalf("unbound userinfo changed identity: %+v", id)
			}
		})
	}
}
