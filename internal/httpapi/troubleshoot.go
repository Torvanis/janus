package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/troubleshoot"
)

// --- Troubleshooting mode ---------------------------------------------------
//
// Janus never stores request or response bodies as part of metering. An
// administrator who needs to see what a failing client actually sent, or
// what a provider actually answered, enables a troubleshooting session: a
// time-boxed capture of payloads for requests matching a filter, kept under
// a retention policy and downloadable as tar.gz. The hot-path cost while a
// session is active is one bounded copy of each candidate response; while
// no session is active the proxy pays a single cached read per request.

// troubleshootMaxSessionHours caps how long a session may stay enabled
// before the retention job switches it off. A week is generous for any
// investigation and stops "enable and forget".
const troubleshootMaxSessionHours = 7 * 24

// troubleshootDefaultSessionHours is the expiry applied when the admin sets
// none: a session is a window, not a mode.
const troubleshootDefaultSessionHours = 24

// recorder returns the troubleshooting recorder, building one lazily from
// the server's dependencies so any construction path (including tests that
// wire Server by hand) gets a working recorder.
func (s *Server) recorder() *troubleshoot.Recorder {
	s.troubleshootOnce.Do(func() {
		if s.Troubleshoot != nil {
			return
		}
		var cipher troubleshoot.Cipher
		if s.Cipher != nil {
			cipher = s.Cipher
		}
		dir := ""
		if s.Config != nil {
			dir = s.Config.TroubleshootDir
		}
		s.Troubleshoot = troubleshoot.New(s.Store, cipher, dir, s.Logger)
	})
	return s.Troubleshoot
}

