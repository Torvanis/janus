package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --- Troubleshooting mode ---------------------------------------------------
//
// A troubleshooting session is an explicit, time-boxed opt-in to capture the
// headers and bodies of proxied requests that match a filter, kept under a
// retention policy the session declares. Nothing here runs unless an
// administrator has enabled a session; the proxy's default remains "bodies
// are never stored".

// Troubleshooting storage backends.
const (
	// TroubleshootStorageDatabase keeps bodies in troubleshooting_capture.
	TroubleshootStorageDatabase = "database"
	// TroubleshootStorageDisk keeps bodies as files under the configured
	// directory (JANUS_TROUBLESHOOT_DIR); the row keeps only metadata and
	// a storage_ref naming the files.
	TroubleshootStorageDisk = "disk"
)

// Filter combinators for TroubleshootingFilter.Match.
const (
	// TroubleshootMatchAll requires every populated criterion to match (AND).
	TroubleshootMatchAll = "all"
	// TroubleshootMatchAny captures when at least one populated criterion
	// matches (OR).
	TroubleshootMatchAny = "any"
)

// Outcome selectors for TroubleshootingFilter.Outcome.
const (
	TroubleshootOutcomeAny     = ""
	TroubleshootOutcomeSuccess = "success"
	TroubleshootOutcomeFailure = "failure"
)

// Bounds on a session's body capture size, in bytes. The default keeps a
// single capture well inside what an admin will download and read; the
// ceiling protects the database from a session configured to swallow
// 100 MB responses wholesale.
const (
	TroubleshootDefaultMaxBodyBytes int64 = 1 << 20  // 1 MiB per body
	TroubleshootMaxBodyBytesCeiling int64 = 32 << 20 // 32 MiB per body
)

// TroubleshootingCriteria is one set of capture criteria. Every list is "any
// of"; an empty list (or nil bound, or empty outcome) means that criterion is
// not applied.
type TroubleshootingCriteria struct {
	Models  []string `json:"models,omitempty"`
	UserIDs []string `json:"user_ids,omitempty"`
	// GroupIDs matches a request whose caller belongs to ANY listed group.
	// A caller in several groups matches if one of them is listed; under
	// exclude, one listed group is enough to keep them out. Service tokens
	// belong to no group and never match this criterion.
	GroupIDs     []string `json:"group_ids,omitempty"`
	UpstreamIDs  []string `json:"upstream_ids,omitempty"`
	ErrorCodes   []string `json:"error_codes,omitempty"`
	HTTPStatuses []int    `json:"http_statuses,omitempty"`
	Outcome      string   `json:"outcome,omitempty"`
	TokensInGT   *int64   `json:"tokens_in_gt,omitempty"`
	TokensInLT   *int64   `json:"tokens_in_lt,omitempty"`
	TokensOutGT  *int64   `json:"tokens_out_gt,omitempty"`
	TokensOutLT  *int64   `json:"tokens_out_lt,omitempty"`
}

// TroubleshootingFilter selects which proxied requests a session captures.
// It is three gates evaluated together, in the shape of a rules builder:
//
//   - The top-level criteria are the "include" gate. Match decides how its
//     populated criteria combine: "any" (OR — a request need only satisfy
//     one) or "all" (AND). An empty include gate admits every request.
//   - Require is the "AND" gate: every populated criterion must match.
//   - Exclude is the "NOT" gate: a request matching any populated criterion
//     is never captured, whatever the other gates say.
//
// Require and Exclude are optional and absent from sessions saved before
// they existed, which therefore behave exactly as they always did. An
// entirely empty filter captures everything, which the admin UI warns about.
type TroubleshootingFilter struct {
	Match        string   `json:"match"`
	Models       []string `json:"models,omitempty"`
	UserIDs      []string `json:"user_ids,omitempty"`
	GroupIDs     []string `json:"group_ids,omitempty"`
	UpstreamIDs  []string `json:"upstream_ids,omitempty"`
	ErrorCodes   []string `json:"error_codes,omitempty"`
	HTTPStatuses []int    `json:"http_statuses,omitempty"`
	Outcome      string   `json:"outcome,omitempty"`
	TokensInGT   *int64   `json:"tokens_in_gt,omitempty"`
	TokensInLT   *int64   `json:"tokens_in_lt,omitempty"`
	TokensOutGT  *int64   `json:"tokens_out_gt,omitempty"`
	TokensOutLT  *int64   `json:"tokens_out_lt,omitempty"`

	Require *TroubleshootingCriteria `json:"require,omitempty"`
	Exclude *TroubleshootingCriteria `json:"exclude,omitempty"`
}

