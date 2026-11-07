package headerpolicy

import (
	"net/http"
	"strings"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func edgePolicy() *Policy {
	p := &Policy{ID: "edge", Name: "edge"}
	for _, n := range EdgeHeaders {
		p.Rules = append(p.Rules, Rule{Action: ActionRemove, Name: n})
	}
	return p
}

// The live failure: a request through a Cloudflare tunnel carried
// Cf-Connecting-Ip (and an Access JWT) to OpenAI, whose edge refused it.
func TestEdgePolicyStripsTunnelHeaders(t *testing.T) {
	h := hdr(
		"Cf-Connecting-Ip", "203.0.113.9", "Cf-Ray", "8a1", "Cf-Access-Jwt-Assertion", "eyJ...",
		"X-Forwarded-For", "203.0.113.9", "X-Forwarded-Proto", "https", "Cdn-Loop", "cloudflare",
		"Via", "1.1 proxy", "X-Real-Ip", "203.0.113.9",
		"Content-Type", "application/json", "User-Agent", "example-client/1", "X-Request-Id", "abc",
		"Authorization", "Bearer caller-key",
	)
	changes := Apply(h, edgePolicy())
	for _, gone := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion", "X-Forwarded-For", "X-Forwarded-Proto", "Cdn-Loop", "Via", "X-Real-Ip"} {
		if h.Get(gone) != "" {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{"Content-Type", "User-Agent", "X-Request-Id"} {
		if h.Get(kept) == "" {
			t.Errorf("%s was removed", kept)
		}
	}
	// Reserved headers are the adapter's business, never the policy's.
	if h.Get("Authorization") == "" {
		t.Error("policy touched Authorization")
	}
	if len(changes) != 8 {
		t.Errorf("changes = %d, want 8: %+v", len(changes), changes)
	}
}

func TestSetAppendAndOrder(t *testing.T) {
	h := hdr("X-Team", "a", "User-Agent", "curl/8")
	Apply(h, &Policy{Rules: []Rule{
		{Action: ActionSet, Name: "User-Agent", Value: "Janus"},
		{Action: ActionAppend, Name: "X-Team", Value: "b"},
		{Action: ActionSet, Name: "X-Org", Value: "acme"},
		{Action: ActionRemove, Name: "x-org"}, // later rules see earlier ones
	}})
	if got := h.Get("User-Agent"); got != "Janus" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := strings.Join(h.Values("X-Team"), ","); got != "a,b" {
		t.Errorf("X-Team = %q", got)
	}
	if h.Get("X-Org") != "" {
		t.Error("remove after set did not apply")
	}
}

func TestAllowOnly(t *testing.T) {
	h := hdr("Content-Type", "application/json", "X-Trace-Id", "1", "X-Trace-Span", "2", "X-Secret-Internal", "z", "Authorization", "Bearer k")
	changes := Apply(h, &Policy{AllowOnly: []string{"content-type", "x-trace-*"}})
	if h.Get("X-Secret-Internal") != "" {
		t.Error("unlisted header kept")
	}
	if h.Get("X-Trace-Id") == "" || h.Get("X-Trace-Span") == "" || h.Get("Content-Type") == "" {
		t.Error("allowed header dropped")
	}
	if h.Get("Authorization") == "" {
		t.Error("allow-list dropped a reserved header")
	}
	if len(changes) != 1 || changes[0].Action != "not_allowed" {
		t.Errorf("changes = %+v", changes)
	}
}

func TestValidateRefusesReservedAndInjection(t *testing.T) {
	cases := map[string]Rule{
		"authorization": {Action: ActionSet, Name: "Authorization", Value: "x"},
		"host":          {Action: ActionRemove, Name: "host"},
		"crlf":          {Action: ActionSet, Name: "X-A", Value: "a\r\nX-Injected: 1"},
		"wildcard set":  {Action: ActionSet, Name: "x-*", Value: "a"},
		"bad name":      {Action: ActionSet, Name: "X A", Value: "a"},
		"bad action":    {Action: "rename", Name: "X-A"},
		"remove value":  {Action: ActionRemove, Name: "X-A", Value: "a"},
	}
	for name, r := range cases {
		if err := Validate(nil, []Rule{r}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Validate(nil, []Rule{{Action: ActionRemove, Name: "cf-*"}, {Action: ActionSet, Name: "X-Org", Value: "acme"}}); err != nil {
		t.Errorf("valid rules refused: %v", err)
	}
}

func TestForLogMasksSecrets(t *testing.T) {
	h := hdr("Authorization", "Bearer sk-live-123", "Cf-Access-Jwt-Assertion", "eyJabc", "X-Custom-Token", "t0k",
		"X-Org-Key", "plain-but-secret-rule", "User-Agent", "example-client/1")
	out := ForLog(h, LogValues, map[string]bool{"x-org-key": true})
	got := map[string]Logged{}
	for _, l := range out {
		got[l.Name] = l
	}
	for _, n := range []string{"Authorization", "Cf-Access-Jwt-Assertion", "X-Custom-Token", "X-Org-Key"} {
		l := got[n]
		if !l.Masked || strings.Contains(strings.Join(l.Values, ""), "sk-live") || strings.Contains(strings.Join(l.Values, ""), "eyJ") || strings.Contains(strings.Join(l.Values, ""), "t0k") || strings.Contains(strings.Join(l.Values, ""), "plain") {
			t.Errorf("%s not masked: %+v", n, l)
		}
	}
	if ua := got["User-Agent"]; ua.Masked || ua.Values[0] != "example-client/1" {
		t.Errorf("User-Agent = %+v", ua)
	}
	names := ForLog(h, LogNames, nil)
	for _, l := range names {
		if len(l.Values) != 0 {
			t.Errorf("names mode logged a value for %s", l.Name)
		}
	}
	if ForLog(h, LogOff, nil) != nil {
		t.Error("off mode logged")
	}
}
