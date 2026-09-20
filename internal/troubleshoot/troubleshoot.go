// Package troubleshoot implements troubleshooting mode: on-demand capture of
// proxied request/response payloads for requests that match an
// administrator-defined filter, kept under an explicit retention policy and
// downloadable as tar.gz archives.
//
// The proxy never stores bodies on its own. While a troubleshooting session
// is active, the proxy wraps the response writer in a CapturingWriter (a
// bounded copy of what the client receives) and, once the usage event has
// been written, hands everything to Recorder.Capture, which applies the
// session's post-response filter (status, error code, token sizes) and
// persists a capture through the session's storage backend.
package troubleshoot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Cipher is the subset of crypto.Cipher the recorder needs for encryption at
// rest. It is an interface so tests can run without a key.
type Cipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(envelope string) (string, error)
}

// Recorder owns capture persistence for one gateway process.
type Recorder struct {
	Store  *store.Store
	Cipher Cipher
	// Dir is the disk backend root (JANUS_TROUBLESHOOT_DIR); empty disables
	// the disk backend.
	Dir    string
	Logger *slog.Logger

	mu      sync.Mutex
	lastRun time.Time
}

// New builds a recorder. A nil logger falls back to slog.Default.
func New(s *store.Store, cipher Cipher, dir string, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{Store: s, Cipher: cipher, Dir: strings.TrimSpace(dir), Logger: logger}
}

// DiskAvailable reports whether the disk backend may be chosen.
func (r *Recorder) DiskAvailable() bool { return r != nil && r.Dir != "" }

// EncryptionAvailable reports whether bodies can be encrypted at rest.
func (r *Recorder) EncryptionAvailable() bool { return r != nil && r.Cipher != nil }

// redactedHeaders are never captured: they carry credentials or session
// material that a troubleshooting archive must not leak, whatever the
// storage backend.
var redactedHeaders = map[string]bool{
	"authorization":        true,
	"proxy-authorization":  true,
	"cookie":               true,
	"set-cookie":           true,
	"x-api-key":            true,
	"api-key":              true,
	"x-goog-api-key":       true,
	"x-amz-security-token": true,
}

// RedactHeaders flattens an http.Header into a JSON-friendly map with
// credential-bearing headers replaced by a marker, so the archive shows the
// header was present without exposing its value.
func RedactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if redactedHeaders[strings.ToLower(name)] {
			out[name] = "[redacted]"
			continue
		}
		out[name] = strings.Join(values, ", ")
	}
	return out
}

func encodeHeaders(h http.Header) string {
	b, _ := json.Marshal(RedactHeaders(h))
	return string(b)
}

// Payload is everything the proxy hands to Capture for one request.
type Payload struct {
	RequestHeaders  http.Header
	RequestBody     []byte
	ResponseHeaders http.Header
	ResponseBody    []byte
	// Truncated reports that at least one body exceeded the capture limit
	// and was cut.
	Truncated bool
}

