package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/store"
)

// Fail-closed audit (STIG profile).
//
// Without the profile, an audit write failure is logged loudly and never
// hides the change (see Server.audit). With JANUS_STIG_PROFILE=true every
// state-changing request under /api/v1 and /scim/v2 runs in one database
// transaction (store.BeginRequestTx), with its response buffered:
//
//   - the handler's writes and its audit rows share the transaction;
//   - if any audit write fails, or the commit fails, the transaction rolls
//     back and the client gets 500 audit.write_failed instead of the
//     buffered success response, so the change does not persist and no one
//     is told it did;
//   - otherwise the transaction commits and the buffered response is sent.
//
// The transaction commits whatever status the handler chose (a 4xx keeps
// what autocommit would have kept); only a failed audit write or a failed
// commit rolls it back. Requests that cannot run in one transaction are
// excluded and keep today's behavior; auditExempt lists them with the reason.

// CodeAuditWriteFailed is returned in the STIG profile when an
// administrative change was refused because its audit record could not be
// written.
const CodeAuditWriteFailed = "audit.write_failed"

// ErrAuditWriteFailed is the response for a change refused because its
// audit record could not be written.
func ErrAuditWriteFailed() *APIError {
	return newError(http.StatusInternalServerError, CodeAuditWriteFailed, "server_error",
		"The change was not saved: the STIG profile requires every administrative change to be recorded in the audit log, and the audit record could not be written. Check the database, then try again.")
}

type auditStateKey struct{}

// auditState tracks audit failures for one fail-closed request, and work
// to start only once its transaction has committed.
type auditState struct {
	failed atomic.Bool
	post   []func()
}

// afterCommit runs fn once the request's changes are visible to other
// connections: at once outside a fail-closed request, after the commit
// inside one, and never when the change is refused. Use it for background
// work that reads what the handler just wrote (discovery of a new
// upstream, say).
func (s *Server) afterCommit(ctx context.Context, fn func()) {
	if st := auditStateFrom(ctx); st != nil {
		st.post = append(st.post, fn)
		return
	}
	fn()
}

func auditStateFrom(ctx context.Context) *auditState {
	st, _ := ctx.Value(auditStateKey{}).(*auditState)
	return st
}

// noteAuditFailure marks the request's change as unsafe to commit. It
// reports whether the request is fail-closed (the caller then need not do
// anything else: the middleware refuses the change).
func noteAuditFailure(ctx context.Context) bool {
	if st := auditStateFrom(ctx); st != nil {
		st.failed.Store(true)
		return true
	}
	return false
}

// appendAudit writes entry and, in a fail-closed request, marks the request
// failed when the write fails. Call sites that wrote audit rows with
// Store.AppendAudit directly use it so they are covered too.
func (s *Server) appendAudit(ctx context.Context, entry *store.AuditEntry) error {
	err := s.Store.AppendAudit(ctx, entry)
	if err != nil {
		noteAuditFailure(ctx)
	}
	return err
}

// auditExempt reports requests the fail-closed wrapper must not hold in a
// transaction, because their handler hands the request context to
// concurrent or background work that would race the transaction, or they
// only exercise configuration without changing it.
func auditExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/v1/chat/") && !strings.Contains(p, "/shares"):
		// Chat: replies run in goroutines with the request context; chat
		// messages are user content, not administrative changes, and have
		// no audit rows. Share links (audited) stay covered.
		return true
	case strings.HasSuffix(p, "/test"), strings.HasSuffix(p, "/test-batch"), strings.HasSuffix(p, "/dry-run"), strings.HasSuffix(p, "/search-test"):
		// Dry runs: classifier fan-out in goroutines; nothing is saved
		// (storage/test and directory/test audit the probe, and a probe
		// that cannot be audited changes nothing).
		return true
	case p == "/api/v1/admin/classification/benchmarks":
		// Starts a background run that updates its own row from another
		// goroutine; not an administrative change.
		return true
	case strings.HasPrefix(p, "/api/v1/me/subscriptions/"):
		// Device sign-in polling and model refresh call vendors and touch
		// rows from goroutines; personal, not administrative.
		return true
	}
	return false
}

// failClosedAudit is the middleware. onFail writes the refusal in the
// surface's own error format.
func (s *Server) failClosedAudit(onFail func(http.ResponseWriter, *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if !stigprofile.Enabled() || auditExempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, finish, err := s.Store.BeginRequestTx(r.Context())
			if err != nil {
				s.Logger.ErrorContext(r.Context(), "fail-closed audit: begin transaction", "error", err.Error(), "path", r.URL.Path)
				onFail(w, r)
				return
			}
			st := &auditState{}
			ctx = context.WithValue(ctx, auditStateKey{}, st)
			buf := &bufferedResponse{header: http.Header{}}
			committed := false
			defer func() {
				if !committed {
					// Panic or early return: never leave the change behind.
					_ = finish(false)
					s.dropCaches(true)
				}
			}()
			next.ServeHTTP(buf, r.WithContext(ctx))
			if st.failed.Load() {
				_ = finish(false)
				committed = true
				s.dropCaches(true)
				s.Logger.ErrorContext(r.Context(), "administrative change refused: audit record could not be written", "path", r.URL.Path, "method", r.Method)
				onFail(w, r)
				return
			}
			// Otherwise commit whatever the handler's status: a refusal
			// (4xx) keeps exactly what autocommit would have kept, such as
			// a failed-attempt counter. Each failed statement was already
			// rolled back to its savepoint.
			if err := finish(true); err != nil {
				committed = true
				s.dropCaches(true)
				s.Logger.ErrorContext(r.Context(), "administrative change refused: commit failed", "error", err.Error(), "path", r.URL.Path)
				onFail(w, r)
				return
			}
			committed = true
			// Caches this replica refreshed while the change was still
			// uncommitted (a concurrent request reading the old rows, say)
			// are dropped now that it is visible.
			s.dropCaches(false)
			for _, fn := range st.post {
				fn()
			}
			buf.flushTo(w)
		})
	}
}

// dropCaches drops in-memory state that may disagree with the database
// after a request transaction ends: a rolled-back handler may have refreshed
// caches from its own uncommitted writes, and a concurrent request may have
// cached the old rows before the commit. After a rollback the license state
// is reloaded too (a refused key install must not stay in force).
func (s *Server) dropCaches(rolledBack bool) {
	s.Store.InvalidateModelRevision()
	s.InvalidateConfigCache()
	s.InvalidateTokenCache()
	s.invalidateQuotaRules()
	s.invalidateChatAdmin()
	if rolledBack && s.License != nil {
		ctx, cancel := context.WithTimeout(context.Background(), licenseRefreshTimeout)
		defer cancel()
		_ = s.License.Refresh(ctx)
	}
}

const licenseRefreshTimeout = 5 * time.Second

// bufferedResponse holds a handler's response until the transaction
// commits. Headers are kept apart from the real writer's so a refused change
// sends none of them (a Set-Cookie for a session the rollback undid, say).
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, v := range b.header {
		dst[k] = v
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.body.Bytes())
}
