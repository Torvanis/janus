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

// Classifier grouping, timing and benchmarks (migration 0048).
//
// Three things are recorded so an administrator can see what classifiers
// cost them:
//
//   - usage_event.classifier_ms / classifier_calls / classification:
//     how long THIS request waited on classifiers before it was forwarded,
//     how many classifier requests it waited for, and the label it was
//     given inline ("coding/rust"). Background classification never delays
//     a request, so it is in neither number; its own timing and request
//     count are on its classification_result row (latency_ms, queue_ms,
//     calls), joined to the request by request_id.
//   - classifier_profile: per classifier model, HOW to send several
//     questions to it (one combined request, or one request per question
//     in parallel) and the most options it accepts in one question.
//   - classifier_benchmark: an admin-started load test of the current
//     configuration: latency at increasing concurrency and the highest
//     number of simultaneous requests that stays within a latency budget.

var classifierGroupingMigration = migration{name: "0048_classifier_grouping", stmt: []string{
	`ALTER TABLE usage_event ADD COLUMN classifier_ms INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_event ADD COLUMN classifier_calls INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE usage_event ADD COLUMN classification TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS classifier_profile (
		upstream_id TEXT NOT NULL,
		model TEXT NOT NULL,
		call_style TEXT NOT NULL DEFAULT '',
		max_options INTEGER NOT NULL DEFAULT 0,
		wire_model TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY (upstream_id, model)
	)`,
	`CREATE TABLE IF NOT EXISTS classifier_benchmark (
		id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		finished_at TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		config TEXT NOT NULL DEFAULT '{}',
		result TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_classifier_benchmark_time ON classifier_benchmark(created_at)`,
	// Requests the classifier received for one classification (one per
	// question when questions are sent separately).
	`ALTER TABLE classification_result ADD COLUMN calls INTEGER NOT NULL DEFAULT 1`,
	// Classifier calls a captured request caused (questions, answers,
	// timing), encoded like capture bodies. Their own rows, so a background
	// classification finishing after the capture was written can still be
	// attached; deleted with their capture.
	`CREATE TABLE IF NOT EXISTS troubleshooting_classifier_run (
		id TEXT PRIMARY KEY,
		capture_id TEXT NOT NULL REFERENCES troubleshooting_capture(id) ON DELETE CASCADE,
		created_at TEXT NOT NULL,
		feature TEXT NOT NULL DEFAULT '',
		classifier TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		encrypted INTEGER NOT NULL DEFAULT 0,
		data TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_troubleshooting_classifier_run_capture ON troubleshooting_classifier_run(capture_id, created_at)`,
}, down: []string{
	`DROP INDEX IF EXISTS idx_troubleshooting_classifier_run_capture`,
	`DROP TABLE IF EXISTS troubleshooting_classifier_run`,
	`DROP INDEX IF EXISTS idx_classifier_benchmark_time`,
	`DROP TABLE IF EXISTS classifier_benchmark`,
	`DROP TABLE IF EXISTS classifier_profile`,
	// The usage_event columns stay on rollback, like every other column
	// added to it: SQLite cannot drop a column some versions still index.
}}

// Classifier call styles (mirrors classify.Style*; duplicated so store has
// no classify dependency).
const (
	// ClassifierStyleAuto lets Janus pick from what it knows about the
	// classifier (see DefaultClassifierStyle).
	ClassifierStyleAuto = ""
	// ClassifierStyleCombined sends every question in one request.
	ClassifierStyleCombined = "combined"
	// ClassifierStyleSeparate sends one request per question, in parallel.
	ClassifierStyleSeparate = "separate"
)

