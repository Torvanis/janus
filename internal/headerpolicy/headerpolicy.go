// Package headerpolicy rewrites the caller's request headers before Janus
// forwards a request to an upstream provider, and masks header values for
// the request log.
//
// A policy is applied to the CALLER's headers, before the adapter builds the
// provider request. The adapter's own headers (credentials, provider identity
// headers, request signatures) are added afterwards and always win, so a
// policy can never break authentication or a SigV4 signature. Names the
// gateway manages itself are refused at validation time (Reserved).
package headerpolicy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Action is what a rule does to a header.
type Action string

const (
	// ActionRemove deletes every header whose name matches.
	ActionRemove Action = "remove"
	// ActionSet replaces the header with one value.
	ActionSet Action = "set"
	// ActionAppend adds one more value, keeping any the caller sent.
	ActionAppend Action = "append"
)

// Rule is one ordered step of a policy.
type Rule struct {
	Action Action `json:"action"`
	// Name is a header name, or a prefix ending in "*" ("cf-*") for remove.
	Name string `json:"name"`
	// Value is used by set and append.
	Value string `json:"value,omitempty"`
	// Secret values are stored encrypted, never returned by the API and
	// always masked in the request log.
	Secret bool `json:"secret,omitempty"`
}

// Policy is the compiled form the proxy applies.
type Policy struct {
	ID   string
	Name string
	// AllowOnly, when non-empty, drops every caller header not matching one
	// of these names/prefixes BEFORE the rules run.
	AllowOnly []string
	Rules     []Rule
}

// Change records one effect of a policy on one request.
type Change struct {
	Action string `json:"action"` // removed | set | appended | not_allowed
	Name   string `json:"name"`
	// Rule is the 1-based rule that caused it (0 for the allow-list).
	Rule int `json:"rule"`
}

// reserved are headers Janus or the HTTP stack owns. A rule naming them
// would either be overwritten silently or break the request.
var reserved = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"authorization": true, "proxy-authorization": true, "cookie": true, "x-janus-csrf": true,
	"accept-encoding": true, "x-api-key": true, "api-key": true, "x-goog-api-key": true,
	"te": true, "upgrade": true, "keep-alive": true, "content-encoding": true,
}

// Reserved reports whether name is managed by Janus and cannot be targeted.
func Reserved(name string) bool { return reserved[strings.ToLower(strings.TrimSpace(name))] }

// validName accepts RFC 7230 token characters, plus a trailing "*".
func validName(n string, allowWildcard bool) bool {
	if n == "" {
		return false
	}
	if strings.HasSuffix(n, "*") {
		if !allowWildcard {
			return false
		}
		n = strings.TrimSuffix(n, "*")
		if n == "" {
			return false
		}
	}
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}

// FieldError is a validation failure on one rule.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// MaxRules bounds a policy so a request never pays for an unbounded loop.
const MaxRules = 100

// Validate checks rules and the allow-list.
func Validate(allowOnly []string, rules []Rule) error {
	if len(rules) > MaxRules {
		return &FieldError{"rules", fmt.Sprintf("A policy can have at most %d rules.", MaxRules)}
	}
	for i, n := range allowOnly {
		if !validName(n, true) {
			return &FieldError{"allow_only", fmt.Sprintf("Allowed header %d (%q) is not a valid header name. Use a name like x-request-id, or a prefix like x-trace-*.", i+1, n)}
		}
	}
	for i, r := range rules {
		f := fmt.Sprintf("rules[%d]", i)
		switch r.Action {
		case ActionRemove, ActionSet, ActionAppend:
		default:
			return &FieldError{f, fmt.Sprintf("Rule %d: action must be remove, set or append.", i+1)}
		}
		if !validName(r.Name, r.Action == ActionRemove) {
			if strings.HasSuffix(r.Name, "*") {
				return &FieldError{f, fmt.Sprintf("Rule %d: a prefix (name ending in *) can only be used to remove headers.", i+1)}
			}
			return &FieldError{f, fmt.Sprintf("Rule %d: %q is not a valid header name.", i+1, r.Name)}
		}
		if Reserved(strings.TrimSuffix(r.Name, "*")) {
			return &FieldError{f, fmt.Sprintf("Rule %d: %s is managed by Janus (credentials and transport) and cannot be changed by a policy.", i+1, r.Name)}
		}
		if r.Action != ActionRemove {
			if strings.ContainsAny(r.Value, "\r\n\x00") {
				return &FieldError{f, fmt.Sprintf("Rule %d: the value cannot contain line breaks.", i+1)}
			}
			if len(r.Value) > 8192 {
				return &FieldError{f, fmt.Sprintf("Rule %d: the value is longer than 8 KB.", i+1)}
			}
		} else if r.Value != "" {
			return &FieldError{f, fmt.Sprintf("Rule %d: remove takes no value.", i+1)}
		}
	}
	return nil
}

func matches(pattern, name string) bool {
	p := strings.ToLower(pattern)
	n := strings.ToLower(name)
	if strings.HasSuffix(p, "*") {
		return strings.HasPrefix(n, strings.TrimSuffix(p, "*"))
	}
	return p == n
}

// Apply rewrites h in place and returns what changed. A nil policy is a
// no-op. Reserved headers are never touched, even by the allow-list: the
// adapter strips or replaces them itself.
func Apply(h http.Header, p *Policy) []Change {
	if p == nil || h == nil {
		return nil
	}
	var changes []Change
	if len(p.AllowOnly) > 0 {
		for _, name := range sortedNames(h) {
			if Reserved(name) {
				continue
			}
			allowed := false
			for _, a := range p.AllowOnly {
				if matches(a, name) {
					allowed = true
					break
				}
			}
			if !allowed {
				h.Del(name)
				changes = append(changes, Change{Action: "not_allowed", Name: canon(name), Rule: 0})
			}
		}
	}
	for i, r := range p.Rules {
		switch r.Action {
		case ActionRemove:
			for _, name := range sortedNames(h) {
				if !Reserved(name) && matches(r.Name, name) {
					h.Del(name)
					changes = append(changes, Change{Action: "removed", Name: canon(name), Rule: i + 1})
				}
			}
		case ActionSet:
			h.Set(r.Name, r.Value)
			changes = append(changes, Change{Action: "set", Name: canon(r.Name), Rule: i + 1})
		case ActionAppend:
			h.Add(r.Name, r.Value)
			changes = append(changes, Change{Action: "appended", Name: canon(r.Name), Rule: i + 1})
		}
	}
	return changes
}

func canon(n string) string { return http.CanonicalHeaderKey(n) }

func sortedNames(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EdgeHeaders is the built-in "strip edge & proxy headers" rule set: what a
// CDN, tunnel or reverse proxy in front of Janus adds about the CALLER. It
// identifies the caller's network and, for Cloudflare Access, carries a
// signed identity token; none of it is meant for a model provider, and some
// providers' own edges refuse requests that carry it.
var EdgeHeaders = []string{
	"cf-*", "cdn-loop", "x-forwarded-*", "forwarded", "x-real-ip",
	"true-client-ip", "x-client-ip", "x-cluster-client-ip", "fastly-*", "akamai-*", "x-envoy-*", "x-amzn-trace-id", "via",
}
