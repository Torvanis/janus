package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/revocation"
)

// TestLiveUpstreamRevocation proves the proxy's real upstream client carries
// the revocation check: built exactly as for model traffic, hard mode, it
// reaches a good public host and refuses a revoked one. Runs only with
// JANUS_LIVE_REVOCATION=1.
func TestLiveUpstreamRevocation(t *testing.T) {
	if os.Getenv("JANUS_LIVE_REVOCATION") == "" {
		t.Skip("set JANUS_LIVE_REVOCATION=1 to dial real hosts")
	}
	restore := revocation.Configure(revocation.Options{Mode: revocation.ModeHard, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer restore()
	s := &Server{Config: &config.Config{TLSRevocation: "hard"}}
	client := s.buildUpstreamClient(config.UpstreamTimeouts{Connect: 10 * time.Second, TTFB: 20 * time.Second, Total: 30 * time.Second})
	get := func(url string) error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	if err := get("https://api.openai.com/v1/models"); err != nil {
		t.Fatalf("good upstream refused: %v", err)
	}
	if err := get("https://revoked-isrgrootx1.letsencrypt.org/"); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("revoked upstream not refused: %v", err)
	}
	t.Logf("upstream client: openai ok, revoked host refused; stats %+v", revocation.Current())
}
