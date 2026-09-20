package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WebSession is a signed-in browser session persisted in the shared database so
// every replica sees the same sign-in state.
type WebSession struct {
	ID           string
	UserID       string
	CSRFToken    string
	CreatedAt    time.Time
	LastSeenAt   time.Time
	AbsoluteEnds time.Time
}

// InsertWebSession stores a freshly created session.
func (s *Store) InsertWebSession(ctx context.Context, ws *WebSession) error {
	return s.exec(ctx,
		`INSERT INTO web_session (id, user_id, csrf_token, created_at, last_seen_at, absolute_ends) VALUES (?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.UserID, ws.CSRFToken, FormatTime(ws.CreatedAt), FormatTime(ws.LastSeenAt), FormatTime(ws.AbsoluteEnds))
}

// WebSessionByID returns one session, expired or not; the caller applies the
// TTL/idle policy so the policy lives in exactly one place (internal/auth).
func (s *Store) WebSessionByID(ctx context.Context, id string) (*WebSession, error) {
	row := s.queryRow(ctx,
		`SELECT id, user_id, csrf_token, created_at, last_seen_at, absolute_ends FROM web_session WHERE id = ?`, id)
	var ws WebSession
	var created, seen, ends string
	if err := row.Scan(&ws.ID, &ws.UserID, &ws.CSRFToken, &created, &seen, &ends); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	ws.CreatedAt, ws.LastSeenAt, ws.AbsoluteEnds = ParseTime(created), ParseTime(seen), ParseTime(ends)
	return &ws, nil
}

// TouchWebSession slides the idle window.
func (s *Store) TouchWebSession(ctx context.Context, id string, seenAt time.Time) error {
	return s.exec(ctx, `UPDATE web_session SET last_seen_at = ? WHERE id = ?`, FormatTime(seenAt), id)
}

// DeleteWebSession ends one session.
func (s *Store) DeleteWebSession(ctx context.Context, id string) error {
	return s.exec(ctx, `DELETE FROM web_session WHERE id = ?`, id)
}

// DeleteWebSessionsForUser ends every session a user holds ("sign out everywhere").
func (s *Store) DeleteWebSessionsForUser(ctx context.Context, userID string) error {
	return s.exec(ctx, `DELETE FROM web_session WHERE user_id = ?`, userID)
}

// CountWebSessions reports how many unexpired sessions a user holds.
func (s *Store) CountWebSessions(ctx context.Context, userID string, now time.Time) (int, error) {
	row := s.queryRow(ctx,
		`SELECT COUNT(*) FROM web_session WHERE user_id = ? AND absolute_ends > ?`, userID, FormatTime(now))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return n, nil
}

// PurgeExpiredWebSessions removes sessions past their absolute end.
func (s *Store) PurgeExpiredWebSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM web_session WHERE absolute_ends <= ?`), FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// OIDCState is one in-flight sign-in: the PKCE verifier, nonce, and redirect
// captured when the browser was sent to the identity provider. Persisting it
// lets the callback complete on any replica.
type OIDCState struct {
	State       string
	Verifier    string
	Nonce       string
	RedirectTo  string
	RedirectURI string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// InsertOIDCState records a sign-in that has been sent to the provider.
func (s *Store) InsertOIDCState(ctx context.Context, st *OIDCState) error {
	return s.exec(ctx,
		`INSERT INTO oidc_state (state, verifier, nonce, redirect_to, redirect_uri, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		st.State, st.Verifier, st.Nonce, st.RedirectTo, st.RedirectURI, FormatTime(st.CreatedAt), FormatTime(st.ExpiresAt))
}

// ConsumeOIDCState atomically removes and returns one pending sign-in, so a
// state value can be used exactly once even when callbacks race across
// replicas. Expired or absent states return ErrNotFound.
func (s *Store) ConsumeOIDCState(ctx context.Context, state string, now time.Time) (*OIDCState, error) {
	// DELETE ... RETURNING is supported by both PostgreSQL and SQLite >= 3.35
	// and is what makes the consume atomic without an explicit transaction.
	row := s.queryRow(ctx,
		`DELETE FROM oidc_state WHERE state = ? RETURNING verifier, nonce, redirect_to, redirect_uri, created_at, expires_at`, state)
	st := &OIDCState{State: state}
	var created, expires string
	if err := row.Scan(&st.Verifier, &st.Nonce, &st.RedirectTo, &st.RedirectURI, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consume oidc state: %w", err)
	}
	st.CreatedAt, st.ExpiresAt = ParseTime(created), ParseTime(expires)
	if !st.ExpiresAt.After(now) {
		return nil, ErrNotFound
	}
	return st, nil
}

// PurgeExpiredOIDCStates removes abandoned sign-ins.
func (s *Store) PurgeExpiredOIDCStates(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM oidc_state WHERE expires_at <= ?`), FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("purge oidc states: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