// Capture applies the session's post-response filter to the completed event
// and, on a match, persists a capture. It never returns an error to the
// proxy path: failures are logged, because troubleshooting must not affect
// the traffic it observes.
func (r *Recorder) Capture(ctx context.Context, session *store.TroubleshootingSession, event *store.UsageEvent, p Payload) {
	if r == nil || session == nil || event == nil || event.ID == "" {
		return
	}
	if !session.Active(time.Now().UTC()) || !session.Config.Filter.Matches(event) {
		return
	}
	cfg := session.Config
	if cfg.Encrypt && r.Cipher == nil {
		// The admin asked for encryption at rest; storing in the clear
		// instead would silently violate that instruction.
		r.Logger.ErrorContext(ctx, "troubleshooting: encryption requested but no cipher configured; capture dropped", "request_id", event.RequestID)
		return
	}
	c := &store.TroubleshootingCapture{
		SessionID: session.ID, UsageEventID: event.ID, RequestID: event.RequestID,
		UserID: event.UserID, ServiceTokenID: event.ServiceTokenID, ModelName: event.ModelName, UpstreamID: event.UpstreamID,
		EndpointPath: event.EndpointPath, HTTPStatus: event.HTTPStatus, ErrorCode: event.ErrorCode,
		TokensIn: event.TokensIn, TokensOut: event.TokensOut, Streaming: event.Streaming,
		RequestHeaders: encodeHeaders(p.RequestHeaders), ResponseHeaders: encodeHeaders(p.ResponseHeaders),
		StorageBackend: cfg.Storage, Truncated: p.Truncated, Encrypted: cfg.Encrypt,
	}
	reqBody, respBody := p.RequestBody, p.ResponseBody
	if !cfg.CaptureRequestBody {
		reqBody = nil
	}
	if !cfg.CaptureResponseBody {
		respBody = nil
	}
	reqBody, respBody, cut := clampBodies(reqBody, respBody, cfg.MaxBodyBytes)
	c.Truncated = c.Truncated || cut
	c.RequestBodyBytes, c.ResponseBodyBytes = int64(len(reqBody)), int64(len(respBody))
	c.SizeBytes = c.RequestBodyBytes + c.ResponseBodyBytes + int64(len(c.RequestHeaders)+len(c.ResponseHeaders))

	encReq, err := r.encodeBody(reqBody, c.Encrypted)
	if err != nil {
		r.Logger.ErrorContext(ctx, "troubleshooting: encode request body", "error", err.Error(), "request_id", event.RequestID)
		return
	}
	encResp, err := r.encodeBody(respBody, c.Encrypted)
	if err != nil {
		r.Logger.ErrorContext(ctx, "troubleshooting: encode response body", "error", err.Error(), "request_id", event.RequestID)
		return
	}
	c.ID = store.NewID()
	switch cfg.Storage {
	case store.TroubleshootStorageDisk:
		if !r.DiskAvailable() {
			r.Logger.ErrorContext(ctx, "troubleshooting: disk backend selected but JANUS_TROUBLESHOOT_DIR is unset; capture dropped", "request_id", event.RequestID)
			return
		}
		ref, err := r.writeDisk(c.ID, encReq, encResp)
		if err != nil {
			r.Logger.ErrorContext(ctx, "troubleshooting: write capture to disk", "error", err.Error(), "request_id", event.RequestID)
			return
		}
		c.StorageRef = ref
	default:
		c.StorageBackend = store.TroubleshootStorageDatabase
		c.RequestBody, c.ResponseBody = encReq, encResp
	}
	if err := r.Store.InsertTroubleshootingCapture(ctx, c); err != nil {
		r.Logger.ErrorContext(ctx, "troubleshooting: persist capture", "error", err.Error(), "request_id", event.RequestID)
		if c.StorageRef != "" {
			r.removeDisk(c.StorageRef)
		}
	}
}

func clampBodies(req, resp []byte, limit int64) ([]byte, []byte, bool) {
	if limit <= 0 {
		limit = store.TroubleshootDefaultMaxBodyBytes
	}
	cut := false
	if int64(len(req)) > limit {
		req, cut = req[:limit], true
	}
	if int64(len(resp)) > limit {
		resp, cut = resp[:limit], true
	}
	return req, resp, cut
}

// encodeBody produces the stored form of a body: base64, optionally wrapped
// in the cipher envelope. Empty bodies encode to "".
func (r *Recorder) encodeBody(body []byte, encrypt bool) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	if !encrypt {
		return encoded, nil
	}
	if r.Cipher == nil {
		return "", errors.New("encryption requested but no cipher configured")
	}
	return r.Cipher.Encrypt(encoded)
}

func (r *Recorder) decodeBody(stored string, encrypted bool) ([]byte, error) {
	if stored == "" {
		return nil, nil
	}
	if encrypted {
		if r.Cipher == nil {
			return nil, errors.New("capture is encrypted but no cipher is configured")
		}
		plain, err := r.Cipher.Decrypt(stored)
		if err != nil {
			return nil, fmt.Errorf("decrypt capture body: %w", err)
		}
		stored = plain
	}
	return base64.StdEncoding.DecodeString(stored)
}

