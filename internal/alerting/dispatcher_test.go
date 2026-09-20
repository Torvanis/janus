package alerting

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

type fixture struct {
	db      *store.Store
	metrics *telemetry.Metrics
	admin   *store.User
	user    *store.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	admin, _, err := db.UpsertUserFromIdentity(ctx, "sub-admin", "admin@example.com", "Admin", true, false)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	user, _, err := db.UpsertUserFromIdentity(ctx, "sub-user", "user@example.com", "User", false, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &fixture{db: db, metrics: telemetry.New("test", "test"), admin: admin, user: user}
}

func (f *fixture) dispatcher(smtpCfg SMTPConfig) *Dispatcher {
	return New(f.db, smtpCfg, nil, f.metrics, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestDispatchWritesInAppNotificationsForUserAndAdmins(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d := f.dispatcher(SMTPConfig{})

	d.Dispatch(ctx, Event{
		Trigger: TriggerQuota80, Title: "Quota at 80%", Body: "You have used 80% of your daily tokens.",
		UserID: f.user.ID, NotifyAdmins: true,
	})

	userNotes, _, err := f.db.ListNotifications(ctx, f.user.ID, false, 10)
	if err != nil {
		t.Fatalf("list user notifications: %v", err)
	}
	if len(userNotes) != 1 {
		t.Fatalf("user has %d notifications, want 1", len(userNotes))
	}
	if userNotes[0].Title != "Quota at 80%" {
		t.Errorf("notification title = %q", userNotes[0].Title)
	}
	if userNotes[0].Severity != "warning" {
		t.Errorf("severity = %q, want the 'warning' default when the event has none", userNotes[0].Severity)
	}

	adminNotes, _, err := f.db.ListNotifications(ctx, f.admin.ID, false, 10)
	if err != nil {
		t.Fatalf("list admin notifications: %v", err)
	}
	if len(adminNotes) != 1 {
		t.Fatalf("admin has %d notifications, want 1 (NotifyAdmins fan-out)", len(adminNotes))
	}

	if got := testutil.ToFloat64(f.metrics.AlertDispatch.WithLabelValues("in_app", "ok")); got != 2 {
		t.Errorf("janus_alert_dispatch_total{channel=in_app,result=ok} = %v, want 2", got)
	}
}

func TestDispatchDeduplicatesAdminWhoIsAlsoTheSubject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d := f.dispatcher(SMTPConfig{})

	d.Dispatch(ctx, Event{
		Trigger: TriggerQuotaBreach, Title: "Quota breached", Body: "b",
		UserID: f.admin.ID, NotifyAdmins: true,
	})
	notes, _, err := f.db.ListNotifications(ctx, f.admin.ID, false, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("admin-as-subject received %d notifications, want exactly 1", len(notes))
	}
}

func TestDispatchRoutesWebhooksByTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var matched, unmatched, disabled atomic.Int64
	var lastPayload atomic.Value
	hook := func(counter *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			body, _ := io.ReadAll(r.Body)
			lastPayload.Store(body)
			w.WriteHeader(http.StatusOK)
		}
	}
	matchedSrv := httptest.NewServer(hook(&matched))
	defer matchedSrv.Close()
	unmatchedSrv := httptest.NewServer(hook(&unmatched))
	defer unmatchedSrv.Close()
	disabledSrv := httptest.NewServer(hook(&disabled))
	defer disabledSrv.Close()

	for _, rule := range []*store.AlertRule{
		{Trigger: TriggerQuotaBreach, Severity: "critical", Channels: []string{"webhook"}, WebhookURL: matchedSrv.URL, Enabled: true},
		{Trigger: TriggerUpstreamDown, Severity: "critical", Channels: []string{"webhook"}, WebhookURL: unmatchedSrv.URL, Enabled: true},
		{Trigger: TriggerQuotaBreach, Severity: "critical", Channels: []string{"webhook"}, WebhookURL: disabledSrv.URL, Enabled: false},
	} {
		if err := f.db.CreateAlertRule(ctx, rule); err != nil {
			t.Fatalf("create rule: %v", err)
		}
	}

	d := f.dispatcher(SMTPConfig{})
	d.Dispatch(ctx, Event{
		Trigger: TriggerQuotaBreach, Severity: "critical",
		Title: "Quota breached", Body: "hard stop", UserID: f.user.ID,
	})

	if matched.Load() != 1 {
		t.Errorf("matching rule delivered %d webhooks, want 1", matched.Load())
	}
	if unmatched.Load() != 0 {
		t.Errorf("rule for a different trigger delivered %d webhooks, want 0", unmatched.Load())
	}
	if disabled.Load() != 0 {
		t.Errorf("disabled rule delivered %d webhooks, want 0", disabled.Load())
	}

	var payload map[string]any
	if err := json.Unmarshal(lastPayload.Load().([]byte), &payload); err != nil {
		t.Fatalf("webhook payload is not JSON: %v", err)
	}
	if payload["trigger"] != TriggerQuotaBreach || payload["title"] != "Quota breached" {
		t.Errorf("webhook payload = %v, want trigger/title carried through", payload)
	}
	if got := testutil.ToFloat64(f.metrics.AlertDispatch.WithLabelValues("webhook", "ok")); got != 1 {
		t.Errorf("janus_alert_dispatch_total{channel=webhook,result=ok} = %v, want 1", got)
	}
}

func TestDispatchTriggerAllMatchesEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	if err := f.db.CreateAlertRule(ctx, &store.AlertRule{
		Trigger: "all", Severity: "info", Channels: []string{"webhook"}, WebhookURL: srv.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	d := f.dispatcher(SMTPConfig{})
	d.Dispatch(ctx, Event{Trigger: TriggerSystem, Title: "t", Body: "b", NotifyAdmins: true})
	d.Dispatch(ctx, Event{Trigger: TriggerQuota95, Title: "t", Body: "b", UserID: f.user.ID})
	if hits.Load() != 2 {
		t.Errorf("catch-all rule delivered %d webhooks, want 2", hits.Load())
	}
}

// TestEmailDeliveryIsBoundedByDeadline is the documented hang regression: alert
// emails are sent from post-response quota-record goroutines, and
// net/smtp.SendMail dials and converses with no deadline — a black-holed SMTP
// host (accepts the TCP connection, never sends the greeting) used to pin
// those goroutines forever. Delivery must now fail within the configured
// conversation timeout.
func TestEmailDeliveryIsBoundedByDeadline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A black-hole SMTP server: accepts and then says nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			// Never write the 220 greeting; just hold the connection open.
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	d := f.dispatcher(SMTPConfig{
		Host: "127.0.0.1", Port: port, From: "janus@example.com",
		Timeout: 250 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		d.sendEmail(ctx, map[string]bool{f.user.ID: true}, Event{Title: "t", Body: "b"})
		close(done)
	}()
	select {
	case <-done:
		// Returned — the deadline held.
	case <-time.After(5 * time.Second):
		t.Fatal("sendEmail still blocked after 5s against a black-holed SMTP server; the conversation deadline is not applied")
	}
	if got := testutil.ToFloat64(f.metrics.AlertDispatch.WithLabelValues("email", "error")); got != 1 {
		t.Errorf("email error metric = %v, want 1 (the bounded failure must be recorded)", got)
	}
}

func TestEmailChannelIsGatedOnSMTPConfig(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	d := f.dispatcher(SMTPConfig{})
	if d.EmailEnabled() {
		t.Error("EmailEnabled() = true with no SMTP host configured")
	}
	if !f.dispatcher(SMTPConfig{Host: "mail.example.com", Port: 587}).EmailEnabled() {
		t.Error("EmailEnabled() = false with an SMTP host configured")
	}

	// SendTest must report the misconfiguration instead of silently dropping.
	err := d.SendTest(ctx, &store.AlertRule{
		Trigger: TriggerQuotaBreach, Severity: "critical", Channels: []string{"email"},
	})
	if err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Errorf("SendTest over email without SMTP = %v, want an explicit SMTP error", err)
	}

	// Dispatch with an email rule and no SMTP: nothing is sent, nothing panics,
	// and no email delivery is recorded.
	if err := f.db.CreateAlertRule(ctx, &store.AlertRule{
		Trigger: TriggerQuotaBreach, Severity: "critical", Channels: []string{"email"}, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	d.Dispatch(ctx, Event{Trigger: TriggerQuotaBreach, Title: "t", Body: "b", UserID: f.user.ID})
	for _, result := range []string{"ok", "error"} {
		if got := testutil.ToFloat64(f.metrics.AlertDispatch.WithLabelValues("email", result)); got != 0 {
			t.Errorf("janus_alert_dispatch_total{channel=email,result=%s} = %v, want 0 while SMTP is unconfigured", result, got)
		}
	}
}

func TestSendTestDeliversWebhook(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	d := f.dispatcher(SMTPConfig{})
	if err := d.SendTest(ctx, &store.AlertRule{
		Trigger: TriggerSystem, Severity: "info", Channels: []string{"webhook", "in_app"}, WebhookURL: srv.URL,
	}); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("test webhook delivered %d times, want 1", hits.Load())
	}
	notes, _, err := f.db.ListNotifications(ctx, f.admin.ID, false, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notes) != 1 {
		t.Errorf("admin received %d in-app test notifications, want 1", len(notes))
	}

	// A webhook rule without a URL must fail loudly.
	if err := d.SendTest(ctx, &store.AlertRule{
		Trigger: TriggerSystem, Severity: "info", Channels: []string{"webhook"},
	}); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Errorf("SendTest without a webhook URL = %v, want an explicit webhook error", err)
	}
}
