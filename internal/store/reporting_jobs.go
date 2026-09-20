package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/torvanis/janus/internal/reporting"
)

var ErrReportConflict = errors.New("report changed or operation no longer available")
var errReportStorage = errors.New("report storage operation failed")
var errReportInvalid = errors.New("invalid report definition or result")

var reportingJobsMigration = migration{name: "0034_reporting_jobs", stmt: []string{
	`CREATE TABLE IF NOT EXISTS report_definition (id TEXT PRIMARY KEY,owner_user_id TEXT NOT NULL,definition_json TEXT NOT NULL,shared INTEGER NOT NULL,revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS report_run (id TEXT PRIMARY KEY,report_id TEXT NOT NULL,owner_user_id TEXT NOT NULL,status TEXT NOT NULL,definition_json TEXT NOT NULL,result_json TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',lease_token TEXT NOT NULL DEFAULT '',lease_until TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,started_at TEXT NOT NULL DEFAULT '',completed_at TEXT NOT NULL DEFAULT '',expires_at TEXT NOT NULL,schedule_id TEXT NOT NULL DEFAULT '',run_key TEXT UNIQUE)`,
	`CREATE INDEX IF NOT EXISTS idx_report_run_claim ON report_run(status,created_at)`,
	`CREATE TABLE IF NOT EXISTS report_worker_lock (id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`,
	`INSERT INTO report_worker_lock(id,version) VALUES (1,0) ON CONFLICT(id) DO NOTHING`,
	`CREATE TABLE IF NOT EXISTS report_schedule (id TEXT PRIMARY KEY,report_id TEXT NOT NULL,owner_user_id TEXT NOT NULL,frequency TEXT NOT NULL,timezone TEXT NOT NULL,at_time TEXT NOT NULL,weekday INTEGER NOT NULL,monthday INTEGER NOT NULL,enabled INTEGER NOT NULL,next_run_at TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_report_schedule_due ON report_schedule(enabled,next_run_at)`,
}}

type ReportDefinition struct {
	ID          string               `json:"id"`
	OwnerUserID string               `json:"owner_user_id"`
	Definition  reporting.Definition `json:"definition"`
	Shared      bool                 `json:"shared"`
	Revision    int                  `json:"revision"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}
type ReportRun struct {
	ID          string               `json:"id"`
	ReportID    string               `json:"report_id"`
	OwnerUserID string               `json:"owner_user_id"`
	Status      string               `json:"status"`
	Error       string               `json:"error"`
	LeaseToken  string               `json:"-"`
	Definition  reporting.Definition `json:"definition"`
	Result      *reporting.Result    `json:"result,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	StartedAt   time.Time            `json:"started_at"`
	CompletedAt time.Time            `json:"completed_at"`
	ExpiresAt   time.Time            `json:"expires_at"`
	Attempts    int                  `json:"attempts"`
	ScheduleID  string               `json:"schedule_id"`
}

func reportError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, ErrReportConflict) || errors.Is(err, errReportInvalid) {
		return err
	}
	return errReportStorage
}
func (s *Store) SaveReportDefinition(ctx context.Context, v *ReportDefinition) error {
	if v == nil || v.OwnerUserID == "" || reporting.Validate(v.Definition) != nil {
		return errReportInvalid
	}
	b, err := json.Marshal(v.Definition)
	if err != nil {
		return errReportInvalid
	}
	now := time.Now().UTC()
	if v.ID == "" {
		id := NewID()
		err = s.exec(ctx, `INSERT INTO report_definition(id,owner_user_id,definition_json,shared,revision,created_at,updated_at) VALUES (?,?,?,?,1,?,?)`, id, v.OwnerUserID, string(b), boolInt(v.Shared), FormatTime(now), FormatTime(now))
		if err == nil {
			v.ID = id
			v.Revision = 1
			v.CreatedAt = now
			v.UpdatedAt = now
		}
		return reportError(err)
	}
	var revision int
	err = s.queryRow(ctx, `UPDATE report_definition SET definition_json=?,shared=?,revision=revision+1,updated_at=? WHERE id=? AND owner_user_id=? AND revision=? RETURNING revision`, string(b), boolInt(v.Shared), FormatTime(now), v.ID, v.OwnerUserID, v.Revision).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReportConflict
	}
	if err == nil {
		v.Revision = revision
		v.UpdatedAt = now
	}
	return reportError(err)
}

const reportDefinitionColumns = `id,owner_user_id,definition_json,shared,revision,created_at,updated_at`

