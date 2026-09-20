// Package auth owns sign-in: OIDC exchange, session lifecycle, and the
// credential cache used by the proxy hot path.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SessionCookie is the browser cookie name holding the opaque session id.
const SessionCookie = "janus_session"

// CSRFCookie carries the double-submit token for state-changing form requests.
const CSRFCookie = "janus_csrf"

// CSRFHeader is the header a client must echo the CSRF cookie back in.
const CSRFHeader = "X-Janus-CSRF"

// Session is a signed-in browser session.
type Session struct {
	ID           string
	UserID       string
	CSRFToken    string
	CreatedAt    time.Time
	LastSeenAt   time.Time
	AbsoluteEnds time.Time
}

// SessionStore persists sessions. The database-backed DBSessionStore is the
// production implementation (shared across replicas); the in-memory
// implementation below backs the test suite.
type SessionStore interface {
	Create(ctx context.Context, userID string) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	Delete(ctx context.Context, id string) error
	DeleteForUser(ctx context.Context, userID string) error
	Count(ctx context.Context, userID string) (int, error)
}

// ErrNoSession indicates an absent or expired session.
var ErrNoSession = fmt.Errorf("session not found or expired")

// MemorySessionStore keeps sessions in process memory with both an absolute TTL
// and an idle timeout.
type MemorySessionStore struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	ttl         time.Duration
	idleTimeout time.Duration
	now         func() time.Time
}

// NewMemorySessionStore builds an in-process session store.
func NewMemorySessionStore(ttl, idleTimeout time.Duration) *MemorySessionStore {
	return &MemorySessionStore{
		sessions:    map[string]*Session{},
		ttl:         ttl,
		idleTimeout: idleTimeout,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// Create issues a new session for the user.
func (m *MemorySessionStore) Create(_ context.Context, userID string) (*Session, error) {
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := m.now()
	s := &Session{ID: id, UserID: userID, CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, AbsoluteEnds: now.Add(m.ttl)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = s
	return s, nil
}

// Get returns a live session and slides its idle window.
func (m *MemorySessionStore) Get(_ context.Context, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNoSession
	}
	now := m.now()
	if now.After(s.AbsoluteEnds) || now.Sub(s.LastSeenAt) > m.idleTimeout {
		delete(m.sessions, id)
		return nil, ErrNoSession
	}
	s.LastSeenAt = now
	copied := *s
	return &copied, nil
}

// Delete ends one session.
func (m *MemorySessionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

// DeleteForUser ends every session a user holds ("sign out everywhere").
func (m *MemorySessionStore) DeleteForUser(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, id)
		}
	}
	return nil
}

// Count reports how many live sessions a user holds.
func (m *MemorySessionStore) Count(_ context.Context, userID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	now := m.now()
	for _, s := range m.sessions {
		if s.UserID == userID && now.Before(s.AbsoluteEnds) {
			n++
		}
	}
	return n, nil
}

// NewRandomToken returns a 256-bit URL-safe random token.
func NewRandomToken() (string, error) { return randomToken() }

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SetSessionCookies writes the session and CSRF cookies with the flags required
// by the security spec: Secure (when served over TLS), HttpOnly on the session,
// and SameSite=Lax on both.
func SetSessionCookies(w http.ResponseWriter, s *Session, secure bool, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: s.ID, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()),
	})
	// The CSRF cookie is deliberately readable by the app so it can echo the
	// value back in a header (double-submit); it carries no authority alone.
	http.SetCookie(w, &http.Cookie{
		Name: CSRFCookie, Value: s.CSRFToken, Path: "/", HttpOnly: false, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()),
	})
}

// ClearSessionCookies removes both cookies on sign-out.
func ClearSessionCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
}
