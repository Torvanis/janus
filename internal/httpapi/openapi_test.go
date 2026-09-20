package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/telemetry"
)

// openapiSpecPath is the published spec the docs site offers for download.
const openapiSpecPath = "../../web/public/openapi.json"

func loadSpecPaths(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(openapiSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", openapiSpecPath, err)
	}
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", openapiSpecPath, err)
	}
	if len(spec.Paths) == 0 {
		t.Fatalf("%s documents no paths", openapiSpecPath)
	}
	out := map[string][]string{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
				out[path] = append(out[path], strings.ToUpper(method))
			}
		}
		sort.Strings(out[path])
	}
	return out
}

func specTestRouter(t *testing.T) *chi.Mux {
	t.Helper()
	s := &Server{
		Config:  &config.Config{},
		Metrics: telemetry.New("test", "test"),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	s.RateLimits = nil
	mux, ok := s.Handler().(*chi.Mux)
	if !ok {
		t.Fatal("Handler() no longer returns a *chi.Mux; update this test's route enumeration")
	}
	return mux
}

var pathParamPattern = regexp.MustCompile(`\{[^}]+\}`)

// TestOpenAPISpecMatchesRouter guards the published spec against router drift
// in both directions: everything documented must route, and every application
// API route must be documented. It exists because the spec is a maintained
// artifact, not generated output — without this test a handler change silently
// diverges from the contract the docs site publishes.
func TestOpenAPISpecMatchesRouter(t *testing.T) {
	specPaths := loadSpecPaths(t)
	mux := specTestRouter(t)

	// Direction 1: every documented operation resolves to a route.
	for path, methods := range specPaths {
		concrete := pathParamPattern.ReplaceAllString(path, "test-value")
		for _, method := range methods {
			rctx := chi.NewRouteContext()
			if !mux.Match(rctx, method, concrete) {
				t.Errorf("documented operation %s %s does not resolve in the router", method, path)
			}
		}
	}

	// Direction 2: every routed application-API endpoint is documented. The
	// proxy catch-all (/v1/*), SPA fallback, and operational endpoints are
	// checked in direction 1 via their documented forms.
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if !strings.HasPrefix(route, "/api/v1") {
			return nil
		}
		documented := false
		for _, m := range specPaths[route] {
			if m == method {
				documented = true
			}
		}
		if !documented {
			t.Errorf("router serves %s %s but %s does not document it", method, route, openapiSpecPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
}

// TestOpenAPISpecProxySurface pins the OpenAI-compatible surface: the four
// documented proxy operations must stay documented (SDK users download this
// spec), and the proxy catch-all must actually serve them.
func TestOpenAPISpecProxySurface(t *testing.T) {
	specPaths := loadSpecPaths(t)
	mux := specTestRouter(t)
	for path, method := range map[string]string{
		"/v1/chat/completions": http.MethodPost,
		"/v1/completions":      http.MethodPost,
		"/v1/embeddings":       http.MethodPost,
		"/v1/models":           http.MethodGet,
	} {
		found := false
		for _, m := range specPaths[path] {
			if m == method {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %s missing from %s", method, path, openapiSpecPath)
		}
		rctx := chi.NewRouteContext()
		if !mux.Match(rctx, method, path) {
			t.Errorf("proxy surface %s %s does not resolve in the router", method, path)
		}
	}
}
