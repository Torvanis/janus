package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
	_ "time/tzdata" // Scheduled reports also work in minimal deployment images.
)

type ReportSchedule struct {
	ID          string    `json:"id"`
	ReportID    string    `json:"report_id"`
	OwnerUserID string    `json:"owner_user_id"`
	Frequency   string    `json:"frequency"`
	Timezone    string    `json:"timezone"`
	At          string    `json:"at"`
	Weekday     int       `json:"weekday"`
	Monthday    int       `json:"monthday"`
	Enabled     bool      `json:"enabled"`
	NextRunAt   time.Time `json:"next_run_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

var errReportSchedule = errors.New("invalid report schedule; use daily, weekly or monthly with an IANA timezone and HH:MM")

func reportScheduleClock(s ReportSchedule) (*time.Location, int, int, error) {
	if s.Timezone == "" || s.Timezone == "Local" || len(s.At) != 5 || s.At[2] != ':' {
		return nil, 0, 0, errReportSchedule
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s.At[i] < '0' || s.At[i] > '9' {
			return nil, 0, 0, errReportSchedule
		}
	}
	h, _ := strconv.Atoi(s.At[:2])
	m, _ := strconv.Atoi(s.At[3:])
	loc, e := time.LoadLocation(s.Timezone)
	if e != nil || h > 23 || m > 59 {
		return nil, 0, 0, errReportSchedule
	}
	if s.Weekday < 0 || s.Weekday > 6 {
		return nil, 0, 0, errReportSchedule
	}
	switch s.Frequency {
	case "daily", "weekly":
	case "monthly":
		if s.Monthday < 1 || s.Monthday > 28 {
			return nil, 0, 0, errReportSchedule
		}
	default:
		return nil, 0, 0, errReportSchedule
	}
	return loc, h, m, nil
}

// reportScheduleOnDate chooses the earliest physical instant for an ambiguous
// wall clock, and skips nonexistent wall clocks. This means one run per local
// calendar day during a fall-back, and no run during a spring-forward gap.
func reportScheduleOnDate(s ReportSchedule, date time.Time, loc *time.Location, h, m int) time.Time {
	if s.Frequency == "weekly" && int(date.Weekday()) != s.Weekday {
		return time.Time{}
	}
	if s.Frequency == "monthly" && date.Day() != s.Monthday {
		return time.Time{}
	}
	wall := time.Date(date.Year(), date.Month(), date.Day(), h, m, 0, 0, time.UTC)
	// Collect both sides of any nearby transition, including half-hour shifts.
	offsets := map[int]bool{}
	for i := -48; i <= 48; i++ {
		_, offset := wall.Add(time.Duration(i) * time.Hour).In(loc).Zone()
		offsets[offset] = true
	}
	var best time.Time
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if local.Year() == date.Year() && local.Month() == date.Month() && local.Day() == date.Day() && local.Hour() == h && local.Minute() == m && (best.IsZero() || candidate.Before(best)) {
			best = candidate
		}
	}
	return best
}

// NextReportSchedule returns the first scheduled instant strictly after after.
func NextReportSchedule(s ReportSchedule, after time.Time) (time.Time, error) {
	loc, h, m, err := reportScheduleClock(s)
	if err != nil {
		return time.Time{}, err
	}
	local := after.In(loc)
	// Iterate dates in UTC, so a timezone skipping an entire date cannot trap or
	// duplicate iteration. Only date components are passed to the wall clock map.
	date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, time.UTC)
	for i := 0; i < 370; i++ {
		candidate := reportScheduleOnDate(s, date.AddDate(0, 0, i), loc, h, m)
		if !candidate.IsZero() && candidate.After(after) {
			return candidate, nil
		}
	}
	return time.Time{}, errReportSchedule
}

// latestReportSchedule coalesces all missed occurrences into the latest one.
// Reverse calendar search is bounded (monthly schedules are restricted to 28),
// regardless of how long a worker was offline. Historic runs are never flooded.
func latestReportSchedule(s ReportSchedule, now time.Time) (time.Time, error) {
	loc, h, m, err := reportScheduleClock(s)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, time.UTC)
	for i := 0; i < 370; i++ {
		candidate := reportScheduleOnDate(s, date.AddDate(0, 0, -i), loc, h, m)
		if !candidate.IsZero() && !candidate.After(now) {
			return candidate, nil
		}
	}
	return time.Time{}, errReportSchedule
}
func (s *Store) SaveReportSchedule(ctx context.Context, v *ReportSchedule) error {
	if v == nil || v.ReportID == "" || v.OwnerUserID == "" {
		return errReportSchedule
	}
	now := time.Now().UTC()
	next, err := NextReportSchedule(*v, now)
	if err != nil {
		return err
	}
	candidate := *v
	err = s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockReportWorker(ctx); err != nil {
			return err
		}
		def, err := tx.ReportDefinitionByID(ctx, v.ReportID)
		if err != nil {
			return err
		}
		if def.OwnerUserID != v.OwnerUserID {
			return errReportInvalid
		}
		if _, err := tx.ResolveReportScope(ctx, v.OwnerUserID, def.Definition); err != nil {
			return errReportInvalid
		}
		candidate.NextRunAt = next
		candidate.UpdatedAt = now
		if v.ID == "" {
			candidate.ID = NewID()
			candidate.CreatedAt = now
			return tx.exec(ctx, `INSERT INTO report_schedule(id,report_id,owner_user_id,frequency,timezone,at_time,weekday,monthday,enabled,next_run_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, candidate.ID, v.ReportID, v.OwnerUserID, v.Frequency, v.Timezone, v.At, v.Weekday, v.Monthday, boolInt(v.Enabled), FormatTime(next), FormatTime(now), FormatTime(now))
		}
		var created string
		err = tx.queryRow(ctx, `UPDATE report_schedule SET frequency=?,timezone=?,at_time=?,weekday=?,monthday=?,enabled=?,next_run_at=?,updated_at=? WHERE id=? AND owner_user_id=? AND report_id=? RETURNING created_at`, v.Frequency, v.Timezone, v.At, v.Weekday, v.Monthday, boolInt(v.Enabled), FormatTime(next), FormatTime(now), v.ID, v.OwnerUserID, v.ReportID).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReportConflict
		}
		candidate.CreatedAt = ParseTime(created)
		return err
	})
	if err == nil {
		*v = candidate
	}
	return reportError(err)
}