// include returns the top-level criteria as a TroubleshootingCriteria so the
// three gates share one evaluator.
func (f TroubleshootingFilter) include() TroubleshootingCriteria {
	return TroubleshootingCriteria{
		Models: f.Models, UserIDs: f.UserIDs, GroupIDs: f.GroupIDs, UpstreamIDs: f.UpstreamIDs,
		ErrorCodes: f.ErrorCodes, HTTPStatuses: f.HTTPStatuses, Outcome: f.Outcome,
		TokensInGT: f.TokensInGT, TokensInLT: f.TokensInLT, TokensOutGT: f.TokensOutGT, TokensOutLT: f.TokensOutLT,
	}
}

// Validate rejects unknown vocabulary in one gate. prefix names the gate in
// error messages ("filter." or "filter.require.").
func (c TroubleshootingCriteria) validate(prefix string) error {
	switch c.Outcome {
	case TroubleshootOutcomeAny, TroubleshootOutcomeSuccess, TroubleshootOutcomeFailure:
	default:
		return fmt.Errorf("%soutcome must be empty, %q or %q", prefix, TroubleshootOutcomeSuccess, TroubleshootOutcomeFailure)
	}
	for _, status := range c.HTTPStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("%shttp_statuses contains %d, which is not an HTTP status", prefix, status)
		}
	}
	for name, bound := range map[string]*int64{
		"tokens_in_gt": c.TokensInGT, "tokens_in_lt": c.TokensInLT, "tokens_out_gt": c.TokensOutGT, "tokens_out_lt": c.TokensOutLT,
	} {
		if bound != nil && *bound < 0 {
			return fmt.Errorf("%s%s must not be negative", prefix, name)
		}
	}
	return nil
}

// IsEmpty reports whether no criterion in this gate is populated.
func (c TroubleshootingCriteria) IsEmpty() bool {
	return len(c.Models) == 0 && len(c.UserIDs) == 0 && len(c.GroupIDs) == 0 && len(c.UpstreamIDs) == 0 &&
		len(c.ErrorCodes) == 0 && len(c.HTTPStatuses) == 0 && c.Outcome == TroubleshootOutcomeAny &&
		c.TokensInGT == nil && c.TokensInLT == nil && c.TokensOutGT == nil && c.TokensOutLT == nil
}

// Validate normalises the combinator, rejects unknown vocabulary in every
// gate, and drops empty optional gates so stored configs stay minimal.
func (f *TroubleshootingFilter) Validate() error {
	switch f.Match {
	case "":
		f.Match = TroubleshootMatchAll
	case TroubleshootMatchAll, TroubleshootMatchAny:
	default:
		return fmt.Errorf("filter.match must be %q or %q", TroubleshootMatchAll, TroubleshootMatchAny)
	}
	if err := f.include().validate("filter."); err != nil {
		return err
	}
	if f.Require != nil {
		if err := f.Require.validate("filter.require."); err != nil {
			return err
		}
		if f.Require.IsEmpty() {
			f.Require = nil
		}
	}
	if f.Exclude != nil {
		if err := f.Exclude.validate("filter.exclude."); err != nil {
			return err
		}
		if f.Exclude.IsEmpty() {
			f.Exclude = nil
		}
	}
	return nil
}

// IsEmpty reports whether no criterion in any gate is populated — the filter
// matches every request.
func (f TroubleshootingFilter) IsEmpty() bool {
	return f.include().IsEmpty() &&
		(f.Require == nil || f.Require.IsEmpty()) &&
		(f.Exclude == nil || f.Exclude.IsEmpty())
}

