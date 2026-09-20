// Package alerting fans quota and system events out to the configured
// channels. In-app delivery is always on; email requires SMTP configuration and
// webhooks require a URL on the matching alert rule.
package alerting

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
)

// Event is one alert-worthy occurrence.
type Event struct {
	Trigger      string
	Severity     string
	Title        string
	Body         string
	UserID       string
	NotifyAdmins bool
	Data         map[string]any
}

// Triggers understood by the dispatcher.
const (
	TriggerQuota80      = "quota_80"
	TriggerQuota95      = "quota_95"
	TriggerQuotaBreach  = "quota_breach"
	TriggerUpstreamDown = "upstream_down"
	TriggerSystem       = "system"
)

// SMTPConfig holds outbound email settings; an empty Host disables email.
type SMTPConfig struct {
	Host string
	Port int
	User string
	Pass string
	From string
	// Timeout bounds the entire SMTP conversation (dial through QUIT).
	// Zero means the 15-second default. Alert emails are sent from
	// post-response goroutines, so an unresponsive server must never pin
	// them indefinitely.
	Timeout time.Duration
}

// timeout returns the effective conversation bound.
func (c SMTPConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Second
}

// Configured reports whether email delivery is possible.
func (c SMTPConfig) Configured() bool { return c.Host != "" }

// Dispatcher delivers events to in-app, email, and webhook channels.
type Dispatcher struct {
	store   *store.Store
	smtp    SMTPConfig
	client  *http.Client
	metrics *telemetry.Metrics
	logger  *slog.Logger
}

// New builds a dispatcher.
func New(s *store.Store, smtpCfg SMTPConfig, client *http.Client, metrics *telemetry.Metrics, logger *slog.Logger) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Dispatcher{store: s, smtp: smtpCfg, client: client, metrics: metrics, logger: logger}
}

// EmailEnabled reports whether SMTP is configured, so the admin UI can explain
// why the email channel is unavailable instead of silently dropping messages.
func (d *Dispatcher) EmailEnabled() bool { return d.smtp.Configured() }

// Dispatch delivers one event. In-app notifications are written first and
// synchronously so the inbox is authoritative even if an external channel fails.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	recipients := map[string]bool{}
	if ev.UserID != "" {
		recipients[ev.UserID] = true
	}
	if ev.NotifyAdmins {
		admins, err := d.store.AdminUserIDs(ctx)
		if err != nil {
			d.logger.ErrorContext(ctx, "resolve alert recipients", "error", err.Error())
		}
		for _, id := range admins {
			recipients[id] = true
		}
	}
	severity := ev.Severity
	if severity == "" {
		severity = "warning"
	}
	for userID := range recipients {
		if err := d.store.CreateNotification(ctx, &store.Notification{
			UserID: userID, Severity: severity, Title: ev.Title, Body: ev.Body,
		}); err != nil {
			d.logger.ErrorContext(ctx, "write in-app notification", "error", err.Error())
			d.metrics.AlertDispatch.WithLabelValues("in_app", "error").Inc()
			continue
		}
		d.metrics.AlertDispatch.WithLabelValues("in_app", "ok").Inc()
	}

	rules, err := d.store.ListAlertRules(ctx)
	if err != nil {
		d.logger.ErrorContext(ctx, "load alert rules", "error", err.Error())
		return
	}
	for _, rule := range rules {
		if !rule.Enabled || (rule.Trigger != ev.Trigger && rule.Trigger != "all") {
			continue
		}
		for _, channel := range rule.Channels {
			switch strings.TrimSpace(channel) {
			case "webhook":
				d.sendWebhook(ctx, rule.WebhookURL, ev)
			case "email":
				d.sendEmail(ctx, recipients, ev)
			}
		}
	}
}

