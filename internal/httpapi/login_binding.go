package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
)

// A sign-in through an identity provider is finished by the browser that
// started it. The provider sends the browser back with a one-time "state"
// value; without a tie to the browser, anyone could start a sign-in, sign in
// as themselves at the provider, and hand the finished link to someone else,
// whose browser would then be signed in to the first person's account
// without knowing it (and what they then did there, prompts and tokens
// included, would belong to that account).
//
// When a sign-in starts, the browser gets a cookie derived from the state.
// The callback is accepted only from a browser that holds it.

// loginBindingCookiePrefix names the cookie; the rest of the name is the
// start of the value, so sign-ins started in two tabs do not replace each
// other's cookie.
const loginBindingCookiePrefix = "janus_login_"

// loginBindingTTL is how long a started sign-in can take to come back.
const loginBindingTTL = 15 * 60

func loginBinding(state string) (name, value string) {
	sum := sha256.Sum256([]byte("janus-login-binding\x00" + state))
	value = hex.EncodeToString(sum[:])
	return loginBindingCookiePrefix + value[:16], value
}

// bindLoginToBrowser sets the cookie for the sign-in that target (the
// identity provider's authorization URL) starts.
func (s *Server) bindLoginToBrowser(w http.ResponseWriter, target string) {
	u, err := url.Parse(target)
	if err != nil {
		return
	}
	state := u.Query().Get("state")
	if state == "" {
		return
	}
	name, value := loginBinding(state)
	// Lax: the provider's redirect back is a top-level navigation from
	// another site, which Lax allows and Strict would not.
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/auth", MaxAge: loginBindingTTL,
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// loginStartedHere reports whether this browser started the sign-in that
// state belongs to, and clears the cookie either way.
func (s *Server) loginStartedHere(w http.ResponseWriter, r *http.Request, state string) bool {
	name, want := loginBinding(state)
	c, err := r.Cookie(name)
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/auth", MaxAge: -1,
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode})
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// loginNotStartedHere is what the person sees.
const loginNotStartedHere = "This sign-in was not started in this browser, or it took too long. Start again from the sign-in page."
