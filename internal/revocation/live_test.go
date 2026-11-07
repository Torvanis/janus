package revocation

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveRevocation dials real public hosts in hard mode. It runs only with
// JANUS_LIVE_REVOCATION=1 (CI stays hermetic):
//
//	JANUS_LIVE_REVOCATION=1 go test -run TestLiveRevocation -v ./internal/revocation
//
// JANUS_LIVE_GOOD (comma list) must pass; JANUS_LIVE_REVOKED (comma list,
// e.g. a CA's published "revoked" test site) must fail with ErrRevoked.
func TestLiveRevocation(t *testing.T) {
	if os.Getenv("JANUS_LIVE_REVOCATION") == "" {
		t.Skip("set JANUS_LIVE_REVOCATION=1 to dial real hosts")
	}
	restore := Configure(Options{Mode: ModeHard, Timeout: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	defer restore()
	dial := func(host string) error {
		d := &net.Dialer{Timeout: 15 * time.Second}
		c, err := tls.DialWithDialer(d, "tcp", host+":443", Wrap(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}))
		if err != nil {
			return err
		}
		return c.Close()
	}
	split := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	for _, h := range split(os.Getenv("JANUS_LIVE_GOOD")) {
		if err := dial(h); err != nil {
			t.Errorf("%s: good host refused: %v", h, err)
		} else {
			t.Logf("%s: ok", h)
		}
	}
	for _, h := range split(os.Getenv("JANUS_LIVE_REVOKED")) {
		err := dial(h)
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("%s: revoked host not refused as revoked: %v", h, err)
		} else {
			t.Logf("%s: refused: %v", h, err)
		}
	}
	t.Logf("stats: %+v", Current())
}