// SendTest delivers a synthetic event through one rule so an admin can verify
// wiring without waiting for a real breach.
func (d *Dispatcher) SendTest(ctx context.Context, rule *store.AlertRule) error {
	ev := Event{
		Trigger:  rule.Trigger,
		Severity: rule.Severity,
		Title:    "Janus test alert",
		Body:     "This is a test alert dispatched from the Janus admin panel. No action is required.",
		Data:     map[string]any{"test": true},
	}
	var failures []string
	for _, channel := range rule.Channels {
		switch strings.TrimSpace(channel) {
		case "webhook":
			if err := d.postWebhook(ctx, rule.WebhookURL, ev); err != nil {
				failures = append(failures, "webhook: "+err.Error())
			}
		case "email":
			if !d.smtp.Configured() {
				failures = append(failures, "email: SMTP is not configured (JANUS_SMTP_HOST is unset)")
			}
		case "in_app":
			admins, err := d.store.AdminUserIDs(ctx)
			if err != nil {
				failures = append(failures, "in_app: "+err.Error())
				continue
			}
			for _, id := range admins {
				if err := d.store.CreateNotification(ctx, &store.Notification{
					UserID: id, Severity: ev.Severity, Title: ev.Title, Body: ev.Body,
				}); err != nil {
					failures = append(failures, "in_app: "+err.Error())
				}
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func (d *Dispatcher) sendWebhook(ctx context.Context, url string, ev Event) {
	if err := d.postWebhook(ctx, url, ev); err != nil {
		d.logger.ErrorContext(ctx, "deliver webhook alert", "error", err.Error())
		d.metrics.AlertDispatch.WithLabelValues("webhook", "error").Inc()
		return
	}
	d.metrics.AlertDispatch.WithLabelValues("webhook", "ok").Inc()
}

func (d *Dispatcher) postWebhook(ctx context.Context, url string, ev Event) error {
	if url == "" {
		return fmt.Errorf("no webhook URL is configured on this rule")
	}
	payload := map[string]any{
		"trigger": ev.Trigger, "severity": ev.Severity, "title": ev.Title,
		"text": ev.Title + " — " + ev.Body, "body": ev.Body, "data": ev.Data,
		"sent_at": time.Now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (d *Dispatcher) sendEmail(ctx context.Context, recipients map[string]bool, ev Event) {
	if !d.smtp.Configured() {
		return
	}
	addresses := []string{}
	for id := range recipients {
		if u, err := d.store.UserByID(ctx, id); err == nil && u.Email != "" && strings.Contains(u.Email, "@") {
			addresses = append(addresses, u.Email)
		}
	}
	if len(addresses) == 0 {
		return
	}
	msg := []byte("From: " + d.smtp.From + "\r\n" +
		"To: " + strings.Join(addresses, ", ") + "\r\n" +
		"Subject: [Janus] " + ev.Title + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + ev.Body + "\r\n")

	addr := net.JoinHostPort(d.smtp.Host, fmt.Sprintf("%d", d.smtp.Port))
	var auth smtp.Auth
	if d.smtp.User != "" {
		auth = smtp.PlainAuth("", d.smtp.User, d.smtp.Pass, d.smtp.Host)
	}
	if err := sendMailWithDeadline(addr, d.smtp, auth, addresses, msg); err != nil {
		d.logger.ErrorContext(ctx, "deliver email alert", "error", err.Error())
		d.metrics.AlertDispatch.WithLabelValues("email", "error").Inc()
		return
	}
	d.metrics.AlertDispatch.WithLabelValues("email", "ok").Inc()
}

// sendMailWithDeadline is smtp.SendMail with an explicit dial timeout and a
// deadline covering the whole conversation. net/smtp.SendMail accepts no
// context and sets no deadlines, so a black-holed SMTP host would pin the
// calling goroutine forever (alert emails run on post-response quota-record
// goroutines tracked by Server.pending).
func sendMailWithDeadline(addr string, cfg SMTPConfig, auth smtp.Auth, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, cfg.timeout())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(cfg.timeout())); err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp server %s does not advertise AUTH but credentials are configured", addr)
		} else if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
