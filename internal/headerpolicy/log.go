package headerpolicy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// LogMode is how much of a header the request log keeps.
type LogMode string

const (
	// LogValues keeps names and values, sensitive values masked.
	LogValues LogMode = "values"
	// LogNames keeps header names only.
	LogNames LogMode = "names"
	// LogOff keeps nothing.
	LogOff LogMode = "off"
)

// ValidLogMode reports whether m is a known mode.
func ValidLogMode(m LogMode) bool { return m == LogValues || m == LogNames || m == LogOff }

// sensitiveExact are headers whose values are credentials or session state.
var sensitiveExact = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "api-key": true, "x-goog-api-key": true, "x-janus-csrf": true,
	"cf-access-jwt-assertion": true, "cf-access-token": true, "cf-authorization": true,
	"x-amz-security-token": true, "chatgpt-account-id": true,
}

// sensitiveParts mark a header name as credential-bearing wherever they
// appear ("x-custom-token", "x-session-id", "x-hub-signature").
var sensitiveParts = []string{"token", "secret", "password", "passwd", "session", "signature", "credential", "apikey", "api-key", "auth", "jwt", "cookie"}

// Sensitive reports whether a header's value must never be logged.
func Sensitive(name string) bool {
	n := strings.ToLower(name)
	if sensitiveExact[n] {
		return true
	}
	for _, p := range sensitiveParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// Logged is one header as the request log stores it.
type Logged struct {
	Name   string   `json:"name"`
	Values []string `json:"values,omitempty"`
	Masked bool     `json:"masked,omitempty"`
}

// Per-request bounds so a hostile caller cannot bloat the log.
const (
	maxLoggedHeaders = 128
	maxLoggedValue   = 1024
)

// ForLog renders headers for the log. extraSecret names (rules marked
// secret) are masked as well. Returns nil for LogOff.
func ForLog(h http.Header, mode LogMode, extraSecret map[string]bool) []Logged {
	if mode == LogOff || h == nil {
		return nil
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]Logged, 0, len(names))
	for _, k := range names {
		if len(out) == maxLoggedHeaders {
			out = append(out, Logged{Name: fmt.Sprintf("(%d more headers not logged)", len(names)-maxLoggedHeaders)})
			break
		}
		l := Logged{Name: http.CanonicalHeaderKey(k)}
		if mode == LogValues {
			if Sensitive(k) || extraSecret[strings.ToLower(k)] {
				l.Masked = true
				for _, v := range h[k] {
					l.Values = append(l.Values, fmt.Sprintf("•••• (%d chars)", len(v)))
				}
			} else {
				for _, v := range h[k] {
					if len(v) > maxLoggedValue {
						v = v[:maxLoggedValue] + "…"
					}
					l.Values = append(l.Values, v)
				}
			}
		}
		out = append(out, l)
	}
	return out
}