const reportScheduleColumns = `id,report_id,owner_user_id,frequency,timezone,at_time,weekday,monthday,enabled,next_run_at,created_at,updated_at`

func (s *Store) ListReportSchedules(ctx context.Context) ([]ReportSchedule, error) {
	return s.listReportSchedules(ctx, ` ORDER BY next_run_at,id`)
}
func (s *Store) listReportSchedules(ctx context.Context, where string, args ...any) ([]ReportSchedule, error) {
	rows, err := s.query(ctx, `SELECT `+reportScheduleColumns+` FROM report_schedule`+where, args...)
	if err != nil {
		return nil, reportError(err)
	}
	defer rows.Close()
	out := []ReportSchedule{}
	for rows.Next() {
		var v ReportSchedule
		var enabled int
		var n, c, u string
		if err := rows.Scan(&v.ID, &v.ReportID, &v.OwnerUserID, &v.Frequency, &v.Timezone, &v.At, &v.Weekday, &v.Monthday, &enabled, &n, &c, &u); err != nil {
			return nil, reportError(err)
		}
		v.Enabled = enabled != 0
		v.NextRunAt = ParseTime(n)
		v.CreatedAt = ParseTime(c)
		v.UpdatedAt = ParseTime(u)
		out = append(out, v)
	}
	return out, reportError(rows.Err())
}
func (s *Store) DeleteReportSchedule(ctx context.Context, id string) error {
	return reportError(s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockReportWorker(ctx); err != nil {
			return err
		}
		return tx.exec(ctx, `DELETE FROM report_schedule WHERE id=?`, id)
	}))
}

// EnqueueDueReports serializes scheduler replicas with claims, then atomically
// freezes definitions and advances next_run_at. One latest missed run is queued.
func (s *Store) EnqueueDueReports(ctx context.Context, now time.Time) (int, error) {
	count := 0
	err := s.modelTx(ctx, func(tx *Store) error {
		if err := tx.lockReportWorker(ctx); err != nil {
			return err
		}
		list, err := tx.listReportSchedules(ctx, ` WHERE enabled=1 AND next_run_at<=? ORDER BY next_run_at,id`, FormatTime(now))
		if err != nil {
			return err
		}
		for _, v := range list {
			def, err := tx.ReportDefinitionByID(ctx, v.ReportID)
			authorized := err == nil && def.OwnerUserID == v.OwnerUserID
			if authorized {
				_, err = tx.ResolveReportScope(ctx, v.OwnerUserID, def.Definition)
				authorized = err == nil
			}
			if !authorized {
				if err := tx.exec(ctx, `UPDATE report_schedule SET enabled=0,updated_at=? WHERE id=?`, FormatTime(now), v.ID); err != nil {
					return err
				}
				if err := tx.CreateNotification(ctx, &Notification{UserID: v.OwnerUserID, Severity: "warning", Title: "Report schedule disabled", Body: "Report access changed. Review your report scope and enable the schedule again in /reports."}); err != nil {
					return err
				}
				continue
			}
			at, err := latestReportSchedule(v, now)
			if err != nil {
				return err
			}
			next, err := NextReportSchedule(v, now)
			if err != nil {
				return err
			}
			if !at.Before(v.NextRunAt) {
				key := v.ID + ":" + FormatTime(at)
				_, err = tx.enqueueReportAt(ctx, v.OwnerUserID, v.ReportID, def.Definition, at, v.ID, key)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				if err == nil {
					count++
				}
			}
			if err := tx.exec(ctx, `UPDATE report_schedule SET next_run_at=?,updated_at=? WHERE id=?`, FormatTime(next), FormatTime(now), v.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, reportError(err)
	}
	return count, nil
}