func scanReportDefinition(scan func(...any) error) (*ReportDefinition, error) {
	v := new(ReportDefinition)
	var b, c, u string
	var shared int
	err := scan(&v.ID, &v.OwnerUserID, &b, &shared, &v.Revision, &c, &u)
	if err != nil {
		return nil, reportError(err)
	}
	if json.Unmarshal([]byte(b), &v.Definition) != nil {
		return nil, errReportStorage
	}
	v.Shared = shared != 0
	v.CreatedAt = ParseTime(c)
	v.UpdatedAt = ParseTime(u)
	return v, nil
}
func (s *Store) ReportDefinitionByID(ctx context.Context, id string) (*ReportDefinition, error) {
	return scanReportDefinition(s.queryRow(ctx, `SELECT `+reportDefinitionColumns+` FROM report_definition WHERE id=?`, id).Scan)
}
func (s *Store) ListReportDefinitions(ctx context.Context) ([]ReportDefinition, error) {
	rows, err := s.query(ctx, `SELECT `+reportDefinitionColumns+` FROM report_definition ORDER BY created_at,id`)
	if err != nil {
		return nil, reportError(err)
	}
	defer rows.Close()
	out := []ReportDefinition{}
	for rows.Next() {
		v, e := scanReportDefinition(rows.Scan)
		if e != nil {
			return nil, e
		}
		out = append(out, *v)
	}
	return out, reportError(rows.Err())
}
func (s *Store) DeleteReportDefinition(ctx context.Context, id string) error {
	return reportError(s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockReportWorker(ctx); err != nil {
			return err
		}
		if err := tx.exec(ctx, `DELETE FROM report_schedule WHERE report_id=?`, id); err != nil {
			return err
		}
		return tx.exec(ctx, `DELETE FROM report_definition WHERE id=?`, id)
	}))
}
func (s *Store) lockReportWorker(ctx context.Context) error {
	return s.exec(ctx, `UPDATE report_worker_lock SET version=version WHERE id=1`)
}

func (s *Store) EnqueueReport(ctx context.Context, ownerID, reportID string, d reporting.Definition) (*ReportRun, error) {
	var out *ReportRun
	err := s.modelTx(ctx, func(tx *Store) error {
		var err error
		out, err = tx.enqueueReportAt(ctx, ownerID, reportID, d, time.Now().UTC(), "", "")
		return err
	})
	return out, reportError(err)
}
func (s *Store) enqueueReportAt(ctx context.Context, ownerID, reportID string, d reporting.Definition, at time.Time, scheduleID, key string) (*ReportRun, error) {
	if ownerID == "" {
		return nil, errReportInvalid
	}
	if _, err := s.ResolveReportScope(ctx, ownerID, d); err != nil {
		return nil, errReportInvalid
	}
	start, end, err := reporting.ResolvePeriod(d, at)
	if err != nil {
		return nil, errReportInvalid
	}
	if d.SourcePeriod == "" || d.Period != "custom" {
		d.SourcePeriod = d.Period
	}
	d.Period = "custom"
	d.Start = start.Format(time.RFC3339Nano)
	d.End = end.Format(time.RFC3339Nano)
	b, err := json.Marshal(d)
	if err != nil {
		return nil, errReportInvalid
	}
	r := &ReportRun{ID: NewID(), ReportID: reportID, OwnerUserID: ownerID, Definition: d, Status: "queued", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour), ScheduleID: scheduleID}
	var runKey any
	if key != "" {
		runKey = key
	}
	err = s.exec(ctx, `INSERT INTO report_run(id,report_id,owner_user_id,status,definition_json,created_at,expires_at,schedule_id,run_key) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(run_key) DO NOTHING`, r.ID, r.ReportID, r.OwnerUserID, r.Status, string(b), FormatTime(r.CreatedAt), FormatTime(r.ExpiresAt), scheduleID, runKey)
	if err != nil {
		return nil, err
	}
	return s.ReportRunByID(ctx, r.ID)
}

const reportRunColumns = `id,report_id,owner_user_id,status,definition_json,result_json,error,lease_token,attempts,created_at,started_at,completed_at,expires_at,schedule_id`

