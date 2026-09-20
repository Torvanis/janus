package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RateLimitRule caps how many proxied requests per minute a person may make to
// an endpoint. An empty SubjectID applies the rule to every user individually
// (each person gets their own bucket). Endpoint is an exact path, a prefix
// ending in '*', or '*' for every proxied path.
type RateLimitRule struct {
	ID                string    `json:"id"`
	SubjectType       string    `json:"subject_type"`
	SubjectID         string    `json:"subject_id"`
	SubjectName       string    `json:"subject_name"`
	Endpoint          string    `json:"endpoint"`
	RequestsPerMinute int       `json:"requests_per_minute"`
	CreatedAt         time.Time `json:"created_at"`
}

const rateLimitColumns = `id, subject_type, subject_id, endpoint, requests_per_minute, created_at`

func scanRateLimitRule(scan func(...any) error) (*RateLimitRule, error) {
	var r RateLimitRule
	var created string
	if err := scan(&r.ID, &r.SubjectType, &r.SubjectID, &r.Endpoint, &r.RequestsPerMinute, &created); err != nil {
		return nil, err
	}
	r.CreatedAt = ParseTime(created)
	return &r, nil
}

// ListRateLimitRules returns every rate-limit rule, newest first.
func (s *Store) ListRateLimitRules(ctx context.Context) ([]*RateLimitRule, error) {
	rows, err := s.query(ctx, `SELECT `+rateLimitColumns+` FROM rate_limit_rule ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*RateLimitRule{}
	for rows.Next() {
		r, err := scanRateLimitRule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan rate limit rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if r.SubjectID != "" {
			if u, err := s.UserByID(ctx, r.SubjectID); err == nil {
				r.SubjectName = displayName(u)
			}
		}
	}
	return out, nil
}

// RateLimitRuleByID loads one rule.
func (s *Store) RateLimitRuleByID(ctx context.Context, id string) (*RateLimitRule, error) {
	r, err := scanRateLimitRule(s.queryRow(ctx, `SELECT `+rateLimitColumns+` FROM rate_limit_rule WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load rate limit rule: %w", err)
	}
	return r, nil
}

// CreateRateLimitRule persists a rule.
func (s *Store) CreateRateLimitRule(ctx context.Context, r *RateLimitRule) error {
	r.ID = NewID()
	if r.SubjectType == "" {
		r.SubjectType = "user"
	}
	if r.Endpoint == "" {
		r.Endpoint = "*"
	}
	r.CreatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO rate_limit_rule (`+rateLimitColumns+`) VALUES (?,?,?,?,?,?)`,
		r.ID, r.SubjectType, r.SubjectID, r.Endpoint, r.RequestsPerMinute, FormatTime(r.CreatedAt))
}

// UpdateRateLimitRule changes the cap on an existing rule in place, so the
// rule's identity (and any token bucket keyed on it) survives the edit.
func (s *Store) UpdateRateLimitRule(ctx context.Context, id string, requestsPerMinute int) error {
	return s.exec(ctx, `UPDATE rate_limit_rule SET requests_per_minute = ? WHERE id = ?`, requestsPerMinute, id)
}

// DeleteRateLimitRule removes a rule.
func (s *Store) DeleteRateLimitRule(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM rate_limit_rule WHERE id = ?`, id)
}
