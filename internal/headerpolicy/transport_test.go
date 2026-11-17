package headerpolicy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestApplyOwn(t *testing.T) {
	p := &Policy{AllowOnly: []string{"x-only"}, Rules: []Rule{
		{Action: ActionRemove, Name: "cf-*"},
		{Action: ActionSet, Name: "CF-Access-Client-Id", Value: "id"},
		{Action: ActionSet, Name: "User-Agent", Value: "first"},
		{Action: ActionSet, Name: "User-Agent", Value: "second"},
		{Action: ActionAppend, Name: "X-Tag", Value: "a"},
		{Action: ActionAppend, Name: "X-Tag", Value: "b"},
		{Action: ActionSet, Name: "Content-Type", Value: "text/plain"},
		{Action: ActionSet, Name: "Anthropic-Version", Value: "wrong"},
		{Action: ActionSet, Name: "Authorization", Value: "Bearer stolen"}, // refused at validation; never applied either way
	}}
	h := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer key"}, "Anthropic-Version": {"2023-06-01"}}
	if !ApplyOwn(h, p) {
		t.Fatal("nothing added")
	}
	want := map[string][]string{
		"Cf-Access-Client-Id": {"id"}, "User-Agent": {"second"}, "X-Tag": {"a", "b"},
		// What Janus set itself is kept, and nothing is removed or filtered.
		"Content-Type": {"application/json"}, "Authorization": {"Bearer key"}, "Anthropic-Version": {"2023-06-01"},
	}
	if len(h) != len(want) {
		t.Errorf("headers: %v", h)
	}
	for k, v := range want {
		if got := h.Values(k); len(got) != len(v) || got[0] != v[0] || got[len(got)-1] != v[len(v)-1] {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	if ApplyOwn(http.Header{}, &Policy{Rules: []Rule{{Action: ActionRemove, Name: "cf-*"}}}) || ApplyOwn(http.Header{}, nil) || ApplyOwn(nil, p) {
		t.Error("a policy that only removes, or no policy, must add nothing")
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		u, base string
		want    bool
	}{
		{"https://m.example.com/v1/models", "https://m.example.com/v1/systemone", true},
		{"https://m.example.com:443/x", "https://M.Example.com", true},
		{"http://m.example.com/x", "https://m.example.com", false},
		{"https://m.example.com:8443/x", "https://m.example.com", false},
		{"https://evil.example.com/x", "https://m.example.com", false},
		{"https://m.example.com.evil.net/x", "https://m.example.com", false},
		{"https://m.example.com/x", "", false},
		{"https://m.example.com/x", "://bad", false},
	} {
		u, _ := url.Parse(tc.u)
		if got := SameOrigin(u, tc.base); got != tc.want {
			t.Errorf("%s vs %s = %v", tc.u, tc.base, got)
		}
	}
}

func TestUpstreamTransport(t *testing.T) {
	var got http.Header
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() })
	upstream, other := httptest.NewServer(record), httptest.NewServer(record)
	defer upstream.Close()
	defer other.Close()
	policy := &Policy{Rules: []Rule{{Action: ActionSet, Name: "CF-Access-Client-Secret", Value: "s3cret"}}}
	calls := 0
	client := Client(&http.Client{}, upstream.URL+"/v1", func(context.Context) *Policy { calls++; return policy })

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("CF-Access-Client-Secret") != "s3cret" || got.Get("Authorization") != "Bearer key" {
		t.Errorf("the upstream received %v", got)
	}
	if req.Header.Get("CF-Access-Client-Secret") != "" {
		t.Error("the caller's request was changed")
	}

	// Another host never receives the upstream's headers, and the policy is not even read for it.
	before := calls
	req, _ = http.NewRequest(http.MethodGet, other.URL+"/token", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("CF-Access-Client-Secret") != "" || calls != before {
		t.Errorf("another host received %v (policy read %d times)", got, calls-before)
	}

	// A redirect from the upstream to another host does not carry them either.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landed", http.StatusFound)
	}))
	defer redirecting.Close()
	client = Client(&http.Client{}, redirecting.URL, func(context.Context) *Policy { return policy })
	resp, err = client.Get(redirecting.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("CF-Access-Client-Secret") != "" {
		t.Errorf("a redirect carried the secret to another host: %v", got)
	}

	// No policy: the request goes out as it was.
	client = Client(&http.Client{}, upstream.URL, func(context.Context) *Policy { return nil })
	resp, err = client.Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("CF-Access-Client-Secret") != "" {
		t.Errorf("headers without a policy: %v", got)
	}
}

// A provider called at several hosts: the policy goes to the ones it owns
// and to no other.
func TestClientForSendsOnlyToOwnedURLs(t *testing.T) {
	got := map[string]string{}
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got[name] = r.Header.Get("X-Gate") })
	}
	signIn, api, other := httptest.NewServer(handler("sign-in")), httptest.NewServer(handler("api")), httptest.NewServer(handler("other"))
	defer signIn.Close()
	defer api.Close()
	defer other.Close()
	policy := func(context.Context) *Policy {
		return &Policy{Rules: []Rule{{Action: ActionSet, Name: "X-Gate", Value: "token"}}}
	}
	c := ClientFor(nil, func(u *url.URL) bool { return SameOrigin(u, signIn.URL) || SameOrigin(u, api.URL) }, policy)
	for _, target := range []string{signIn.URL, api.URL, other.URL} {
		resp, err := c.Get(target + "/x")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got["sign-in"] != "token" || got["api"] != "token" || got["other"] != "" {
		t.Errorf("X-Gate seen: %v", got)
	}
	// No allow rule: nothing is anyone's own.
	got = map[string]string{}
	resp, err := ClientFor(nil, nil, policy).Get(api.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got["api"] != "" {
		t.Errorf("no allow rule must send nothing: %v", got)
	}
}
