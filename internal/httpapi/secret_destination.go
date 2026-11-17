package httpapi

import (
	"net/url"
	"strings"
)

// A stored credential (a provider key, a directory bind password, an
// identity provider's client secret, a storage secret key) is shown to no
// one: the API returns a mask, and an edit that leaves the field blank keeps
// what is stored. That left one way to read it. An administrator who never
// knew the credential could change only the address it is sent to, to a
// server of their own, and the gateway would deliver it there.
//
// So a stored credential stays with the server it was saved for. Changing
// the address to another server, or testing against another server, needs
// the credential entered again.

// sameDestination reports whether two addresses are the same server: the
// same scheme, host and port. The path may differ.
func sameDestination(stored, next string) bool {
	a, errA := url.Parse(strings.TrimSpace(stored))
	b, errB := url.Parse(strings.TrimSpace(next))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(hostPort(a), hostPort(b))
}

// hostPort is the host with its port, the scheme's default when none is given.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		case "ldaps":
			port = "636"
		case "ldap":
			port = "389"
		}
	}
	return u.Hostname() + ":" + port
}

// errSecretNeedsReentry is the refusal: what names the credential ("API
// key"), param its field.
func errSecretNeedsReentry(what, param string) *APIError {
	return ErrInvalidRequest("Enter the " + what + " again. The address changed to a different server, and a stored " + what +
		" is only ever sent to the server it was saved for.").WithParam(param)
}
