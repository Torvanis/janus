package safehttp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Regression tests for the 2026-11 security review (INFRA-06): the clients
// that call model providers followed any redirect to any host, taking the
// provider key headers and, on a 307, the prompt along.

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (s *seen) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Clone(r.Context()))
	s.body = append(s.body, string(b))
	s.mu.Unlock()
	_, _ = w.Write([]byte("collected"))
}

func call(t *testing.T, client *http.Client, method, url string) (*http.Response, error) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"prompt":"private text"}`)
	}
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("x-api-key", "sk-provider")
	req.Header.Set("x-goog-api-key", "goog-provider")
	req.Header.Set("Authorization", "Bearer provider")
	req.Header.Set("Accept", "application/json")
	return client.Do(req)
}

func TestRedirectsDoNotCarryCredentialsToAnotherHost(t *testing.T) {
	other := &seen{}
	collector := httptest.NewServer(http.HandlerFunc(other.handler))
	defer collector.Close()
	same := &seen{}
	mux := http.NewServeMux()
	provider := httptest.NewServer(mux)
	defer provider.Close()
	mux.HandleFunc("/away307", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+"/x", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/away302", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+"/x", http.StatusFound)
	})
	mux.HandleFunc("/here", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landed", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/landed", same.handler)

	fixed := false
	client := &http.Client{CheckRedirect: CheckRedirect(func(h http.Header) { fixed = true; h.Del("Authorization") })}

	// A POST redirected (307: method and body kept) to another host is not sent.
	if _, err := call(t, client, http.MethodPost, provider.URL+"/away307"); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("cross-host POST redirect: err = %v, want refused", err)
	}
	if len(other.reqs) != 0 {
		t.Fatalf("the other host received the request: %v %q", other.reqs[0].Header, other.body[0])
	}
	// A GET to another host (a download link) is followed without the keys.
	resp, err := call(t, client, http.MethodGet, provider.URL+"/away302")
	if err != nil {
		t.Fatalf("cross-host GET redirect: %v", err)
	}
	_ = resp.Body.Close()
	if len(other.reqs) != 1 {
		t.Fatalf("download redirect was not followed (%d requests)", len(other.reqs))
	}
	got := other.reqs[0].Header
	for _, name := range []string{"x-api-key", "x-goog-api-key", "Authorization"} {
		if got.Get(name) != "" {
			t.Errorf("%s went to the other host: %q", name, got.Get(name))
		}
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept was dropped: %v", got)
	}
	// A POST that a 302 turns into a GET carries no body and no keys.
	other.reqs, other.body = nil, nil
	if resp, err = call(t, client, http.MethodPost, provider.URL+"/away302"); err != nil {
		t.Fatalf("302 after POST: %v", err)
	}
	_ = resp.Body.Close()
	if len(other.reqs) != 1 || other.body[0] != "" || other.reqs[0].Header.Get("x-api-key") != "" {
		t.Fatalf("302 after POST leaked: %v %q", other.reqs, other.body)
	}
	// The same host: followed as sent, with the adapter's adjustment.
	if resp, err = call(t, client, http.MethodPost, provider.URL+"/here"); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
	_ = resp.Body.Close()
	if len(same.reqs) != 1 || same.reqs[0].Header.Get("x-api-key") != "sk-provider" || same.body[0] == "" || !fixed || same.reqs[0].Header.Get("Authorization") != "" {
		t.Fatalf("same-host redirect: %d requests, fixed=%v", len(same.reqs), fixed)
	}
}

func TestRedirectFromHTTPSToHTTPIsRefused(t *testing.T) {
	plain := &seen{}
	collector := httptest.NewServer(http.HandlerFunc(plain.handler))
	defer collector.Close()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL+"/x", http.StatusFound)
	}))
	defer provider.Close()
	client := provider.Client()
	client.CheckRedirect = CheckRedirect(nil)
	if _, err := call(t, client, http.MethodGet, provider.URL+"/"); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("https to http: err = %v, want refused", err)
	}
	if len(plain.reqs) != 0 {
		t.Fatal("the plain-HTTP host received the request")
	}
}