func scanReportRun(scan func(...any) error) (*ReportRun, error) {
	r := new(ReportRun)
	var d, b, c, st, co, ex string
	err := scan(&r.ID, &r.ReportID, &r.OwnerUserID, &r.Status, &d, &b, &r.Error, &r.LeaseToken, &r.Attempts, &c, &st, &co, &ex, &r.ScheduleID)
	if err != nil {
		return nil, reportError(err)
	}
	if json.Unmarshal([]byte(d), &r.Definition) != nil {
		return nil, errReportStorage
	}
	if b != "" && r.Status == "complete" {
		if json.Unmarshal([]byte(b), &r.Result) != nil {
			return nil, errReportStorage
		}
	}
	r.CreatedAt = ParseTime(c)
	r.StartedAt = ParseTime(st)
	r.CompletedAt = ParseTime(co)
	r.ExpiresAt = ParseTime(ex)
	return r, nil
}
func (s *Store) ReportRunByID(ctx context.Context, id string) (*ReportRun, error) {
	r, err := scanReportRun(s.queryRow(ctx, `SELECT `+reportRunColumns+` FROM report_run WHERE id=?`, id).Scan)
	if err == nil && !r.ExpiresAt.After(time.Now()) {
		r.Result = nil
		r.Status = "expired"
	}
	return r, err
}
func (s *Store) ListReportRuns(ctx context.Context) ([]ReportRun, error) {
	rows, err := s.query(ctx, `SELECT id,report_id,owner_user_id,status,definition_json,'' AS result_json,error,'' AS lease_token,attempts,created_at,started_at,completed_at,expires_at,schedule_id FROM report_run ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, reportError(err)
	}
	defer rows.Close()
	out := []ReportRun{}
	for rows.Next() {
		r, e := scanReportRun(rows.Scan)
		if e != nil {
			return nil, e
		}
		if !r.ExpiresAt.After(time.Now()) {
			r.Status = "expired"
		}
		out = append(out, *r)
	}
	return out, reportError(rows.Err())
}
func (s *Store) ClaimReportRun(ctx context.Context, lease time.Duration) (*ReportRun, error) {
	if lease <= 0 {
		return nil, errReportInvalid
	}
	var out *ReportRun
	err := s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockReportWorker(ctx); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.ExpireReportRuns(ctx, now); err != nil {
			return err
		}
		rows, err := tx.query(ctx, `SELECT id,owner_user_id FROM report_run WHERE status='running' AND lease_until<=? AND attempts>=3`, FormatTime(now))
		if err != nil {
			return err
		}
		type exhausted struct{ id, owner string }
		dead := []exhausted{}
		for rows.Next() {
			var x exhausted
			if err := rows.Scan(&x.id, &x.owner); err != nil {
				rows.Close()
				return err
			}
			dead = append(dead, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, x := range dead {
			var failedID string
			err := tx.queryRow(ctx, `UPDATE report_run SET status='failed',error='Report failed after three worker attempts; create a new run.',completed_at=?,lease_token='',lease_until='' WHERE id=? AND status='running' AND lease_until<=? AND attempts>=3 RETURNING id`, FormatTime(now), x.id, FormatTime(now)).Scan(&failedID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if err := tx.reportRunNotification(ctx, x.owner, x.id, "failed"); err != nil {
				return err
			}
		}
		var id string
		err = tx.queryRow(ctx, `SELECT id FROM report_run WHERE status='queued' OR (status='running' AND lease_until<=? AND attempts<3) ORDER BY created_at,id LIMIT 1`, FormatTime(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		token := NewID() + NewID()
		var claimedID string
		err = tx.queryRow(ctx, `UPDATE report_run SET status='running',lease_token=?,lease_until=?,attempts=attempts+1,started_at=? WHERE id=? AND (status='queued' OR (status='running' AND lease_until<=? AND attempts<3)) RETURNING id`, token, FormatTime(now.Add(lease)), FormatTime(now), id, FormatTime(now)).Scan(&claimedID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = tx.ReportRunByID(ctx, id)
		return err
	})
	if err != nil {
		return nil, reportError(err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}
func (s *Store) reportRunNotification(ctx context.Context, owner, id, status string) error {
	return s.CreateNotification(ctx, &Notification{UserID: owner, Severity: map[string]string{"complete": "info", "failed": "warning"}[status], Title: "Report " + status, Body: "View your report status: /reports?run=" + id})
}
func (s *Store) FinishReportRun(ctx context.Context, id, leaseToken string, result *reporting.Result, runError string) error {
	if leaseToken == "" {
		return ErrReportConflict
	}
	return reportError(s.modelTx(ctx, func(tx *Store) error {
		r, err := tx.ReportRunByID(ctx, id)
		if err != nil {
			return err
		}
		status := "complete"
		message := ""
		encoded := ""
		if runError != "" {
			status = "failed"
			message = "Report generation failed. Retry or contact an administrator."
		} else if result == nil {
			return errReportInvalid
		}
		if _, err := tx.ResolveReportScope(ctx, r.OwnerUserID, r.Definition); err != nil {
			status = "failed"
			message = "Report access changed. Review your scope before trying again."
		}
		if status == "complete" {
			b, err := json.Marshal(result)
			if err != nil {
				return errReportInvalid
			}
			encoded = string(b)
		}
		now := time.Now().UTC()
		var updated string
		err = tx.queryRow(ctx, `UPDATE report_run SET status=?,result_json=?,error=?,completed_at=?,expires_at=?,lease_token='',lease_until='' WHERE id=? AND status='running' AND lease_token=? AND lease_until>? AND expires_at>? RETURNING id`, status, encoded, message, FormatTime(now), FormatTime(now.Add(30*24*time.Hour)), id, leaseToken, FormatTime(now), FormatTime(now)).Scan(&updated)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportConflict
		}
		if err != nil {
			return err
		}
		return tx.reportRunNotification(ctx, r.OwnerUserID, id, status)
	}))
}
func (s *Store) CancelReportRun(ctx context.Context, id string) error {
	var updated string
	err := s.queryRow(ctx, `UPDATE report_run SET status='cancelled',result_json='',lease_token='',lease_until='',completed_at=? WHERE id=? AND status IN ('queued','running') RETURNING id`, FormatTime(time.Now()), id).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReportConflict
	}
	return reportError(err)
}
func (s *Store) ExpireReportRuns(ctx context.Context, now time.Time) error {
	return reportError(s.exec(ctx, `UPDATE report_run SET status='expired',result_json='',lease_token='',lease_until='' WHERE expires_at<=? AND status<>'expired'`, FormatTime(now)))
}