// ClassifierProfile is how Janus talks to one classifier.
type ClassifierProfile struct {
	UpstreamID string `json:"upstream_id"`
	Model      string `json:"model"`
	// CallStyle is the admin's choice; empty = automatic.
	CallStyle string `json:"call_style"`
	// MaxOptions overrides the detected per-question option limit
	// (0 = use the detected limit).
	MaxOptions int `json:"max_options"`
	// WireModel is the name the decision API answers to, when it differs
	// from the catalog name (OpenRouter: catalog typesafe/jev-router,
	// decisions ~typesafe/jev-latest). Empty = the catalog name.
	WireModel string `json:"wire_model"`
	// Dialect is the decision wire format (DecisionDialect*); empty =
	// detected from the provider's base URL.
	Dialect   string    `json:"dialect"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Wire returns the model name to send.
func (p ClassifierProfile) Wire() string {
	if p.WireModel != "" {
		return p.WireModel
	}
	return p.Model
}

// ClassifierProfileFor returns the stored profile, or an empty (automatic)
// one.
func (s *Store) ClassifierProfileFor(ctx context.Context, upstreamID, model string) (ClassifierProfile, error) {
	p := ClassifierProfile{UpstreamID: upstreamID, Model: model}
	var updated string
	err := s.queryRow(ctx, `SELECT call_style, max_options, wire_model, dialect, updated_at FROM classifier_profile WHERE upstream_id = ? AND model = ?`,
		upstreamID, model).Scan(&p.CallStyle, &p.MaxOptions, &p.WireModel, &p.Dialect, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("classifier profile: %w", err)
	}
	p.UpdatedAt = ParseTime(updated)
	return p, nil
}

// ClassifierProfileForWire finds the profile of the classifier answering to
// wire name `wire` on an upstream: a profile whose wire model or catalog
// name matches, else an empty (automatic) one. Usage insights names its
// classifier by the name sent on the wire.
func (s *Store) ClassifierProfileForWire(ctx context.Context, upstreamID, wire string) (ClassifierProfile, error) {
	p := ClassifierProfile{UpstreamID: upstreamID, Model: wire}
	var updated string
	err := s.queryRow(ctx, `SELECT model, call_style, max_options, wire_model, dialect, updated_at FROM classifier_profile
		WHERE upstream_id = ? AND (wire_model = ? OR (wire_model = '' AND model = ?)) ORDER BY wire_model DESC LIMIT 1`,
		upstreamID, wire, wire).Scan(&p.Model, &p.CallStyle, &p.MaxOptions, &p.WireModel, &p.Dialect, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ClassifierProfile{UpstreamID: upstreamID, Model: wire}, nil
	}
	if err != nil {
		return p, fmt.Errorf("classifier profile: %w", err)
	}
	p.UpdatedAt = ParseTime(updated)
	return p, nil
}

// ListClassifierProfiles returns every stored profile.
func (s *Store) ListClassifierProfiles(ctx context.Context) ([]ClassifierProfile, error) {
	rows, err := s.query(ctx, `SELECT upstream_id, model, call_style, max_options, wire_model, dialect, updated_at FROM classifier_profile ORDER BY upstream_id, model`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ClassifierProfile{}
	for rows.Next() {
		var p ClassifierProfile
		var updated string
		if err := rows.Scan(&p.UpstreamID, &p.Model, &p.CallStyle, &p.MaxOptions, &p.WireModel, &p.Dialect, &updated); err != nil {
			return nil, err
		}
		p.UpdatedAt = ParseTime(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetClassifierProfile stores (or, when both fields are automatic, removes)
// a profile.
func (s *Store) SetClassifierProfile(ctx context.Context, p ClassifierProfile) error {
	switch p.CallStyle {
	case ClassifierStyleAuto, ClassifierStyleCombined, ClassifierStyleSeparate:
	default:
		return &ValidationError{Field: "call_style", Message: "call_style must be empty (automatic), combined or separate"}
	}
	if p.MaxOptions < 0 || p.MaxOptions > 1000 {
		return &ValidationError{Field: "max_options", Message: "max_options must be between 0 (detected) and 1000"}
	}
	if strings.TrimSpace(p.UpstreamID) == "" || strings.TrimSpace(p.Model) == "" {
		return &ValidationError{Field: "model", Message: "a classifier profile needs a provider and a model"}
	}
	p.WireModel = strings.TrimSpace(p.WireModel)
	if len(p.WireModel) > 200 {
		return &ValidationError{Field: "wire_model", Message: "keep the decision model name under 200 characters"}
	}
	switch p.Dialect {
	case DecisionDialectAuto, DecisionDialectSystemOne, DecisionDialectOpenAI:
	default:
		return &ValidationError{Field: "dialect", Message: "dialect must be empty (automatic), systemone or openai"}
	}
	// Classifier lookups are cached against the model revision.
	defer s.bumpModelRevision()
	if p.CallStyle == ClassifierStyleAuto && p.MaxOptions == 0 && p.WireModel == "" && p.Dialect == "" {
		return s.exec(ctx, `DELETE FROM classifier_profile WHERE upstream_id = ? AND model = ?`, p.UpstreamID, p.Model)
	}
	now := FormatTime(nowUTC())
	return s.exec(ctx, `INSERT INTO classifier_profile (upstream_id, model, call_style, max_options, wire_model, dialect, updated_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT (upstream_id, model) DO UPDATE SET call_style = excluded.call_style, max_options = excluded.max_options, wire_model = excluded.wire_model,
		dialect = excluded.dialect, updated_at = excluded.updated_at`,
		p.UpstreamID, p.Model, p.CallStyle, p.MaxOptions, p.WireModel, p.Dialect, now)
}

// Benchmark statuses.
const (
	BenchmarkRunning = "running"
	BenchmarkDone    = "done"
	BenchmarkFailed  = "failed"
)

// ClassifierBenchmark is one stored load test.
type ClassifierBenchmark struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt time.Time       `json:"finished_at,omitzero"`
	CreatedBy  string          `json:"created_by"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	Config     json.RawMessage `json:"config"`
	Result     json.RawMessage `json:"result"`
}

// CreateClassifierBenchmark records a new running benchmark.
func (s *Store) CreateClassifierBenchmark(ctx context.Context, b *ClassifierBenchmark) error {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = nowUTC()
	}
	if b.Status == "" {
		b.Status = BenchmarkRunning
	}
	cfg, res := string(b.Config), string(b.Result)
	if cfg == "" {
		cfg = "{}"
	}
	if res == "" {
		res = "{}"
	}
	return s.exec(ctx, `INSERT INTO classifier_benchmark (id, created_at, created_by, status, config, result) VALUES (?,?,?,?,?,?)`,
		b.ID, FormatTime(b.CreatedAt), b.CreatedBy, b.Status, cfg, res)
}