// criteria evaluates each populated criterion against the event and returns
// the individual verdicts. Only populated criteria contribute, so the
// combinator can be applied uniformly.
func (c TroubleshootingCriteria) criteria(ev *UsageEvent) []bool {
	var verdicts []bool
	if len(c.Models) > 0 {
		verdicts = append(verdicts, containsFold(c.Models, ev.ModelName))
	}
	if len(c.UserIDs) > 0 {
		verdicts = append(verdicts, containsFold(c.UserIDs, ev.UserID) || containsFold(c.UserIDs, ev.ServiceTokenID))
	}
	if len(c.GroupIDs) > 0 {
		verdicts = append(verdicts, anyInGroups(c.GroupIDs, ev.GroupIDs))
	}
	if len(c.UpstreamIDs) > 0 {
		verdicts = append(verdicts, containsFold(c.UpstreamIDs, ev.UpstreamID))
	}
	if len(c.ErrorCodes) > 0 {
		verdicts = append(verdicts, containsFold(c.ErrorCodes, ev.ErrorCode))
	}
	if len(c.HTTPStatuses) > 0 {
		hit := false
		for _, status := range c.HTTPStatuses {
			if status == ev.HTTPStatus {
				hit = true
				break
			}
		}
		verdicts = append(verdicts, hit)
	}
	switch c.Outcome {
	case TroubleshootOutcomeSuccess:
		verdicts = append(verdicts, ev.HTTPStatus >= 200 && ev.HTTPStatus < 300 && ev.ErrorCode == "")
	case TroubleshootOutcomeFailure:
		verdicts = append(verdicts, ev.HTTPStatus >= 400 || ev.ErrorCode != "")
	}
	if c.TokensInGT != nil {
		verdicts = append(verdicts, ev.TokensIn > *c.TokensInGT)
	}
	if c.TokensInLT != nil {
		verdicts = append(verdicts, ev.TokensIn < *c.TokensInLT)
	}
	if c.TokensOutGT != nil {
		verdicts = append(verdicts, ev.TokensOut > *c.TokensOutGT)
	}
	if c.TokensOutLT != nil {
		verdicts = append(verdicts, ev.TokensOut < *c.TokensOutLT)
	}
	return verdicts
}

// anyTrue / allTrue fold a verdict list; both are true for an empty list, so
// an unpopulated gate never vetoes.
func anyTrue(verdicts []bool) bool {
	if len(verdicts) == 0 {
		return true
	}
	for _, v := range verdicts {
		if v {
			return true
		}
	}
	return false
}

func allTrue(verdicts []bool) bool {
	for _, v := range verdicts {
		if !v {
			return false
		}
	}
	return true
}

// matchesAny reports whether at least one populated criterion matches; false
// when none is populated (used for the exclude gate, where "nothing set"
// must mean "nothing excluded").
func (c TroubleshootingCriteria) matchesAny(ev *UsageEvent) bool {
	for _, v := range c.criteria(ev) {
		if v {
			return true
		}
	}
	return false
}

// upfront returns the verdicts of the criteria knowable before the upstream
// answers (model and principal).
func (c TroubleshootingCriteria) upfront(ev *UsageEvent) []bool {
	out := []bool{}
	if len(c.Models) > 0 {
		out = append(out, containsFold(c.Models, ev.ModelName))
	}
	if len(c.UserIDs) > 0 {
		out = append(out, containsFold(c.UserIDs, ev.UserID) || containsFold(c.UserIDs, ev.ServiceTokenID))
	}
	if len(c.GroupIDs) > 0 {
		out = append(out, anyInGroups(c.GroupIDs, ev.GroupIDs))
	}
	return out
}

// anyInGroups reports whether the caller is in at least one wanted group.
func anyInGroups(wanted, have []string) bool {
	for _, g := range have {
		if containsFold(wanted, g) {
			return true
		}
	}
	return false
}

// Matches reports whether a completed usage event satisfies every gate: the
// include gate under its combinator, every populated require criterion, and
// no populated exclude criterion. An empty filter matches everything.
func (f TroubleshootingFilter) Matches(ev *UsageEvent) bool {
	verdicts := f.include().criteria(ev)
	if f.Match == TroubleshootMatchAny {
		if !anyTrue(verdicts) {
			return false
		}
	} else if !allTrue(verdicts) {
		return false
	}
	if f.Require != nil && !allTrue(f.Require.criteria(ev)) {
		return false
	}
	if f.Exclude != nil && f.Exclude.matchesAny(ev) {
		return false
	}
	return true
}

// CouldMatch is the pre-response check the proxy runs before deciding to
// buffer a response for capture. Only the criteria knowable up front (model
// and principal) are consulted; everything else is decided by Matches once
// the response has completed. Under "all" a request whose model or principal
// already fails can never match; under "any" a request can always still
// match via a later criterion unless the filter has only up-front criteria.
// A require criterion that already fails, or an exclude criterion that
// already matches, rules the request out regardless of the include gate.
func (f TroubleshootingFilter) CouldMatch(ev *UsageEvent) bool {
	if f.Require != nil && !allTrue(f.Require.upfront(ev)) {
		return false
	}
	if f.Exclude != nil {
		for _, v := range f.Exclude.upfront(ev) {
			if v {
				return false
			}
		}
	}
	inc := f.include()
	upfront := inc.upfront(ev)
	if len(upfront) == 0 {
		return true
	}
	if f.Match == TroubleshootMatchAny {
		for _, v := range upfront {
			if v {
				return true
			}
		}
		// Other criteria may still fire later.
		return len(inc.criteria(ev)) > len(upfront)
	}
	return allTrue(upfront)
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), v) {
			return true
		}
	}
	return false
}

