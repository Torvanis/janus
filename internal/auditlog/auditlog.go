// Package auditlog forwards audit records to a syslog server or SIEM
// (Administration → Settings → Security → Audit forwarding).
//
// Records are queued (bounded; a full queue drops and counts, it never
// blocks a request) and sent by one goroutine over UDP, TCP (RFC 6587
// octet counting) or TLS (RFC 5425), formatted as RFC 5424 with structured
// data or as ArcSight CEF. The TLS connection verifies the server against
// the outbound trust store and the revocation checker, and can present a
// client certificate from mounted files.
package auditlog

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/torvanis/janus/internal/revocation"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/truststore"
)

// QueueSize bounds records waiting to be sent.
const QueueSize = 10000

// Forwarder sends audit records to the configured destination.
type Forwarder struct {
	logger   *slog.Logger
	policy   func(ctx context.Context) store.SyslogPolicy
	hostname string
	version  string
	q        chan store.AuditEntry

	sent, dropped, failed atomic.Int64
	lastErr               atomic.Value // string
	lastErrAt             atomic.Value // time.Time
	lastSentAt            atomic.Value // time.Time

	mu   sync.Mutex
	conn net.Conn
	key  string // the configuration conn was opened for
}

// Status is shown on the settings card and in system status.
type Status struct {
	Enabled     bool      `json:"enabled"`
	Destination string    `json:"destination,omitempty"`
	Sent        int64     `json:"sent"`
	Dropped     int64     `json:"dropped"`
	Failed      int64     `json:"failed"`
	Queued      int       `json:"queued"`
	LastSentAt  time.Time `json:"last_sent_at,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// New builds a forwarder. policy is read for every record, so a settings
// change applies within the policy cache's lifetime.
func New(logger *slog.Logger, hostname, version string, policy func(ctx context.Context) store.SyslogPolicy) *Forwarder {
	if logger == nil {
		logger = slog.Default()
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &Forwarder{logger: logger, policy: policy, hostname: sanitizeHeader(hostname), version: version, q: make(chan store.AuditEntry, QueueSize)}
}

// Enqueue hands one committed record to the forwarder. It never blocks.
func (f *Forwarder) Enqueue(e store.AuditEntry) {
	if f == nil {
		return
	}
	select {
	case f.q <- e:
	default:
		if f.dropped.Add(1)%1000 == 1 {
			f.logger.Error("audit forwarding queue is full; records dropped", "dropped", f.dropped.Load())
		}
	}
}

// Run sends queued records until ctx ends.
func (f *Forwarder) Run(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			f.closeConn()
			return
		case e := <-f.q:
			cfg := f.policy(ctx)
			if !cfg.Enabled {
				continue // forwarding is off: records stay only in the database
			}
			for attempt := 0; ; attempt++ {
				err := f.send(ctx, cfg, e)
				if err == nil {
					backoff = time.Second
					break
				}
				f.noteError(err)
				if attempt >= 2 || ctx.Err() != nil {
					f.failed.Add(1)
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
			}
		}
	}
}

// Status reports counters and the last error.
func (f *Forwarder) Status() Status {
	if f == nil {
		return Status{}
	}
	cfg := f.policy(context.Background())
	st := Status{Enabled: cfg.Enabled, Sent: f.sent.Load(), Dropped: f.dropped.Load(), Failed: f.failed.Load(), Queued: len(f.q)}
	if cfg.Enabled {
		st.Destination = strings.ToUpper(cfg.Protocol) + " " + cfg.Address
	}
	if v, ok := f.lastErr.Load().(string); ok {
		st.LastError = v
	}
	if v, ok := f.lastErrAt.Load().(time.Time); ok {
		st.LastErrorAt = v
	}
	if v, ok := f.lastSentAt.Load().(time.Time); ok {
		st.LastSentAt = v
	}
	return st
}

// Test sends one record to cfg on a fresh connection and reports the
// result (for TCP and TLS: the connection and write succeeded; UDP has no
// acknowledgment, so only local errors show).
func (f *Forwarder) Test(ctx context.Context, cfg store.SyslogPolicy, e store.AuditEntry) error {
	conn, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(frame(cfg, Format(cfg, f.hostname, f.version, e)))
	if err != nil {
		return fmt.Errorf("send to %s: %w", cfg.Address, err)
	}
	return nil
}

func (f *Forwarder) noteError(err error) {
	f.lastErr.Store(err.Error())
	f.lastErrAt.Store(time.Now().UTC())
	f.logger.Warn("audit forwarding failed", "error", err.Error())
	f.closeConn()
}

func (f *Forwarder) closeConn() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		_ = f.conn.Close()
		f.conn = nil
	}
}

func configKey(c store.SyslogPolicy) string {
	return strings.Join([]string{c.Protocol, c.Address, c.ServerName, c.ClientCertFile, c.ClientKeyFile, strconv.FormatUint(truststore.Generation(), 10)}, "|")
}

func (f *Forwarder) send(ctx context.Context, cfg store.SyslogPolicy, e store.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := configKey(cfg)
	if f.conn != nil && f.key != key {
		_ = f.conn.Close()
		f.conn = nil
	}
	if f.conn == nil {
		c, err := dial(ctx, cfg)
		if err != nil {
			return err
		}
		f.conn, f.key = c, key
	}
	_ = f.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := f.conn.Write(frame(cfg, Format(cfg, f.hostname, f.version, e))); err != nil {
		_ = f.conn.Close()
		f.conn = nil
		return fmt.Errorf("send to %s: %w", cfg.Address, err)
	}
	f.sent.Add(1)
	f.lastSentAt.Store(time.Now().UTC())
	return nil
}

func dial(ctx context.Context, cfg store.SyslogPolicy) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	switch cfg.Protocol {
	case "udp":
		c, err := d.DialContext(ctx, "udp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("connect to %s over UDP: %w", cfg.Address, err)
		}
		return c, nil
	case "tcp":
		c, err := d.DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("connect to %s over TCP: %w", cfg.Address, err)
		}
		return c, nil
	case "tls":
		host, _, err := net.SplitHostPort(cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("syslog address %q: %w", cfg.Address, err)
		}
		tc := truststore.ClientConfig()
		if tc == nil {
			tc = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tc = tc.Clone()
		tc.ServerName = host
		if cfg.ServerName != "" {
			tc.ServerName = cfg.ServerName
		}
		if cfg.ClientCertFile != "" {
			pair, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load the syslog client certificate: %w", err)
			}
			tc.Certificates = []tls.Certificate{pair}
		}
		tc = revocation.Wrap(tc)
		c, err := (&tls.Dialer{NetDialer: d, Config: tc}).DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			var cv *tls.CertificateVerificationError
			if errors.As(err, &cv) {
				return nil, fmt.Errorf("the syslog server's certificate is not trusted by this deployment (add its CA under Settings → Certificates): %w", err)
			}
			return nil, fmt.Errorf("connect to %s over TLS: %w", cfg.Address, err)
		}
		return c, nil
	}
	return nil, fmt.Errorf("unknown syslog protocol %q", cfg.Protocol)
}

// frame adds TCP/TLS octet counting (RFC 6587 / RFC 5425); UDP sends one
// message per datagram.
func frame(cfg store.SyslogPolicy, msg string) []byte {
	if cfg.Protocol == "udp" {
		return []byte(msg)
	}
	return []byte(strconv.Itoa(len(msg)) + " " + msg)
}

// severity: failures are warnings (4), everything else notice (5).
func severity(e store.AuditEntry) int {
	if e.Outcome == store.AuditFailure {
		return 4
	}
	return 5
}

// Format renders one record.
func Format(cfg store.SyslogPolicy, hostname, version string, e store.AuditEntry) string {
	app := sanitizeHeader(firstNonEmpty(cfg.AppName, "janus"))
	ts := e.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	outcome := firstNonEmpty(e.Outcome, store.AuditSuccess)
	if cfg.Format == "cef" {
		// CEF:Version|Vendor|Product|Version|SignatureID|Name|Severity|Extension
		sev := 3
		if outcome == store.AuditFailure {
			sev = 6
		}
		ext := []string{
			"rt=" + strconv.FormatInt(ts.UnixMilli(), 10),
			"act=" + cefExt(e.Action),
			"outcome=" + cefExt(outcome),
			"suser=" + cefExt(e.ActorLabel),
			"suid=" + cefExt(e.ActorUserID),
			"src=" + cefExt(e.SourceIP),
			"cs1Label=resourceType", "cs1=" + cefExt(e.ResourceType),
			"cs2Label=resourceId", "cs2=" + cefExt(e.ResourceID),
			"cs3Label=detail", "cs3=" + cefExt(truncate(e.NewValue, 2000)),
			"externalId=" + cefExt(e.ID),
			"dvchost=" + cefExt(hostname),
		}
		body := fmt.Sprintf("CEF:0|Janus|%s|%s|%s|%s|%d|%s", cefHdr(app), cefHdr(firstNonEmpty(version, "dev")), cefHdr(e.Action), cefHdr(e.Action), sev, strings.Join(ext, " "))
		return fmt.Sprintf("<%d>1 %s %s %s - %s - %s", cfg.Facility*8+severity(e), ts.UTC().Format(time.RFC3339Nano), hostname, app, "audit", body)
	}
	sd := fmt.Sprintf(`[janus@32473 id="%s" action="%s" outcome="%s" actor="%s" actorId="%s" ip="%s" resourceType="%s" resourceId="%s"]`,
		sdEsc(e.ID), sdEsc(e.Action), sdEsc(outcome), sdEsc(e.ActorLabel), sdEsc(e.ActorUserID), sdEsc(e.SourceIP), sdEsc(e.ResourceType), sdEsc(e.ResourceID))
	msg := e.Action
	if e.ActorLabel != "" {
		msg += " by " + e.ActorLabel
	}
	if e.SourceIP != "" {
		msg += " from " + e.SourceIP
	}
	if e.NewValue != "" {
		msg += " " + truncate(e.NewValue, 2000)
	}
	// PRI VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG
	return fmt.Sprintf("<%d>1 %s %s %s - %s %s %s", cfg.Facility*8+severity(e), ts.UTC().Format(time.RFC3339Nano), hostname, app, "audit", sd, sanitizeMsg(msg))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sanitizeHeader keeps RFC 5424 header fields printable ASCII without spaces.
func sanitizeHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 32 && r < 127 {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "-"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func sanitizeMsg(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func sdEsc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`, "\n", " ", "\r", " ").Replace(s)
}

func cefHdr(s string) string {
	return strings.NewReplacer(`\`, `\\`, `|`, `\|`, "\n", " ", "\r", " ").Replace(s)
}

func cefExt(s string) string {
	return strings.NewReplacer(`\`, `\\`, `=`, `\=`, "\n", `\n`, "\r", `\r`).Replace(s)
}
