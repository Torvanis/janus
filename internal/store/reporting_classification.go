package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// Classification is administrator-maintained, never inferred from names or adapters.
// Updates affect future admissions only; usage-event snapshots are immutable.
var reportingClassificationMigration = migration{
	name: "0035_reporting_classification",
	stmt: []string{`CREATE TABLE IF NOT EXISTS report_model_classification (
 model_id TEXT PRIMARY KEY,
 family TEXT NOT NULL,
 provider TEXT NOT NULL,
 hosting TEXT NOT NULL,
 updated_at TEXT NOT NULL
 )`},
}

type ReportModelClassification struct {
	ModelID   string    `json:"model_id"`
	ModelName string    `json:"model_name"`
	Family    string    `json:"family"`
	Provider  string    `json:"provider"`
	Hosting   string    `json:"hosting"`
	UpdatedAt time.Time `json:"updated_at"`
}

const reportClassificationSelect = `SELECT m.id, CASE WHEN m.display_name <> '' THEN m.display_name ELSE m.name END,
 COALESCE(c.family,''), COALESCE(c.provider,''), COALESCE(c.hosting,''), COALESCE(c.updated_at,'')
 FROM model m JOIN upstream u ON u.id=m.upstream_id
 LEFT JOIN report_model_classification c ON c.model_id=m.id `

func scanReportClassification(scan func(...any) error) (*ReportModelClassification, error) {
	var v ReportModelClassification
	var updated string
	if err := scan(&v.ModelID, &v.ModelName, &v.Family, &v.Provider, &v.Hosting, &updated); err != nil {
		return nil, err
	}
	v.UpdatedAt = ParseTime(updated)
	return &v, nil
}

func (s *Store) GetReportModelClassification(ctx context.Context, modelID string) (*ReportModelClassification, error) {
	v, err := scanReportClassification(s.queryRow(ctx, reportClassificationSelect+` WHERE m.id=? AND u.deleted_at='' AND c.model_id IS NOT NULL`, modelID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *Store) ListReportModelClassifications(ctx context.Context) ([]ReportModelClassification, error) {
	rows, err := s.query(ctx, reportClassificationSelect+` WHERE u.deleted_at='' ORDER BY m.name,m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportModelClassification{}
	for rows.Next() {
		v, err := scanReportClassification(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ErrInvalidReportClassification identifies invalid administrator-supplied taxonomy.
var ErrInvalidReportClassification = errors.New("invalid reporting classification")

func (s *Store) SaveReportModelClassification(ctx context.Context, v *ReportModelClassification) error {
	if v == nil {
		return fmt.Errorf("%w: classification is required", ErrInvalidReportClassification)
	}
	for _, field := range []struct{ name, value string }{{"family", v.Family}, {"provider", v.Provider}} {
		if len(field.value) > 100 || !utf8.ValidString(field.value) {
			return fmt.Errorf("%w: %s must contain at most 100 bytes of printable UTF-8", ErrInvalidReportClassification, field.name)
		}
		for _, r := range field.value {
			if !unicode.IsPrint(r) {
				return fmt.Errorf("%w: %s must contain only printable characters", ErrInvalidReportClassification, field.name)
			}
		}
	}
	switch v.Hosting {
	case "", "self_hosted", "external", "hybrid":
	default:
		return fmt.Errorf("%w: hosting must be empty, self_hosted, external, or hybrid", ErrInvalidReportClassification)
	}
	var name string
	if err := s.queryRow(ctx, `SELECT CASE WHEN m.display_name <> '' THEN m.display_name ELSE m.name END FROM model m JOIN upstream u ON u.id=m.upstream_id WHERE m.id=? AND u.deleted_at=''`, v.ModelID).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	updated := nowUTC()
	if err := s.exec(ctx, `INSERT INTO report_model_classification(model_id,family,provider,hosting,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(model_id) DO UPDATE SET family=excluded.family,provider=excluded.provider,hosting=excluded.hosting,updated_at=excluded.updated_at`, v.ModelID, v.Family, v.Provider, v.Hosting, FormatTime(updated)); err != nil {
		return err
	}
	v.ModelName = name
	v.UpdatedAt = updated
	return nil
}