// TroubleshootingRetention bounds how much captured data a session keeps.
// Zero disables a bound. The retention job applies all three: captures older
// than MaxAgeHours go first, then the oldest beyond MaxCount, then the oldest
// until the total is within MaxBytes.
type TroubleshootingRetention struct {
	MaxAgeHours int   `json:"max_age_hours"`
	MaxCount    int   `json:"max_count"`
	MaxBytes    int64 `json:"max_bytes"`
}

// Validate rejects negative bounds and an entirely unbounded policy: captured
// bodies are the one place Janus stores payloads, and "keep forever" must be
// a deliberate impossibility rather than a default anyone can fall into.
func (r TroubleshootingRetention) Validate() error {
	if r.MaxAgeHours < 0 || r.MaxCount < 0 || r.MaxBytes < 0 {
		return errors.New("retention bounds must not be negative")
	}
	if r.MaxAgeHours == 0 && r.MaxCount == 0 && r.MaxBytes == 0 {
		return errors.New("retention must set at least one of max_age_hours, max_count or max_bytes")
	}
	return nil
}

// TroubleshootingConfig is the JSON document stored on a session.
type TroubleshootingConfig struct {
	Filter    TroubleshootingFilter    `json:"filter"`
	Retention TroubleshootingRetention `json:"retention"`
	// Storage names the backend for bodies: database (default) or disk.
	Storage string `json:"storage"`
	// CaptureRequestBody / CaptureResponseBody may be switched off
	// independently — headers and metadata are always captured.
	CaptureRequestBody  bool `json:"capture_request_body"`
	CaptureResponseBody bool `json:"capture_response_body"`
	// Encrypt stores bodies through the gateway's AES-GCM cipher (the same
	// key that protects upstream credentials). Off means bodies are at rest
	// in the clear, which the UI warns about prominently.
	Encrypt bool `json:"encrypt"`
	// MaxBodyBytes caps each captured body; larger bodies are truncated and
	// the capture flagged.
	MaxBodyBytes int64 `json:"max_body_bytes"`
}

// Validate normalises defaults and rejects impossible configurations.
func (c *TroubleshootingConfig) Validate(diskAvailable bool) error {
	if err := c.Filter.Validate(); err != nil {
		return err
	}
	if err := c.Retention.Validate(); err != nil {
		return err
	}
	switch c.Storage {
	case "":
		c.Storage = TroubleshootStorageDatabase
	case TroubleshootStorageDatabase:
	case TroubleshootStorageDisk:
		if !diskAvailable {
			return errors.New("storage=disk requires JANUS_TROUBLESHOOT_DIR to be configured on the gateway")
		}
	default:
		return fmt.Errorf("storage must be %q or %q", TroubleshootStorageDatabase, TroubleshootStorageDisk)
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = TroubleshootDefaultMaxBodyBytes
	}
	if c.MaxBodyBytes > TroubleshootMaxBodyBytesCeiling {
		return fmt.Errorf("max_body_bytes must not exceed %d", TroubleshootMaxBodyBytesCeiling)
	}
	return nil
}

