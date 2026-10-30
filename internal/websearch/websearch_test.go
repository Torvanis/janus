package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchEachProvider(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/search") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"results":[{"title":"A","url":"https://a.test/1","content":"alpha"},{"title":"bad","url":"javascript:alert(1)","content":"x"}]}`))
		case r.URL.Path == "/brave":
			_, _ = w.Write([]byte(`{"web":{"results":[{"title":"<b>B</b>","url":"https://b.test","description":"be <strong>bold</strong>"}]}}`))
		case r.URL.Path == "/exa":
			_, _ = w.Write([]byte(`{"results":[{"title":"E","url":"https://e.test","highlights":["one","two"]}]}`))
		case r.URL.Path == "/tavily":
			_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://t.test","content":"tee"}]}`))
		}
	}))
	defer srv.Close()
	BraveURL, ExaURL, TavilyURL = srv.URL+"/brave", srv.URL+"/exa", srv.URL+"/tavily"
	ctx := context.Background()

	r, err := Search(ctx, srv.Client(), Config{Provider: "searxng", URL: srv.URL}, "news")
	if err != nil || len(r) != 1 || r[0].URL != "https://a.test/1" || got.URL.Query().Get("format") != "json" {
		t.Fatalf("searxng = %+v %v (%s)", r, err, got.URL)
	}
	r, err = Search(ctx, srv.Client(), Config{Provider: "brave", Key: "k1"}, "news")
	if err != nil || r[0].Title != "B" || r[0].Snippet != "be bold" || got.Header.Get("X-Subscription-Token") != "k1" {
		t.Fatalf("brave = %+v %v", r, err)
	}
	r, err = Search(ctx, srv.Client(), Config{Provider: "exa", Key: "k2", MaxResults: 3}, "news")
	var eb map[string]any
	_ = json.Unmarshal([]byte(body), &eb)
	if err != nil || r[0].Snippet != "one … two" || got.Header.Get("x-api-key") != "k2" || eb["numResults"] != float64(3) {
		t.Fatalf("exa = %+v %v %s", r, err, body)
	}
	r, err = Search(ctx, srv.Client(), Config{Provider: "tavily", Key: "k3"}, "news")
	if err != nil || r[0].Snippet != "tee" || got.Header.Get("Authorization") != "Bearer k3" {
		t.Fatalf("tavily = %+v %v", r, err)
	}
	if _, err := Search(ctx, srv.Client(), Config{Provider: "brave"}, "news"); err != ErrNotConfigured {
		t.Fatalf("brave without key: %v", err)
	}
	if !strings.Contains(FormatForModel("news", r), "[1] T\nhttps://t.test") {
		t.Fatalf("format = %s", FormatForModel("news", r))
	}
}

func TestSearchNamesTheProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	BraveURL = srv.URL
	_, err := Search(context.Background(), srv.Client(), Config{Provider: "brave", Key: "bad"}, "news")
	if err == nil || !strings.Contains(err.Error(), "Brave Search refused the API key") {
		t.Fatalf("err = %v", err)
	}
}
