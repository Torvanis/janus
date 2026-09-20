package updates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDisabledNeverCalls(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	for _, o := range []Options{
		{Endpoint: srv.URL, Version: "1.0.0", Enabled: false},
		{Endpoint: srv.URL, Version: "1.0.0", Enabled: true, Offline: true},
	} {
		c := New(o)
		c.Run(context.Background()) // returns immediately when disabled
		r := c.Check(context.Background())
		if called || r.Enabled || c.Enabled() {
			t.Fatalf("disabled checker made a request or reports enabled: %+v", r)
		}
		if o.Offline && !r.Offline {
			t.Fatal("offline flag not reported")
		}
	}
}

func TestCheckSendsOnlyVersionEditionInstance(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"latest":"1.4.0","published":"2026-10-01T00:00:00Z","notes_url":"https://janusedge.com/releases/1.4.0","advisory":"","min_supported":"1.2.0","update_available":true,"unsupported":false}`))
	}))
	defer srv.Close()
	c := New(Options{Endpoint: srv.URL, Version: "1.3.0", Edition: func() string { return "business" }, InstanceID: "inst-1", Enabled: true})
	r := c.Check(context.Background())
	if got == nil {
		t.Fatal("no request")
	}
	q := got.URL.Query()
	if q.Get("version") != "1.3.0" || q.Get("edition") != "business" || len(q) != 2 {
		t.Fatalf("query leaked more than version+edition: %v", q)
	}
	if got.Header.Get("X-Janus-Instance") != "inst-1" {
		t.Fatalf("instance header: %q", got.Header.Get("X-Janus-Instance"))
	}
	if r.Error != "" || !r.UpdateAvailable || r.Latest != "1.4.0" || r.MinSupported != "1.2.0" || r.CheckedAt == nil {
		t.Fatalf("result: %+v", r)
	}
	if c.Last().Latest != "1.4.0" {
		t.Fatal("result not cached")
	}
}

func TestCheckRecordsFailureQuietly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	c := New(Options{Endpoint: srv.URL, Version: "1.3.0", Enabled: true})
	r := c.Check(context.Background())
	if r.Error == "" || r.UpdateAvailable || r.CheckedAt == nil {
		t.Fatalf("expected recorded error, got %+v", r)
	}
	// Unreachable host: still just an error, never a panic or block.
	c2 := New(Options{Endpoint: "http://127.0.0.1:1", Version: "1.3.0", Enabled: true})
	if r := c2.Check(context.Background()); r.Error == "" {
		t.Fatal("expected connection error")
	}
}

func TestNoReleasesYet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"latest":null,"update_available":false}`))
	}))
	defer srv.Close()
	c := New(Options{Endpoint: srv.URL, Version: "1.3.0", Enabled: true})
	r := c.Check(context.Background())
	if r.Error != "" || r.Latest != "" || r.UpdateAvailable {
		t.Fatalf("result: %+v", r)
	}
}
