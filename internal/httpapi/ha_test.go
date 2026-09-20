package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/jobs"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
)

// secondReplica builds another Server over the same database, with its own
// process-local state (session cache, pending logins if any). Requests that
// start on one and finish on the other are what "two replicas behind one
// Service" looks like in production.
func secondReplica(t *testing.T, h *harness) *harness {
	t.Helper()
	b := &harness{t: t, store: h.store, upstream: h.upstream}
	b.server = &Server{
		Config: h.server.Config, Store: h.store,
		Sessions: auth.NewDBSessionStore(h.store, time.Hour, time.Hour),
		Cipher:   h.server.Cipher, Quota: h.server.Quota, Metrics: h.server.Metrics, Alerts: h.server.Alerts,
		Discovery: h.server.Discovery, Logger: h.server.Logger, StartedAt: time.Now(),
	}
	b.handler = b.server.Handler()
	return b
}

// TestHATOTPStepSpansReplicas: password step on replica A, code step on
// replica B. Before pending logins lived in the database this was
// "pending_expired" every time the load balancer switched pods mid-login.
func TestHATOTPStepSpansReplicas(t *testing.T) {
	a := newHarness(t)
	a.server.Sessions = auth.NewDBSessionStore(a.store, time.Hour, time.Hour)
	a.handler = a.server.Handler()
	b := secondReplica(t, a)
	ctx := context.Background()

	u, err := a.store.CreateLocalUser(ctx, "alice@example.com", "Alice", "alice-password-12", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.StageTOTP(ctx, a.server.Cipher, u.ID, "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.ConfirmTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	step1 := a.jsonPost("/auth/local/login", map[string]any{"email": "alice@example.com", "password": "alice-password-12", "redirect_to": "/models"}, nil)
	body := decode(t, step1)
	if step1.Code != 200 || body["totp_required"] != true {
		t.Fatalf("step1 on A: %d %s", step1.Code, step1.Body)
	}
	pending := body["pending"].(string)

	// Wrong code on B counts against the same token.
	if r := b.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": "000000"}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code on B: %d %s", r.Code, r.Body)
	}
	code, _ := totp.GenerateCode("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", time.Now())
	step2 := b.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": code, "redirect_to": "/models"}, nil)
	if step2.Code != 200 || decode(t, step2)["signed_in"] != true {
		t.Fatalf("step2 on B: %d %s", step2.Code, step2.Body)
	}
	cookies := step2.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie from B")
	}
	// …and the session B issued is honoured by A (DB-backed sessions).
	if r := a.withSession(http.MethodGet, "/api/v1/me", nil, cookies); r.Code != 200 || decode(t, r)["email"] != "alice@example.com" {
		t.Fatalf("session from B on A: %d %s", r.Code, r.Body)
	}
	// Token consumed: replaying step2 on A fails.
	if r := a.jsonPost("/auth/local/totp", map[string]any{"pending": pending, "code": code}, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", r.Code)
	}
}

// TestPendingLoginAttemptsExhaust: five wrong codes burn the token even
// when they land on different replicas.
func TestPendingLoginAttemptsExhaust(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.PutPendingLogin(ctx, "tok", "u1", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.PendingLoginMaxAttempts; i++ {
		if _, ok, _ := h.store.TakePendingLogin(ctx, "tok", false); !ok {
			t.Fatalf("attempt %d refused early", i+1)
		}
	}
	if _, ok, _ := h.store.TakePendingLogin(ctx, "tok", false); ok {
		t.Fatal("token survived max attempts")
	}
	if err := h.store.PutPendingLogin(ctx, "old", "u1", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.store.TakePendingLogin(ctx, "old", false); ok {
		t.Fatal("expired token accepted")
	}
}

// TestHeartbeatWarnsOverLicensedNodes: the warning fires on the transition
// only, and never changes the rate-limiter divisor or refuses anything.
func TestHeartbeatWarnsOverLicensedNodes(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	// Two other "replicas" already heartbeating.
	now := time.Now().UTC()
	for _, id := range []string{"pod-a", "pod-b"} {
		if _, err := h.store.HeartbeatInstance(ctx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	rl := quota.NewRateLimiter()
	var wg sync.WaitGroup
	jobs.StartInstanceHeartbeat(ctx, &wg, h.store, rl, "pod-c", logger, jobs.WithLicensedNodes(func() int { return 1 }))
	if rl.Replicas() != 3 {
		t.Fatalf("replicas = %d", rl.Replicas())
	}
	out := logs.String()
	if !bytes.Contains([]byte(out), []byte("more gateway replicas than the license covers")) || !bytes.Contains([]byte(out), []byte("licensed_nodes=1")) {
		t.Fatalf("no over-limit warning: %s", out)
	}
	if bytes.Contains([]byte(out), []byte("level=ERROR")) {
		t.Fatalf("over-limit must not be an error: %s", out)
	}
	cancel()
	wg.Wait()
}
