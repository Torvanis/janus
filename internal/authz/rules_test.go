package authz

import (
	"net/http"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

func rule(name, combinator string, clauses ...store.RuleClause) *store.BlockingRule {
	return &store.BlockingRule{ID: name, Name: name, Combinator: combinator, Enabled: true, Clauses: clauses, Reason: "blocked by " + name}
}

func baseSignals() RequestSignals {
	h := http.Header{}
	h.Set("X-Client-Tool", "hermes")
	return RequestSignals{
		UserAgent: "hermes/2.1", SourceIP: "10.1.2.3", ForwardedFor: "203.0.113.9, 10.1.2.3",
		Path: "/v1/chat/completions", Method: "POST", Header: h,
		TokenPrefix: "janus_abc123", Model: "gpt-4o-mini",
	}
}

func TestMatchEachSignalType(t *testing.T) {
	tests := []struct {
		name   string
		clause store.RuleClause
		want   bool
	}{
		{"user agent matches", store.RuleClause{Type: store.ClauseUserAgent, Pattern: "hermes"}, true},
		{"user agent does not match", store.RuleClause{Type: store.ClauseUserAgent, Pattern: "openclaw"}, false},
		{"source ip in cidr", store.RuleClause{Type: store.ClauseSourceIP, Pattern: "10.0.0.0/8"}, true},
		{"source ip outside cidr", store.RuleClause{Type: store.ClauseSourceIP, Pattern: "192.168.0.0/16"}, false},
		{"forwarded-for chain scanned", store.RuleClause{Type: store.ClauseXFF, Pattern: "203.0.113.0/24"}, true},
		{"endpoint regex", store.RuleClause{Type: store.ClauseEndpoint, Pattern: "^/v1/chat/.*"}, true},
		{"http method", store.RuleClause{Type: store.ClauseMethod, Pattern: "post"}, true},
		{"http method mismatch", store.RuleClause{Type: store.ClauseMethod, Pattern: "GET"}, false},
		{"header match", store.RuleClause{Type: store.ClauseHeader, Header: "X-Client-Tool", Pattern: "hermes"}, true},
		{"header absent", store.RuleClause{Type: store.ClauseHeader, Header: "X-Missing", Pattern: "anything"}, false},
		{"token prefix", store.RuleClause{Type: store.ClauseTokenMatch, Pattern: "^janus_abc"}, true},
		{"model name", store.RuleClause{Type: store.ClauseModelName, Pattern: "gpt-4o"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, _ := Match([]*store.BlockingRule{rule("r", "and", tc.clause)}, baseSignals())
			if (matched != nil) != tc.want {
				t.Fatalf("clause %+v matched=%v, want %v", tc.clause, matched != nil, tc.want)
			}
		})
	}
}

func TestAndCombinatorRequiresEveryClause(t *testing.T) {
	both := rule("both", "and",
		store.RuleClause{Type: store.ClauseUserAgent, Pattern: "hermes"},
		store.RuleClause{Type: store.ClauseSourceIP, Pattern: "10.0.0.0/8"},
	)
	if matched, _ := Match([]*store.BlockingRule{both}, baseSignals()); matched == nil {
		t.Fatal("AND rule with both clauses satisfied should block")
	}

	partial := rule("partial", "and",
		store.RuleClause{Type: store.ClauseUserAgent, Pattern: "hermes"},
		store.RuleClause{Type: store.ClauseSourceIP, Pattern: "192.168.0.0/16"},
	)
	if matched, _ := Match([]*store.BlockingRule{partial}, baseSignals()); matched != nil {
		t.Fatal("AND rule with one unsatisfied clause must not block")
	}
}

func TestOrCombinatorNeedsOneClause(t *testing.T) {
	r := rule("either", "or",
		store.RuleClause{Type: store.ClauseUserAgent, Pattern: "openclaw"},
		store.RuleClause{Type: store.ClauseModelName, Pattern: "gpt-4o"},
	)
	matched, clause := Match([]*store.BlockingRule{r}, baseSignals())
	if matched == nil {
		t.Fatal("OR rule with one satisfied clause should block")
	}
	if clause == "" {
		t.Fatal("the matching clause should be reported for the admin UI")
	}
}

func TestDisabledRuleIsIgnored(t *testing.T) {
	r := rule("off", "and", store.RuleClause{Type: store.ClauseUserAgent, Pattern: "hermes"})
	r.Enabled = false
	if matched, _ := Match([]*store.BlockingRule{r}, baseSignals()); matched != nil {
		t.Fatal("a disabled rule must never block traffic")
	}
}

func TestNegatedClauseAllowlistsATool(t *testing.T) {
	// "Block everything that is NOT hermes" — the allowlist shape admins ask for.
	r := rule("only-hermes", "and", store.RuleClause{Type: store.ClauseUserAgent, Pattern: "hermes", Negate: true})
	if matched, _ := Match([]*store.BlockingRule{r}, baseSignals()); matched != nil {
		t.Fatal("hermes should be allowed through a negated hermes clause")
	}

	other := baseSignals()
	other.UserAgent = "openclaw/0.9"
	if matched, _ := Match([]*store.BlockingRule{r}, other); matched == nil {
		t.Fatal("a non-hermes client should be blocked by the negated clause")
	}
}

func TestValidateClause(t *testing.T) {
	valid := []store.RuleClause{
		{Type: store.ClauseUserAgent, Pattern: ".*bot.*"},
		{Type: store.ClauseSourceIP, Pattern: "192.168.0.0/16"},
		{Type: store.ClauseSourceIP, Pattern: "10.1.2.3"},
		{Type: store.ClauseMethod, Pattern: "POST"},
		{Type: store.ClauseHeader, Header: "X-Tool", Pattern: "hermes"},
	}
	for _, c := range valid {
		if err := ValidateClause(c); err != nil {
			t.Errorf("ValidateClause(%+v) returned %v, want nil", c, err)
		}
	}

	invalid := []store.RuleClause{
		{Type: store.ClauseUserAgent, Pattern: "("},
		{Type: store.ClauseSourceIP, Pattern: "not-an-ip"},
		{Type: store.ClauseMethod, Pattern: "TELEPORT"},
		{Type: store.ClauseHeader, Pattern: "value"},
		{Type: "nonsense", Pattern: "x"},
		{Type: store.ClauseUserAgent},
	}
	for _, c := range invalid {
		if err := ValidateClause(c); err == nil {
			t.Errorf("ValidateClause(%+v) returned nil, want a validation error", c)
		}
	}
}

func TestClauseTypesCoversEightSignals(t *testing.T) {
	if got := len(ClauseTypes()); got != 8 {
		t.Fatalf("ClauseTypes() has %d entries, want the 8 documented signals", got)
	}
}