// TroubleshootingSession is one capture window. The newest row is the
// current session; Enabled false with DisabledAt set is a finished one kept
// for the record of what was captured under which rules.
type TroubleshootingSession struct {
	ID         string                `json:"id"`
	Enabled    bool                  `json:"enabled"`
	Config     TroubleshootingConfig `json:"config"`
	CreatedBy  string                `json:"created_by"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
	ExpiresAt  time.Time             `json:"expires_at"`
	DisabledAt time.Time             `json:"disabled_at"`
}

// Active reports whether the session is capturing right now: enabled and not
// past its expiry.
func (s *TroubleshootingSession) Active(now time.Time) bool {
	if s == nil || !s.Enabled {
		return false
	}
	return s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt)
}

const troubleshootingSessionColumns = `id, enabled, config, created_by, created_at, updated_at, expires_at, disabled_at`

func scanTroubleshootingSession(row interface{ Scan(...any) error }) (*TroubleshootingSession, error) {
	var (
		out                                         TroubleshootingSession
		enabled                                     int
		config, created, updated, expires, disabled string
	)
	if err := row.Scan(&out.ID, &enabled, &config, &out.CreatedBy, &created, &updated, &expires, &disabled); err != nil {
		return nil, err
	}
	out.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(config), &out.Config); err != nil {
		return nil, fmt.Errorf("decode troubleshooting session config: %w", err)
	}
	out.CreatedAt, out.UpdatedAt = ParseTime(created), ParseTime(updated)
	if expires != "" {
		out.ExpiresAt = ParseTime(expires)
	}
	if disabled != "" {
		out.DisabledAt = ParseTime(disabled)
	}
	return &out, nil
}

// CreateTroubleshootingSession opens a new session and closes any session
// still enabled, so exactly one can be capturing at a time.
func (s *Store) CreateTroubleshootingSession(ctx context.Context, cfg TroubleshootingConfig, expiresAt time.Time, createdBy string) (*TroubleshootingSession, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode troubleshooting config: %w", err)
	}
	now := nowUTC()
	expires := ""
	if !expiresAt.IsZero() {
		expires = FormatTime(expiresAt)
	}
	id := NewID()
	if err := s.exec(ctx, `UPDATE troubleshooting_session SET enabled = 0, disabled_at = ?, updated_at = ? WHERE enabled = 1`,
		FormatTime(now), FormatTime(now)); err != nil {
		return nil, fmt.Errorf("close previous troubleshooting sessions: %w", err)
	}
	if err := s.exec(ctx, `INSERT INTO troubleshooting_session (`+troubleshootingSessionColumns+`) VALUES (?,1,?,?,?,?,?,'')`,
		id, string(encoded), createdBy, FormatTime(now), FormatTime(now), expires); err != nil {
		return nil, fmt.Errorf("create troubleshooting session: %w", err)
	}
	return s.GetTroubleshootingSession(ctx, id)
}

// UpdateTroubleshootingSession replaces the rules and expiry of a session.
func (s *Store) UpdateTroubleshootingSession(ctx context.Context, id string, cfg TroubleshootingConfig, expiresAt time.Time) (*TroubleshootingSession, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode troubleshooting config: %w", err)
	}
	expires := ""
	if !expiresAt.IsZero() {
		expires = FormatTime(expiresAt)
	}
	if err := s.exec(ctx, `UPDATE troubleshooting_session SET config = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		string(encoded), expires, FormatTime(nowUTC()), id); err != nil {
		return nil, fmt.Errorf("update troubleshooting session: %w", err)
	}
	return s.GetTroubleshootingSession(ctx, id)
}

// DisableTroubleshootingSession stops capture for a session. Captured data is
// retained (under the session's retention policy) until purged.
func (s *Store) DisableTroubleshootingSession(ctx context.Context, id string) error {
	now := FormatTime(nowUTC())
	if err := s.exec(ctx, `UPDATE troubleshooting_session SET enabled = 0, disabled_at = ?, updated_at = ? WHERE id = ? AND enabled = 1`,
		now, now, id); err != nil {
		return fmt.Errorf("disable troubleshooting session: %w", err)
	}
	return nil
}

