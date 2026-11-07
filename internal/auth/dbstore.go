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
//
// Limits come from the administrator's security policy (idle time by role,
// absolute lifetime), read on every request so a change applies to sessions
// already open. ttl and idleTimeout (JANUS_SESSION_TTL_HOURS,
// JANUS_SESSION_IDLE_TIMEOUT_HOURS) are upper bounds the policy cannot
// exceed.
type DBSessionStore struct {
	db          *store.Store
	ttl         time.Duration
	idleTimeout time.Duration
	now         func() time.Time
	// Policy, when set, returns the session policy in force (tests set a
	// fixed one); default: the store's security policy.
	Policy func(ctx context.Context) store.SessionPolicy
}

// NewDBSessionStore builds the database-backed session store.
func NewDBSessionStore(db *store.Store, ttl, idleTimeout time.Duration) *DBSessionStore {
	d := &DBSessionStore{db: db, ttl: ttl, idleTimeout: idleTimeout, now: func() time.Time { return time.Now().UTC() }}
	d.Policy = func(ctx context.Context) store.SessionPolicy { return db.SecurityPolicy(ctx).Sessions }
	return d
}

// limits returns the absolute lifetime and the idle timeout for a user.
func (d *DBSessionStore) limits(ctx context.Context, admin bool) (absolute, idle time.Duration) {
	absolute, idle = d.ttl, d.idleTimeout
	if d.Policy == nil {
		return
	}
	p := d.Policy(ctx)
	if v := time.Duration(p.AbsoluteMinutes) * time.Minute; v > 0 && v < absolute {
		absolute = v
	}
	m := p.IdleMinutesUser
	if admin {
		m = p.IdleMinutesAdmin
	}
	if v := time.Duration(m) * time.Minute; v > 0 && v < idle {
		idle = v
	}
	return
}

// Limits reports the absolute lifetime and idle timeout that apply to a
// user now (the SPA warns before the idle sign-out).
func (d *DBSessionStore) Limits(ctx context.Context, admin bool) (absolute, idle time.Duration) {
	return d.limits(ctx, admin)
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
	absolute, _ := d.limits(ctx, false)
	ws := &store.WebSession{
		ID: id, UserID: userID, CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now, AbsoluteEnds: now.Add(absolute),
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
	admin := false
	if u, err := d.db.UserByID(ctx, ws.UserID); err == nil {
		admin = u.IsAdmin()
	}
	absolute, idle := d.limits(ctx, admin)
	ends := ws.AbsoluteEnds
	if e := ws.CreatedAt.Add(absolute); e.Before(ends) {
		ends = e // the policy was shortened after the session began
	}
	switch {
	case now.After(ends):
		_ = d.db.DeleteWebSession(ctx, id)
		return nil, &TimeoutError{UserID: ws.UserID, Reason: "expired"}
	case now.Sub(ws.LastSeenAt) > idle:
		_ = d.db.DeleteWebSession(ctx, id)
		return nil, &TimeoutError{UserID: ws.UserID, Reason: "idle"}
	}
	// Slide the idle window, unless this is the app polling in the
	// background (X-Janus-Activity: background): a dashboard left open
	// must still time out. Failing to record the touch must not sign the
	// user out mid-request; the next successful touch catches up.
	if !IsBackground(ctx) {
		if err := d.db.TouchWebSession(ctx, id, now); err == nil {
			ws.LastSeenAt = now
		}
	}
	return &Session{ID: ws.ID, UserID: ws.UserID, CSRFToken: ws.CSRFToken, CreatedAt: ws.CreatedAt, LastSeenAt: ws.LastSeenAt, AbsoluteEnds: ends, IdleTimeout: idle}, nil
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
