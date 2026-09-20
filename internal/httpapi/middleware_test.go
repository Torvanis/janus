package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/torvanis/janus/internal/auth"
)

// doAsSessionCSRF issues an app-API request authenticated by session cookie
// with an arbitrary CSRF header value; the empty string sends no header at
// all. This deliberately bypasses doAsSession, which always sends the correct
// token.
func (h *harness) doAsSessionCSRF(session *auth.Session, method, path, csrf string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session.ID})
	if csrf != "" {
		req.Header.Set(auth.CSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) tokenCount(t *testing.T) int {
	t.Helper()
	tokens, err := h.store.ListTokens(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	return len(tokens)
}

// TestCSRFGuardRejectsSessionMutations locks the double-submit contract: a
// state-changing request authenticated only by the ambient session cookie is
// refused unless it also presents the session's CSRF token, and the refusal
// has no side effect. A refactor that drops CSRFGuard from the /api/v1 chain
// turns this red.
func TestCSRFGuardRejectsSessionMutations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createBody := map[string]any{"description": "csrf test token"}
	before := h.tokenCount(t)

	// Missing header → 403, nothing created.
	rec := h.doAsSessionCSRF(session, http.MethodPost, "/api/v1/tokens", "", createBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mutation without CSRF header = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != CodePermission {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, CodePermission)
	}
	if got := h.tokenCount(t); got != before {
		t.Fatalf("a rejected mutation still created a token: %d -> %d", before, got)
	}

	// Wrong value → 403, nothing created. Also covers the constant-time
	// comparison branch with equal-length garbage.
	for _, wrong := range []string{"forged-token", session.CSRFToken + "x", session.CSRFToken[:len(session.CSRFToken)-1] + "!"} {
		rec = h.doAsSessionCSRF(session, http.MethodPost, "/api/v1/tokens", wrong, createBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("mutation with wrong CSRF %q = %d, want 403: %s", wrong, rec.Code, rec.Body.String())
		}
	}
	if got := h.tokenCount(t); got != before {
		t.Fatalf("a rejected mutation still created a token: %d -> %d", before, got)
	}

	// DELETE is guarded the same way as POST.
	rec = h.doAsSessionCSRF(session, http.MethodDelete, "/api/v1/tokens/some-id", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE without CSRF header = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Positive control: the correct token is accepted and the mutation lands.
	rec = h.doAsSessionCSRF(session, http.MethodPost, "/api/v1/tokens", session.CSRFToken, createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mutation with correct CSRF = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := h.tokenCount(t); got != before+1 {
		t.Fatalf("token count after valid create = %d, want %d", got, before+1)
	}
}

// TestCSRFGuardExemptsSafeMethodsAndBearerCalls locks the two intentional
// bypasses: reads carry no CSRF risk, and bearer-token requests carry no
// ambient cookie authority so the double-submit check does not apply.
func TestCSRFGuardExemptsSafeMethodsAndBearerCalls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// GET with a session cookie and no CSRF header is allowed.
	rec := h.doAsSessionCSRF(session, http.MethodGet, "/api/v1/tokens", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session GET without CSRF header = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// A bearer-token mutation needs no CSRF header (h.token is the harness
	// user's plaintext credential).
	body, _ := json.Marshal(map[string]any{"description": "bearer-created token"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	bearerRec := httptest.NewRecorder()
	h.handler.ServeHTTP(bearerRec, req)
	if bearerRec.Code != http.StatusCreated {
		t.Fatalf("bearer mutation without CSRF header = %d, want 201: %s", bearerRec.Code, bearerRec.Body.String())
	}
}
