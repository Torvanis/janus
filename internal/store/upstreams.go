package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const upstreamColumns = `id, name, adapter_type, base_url, api_key_encrypted, api_key_mask, enabled, last_check_at, last_error, last_latency_ms, created_at`

func scanUpstream(scan func(...any) error) (*Upstream, error) {
	var u Upstream
	var enabled int
	var lastCheck, created string
	if err := scan(&u.ID, &u.Name, &u.AdapterType, &u.BaseURL, &u.encryptedKey, &u.APIKeyMask, &enabled, &lastCheck, &u.LastError, &u.LastLatencyMs, &created); err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	u.HasAPIKey = u.encryptedKey != ""
	u.LastCheckAt = ParseTime(lastCheck)
	u.CreatedAt = ParseTime(created)
	return &u, nil
}

// ListUpstreams returns every non-deleted upstream with its model count.
func (s *Store) ListUpstreams(ctx context.Context) ([]*Upstream, error) {
	rows, err := s.query(ctx, `SELECT `+upstreamColumns+` FROM upstream WHERE deleted_at = '' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Upstream{}
	for rows.Next() {
		u, err := scanUpstream(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan upstream: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, u := range out {
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM model WHERE upstream_id = ?`, u.ID).Scan(&u.ModelCount); err != nil {
			return nil, fmt.Errorf("count upstream models: %w", err)
		}
	}
	return out, nil
}

// UpstreamByID loads one upstream.
func (s *Store) UpstreamByID(ctx context.Context, id string) (*Upstream, error) {
	u, err := scanUpstream(s.queryRow(ctx, `SELECT `+upstreamColumns+` FROM upstream WHERE id = ? AND deleted_at = ''`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load upstream: %w", err)
	}
	return u, nil
}

// CreateUpstream inserts a provider endpoint. The credential arrives already
// encrypted; this layer never sees plaintext keys.
func (s *Store) CreateUpstream(ctx context.Context, name, adapterType, baseURL, encryptedKey, mask string) (*Upstream, error) {
	u := &Upstream{
		ID: NewID(), Name: name, AdapterType: adapterType, BaseURL: strings.TrimRight(baseURL, "/"),
		APIKeyMask: mask, Enabled: true, CreatedAt: nowUTC(), encryptedKey: encryptedKey, HasAPIKey: encryptedKey != "",
	}
	if err := s.exec(ctx,
		`INSERT INTO upstream (id, name, adapter_type, base_url, api_key_encrypted, api_key_mask, enabled, deleted_at, last_check_at, last_error, last_latency_ms, created_at)
		 VALUES (?,?,?,?,?,?,1,'','','',0,?)`,
		u.ID, u.Name, u.AdapterType, u.BaseURL, encryptedKey, mask, FormatTime(u.CreatedAt)); err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateUpstream changes mutable fields. adapter_type is immutable by contract
// and is deliberately absent from this statement.
func (s *Store) UpdateUpstream(ctx context.Context, id, baseURL string, enabled bool, encryptedKey, mask string) error {
	if encryptedKey != "" {
		return s.exec(ctx, `UPDATE upstream SET base_url = ?, enabled = ?, api_key_encrypted = ?, api_key_mask = ? WHERE id = ?`,
			strings.TrimRight(baseURL, "/"), boolInt(enabled), encryptedKey, mask, id)
	}
	return s.exec(ctx, `UPDATE upstream SET base_url = ?, enabled = ? WHERE id = ?`, strings.TrimRight(baseURL, "/"), boolInt(enabled), id)
}

// RenameUpstream changes an upstream's display name. The name column is
// UNIQUE; a conflicting rename surfaces the database error to the caller.
func (s *Store) RenameUpstream(ctx context.Context, id, name string) error {
	return s.exec(ctx, `UPDATE upstream SET name = ? WHERE id = ?`, name, id)
}

// SoftDeleteUpstream disables an upstream and hides it from listings while
// retaining every historical usage event that references it.
func (s *Store) SoftDeleteUpstream(ctx context.Context, id string) error {
	if err := s.exec(ctx, `UPDATE model SET status = ? WHERE upstream_id = ?`, ModelDisabled, id); err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE upstream SET deleted_at = ?, enabled = 0 WHERE id = ?`, FormatTime(nowUTC()), id)
}

// CountGrantsForUpstreamModels reports how many direct grants (model_kind =
// model) point at real models hosted by the upstream. Grants on managed
// models are not counted: they belong to the alias, which survives the
// upstream and is repointed rather than deleted.
func (s *Store) CountGrantsForUpstreamModels(ctx context.Context, upstreamID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM model_grant
		WHERE model_kind = ? AND model_id IN (SELECT id FROM model WHERE upstream_id = ?)`,
		ModelKindModel, upstreamID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count upstream model grants: %w", err)
	}
	return n, nil
}

// DeleteGrantsForUpstreamModels removes every direct grant on the real models
// hosted by the upstream and returns how many rows went. It is the optional
// cleanup step of upstream deletion: once the models are disabled with their
// upstream, those grants can never take effect again and only clutter the
// Grants page as references to models nobody can call.
func (s *Store) DeleteGrantsForUpstreamModels(ctx context.Context, upstreamID string) (int, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM model_grant
		WHERE model_kind = ? AND model_id IN (SELECT id FROM model WHERE upstream_id = ?)`),
		ModelKindModel, upstreamID)
	if err != nil {
		return 0, fmt.Errorf("delete upstream model grants: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RecordUpstreamCheck stores the outcome of a reachability probe.
func (s *Store) RecordUpstreamCheck(ctx context.Context, id string, latency time.Duration, checkErr string) error {
	return s.exec(ctx, `UPDATE upstream SET last_check_at = ?, last_error = ?, last_latency_ms = ? WHERE id = ?`,
		FormatTime(nowUTC()), checkErr, int(latency.Milliseconds()), id)
}
