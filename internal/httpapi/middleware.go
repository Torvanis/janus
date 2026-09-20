package httpapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

type ctxKey string

const (
	ctxRequestID    ctxKey = "request_id"
	ctxUser         ctxKey = "user"
	ctxSession      ctxKey = "session"
	ctxToken        ctxKey = "token"
	ctxServiceToken ctxKey = "service_token"
)

// RequestIDFrom returns the correlation id attached to a request context.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// UserFrom returns the authenticated principal, or nil.
func UserFrom(ctx context.Context) *store.User {
	if v, ok := ctx.Value(ctxUser).(*store.User); ok {
		return v
	}
	return nil
}

// ServiceTokenFrom returns the service credential that authenticated the
// request, or nil for human principals. A request never carries both a user
// and a service token: they are mutually exclusive kinds of principal.
func ServiceTokenFrom(ctx context.Context) *store.ServiceToken {
	if v, ok := ctx.Value(ctxServiceToken).(*store.ServiceToken); ok {
		return v
	}
	return nil
}

// SessionFrom returns the browser session, or nil for bearer-token calls.
func SessionFrom(ctx context.Context) *auth.Session {
	if v, ok := ctx.Value(ctxSession).(*auth.Session); ok {
		return v
	}
	return nil
}

// TokenFrom returns the API credential used, or nil for browser sessions.
func TokenFrom(ctx context.Context) *store.Token {
	if v, ok := ctx.Value(ctxToken).(*store.Token); ok {
		return v
	}
	return nil
}

// responseRecorder captures status and byte count for logging and metrics.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer so SSE streams are not buffered.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RequestContext assigns a correlation id and echoes it on the response.
func RequestContext() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Janus-Request-ID")
			if id == "" {
				id = store.NewID()
			}
			w.Header().Set("X-Janus-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
		})
	}
}

// AccessLog emits one structured line per request. Secrets are never included:
// the Authorization header and all request bodies are excluded by construction.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			level := slog.LevelInfo
			if rec.status >= 500 {
				level = slog.LevelError
			} else if rec.status >= 400 {
				level = slog.LevelWarn
			}
			user := UserFrom(r.Context())
			userID := ""
			if user != nil {
				userID = user.ID
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("user_id", userID),
			)
		})
	}
}

// Recoverer converts a panic into the standard 500 envelope so a single bad
// request can never take the process down or leak a stack trace.
func Recoverer() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.ErrorContext(r.Context(), "panic recovered",
						"panic", rec, "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
					WriteError(w, r, ErrInternal())
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP resolves the caller address, honouring X-Forwarded-For only when the
// immediate peer is a configured trusted proxy.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	for _, network := range trusted {
		if network.Contains(peer) {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				parts := strings.Split(fwd, ",")
				candidate := strings.TrimSpace(parts[0])
				if net.ParseIP(candidate) != nil {
					return candidate
				}
			}
			break
		}
	}
	return host
}

// RequireUser rejects unauthenticated callers.
//
// A service token is authenticated but is NOT a user: it may reach only the
// /v1 proxy surface. Rejecting it here (rather than letting it fall through as
// "unauthenticated") gives the operator an accurate, actionable message
// instead of a misleading sign-in prompt.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if svc := ServiceTokenFrom(r.Context()); svc != nil {
			WriteError(w, r, ErrServiceTokenScope())
			return
		}
		user := UserFrom(r.Context())
		if user == nil {
			WriteError(w, r, ErrUnauthenticated())
			return
		}
		if !user.IsActive {
			WriteError(w, r, ErrUserDisabled())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin rejects callers without the admin role. Service tokens can
// never hold the admin role, and are refused with the same scope error the
// user-level guard uses.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if svc := ServiceTokenFrom(r.Context()); svc != nil {
			WriteError(w, r, ErrServiceTokenScope())
			return
		}
		user := UserFrom(r.Context())
		if user == nil {
			WriteError(w, r, ErrUnauthenticated())
			return
		}
		if !user.IsAdmin() {
			WriteError(w, r, ErrForbidden("This action requires the administrator role."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CSRFGuard enforces double-submit verification on state-changing requests that
// authenticate with an ambient cookie. Bearer-token calls carry no ambient
// authority and are therefore exempt.
func CSRFGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		session := SessionFrom(r.Context())
		if session == nil {
			next.ServeHTTP(w, r)
			return
		}
		presented := r.Header.Get(auth.CSRFHeader)
		if presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(session.CSRFToken)) != 1 {
			WriteError(w, r, ErrForbidden("This request could not be verified. Reload the page and try again."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
