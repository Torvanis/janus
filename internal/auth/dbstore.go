package auth

import (
	"context"
	"errors"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// DBSessionStore persists browser sessions in the shared database so every
// replica sees the same sign-in state: sessions survive a pod restart and a
// request can be served by any replica.
type DBSessionStore struct {
	db          *store.Store
	ttl         time.Duration
	idleTimeout time.Duration
	now         func() time.Time
}

// NewDBSessionStore builds the database-backed session store.
func NewDBSessionStore(db *store.Store, ttl, idleTimeout time.Duration) *DBSessionStore {
	return &DBSessionStore{db: db, ttl: ttl, idleTimeout: idleTimeout, now: func() time.Time { return time.Now().UTC() }}
}

// Create issues a new session for the user.
func (d *DBSessionStore) Create(ctx context.Context, userID string) (*Session, error) {
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := d.now()
	ws := &store.WebSession{
		ID: id, UserID: userID, CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now, AbsoluteEnds: now.Add(d.ttl),
	}
	if err := d.db.InsertWebSession(ctx, ws); err != nil {
		return nil, err
	}
	return &Session{ID: ws.ID, UserID: ws.UserID, CSRFToken: ws.CSRFToken, CreatedAt: ws.CreatedAt, LastSeenAt: ws.LastSeenAt, AbsoluteEnds: ws.AbsoluteEnds}, nil
}

// Get returns a live session and slides its idle window.
func (d *DBSessionStore) Get(ctx context.Context, id string) (*Session, error) {
	ws, err := d.db.WebSessionByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	now := d.now()
	if now.After(ws.AbsoluteEnds) || now.Sub(ws.LastSeenAt) > d.idleTimeout {
		_ = d.db.DeleteWebSession(ctx, id)
		return nil, ErrNoSession
	}
	// Slide the idle window. Failing to record the touch must not sign the
	// user out mid-request; the next successful touch catches up.
	if err := d.db.TouchWebSession(ctx, id, now); err == nil {
		ws.LastSeenAt = now
	}
	return &Session{ID: ws.ID, UserID: ws.UserID, CSRFToken: ws.CSRFToken, CreatedAt: ws.CreatedAt, LastSeenAt: ws.LastSeenAt, AbsoluteEnds: ws.AbsoluteEnds}, nil
}

// Delete ends one session.
func (d *DBSessionStore) Delete(ctx context.Context, id string) error {
	return d.db.DeleteWebSession(ctx, id)
}

// DeleteForUser ends every session a user holds ("sign out everywhere"), on
// every replica at once.
func (d *DBSessionStore) DeleteForUser(ctx context.Context, userID string) error {
	return d.db.DeleteWebSessionsForUser(ctx, userID)
}

// Count reports how many live sessions a user holds.
func (d *DBSessionStore) Count(ctx context.Context, userID string) (int, error) {
	return d.db.CountWebSessions(ctx, userID, d.now())
}

// DBOIDCStateStore persists in-flight sign-ins in the shared database so the
// OIDC callback can complete on a replica other than the one that issued the
// state.
type DBOIDCStateStore struct {
	db  *store.Store
	now func() time.Time
}

// NewDBOIDCStateStore builds the database-backed OIDC state store.
func NewDBOIDCStateStore(db *store.Store) *DBOIDCStateStore {
	return &DBOIDCStateStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// Save records a pending sign-in.
func (d *DBOIDCStateStore) Save(ctx context.Context, st *AuthState) error {
	return d.db.InsertOIDCState(ctx, &store.OIDCState{
		State: st.State, Verifier: st.Verifier, Nonce: st.Nonce,
		RedirectTo: st.RedirectTo, RedirectURI: st.RedirectURI,
		CreatedAt: st.CreatedAt, ExpiresAt: st.ExpiresAt,
	})
}

// Consume atomically removes and returns a pending sign-in; replayed or
// expired states yield ErrStateNotFound.
func (d *DBOIDCStateStore) Consume(ctx context.Context, state string) (*AuthState, error) {
	st, err := d.db.ConsumeOIDCState(ctx, state, d.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, err
	}
	return &AuthState{
		State: st.State, Verifier: st.Verifier, Nonce: st.Nonce,
		RedirectTo: st.RedirectTo, RedirectURI: st.RedirectURI,
		CreatedAt: st.CreatedAt, ExpiresAt: st.ExpiresAt,
	}, nil
}