// GetTroubleshootingSession loads one session by id.
func (s *Store) GetTroubleshootingSession(ctx context.Context, id string) (*TroubleshootingSession, error) {
	out, err := scanTroubleshootingSession(s.queryRow(ctx,
		`SELECT `+troubleshootingSessionColumns+` FROM troubleshooting_session WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get troubleshooting session: %w", err)
	}
	return out, nil
}

// CurrentTroubleshootingSession returns the newest session, enabled or not,
// or ErrNotFound when troubleshooting mode has never been switched on.
func (s *Store) CurrentTroubleshootingSession(ctx context.Context) (*TroubleshootingSession, error) {
	out, err := scanTroubleshootingSession(s.queryRow(ctx,
		`SELECT `+troubleshootingSessionColumns+` FROM troubleshooting_session ORDER BY created_at DESC, id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("current troubleshooting session: %w", err)
	}
	return out, nil
}

// ListTroubleshootingSessions returns sessions newest first.
func (s *Store) ListTroubleshootingSessions(ctx context.Context, limit int) ([]*TroubleshootingSession, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.query(ctx, `SELECT `+troubleshootingSessionColumns+` FROM troubleshooting_session ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list troubleshooting sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*TroubleshootingSession{}
	for rows.Next() {
		sess, err := scanTroubleshootingSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// TroubleshootingCapture is one captured request. Body columns hold base64
// (or an encrypted envelope of it) for the database backend and are empty
// for disk-backed captures, whose StorageRef names the files.
type TroubleshootingCapture struct {
	ID                string    `json:"id"`
	SessionID         string    `json:"session_id"`
	UsageEventID      string    `json:"usage_event_id"`
	RequestID         string    `json:"request_id"`
	CapturedAt        time.Time `json:"captured_at"`
	UserID            string    `json:"user_id"`
	ServiceTokenID    string    `json:"service_token_id,omitempty"`
	ModelName         string    `json:"model_name"`
	UpstreamID        string    `json:"upstream_id"`
	EndpointPath      string    `json:"endpoint_path"`
	HTTPStatus        int       `json:"http_status"`
	ErrorCode         string    `json:"error_code"`
	TokensIn          int64     `json:"tokens_in"`
	TokensOut         int64     `json:"tokens_out"`
	Streaming         bool      `json:"streaming"`
	RequestHeaders    string    `json:"request_headers"`
	ResponseHeaders   string    `json:"response_headers"`
	RequestBody       string    `json:"-"`
	ResponseBody      string    `json:"-"`
	RequestBodyBytes  int64     `json:"request_body_bytes"`
	ResponseBodyBytes int64     `json:"response_body_bytes"`
	SizeBytes         int64     `json:"size_bytes"`
	StorageBackend    string    `json:"storage_backend"`
	StorageRef        string    `json:"storage_ref,omitempty"`
	Encrypted         bool      `json:"encrypted"`
	Truncated         bool      `json:"truncated"`
}

const troubleshootingCaptureColumns = `id, session_id, usage_event_id, request_id, captured_at, user_id, service_token_id,
	model_name, upstream_id, endpoint_path, http_status, error_code, tokens_in, tokens_out, streaming,
	request_headers, response_headers, request_body, response_body, request_body_bytes, response_body_bytes,
	size_bytes, storage_backend, storage_ref, encrypted, truncated`

func scanTroubleshootingCapture(row interface{ Scan(...any) error }) (*TroubleshootingCapture, error) {
	var (
		out                             TroubleshootingCapture
		captured                        string
		streaming, encrypted, truncated int
	)
	if err := row.Scan(&out.ID, &out.SessionID, &out.UsageEventID, &out.RequestID, &captured, &out.UserID, &out.ServiceTokenID,
		&out.ModelName, &out.UpstreamID, &out.EndpointPath, &out.HTTPStatus, &out.ErrorCode, &out.TokensIn, &out.TokensOut, &streaming,
		&out.RequestHeaders, &out.ResponseHeaders, &out.RequestBody, &out.ResponseBody, &out.RequestBodyBytes, &out.ResponseBodyBytes,
		&out.SizeBytes, &out.StorageBackend, &out.StorageRef, &encrypted, &truncated); err != nil {
		return nil, err
	}
	out.CapturedAt = ParseTime(captured)
	out.Streaming, out.Encrypted, out.Truncated = streaming == 1, encrypted == 1, truncated == 1
	return &out, nil
}

// InsertTroubleshootingCapture stores one capture. The id and captured_at are
// assigned when empty.
func (s *Store) InsertTroubleshootingCapture(ctx context.Context, c *TroubleshootingCapture) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.CapturedAt.IsZero() {
		c.CapturedAt = nowUTC()
	}
	if c.StorageBackend == "" {
		c.StorageBackend = TroubleshootStorageDatabase
	}
	if err := s.exec(ctx, `INSERT INTO troubleshooting_capture (`+troubleshootingCaptureColumns+`) VALUES (`+placeholders(26)+`)`,
		c.ID, c.SessionID, c.UsageEventID, c.RequestID, FormatTime(c.CapturedAt), c.UserID, c.ServiceTokenID,
		c.ModelName, c.UpstreamID, c.EndpointPath, c.HTTPStatus, c.ErrorCode, c.TokensIn, c.TokensOut, boolInt(c.Streaming),
		c.RequestHeaders, c.ResponseHeaders, c.RequestBody, c.ResponseBody, c.RequestBodyBytes, c.ResponseBodyBytes,
		c.SizeBytes, c.StorageBackend, c.StorageRef, boolInt(c.Encrypted), boolInt(c.Truncated)); err != nil {
		return fmt.Errorf("insert troubleshooting capture: %w", err)
	}
	return nil
}

// TroubleshootingCaptureFilter narrows ListTroubleshootingCaptures.
type TroubleshootingCaptureFilter struct {
	SessionID   string
	UserID      string
	Model       string
	UpstreamID  string
	ErrorCode   string
	HTTPStatus  int
	Outcome     string
	TokensInGT  *int64
	TokensInLT  *int64
	TokensOutGT *int64
	TokensOutLT *int64
	Since       time.Time
	Limit       int
	Offset      int
}

func (f TroubleshootingCaptureFilter) clause() (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.SessionID != "" {
		add("session_id = ?", f.SessionID)
	}
	if f.UserID != "" {
		add("(user_id = ? OR service_token_id = ?)", f.UserID)
		args = append(args, f.UserID)
	}
	if f.Model != "" {
		add("model_name = ?", f.Model)
	}
	if f.UpstreamID != "" {
		add("upstream_id = ?", f.UpstreamID)
	}
	if f.ErrorCode != "" {
		add("error_code = ?", f.ErrorCode)
	}
	if f.HTTPStatus > 0 {
		add("http_status = ?", f.HTTPStatus)
	}
	switch f.Outcome {
	case TroubleshootOutcomeSuccess:
		where = append(where, "http_status >= 200 AND http_status < 300 AND error_code = ''")
	case TroubleshootOutcomeFailure:
		where = append(where, "(http_status >= 400 OR error_code <> '')")
	}
	if f.TokensInGT != nil {
		add("tokens_in > ?", *f.TokensInGT)
	}
	if f.TokensInLT != nil {
		add("tokens_in < ?", *f.TokensInLT)
	}
	if f.TokensOutGT != nil {
		add("tokens_out > ?", *f.TokensOutGT)
	}
	if f.TokensOutLT != nil {
		add("tokens_out < ?", *f.TokensOutLT)
	}
	if !f.Since.IsZero() {
		add("captured_at >= ?", FormatTime(f.Since))
	}
	return strings.Join(where, " AND "), args
}

// ListTroubleshootingCaptures returns captures newest first with the total
// matching count. Bodies are included; callers that only need metadata
// should ignore them (the archive exporter needs them in one pass).
func (s *Store) ListTroubleshootingCaptures(ctx context.Context, f TroubleshootingCaptureFilter) ([]*TroubleshootingCapture, int, error) {
	clause, args := f.clause()
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM troubleshooting_capture WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count troubleshooting captures: %w", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.query(ctx, `SELECT `+troubleshootingCaptureColumns+` FROM troubleshooting_capture WHERE `+clause+
		` ORDER BY captured_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list troubleshooting captures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*TroubleshootingCapture{}
	for rows.Next() {
		c, err := scanTroubleshootingCapture(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan troubleshooting capture: %w", err)
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// TroubleshootingCaptureByEvent finds the capture for one usage event.
func (s *Store) TroubleshootingCaptureByEvent(ctx context.Context, usageEventID string) (*TroubleshootingCapture, error) {
	out, err := scanTroubleshootingCapture(s.queryRow(ctx,
		`SELECT `+troubleshootingCaptureColumns+` FROM troubleshooting_capture WHERE usage_event_id = ? ORDER BY captured_at DESC LIMIT 1`, usageEventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("troubleshooting capture by event: %w", err)
	}
	return out, nil
}

// TroubleshootingCapturedEventIDs reports which of the given usage events
// have a capture, so a request listing can offer a download per row without
// a query per row.
func (s *Store) TroubleshootingCapturedEventIDs(ctx context.Context, eventIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		args = append(args, id)
	}
	rows, err := s.query(ctx, `SELECT DISTINCT usage_event_id FROM troubleshooting_capture WHERE usage_event_id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("captured event ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// TroubleshootingCaptureStats summarises storage use for the admin panel.
type TroubleshootingCaptureStats struct {
	Count      int       `json:"count"`
	TotalBytes int64     `json:"total_bytes"`
	OldestAt   time.Time `json:"oldest_at"`
	NewestAt   time.Time `json:"newest_at"`
}

// TroubleshootingCaptureStats returns count, total size and the time span of
// everything captured, across all sessions.
func (s *Store) TroubleshootingCaptureStats(ctx context.Context) (TroubleshootingCaptureStats, error) {
	var (
		out            TroubleshootingCaptureStats
		total          sql.NullInt64
		oldest, newest sql.NullString
	)
	if err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0), MIN(captured_at), MAX(captured_at) FROM troubleshooting_capture`).
		Scan(&out.Count, &total, &oldest, &newest); err != nil {
		return out, fmt.Errorf("troubleshooting capture stats: %w", err)
	}
	out.TotalBytes = total.Int64
	if oldest.Valid && oldest.String != "" {
		out.OldestAt = ParseTime(oldest.String)
	}
	if newest.Valid && newest.String != "" {
		out.NewestAt = ParseTime(newest.String)
	}
	return out, nil
}

// deleteCaptures removes the captures matched by a WHERE clause and returns
// the storage refs of disk-backed captures so the caller can remove their
// files. Refs are read before the delete in the same transaction.
func (s *Store) deleteCaptures(ctx context.Context, where string, args ...any) ([]string, int64, error) {
	var refs []string
	var deleted int64
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.rebind(`SELECT storage_ref FROM troubleshooting_capture WHERE storage_ref <> '' AND `+where), args...)
		if err != nil {
			return fmt.Errorf("collect capture refs: %w", err)
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				_ = rows.Close()
				return err
			}
			refs = append(refs, ref)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM troubleshooting_capture WHERE `+where), args...)
		if err != nil {
			return fmt.Errorf("delete captures: %w", err)
		}
		deleted, _ = res.RowsAffected()
		return nil
	})
	return refs, deleted, err
}

// DeleteTroubleshootingCapturesOlderThan removes captures taken before the
// cut-off (time-based retention).
func (s *Store) DeleteTroubleshootingCapturesOlderThan(ctx context.Context, before time.Time) ([]string, int64, error) {
	return s.deleteCaptures(ctx, `captured_at < ?`, FormatTime(before))
}

// DeleteTroubleshootingCapturesBeyondCount keeps the newest keep captures
// and removes the rest (count-based retention).
func (s *Store) DeleteTroubleshootingCapturesBeyondCount(ctx context.Context, keep int) ([]string, int64, error) {
	if keep <= 0 {
		return s.DeleteAllTroubleshootingCaptures(ctx)
	}
	// The cut-off is the keep-th newest row; everything strictly older (by
	// captured_at, then id for a stable tiebreak) goes. Two portable
	// statements rather than one negative-LIMIT subquery, which PostgreSQL
	// rejects.
	var cutAt, cutID string
	err := s.queryRow(ctx, `SELECT captured_at, id FROM troubleshooting_capture ORDER BY captured_at DESC, id DESC LIMIT 1 OFFSET ?`, keep-1).Scan(&cutAt, &cutID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil // fewer than keep rows exist
	}
	if err != nil {
		return nil, 0, fmt.Errorf("find capture cut-off: %w", err)
	}
	return s.deleteCaptures(ctx, `(captured_at < ? OR (captured_at = ? AND id < ?))`, cutAt, cutAt, cutID)
}

// DeleteTroubleshootingCapturesBeyondBytes removes the oldest captures until
// the total size fits within maxBytes (size-based retention). It walks
// oldest-first in memory over ids and sizes only, which stays cheap because
// count-based retention already bounds the table.
func (s *Store) DeleteTroubleshootingCapturesBeyondBytes(ctx context.Context, maxBytes int64) ([]string, int64, error) {
	if maxBytes <= 0 {
		return nil, 0, nil
	}
	stats, err := s.TroubleshootingCaptureStats(ctx)
	if err != nil {
		return nil, 0, err
	}
	if stats.TotalBytes <= maxBytes {
		return nil, 0, nil
	}
	rows, err := s.query(ctx, `SELECT id, size_bytes FROM troubleshooting_capture ORDER BY captured_at ASC, id ASC`)
	if err != nil {
		return nil, 0, fmt.Errorf("walk captures by age: %w", err)
	}
	var victims []any
	excess := stats.TotalBytes - maxBytes
	for rows.Next() && excess > 0 {
		var id string
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		victims = append(victims, id)
		excess -= size
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(victims) == 0 {
		return nil, 0, nil
	}
	return s.deleteCaptures(ctx, `id IN (`+placeholders(len(victims))+`)`, victims...)
}

// DeleteTroubleshootingCapturesBySession removes every capture of one session.
func (s *Store) DeleteTroubleshootingCapturesBySession(ctx context.Context, sessionID string) ([]string, int64, error) {
	return s.deleteCaptures(ctx, `session_id = ?`, sessionID)
}

// DeleteAllTroubleshootingCaptures is the "purge everything" button.
func (s *Store) DeleteAllTroubleshootingCaptures(ctx context.Context) ([]string, int64, error) {
	return s.deleteCaptures(ctx, `1=1`)
}