// cachedTroubleshootingSession reads the current session through the config
// cache so the proxy hot path costs a map lookup per request, and a change
// made by an admin on another replica lands within the cache TTL. A missing
// session is cached as nil (not an error) so an instance that has never
// used troubleshooting mode does not re-query on every request.
func (s *Server) cachedTroubleshootingSession(ctx context.Context) *store.TroubleshootingSession {
	session, err := cachedRead(&s.configCache, "troubleshooting_session", func() (*store.TroubleshootingSession, error) {
		sess, err := s.Store.CurrentTroubleshootingSession(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return sess, err
	})
	if err != nil {
		s.Logger.WarnContext(ctx, "troubleshooting: read session", "error", err.Error())
		return nil
	}
	return session
}

// beginCapture decides, at the top of the proxy handler, whether this
// request is a capture candidate. It returns the (possibly wrapped) writer
// and request the rest of the handler must use. Only the criteria knowable
// up front (model, principal) are applied here; the final verdict is made
// by finishCapture once the response has completed.
func (s *Server) beginCapture(w http.ResponseWriter, r *http.Request, event *store.UsageEvent, body []byte, bodyOverflows bool) (http.ResponseWriter, *http.Request) {
	session := s.cachedTroubleshootingSession(r.Context())
	if !session.Active(time.Now().UTC()) || !session.Config.Filter.CouldMatch(event) {
		return w, r
	}
	limit := session.Config.MaxBodyBytes
	if limit <= 0 {
		limit = store.TroubleshootDefaultMaxBodyBytes
	}
	pending := &troubleshoot.Pending{
		Writer:           troubleshoot.NewCapturingWriter(w, limit),
		RequestHeaders:   r.Header.Clone(),
		RequestBody:      body,
		RequestTruncated: bodyOverflows,
	}
	return pending.Writer, r.WithContext(troubleshoot.WithPending(r.Context(), pending))
}

// finishCapture runs after the usage event has been written (so event.ID is
// known) and persists the capture if the completed event matches the
// session filter. ctx is the request context (possibly already detached
// from cancellation by the caller).
func (s *Server) finishCapture(ctx context.Context, event *store.UsageEvent) {
	pending := troubleshoot.PendingFrom(ctx)
	if pending == nil {
		return
	}
	session := s.cachedTroubleshootingSession(ctx)
	if session == nil {
		return
	}
	s.recorder().Capture(ctx, session, event, pending.Payload())
}

// attachCaptureMarkers flags the events in a request-log page that have a
// capture, so the admin UI can show a download control per row.
func (s *Server) attachCaptureMarkers(ctx context.Context, events []*store.UsageEvent) {
	if len(events) == 0 {
		return
	}
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	captured, err := s.Store.TroubleshootingCapturedEventIDs(ctx, ids)
	if err != nil {
		s.Logger.WarnContext(ctx, "troubleshooting: capture markers", "error", err.Error())
		return
	}
	for _, e := range events {
		e.HasCapture = captured[e.ID]
	}
}

// --- Admin API ----------------------------------------------------------------

// troubleshootingStatus is the document served by GET /admin/troubleshooting.
type troubleshootingStatus struct {
	// Active is the one-word answer: is anything being captured right now?
	Active bool `json:"active"`
	// Session is the current (newest) session, enabled or not; null when
	// troubleshooting mode has never been switched on.
	Session *store.TroubleshootingSession     `json:"session"`
	Stats   store.TroubleshootingCaptureStats `json:"stats"`
	// Backends the admin may choose from on this gateway.
	Backends []string `json:"backends"`
	// EncryptionAvailable reports whether bodies can be encrypted at rest.
	EncryptionAvailable bool `json:"encryption_available"`
	// Limits the UI validates against.
	MaxBodyBytesCeiling int64 `json:"max_body_bytes_ceiling"`
	MaxSessionHours     int   `json:"max_session_hours"`
	// RetentionLastRunAt is when the retention job last ran on this replica.
	RetentionLastRunAt time.Time `json:"retention_last_run_at"`
	// Warnings are human-readable cautions about the current configuration
	// (unencrypted storage, an empty filter capturing everything, …).
	Warnings []string `json:"warnings"`
}

func (s *Server) troubleshootingStatusDocument(ctx context.Context) (*troubleshootingStatus, error) {
	rec := s.recorder()
	session, err := s.Store.CurrentTroubleshootingSession(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	stats, err := s.Store.TroubleshootingCaptureStats(ctx)
	if err != nil {
		return nil, err
	}
	doc := &troubleshootingStatus{
		Active:              session.Active(time.Now().UTC()),
		Session:             session,
		Stats:               stats,
		Backends:            []string{store.TroubleshootStorageDatabase},
		EncryptionAvailable: rec.EncryptionAvailable(),
		MaxBodyBytesCeiling: store.TroubleshootMaxBodyBytesCeiling,
		MaxSessionHours:     troubleshootMaxSessionHours,
		RetentionLastRunAt:  rec.LastRun(),
		Warnings:            []string{},
	}
	if rec.DiskAvailable() {
		doc.Backends = append(doc.Backends, store.TroubleshootStorageDisk)
	}
	if session != nil && session.Enabled {
		cfg := session.Config
		if (cfg.CaptureRequestBody || cfg.CaptureResponseBody) && !cfg.Encrypt {
			doc.Warnings = append(doc.Warnings, "Captured bodies are stored unencrypted. Anyone with database or disk access can read prompts and completions verbatim.")
		}
		if cfg.Filter.IsEmpty() {
			doc.Warnings = append(doc.Warnings, "The filter is empty: every proxied request is captured. Expect storage to fill quickly and each response to be buffered in memory once more.")
		}
		if session.ExpiresAt.IsZero() {
			doc.Warnings = append(doc.Warnings, "This session has no expiry. Capture keeps running until it is disabled by hand.")
		}
	}
	return doc, nil
}

// handleGetTroubleshooting serves GET /api/v1/admin/troubleshooting.
func (s *Server) handleGetTroubleshooting(w http.ResponseWriter, r *http.Request) {
	doc, err := s.troubleshootingStatusDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// troubleshootingRequest is the body of PUT /api/v1/admin/troubleshooting.
type troubleshootingRequest struct {
	Enabled *bool                       `json:"enabled"`
	Config  store.TroubleshootingConfig `json:"config"`
	// ExpiresInHours bounds the capture window from now; omitted/zero
	// applies the default, a negative value means "no expiry" (warned
	// about, capped by nothing but the retention policy).
	ExpiresInHours *int `json:"expires_in_hours"`
}

// handlePutTroubleshooting serves PUT /api/v1/admin/troubleshooting: enable a
// new session (when none is active) or update the rules of the active one.
// Enabling while a session is active is an update; disabling is DELETE.
func (s *Server) handlePutTroubleshooting(w http.ResponseWriter, r *http.Request) {
	var body troubleshootingRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Enabled != nil && *body.Enabled {
		if err := s.requireFeature("captures"); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	rec := s.recorder()
	if err := body.Config.Validate(rec.DiskAvailable()); err != nil {
		WriteError(w, r, ErrInvalidRequest(err.Error()))
		return
	}
	if body.Config.Encrypt && !rec.EncryptionAvailable() {
		WriteError(w, r, ErrInvalidRequest("Encryption at rest is unavailable: the gateway has no encryption key configured."))
		return
	}
	var expiresAt time.Time
	switch {
	case body.ExpiresInHours == nil || *body.ExpiresInHours == 0:
		expiresAt = time.Now().UTC().Add(troubleshootDefaultSessionHours * time.Hour)
	case *body.ExpiresInHours < 0:
		expiresAt = time.Time{}
	case *body.ExpiresInHours > troubleshootMaxSessionHours:
		WriteError(w, r, ErrInvalidRequest(fmt.Sprintf("expires_in_hours must not exceed %d (one week). Re-enable the session if the investigation runs longer.", troubleshootMaxSessionHours)))
		return
	default:
		expiresAt = time.Now().UTC().Add(time.Duration(*body.ExpiresInHours) * time.Hour)
	}
	actor := UserFrom(r.Context())
	actorID := ""
	if actor != nil {
		actorID = actor.ID
	}
	current, err := s.Store.CurrentTroubleshootingSession(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, err)
		return
	}
	var (
		session *store.TroubleshootingSession
		action  string
		before  any
	)
	if current != nil && current.Enabled && body.Enabled != nil && !*body.Enabled {
		// enabled=false on an active session is a disable, not an update.
		s.handleDeleteTroubleshooting(w, r)
		return
	}
	if current != nil && current.Enabled {
		before = current.Config
		session, err = s.Store.UpdateTroubleshootingSession(r.Context(), current.ID, body.Config, expiresAt)
		action = "troubleshooting_updated"
	} else {
		if body.Enabled != nil && !*body.Enabled {
			WriteError(w, r, ErrInvalidRequest("No troubleshooting session is active. Set enabled=true to start one."))
			return
		}
		session, err = s.Store.CreateTroubleshootingSession(r.Context(), body.Config, expiresAt, actorID)
		action = "troubleshooting_enabled"
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, action, "troubleshooting_session", session.ID, before, map[string]any{
		"config": session.Config, "expires_at": session.ExpiresAt,
	})
	doc, err := s.troubleshootingStatusDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// handleDeleteTroubleshooting serves DELETE /api/v1/admin/troubleshooting:
// stop capturing. Data already captured stays until retention or a purge
// removes it.
func (s *Server) handleDeleteTroubleshooting(w http.ResponseWriter, r *http.Request) {
	current, err := s.Store.CurrentTroubleshootingSession(r.Context())
	if errors.Is(err, store.ErrNotFound) || (err == nil && !current.Enabled) {
		WriteError(w, r, ErrNotFoundf("An active troubleshooting session"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DisableTroubleshootingSession(r.Context(), current.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "troubleshooting_disabled", "troubleshooting_session", current.ID, map[string]any{"config": current.Config}, nil)
	doc, err := s.troubleshootingStatusDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// handleGetTroubleshootingStats serves GET /api/v1/admin/troubleshooting/stats.
func (s *Server) handleGetTroubleshootingStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.TroubleshootingCaptureStats(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"stats": stats, "retention_last_run_at": s.recorder().LastRun()})
}

// handlePurgeTroubleshootingData serves DELETE /api/v1/admin/troubleshooting/data:
// remove every capture (rows and files) regardless of retention.
func (s *Server) handlePurgeTroubleshootingData(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Store.TroubleshootingCaptureStats(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	n, err := s.recorder().PurgeAll(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "troubleshooting_purged", "troubleshooting_capture", "all", map[string]any{"count": stats.Count, "bytes": stats.TotalBytes}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": n, "bytes_freed": stats.TotalBytes})
}

// handleTroubleshootingCleanup serves POST /api/v1/admin/troubleshooting/cleanup:
// run the retention policy now instead of waiting for the hourly job.
func (s *Server) handleTroubleshootingCleanup(w http.ResponseWriter, r *http.Request) {
	res, err := s.recorder().EnforceRetention(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.SessionExpired {
		s.InvalidateConfigCache()
	}
	s.audit(r, "troubleshooting_cleanup", "troubleshooting_capture", "retention", nil, res)
	WriteJSON(w, http.StatusOK, res)
}

// handleListTroubleshootingCaptures serves GET /api/v1/admin/troubleshooting/captures:
// metadata of captured requests, filterable by the same vocabulary as the
// session filter, newest first.
func (s *Server) handleListTroubleshootingCaptures(w http.ResponseWriter, r *http.Request) {
	captures, total, err := s.Store.ListTroubleshootingCaptures(r.Context(), captureFilterFromQuery(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"captures": captures, "total_count": total})
}

func captureFilterFromQuery(r *http.Request) store.TroubleshootingCaptureFilter {
	q := r.URL.Query()
	f := store.TroubleshootingCaptureFilter{
		SessionID: q.Get("session_id"), UserID: q.Get("user_id"), Model: q.Get("model"), UpstreamID: q.Get("upstream_id"),
		ErrorCode: q.Get("error_code"), Outcome: q.Get("outcome"),
	}
	f.HTTPStatus, _ = strconv.Atoi(q.Get("http_status"))
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	var rf store.RequestFilter
	applyTokenSizeParams(r, &rf)
	f.TokensInGT, f.TokensInLT, f.TokensOutGT, f.TokensOutLT = rf.TokensInGT, rf.TokensInLT, rf.TokensOutGT, rf.TokensOutLT
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			f.Since = t
		}
	}
	return f
}

func (s *Server) archiver() troubleshoot.Archiver {
	a := troubleshoot.Archiver{}
	if s.Config != nil {
		a.Gateway = troubleshoot.ManifestGateway{Version: s.Config.BuildVersion, SHA: s.Config.BuildSHA}
	}
	return a
}

// handleDownloadRequestCapture serves GET /api/v1/admin/requests/{id}/download:
// one usage event's capture as a tar.gz archive.
func (s *Server) handleDownloadRequestCapture(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	capture, err := s.Store.TroubleshootingCaptureByEvent(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("A troubleshooting capture for that request"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	event, err := s.Store.UsageEventByID(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, err)
		return
	}
	reqBody, respBody, err := s.recorder().Bodies(capture)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "troubleshooting: read capture bodies", "error", err.Error(), "capture_id", capture.ID)
		WriteError(w, r, ErrInternal().WithReason("The captured bodies could not be read."))
		return
	}
	name := capture.RequestID
	if name == "" {
		name = capture.UsageEventID
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-request-`+sanitizeFilename(name)+`.tar.gz"`)
	w.WriteHeader(http.StatusOK)
	if err := s.archiver().WriteOne(w, troubleshoot.Entry{Event: event, Capture: capture, RequestBody: reqBody, ResponseBody: respBody}); err != nil {
		s.Logger.WarnContext(r.Context(), "troubleshooting: write archive", "error", err.Error(), "usage_event_id", id)
	}
}

// handleExportTroubleshooting serves GET /api/v1/admin/troubleshooting/export:
// every capture matching the query as one tar.gz with captures.jsonl at the
// root for bulk analysis. Streams page by page so the archive size is not
// bounded by memory.
func (s *Server) handleExportTroubleshooting(w http.ResponseWriter, r *http.Request) {
	filter := captureFilterFromQuery(r)
	filter.Limit, filter.Offset = 200, 0
	rec := s.recorder()
	var page []*store.TroubleshootingCapture
	idx := 0
	next := func() (*troubleshoot.Entry, error) {
		for idx >= len(page) {
			if page != nil && len(page) < filter.Limit {
				return nil, nil
			}
			var err error
			page, _, err = s.Store.ListTroubleshootingCaptures(r.Context(), filter)
			if err != nil {
				return nil, err
			}
			if len(page) == 0 {
				return nil, nil
			}
			filter.Offset += len(page)
			idx = 0
		}
		c := page[idx]
		idx++
		reqBody, respBody, err := rec.Bodies(c)
		if err != nil {
			// A single unreadable capture must not abort a bulk export;
			// the manifest still carries its metadata.
			s.Logger.WarnContext(r.Context(), "troubleshooting: export capture bodies", "error", err.Error(), "capture_id", c.ID)
			reqBody, respBody = nil, nil
		}
		event, err := s.Store.UsageEventByID(r.Context(), c.UsageEventID)
		if err != nil {
			event = nil // purged by usage retention; the capture is self-describing
		}
		return &troubleshoot.Entry{Event: event, Capture: c, RequestBody: reqBody, ResponseBody: respBody}, nil
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-troubleshooting-`+time.Now().UTC().Format("20060102-150405")+`.tar.gz"`)
	w.WriteHeader(http.StatusOK)
	if err := s.archiver().WriteMany(w, next); err != nil {
		s.Logger.WarnContext(r.Context(), "troubleshooting: write export", "error", err.Error())
	}
	s.audit(r, "troubleshooting_exported", "troubleshooting_capture", "export", nil, map[string]any{"query": r.URL.RawQuery})
}

func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}