// UpdateClassifierBenchmark stores progress or the final result.
func (s *Store) UpdateClassifierBenchmark(ctx context.Context, b *ClassifierBenchmark) error {
	finished := ""
	if !b.FinishedAt.IsZero() {
		finished = FormatTime(b.FinishedAt)
	}
	res := string(b.Result)
	if res == "" {
		res = "{}"
	}
	if len(b.Error) > 500 {
		b.Error = b.Error[:500]
	}
	return s.exec(ctx, `UPDATE classifier_benchmark SET status = ?, error = ?, result = ?, finished_at = ? WHERE id = ?`,
		b.Status, b.Error, res, finished, b.ID)
}

// ListClassifierBenchmarks returns the most recent benchmarks, newest first.
func (s *Store) ListClassifierBenchmarks(ctx context.Context, limit int) ([]ClassifierBenchmark, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.query(ctx, fmt.Sprintf(`SELECT id, created_at, finished_at, created_by, status, error, config, result FROM classifier_benchmark ORDER BY created_at DESC LIMIT %d`, limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ClassifierBenchmark{}
	for rows.Next() {
		b, err := scanBenchmark(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// ClassifierBenchmarkByID returns one benchmark.
func (s *Store) ClassifierBenchmarkByID(ctx context.Context, id string) (*ClassifierBenchmark, error) {
	b, err := scanBenchmark(s.queryRow(ctx, `SELECT id, created_at, finished_at, created_by, status, error, config, result FROM classifier_benchmark WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// FailStaleClassifierBenchmarks marks benchmarks left running by a replica
// that stopped (restart, crash) as failed, so the page never shows a run
// that will never finish.
func (s *Store) FailStaleClassifierBenchmarks(ctx context.Context, before time.Time) error {
	return s.exec(ctx, `UPDATE classifier_benchmark SET status = ?, error = ?, finished_at = ? WHERE status = ? AND created_at < ?`,
		BenchmarkFailed, "the gateway restarted before the benchmark finished", FormatTime(nowUTC()), BenchmarkRunning, FormatTime(before))
}

// ErrBenchmarkRunning is returned by StartClassifierBenchmark while another
// run (on any replica) is still running.
var ErrBenchmarkRunning = errors.New("a classifier benchmark is already running")

// StartClassifierBenchmark records a new running benchmark unless another
// one started after staleBefore is still running, on any replica: two runs
// would measure each other. The check and the insert are one statement, so
// two replicas starting at once cannot both win.
func (s *Store) StartClassifierBenchmark(ctx context.Context, b *ClassifierBenchmark, staleBefore time.Time) error {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = nowUTC()
	}
	b.Status = BenchmarkRunning
	cfg := string(b.Config)
	if cfg == "" {
		cfg = "{}"
	}
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	res, err := runner.ExecContext(ctx, s.rebind(`INSERT INTO classifier_benchmark (id, created_at, created_by, status, config, result)
		SELECT ?,?,?,?,?,'{}' WHERE NOT EXISTS (SELECT 1 FROM classifier_benchmark WHERE status = ? AND created_at >= ?)`),
		b.ID, FormatTime(b.CreatedAt), b.CreatedBy, BenchmarkRunning, cfg, BenchmarkRunning, FormatTime(staleBefore))
	if err != nil {
		return fmt.Errorf("start classifier benchmark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBenchmarkRunning
	}
	return nil
}

func scanBenchmark(scan func(...any) error) (*ClassifierBenchmark, error) {
	var b ClassifierBenchmark
	var created, finished, cfg, res string
	if err := scan(&b.ID, &created, &finished, &b.CreatedBy, &b.Status, &b.Error, &cfg, &res); err != nil {
		return nil, err
	}
	b.CreatedAt = ParseTime(created)
	if finished != "" {
		b.FinishedAt = ParseTime(finished)
	}
	b.Config, b.Result = json.RawMessage(cfg), json.RawMessage(res)
	return &b, nil
}

// ClassificationTag is how one request was classified, for the request log.
type ClassificationTag struct {
	Category    string  `json:"category"`
	Subcategory string  `json:"subcategory,omitempty"`
	Confidence  float64 `json:"confidence"`
	// Mode is inline (the request waited for the label) or async (labelled
	// in the background after it was forwarded).
	Mode       string `json:"mode"`
	Status     string `json:"status"`
	Classifier string `json:"classifier"`
	LatencyMs  int    `json:"latency_ms"`
	QueueMs    int    `json:"queue_ms"`
	Calls      int    `json:"calls"`
}

// ClassificationTagsForRequests returns the classification of each request
// id that has one (the latest, should a request have been classified twice).
func (s *Store) ClassificationTagsForRequests(ctx context.Context, requestIDs []string) (map[string]ClassificationTag, error) {
	out := map[string]ClassificationTag{}
	ids := make([]any, 0, len(requestIDs))
	seen := map[string]bool{}
	for _, id := range requestIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.query(ctx, `SELECT request_id, category, subcategory, confidence, mode, status, classifier, latency_ms, queue_ms, calls, created_at
		FROM classification_result WHERE request_id IN (`+ph+`) ORDER BY created_at`, ids...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, created string
		var t ClassificationTag
		if err := rows.Scan(&id, &t.Category, &t.Subcategory, &t.Confidence, &t.Mode, &t.Status, &t.Classifier, &t.LatencyMs, &t.QueueMs, &t.Calls, &created); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// Request-log classification filters besides a category slug.
const (
	// ClassifiedAny keeps requests that were classified.
	ClassifiedAny = "*"
	// ClassifiedNone keeps requests that were not (not sampled, not
	// eligible, or classification off).
	ClassifiedNone = "-"
)

func classifiedWhere(v string) (string, []any) {
	switch v {
	case "":
		return "", nil
	case ClassifiedAny:
		return "request_id IN (SELECT request_id FROM classification_result WHERE status = 'ok')", nil
	case ClassifiedNone:
		return "request_id NOT IN (SELECT request_id FROM classification_result WHERE status = 'ok')", nil
	}
	cat, sub, _ := strings.Cut(v, "/")
	if sub != "" {
		return "request_id IN (SELECT request_id FROM classification_result WHERE status = 'ok' AND category = ? AND subcategory = ?)", []any{cat, sub}
	}
	return "request_id IN (SELECT request_id FROM classification_result WHERE status = 'ok' AND category = ?)", []any{cat}
}

// TroubleshootingClassifierRun is one classifier call stored with a
// troubleshooting capture. Data is the encoded (base64, optionally
// encrypted) JSON of the call: questions, wire request and response.
type TroubleshootingClassifierRun struct {
	ID         string    `json:"id"`
	CaptureID  string    `json:"capture_id"`
	CreatedAt  time.Time `json:"created_at"`
	Feature    string    `json:"feature"`
	Classifier string    `json:"classifier"`
	Mode       string    `json:"mode"`
	Status     string    `json:"status"`
	LatencyMs  int64     `json:"latency_ms"`
	Encrypted  bool      `json:"encrypted"`
	Data       string    `json:"-"`
}

// InsertTroubleshootingClassifierRun stores one run. A capture deleted in
// the meantime (retention) makes this a no-op error the caller logs.
func (s *Store) InsertTroubleshootingClassifierRun(ctx context.Context, r *TroubleshootingClassifierRun) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = nowUTC()
	}
	return s.exec(ctx, `INSERT INTO troubleshooting_classifier_run (id, capture_id, created_at, feature, classifier, mode, status, latency_ms, encrypted, data) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.CaptureID, FormatTime(r.CreatedAt), r.Feature, r.Classifier, r.Mode, r.Status, r.LatencyMs, boolInt(r.Encrypted), r.Data)
}

// TroubleshootingClassifierRuns returns a capture's runs in order.
func (s *Store) TroubleshootingClassifierRuns(ctx context.Context, captureID string) ([]TroubleshootingClassifierRun, error) {
	rows, err := s.query(ctx, `SELECT id, capture_id, created_at, feature, classifier, mode, status, latency_ms, encrypted, data
		FROM troubleshooting_classifier_run WHERE capture_id = ? ORDER BY created_at, id`, captureID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []TroubleshootingClassifierRun{}
	for rows.Next() {
		var r TroubleshootingClassifierRun
		var created string
		var enc int
		if err := rows.Scan(&r.ID, &r.CaptureID, &created, &r.Feature, &r.Classifier, &r.Mode, &r.Status, &r.LatencyMs, &enc, &r.Data); err != nil {
			return nil, err
		}
		r.CreatedAt, r.Encrypted = ParseTime(created), enc != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// TroubleshootingClassifierRunCounts returns how many runs each capture has.
func (s *Store) TroubleshootingClassifierRunCounts(ctx context.Context, captureIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(captureIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(captureIDs))
	for i, id := range captureIDs {
		args[i] = id
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := s.query(ctx, `SELECT capture_id, COUNT(*) FROM troubleshooting_classifier_run WHERE capture_id IN (`+ph+`) GROUP BY capture_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
