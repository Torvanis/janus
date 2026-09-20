// Package authz evaluates access decisions: model grants and the policy
// blocking-rule layer.
//
// Blocking rules are POLICY controls, not security controls. Every signal they
// inspect except the bearer token is supplied by the client and can be spoofed.
// The admin UI states this prominently; this package does not pretend otherwise.
package authz

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/torvanis/janus/internal/store"
)

// RequestSignals is the evidence a rule may inspect.
type RequestSignals struct {
	UserAgent    string
	SourceIP     string
	ForwardedFor string
	Path         string
	Method       string
	Header       http.Header
	TokenPrefix  string
	Model        string
}

// SignalsFromRequest extracts rule inputs from an inbound request.
func SignalsFromRequest(r *http.Request, clientIP, tokenPrefix, model string) RequestSignals {
	return RequestSignals{
		UserAgent:    r.Header.Get("User-Agent"),
		SourceIP:     clientIP,
		ForwardedFor: r.Header.Get("X-Forwarded-For"),
		Path:         r.URL.Path,
		Method:       r.Method,
		Header:       r.Header,
		TokenPrefix:  tokenPrefix,
		Model:        model,
	}
}

// Match reports the first enabled rule that blocks the request.
func Match(rules []*store.BlockingRule, sig RequestSignals) (*store.BlockingRule, string) {
	for _, rule := range rules {
		if !rule.Enabled || len(rule.Clauses) == 0 {
			continue
		}
		matched, clause := evaluateRule(rule, sig)
		if matched {
			return rule, clause
		}
	}
	return nil, ""
}

func evaluateRule(rule *store.BlockingRule, sig RequestSignals) (bool, string) {
	firstMatch := ""
	for _, clause := range rule.Clauses {
		ok := matchClause(clause, sig)
		if clause.Negate {
			ok = !ok
		}
		switch strings.ToLower(rule.Combinator) {
		case "or":
			if ok {
				return true, describeClause(clause)
			}
		default: // AND
			if !ok {
				return false, ""
			}
			if firstMatch == "" {
				firstMatch = describeClause(clause)
			}
		}
	}
	if strings.EqualFold(rule.Combinator, "or") {
		return false, ""
	}
	return firstMatch != "", firstMatch
}

func describeClause(c store.RuleClause) string {
	if c.Type == store.ClauseHeader {
		return fmt.Sprintf("header %s matches %q", c.Header, c.Pattern)
	}
	return fmt.Sprintf("%s matches %q", c.Type, c.Pattern)
}

func matchClause(c store.RuleClause, sig RequestSignals) bool {
	switch c.Type {
	case store.ClauseUserAgent:
		return matchPattern(c.Pattern, sig.UserAgent)
	case store.ClauseSourceIP:
		return matchCIDR(c.Pattern, sig.SourceIP)
	case store.ClauseXFF:
		for _, ip := range strings.Split(sig.ForwardedFor, ",") {
			if matchCIDR(c.Pattern, strings.TrimSpace(ip)) {
				return true
			}
		}
		return false
	case store.ClauseEndpoint:
		return matchPattern(c.Pattern, sig.Path)
	case store.ClauseMethod:
		return strings.EqualFold(strings.TrimSpace(c.Pattern), sig.Method)
	case store.ClauseHeader:
		if c.Header == "" {
			return false
		}
		return matchPattern(c.Pattern, sig.Header.Get(c.Header))
	case store.ClauseTokenMatch:
		return matchPattern(c.Pattern, sig.TokenPrefix)
	case store.ClauseModelName:
		return matchPattern(c.Pattern, sig.Model)
	default:
		return false
	}
}

var (
	regexMu    sync.RWMutex
	regexCache = map[string]*regexp.Regexp{}
)

func matchPattern(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	re, err := compile(pattern)
	if err != nil {
		// An invalid stored pattern must never block traffic silently; fall back
		// to a case-insensitive substring test.
		return strings.Contains(strings.ToLower(value), strings.ToLower(pattern))
	}
	return re.MatchString(value)
}

func compile(pattern string) (*regexp.Regexp, error) {
	regexMu.RLock()
	re, ok := regexCache[pattern]
	regexMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, err
	}
	regexMu.Lock()
	regexCache[pattern] = re
	regexMu.Unlock()
	return re, nil
}

func matchCIDR(pattern, value string) bool {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return false
	}
	pattern = strings.TrimSpace(pattern)
	if !strings.Contains(pattern, "/") {
		return ip.Equal(net.ParseIP(pattern))
	}
	_, network, err := net.ParseCIDR(pattern)
	if err != nil {
		return false
	}
	return network.Contains(ip)
}

// ValidateClause checks a clause before it is stored so an admin sees the
// problem in the form rather than at enforcement time.
func ValidateClause(c store.RuleClause) error {
	if c.Pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	switch c.Type {
	case store.ClauseSourceIP, store.ClauseXFF:
		if strings.Contains(c.Pattern, "/") {
			if _, _, err := net.ParseCIDR(c.Pattern); err != nil {
				return fmt.Errorf("%q is not a valid CIDR block (example: 192.168.0.0/16)", c.Pattern)
			}
		} else if net.ParseIP(c.Pattern) == nil {
			return fmt.Errorf("%q is not a valid IP address or CIDR block", c.Pattern)
		}
	case store.ClauseUserAgent, store.ClauseEndpoint, store.ClauseTokenMatch, store.ClauseModelName:
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return fmt.Errorf("%q is not a valid regular expression: %v", c.Pattern, err)
		}
	case store.ClauseHeader:
		if c.Header == "" {
			return fmt.Errorf("a header name is required for header_match clauses")
		}
	case store.ClauseMethod:
		switch strings.ToUpper(c.Pattern) {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			return fmt.Errorf("%q is not an HTTP method", c.Pattern)
		}
	default:
		return fmt.Errorf("unknown rule type %q", c.Type)
	}
	return nil
}

// ClauseTypes lists the eight supported signals for the admin UI.
func ClauseTypes() []string {
	return []string{
		store.ClauseUserAgent, store.ClauseSourceIP, store.ClauseXFF, store.ClauseEndpoint,
		store.ClauseMethod, store.ClauseHeader, store.ClauseTokenMatch, store.ClauseModelName,
	}
}