// Disk layout: <Dir>/<yyyy-mm-dd>/<capture-id>/{request,response}.body, the
// day directory keeps listings manageable and makes an operator's manual
// cleanup by date trivial. storage_ref holds the path relative to Dir.
func (r *Recorder) writeDisk(captureID, encReq, encResp string) (string, error) {
	rel := filepath.Join(time.Now().UTC().Format("2006-01-02"), captureID)
	dir := filepath.Join(r.Dir, rel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "request.body"), []byte(encReq), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "response.body"), []byte(encResp), 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func (r *Recorder) readDisk(ref string) (string, string, error) {
	dir := filepath.Join(r.Dir, filepath.Clean("/"+ref)) // Clean("/"+ref) refuses traversal
	req, err := os.ReadFile(filepath.Join(dir, "request.body"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	resp, err := os.ReadFile(filepath.Join(dir, "response.body"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	return string(req), string(resp), nil
}

func (r *Recorder) removeDisk(ref string) {
	if r.Dir == "" || ref == "" {
		return
	}
	dir := filepath.Join(r.Dir, filepath.Clean("/"+ref))
	if err := os.RemoveAll(dir); err != nil {
		r.Logger.Warn("troubleshooting: remove capture files", "ref", ref, "error", err.Error())
		return
	}
	// Drop the day directory once empty; ignore failures (another capture
	// may have landed in it meanwhile).
	_ = os.Remove(filepath.Dir(dir))
}

// RemoveFiles deletes the on-disk files for a set of storage refs, after the
// rows have gone.
func (r *Recorder) RemoveFiles(refs []string) {
	for _, ref := range refs {
		r.removeDisk(ref)
	}
}

// Bodies returns the decoded request and response bodies of a capture,
// reading files for the disk backend and decrypting when needed.
func (r *Recorder) Bodies(c *store.TroubleshootingCapture) ([]byte, []byte, error) {
	encReq, encResp := c.RequestBody, c.ResponseBody
	if c.StorageBackend == store.TroubleshootStorageDisk {
		if !r.DiskAvailable() {
			return nil, nil, errors.New("capture is disk-backed but JANUS_TROUBLESHOOT_DIR is unset on this gateway")
		}
		var err error
		if encReq, encResp, err = r.readDisk(c.StorageRef); err != nil {
			return nil, nil, fmt.Errorf("read capture files: %w", err)
		}
	}
	req, err := r.decodeBody(encReq, c.Encrypted)
	if err != nil {
		return nil, nil, err
	}
	resp, err := r.decodeBody(encResp, c.Encrypted)
	if err != nil {
		return nil, nil, err
	}
	return req, resp, nil
}

// RetentionResult summarises one retention pass.
type RetentionResult struct {
	Deleted         int64     `json:"deleted"`
	SessionExpired  bool      `json:"session_expired"`
	RanAt           time.Time `json:"ran_at"`
	AppliedMaxAge   bool      `json:"applied_max_age"`
	AppliedMaxCount bool      `json:"applied_max_count"`
	AppliedMaxBytes bool      `json:"applied_max_bytes"`
}

// EnforceRetention applies the current session's retention policy to every
// capture and auto-disables a session past its expiry. Captures from
// earlier sessions are governed by the current policy too: there is exactly
// one place captured payloads live, and the newest policy is the one the
// administrator most recently agreed to. With no session ever created there
// is nothing to enforce.
func (r *Recorder) EnforceRetention(ctx context.Context) (RetentionResult, error) {
	res := RetentionResult{RanAt: time.Now().UTC()}
	session, err := r.Store.CurrentTroubleshootingSession(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if session.Enabled && !session.ExpiresAt.IsZero() && !res.RanAt.Before(session.ExpiresAt) {
		if err := r.Store.DisableTroubleshootingSession(ctx, session.ID); err != nil {
			return res, err
		}
		res.SessionExpired = true
	}
	pol := session.Config.Retention
	if pol.MaxAgeHours > 0 {
		refs, n, err := r.Store.DeleteTroubleshootingCapturesOlderThan(ctx, res.RanAt.Add(-time.Duration(pol.MaxAgeHours)*time.Hour))
		if err != nil {
			return res, err
		}
		r.RemoveFiles(refs)
		res.Deleted += n
		res.AppliedMaxAge = true
	}
	if pol.MaxCount > 0 {
		refs, n, err := r.Store.DeleteTroubleshootingCapturesBeyondCount(ctx, pol.MaxCount)
		if err != nil {
			return res, err
		}
		r.RemoveFiles(refs)
		res.Deleted += n
		res.AppliedMaxCount = true
	}
	if pol.MaxBytes > 0 {
		refs, n, err := r.Store.DeleteTroubleshootingCapturesBeyondBytes(ctx, pol.MaxBytes)
		if err != nil {
			return res, err
		}
		r.RemoveFiles(refs)
		res.Deleted += n
		res.AppliedMaxBytes = true
	}
	r.mu.Lock()
	r.lastRun = res.RanAt
	r.mu.Unlock()
	return res, nil
}

// PurgeAll removes every capture and its files.
func (r *Recorder) PurgeAll(ctx context.Context) (int64, error) {
	refs, n, err := r.Store.DeleteAllTroubleshootingCaptures(ctx)
	if err != nil {
		return 0, err
	}
	r.RemoveFiles(refs)
	return n, nil
}

// LastRun reports when retention last ran on this replica.
func (r *Recorder) LastRun() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRun
}
