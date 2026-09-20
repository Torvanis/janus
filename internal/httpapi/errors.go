// Package httpapi holds the HTTP surface: the shared error envelope, request
// middleware, and every route handler. Handlers stay thin — decode, validate,
// call a service, encode.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Error codes. Every failure the gateway originates carries one of these so a
// downstream client can branch on `error.code` instead of parsing prose.
const (
	CodeQuotaExceeded   = "policy.quota_exceeded"
	CodeUserDisabled    = "policy.user_disabled"
	CodeModelNotGranted = "policy.model_not_granted"
	CodeEndpointBlocked = "policy.endpoint_blocked"
	CodeTokenInvalid    = "policy.token_invalid"
	CodeRateLimit       = "policy.rate_limit"
	CodeUpstreamDown    = "upstream.unavailable"
	// CodeResponseTooLarge marks a response (or stream) cut by the gateway's
	// JANUS_MAX_RESPONSE_BYTES cap.
	CodeResponseTooLarge = "policy.response_too_large"
	CodeUpstreamLimit    = "upstream.rate_limit"
	// CodeServiceTokenScope marks an attempt to use a service token outside
	// the /v1 proxy surface it is scoped to.
	CodeServiceTokenScope = "policy.service_token_scope"
	// CodeServiceTokenExpired marks a credential whose configured lifetime
	// has elapsed. It is distinct from a revocation so an operator can tell
	// "someone turned this off" from "this aged out".
	CodeServiceTokenExpired = "policy.service_token_expired"
	// CodeManagedModelBroken marks a managed model whose underlying target
	// is missing or disabled — a configuration fault, not a caller error.
	CodeManagedModelBroken = "policy.managed_model_unavailable"
	// CodeManagedModelFallbackExhausted marks a managed model whose target
	// AND configured fallback are both unavailable: the admin set up a safety
	// net for exactly this case and it has failed too.
	CodeManagedModelFallbackExhausted = "policy.managed_model_fallback_exhausted"
	CodeInvalidRequest                = "invalid_request_error"
	CodeAuthentication                = "authentication_error"
	CodePermission                    = "permission_error"
	CodeNotFound                      = "not_found_error"
	CodeServerError                   = "server_error"
)

// APIError is the OpenAI-compatible error envelope. It is the only shape any
// failure may take, on both the proxy surface and the app API.
type APIError struct {
	Message    string `json:"message"`
	Code       string `json:"code"`
	Type       string `json:"type"`
	Param      string `json:"param,omitempty"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	ResetAt    string `json:"reset_at,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	DocsURL    string `json:"docs_url,omitempty"`

	status int
}

// Error implements the error interface.
func (e *APIError) Error() string { return e.Message }

// Status reports the HTTP status this error maps to.
func (e *APIError) Status() int {
	if e.status == 0 {
		return http.StatusInternalServerError
	}
	return e.status
}

// WithReason attaches an operator-authored explanation (used by policy rules).
func (e *APIError) WithReason(reason string) *APIError { e.Reason = reason; return e }

// WithParam names the offending request field for validation failures.
func (e *APIError) WithParam(param string) *APIError { e.Param = param; return e }

// WithReset attaches the instant a quota or rate limit frees up.
func (e *APIError) WithReset(at time.Time) *APIError {
	if at.IsZero() {
		return e
	}
	e.ResetAt = at.UTC().Format(time.RFC3339)
	if secs := int(time.Until(at).Seconds()); secs > 0 {
		e.RetryAfter = secs
	}
	return e
}

func newError(status int, code, errType, message string) *APIError {
	return &APIError{Message: message, Code: code, Type: errType, status: status, DocsURL: "/docs/api/errors#" + code}
}

// Errorf builders. Messages always tell the caller what to do next.

// ErrQuotaExceeded reports a breached usage limit.
func ErrQuotaExceeded(message string) *APIError {
	return newError(http.StatusTooManyRequests, CodeQuotaExceeded, "rate_limit_error", message)
}

// ErrUserDisabled reports a deactivated account.
func ErrUserDisabled() *APIError {
	return newError(http.StatusForbidden, CodeUserDisabled, "permission_error",
		"Your account has been disabled. Contact a Janus administrator to restore access.")
}

// ErrModelNotGranted reports a missing model grant.
func ErrModelNotGranted(model string) *APIError {
	msg := "You do not have access to this model. Ask an administrator to grant it, or call /v1/models to list the models available to you."
	if model != "" {
		msg = "You do not have access to the model " + model + ". Ask an administrator to grant it, or call /v1/models to list the models available to you."
	}
	return newError(http.StatusForbidden, CodeModelNotGranted, "permission_error", msg)
}

// ErrEndpointBlocked reports a policy-rule match.
func ErrEndpointBlocked() *APIError {
	return newError(http.StatusForbidden, CodeEndpointBlocked, "permission_error",
		"This request was blocked by a gateway policy rule. Contact a Janus administrator if you believe this is a mistake.")
}

// ErrTokenInvalid reports a missing, malformed, or revoked credential.
func ErrTokenInvalid(detail string) *APIError {
	if detail == "" {
		detail = "The API token is missing, revoked, or invalid. Generate a new token from the Janus tokens page."
	}
	return newError(http.StatusUnauthorized, CodeTokenInvalid, "authentication_error", detail)
}

// ErrRateLimited reports a per-endpoint rate-limit breach.
func ErrRateLimited(message string) *APIError {
	return newError(http.StatusTooManyRequests, CodeRateLimit, "rate_limit_error", message)
}

// ErrServiceTokenScope reports a service token used outside the inference
// surface it is scoped to. Service tokens authenticate unattended integrations
// against /v1 only; they deliberately cannot read dashboards, list users, or
// reach any administrative endpoint.
func ErrServiceTokenScope() *APIError {
	return newError(http.StatusForbidden, CodeServiceTokenScope, "permission_error",
		"Service tokens may only be used on the /v1 inference API. Sign in with a user account to use the Janus web API.")
}

// ErrServiceTokenExpired reports a credential past its configured expiry.
func ErrServiceTokenExpired(expiredAt time.Time) *APIError {
	err := newError(http.StatusUnauthorized, CodeServiceTokenExpired, "authentication_error",
		"This service token expired on "+expiredAt.UTC().Format(time.RFC3339)+
			". Ask a Janus administrator to issue a replacement.")
	return err
}

// ErrManagedModelUnavailable reports that a managed model exists but cannot
// serve traffic because its underlying model is missing or not enabled.
//
// This is deliberately NOT "model not found": the caller's configuration is
// correct and there is nothing they can do about it, so the message names the
// alias, says the fault is in the gateway's configuration, and points at the
// administrator rather than implying a typo.
func ErrManagedModelUnavailable(alias, reason string) *APIError {
	msg := "The managed model " + alias + " is not currently available: " + reason +
		". This is a gateway configuration issue — contact a Janus administrator."
	return newError(http.StatusServiceUnavailable, CodeManagedModelBroken, "server_error", msg)
}

// ErrManagedModelFallbackExhausted reports that a managed model's target is
// unavailable and the fallback configured for that case cannot take over
// either. Both reasons are spelled out so the administrator reading the
// request log knows which two things to fix.
func ErrManagedModelFallbackExhausted(alias, primaryReason, fallbackReason string) *APIError {
	msg := "The managed model " + alias + " is not currently available: " + primaryReason +
		", and its fallback cannot take over: " + fallbackReason +
		". This is a gateway configuration issue — contact a Janus administrator."
	return newError(http.StatusServiceUnavailable, CodeManagedModelFallbackExhausted, "server_error", msg)
}

// ErrUpstreamUnavailable reports an unreachable provider.
func ErrUpstreamUnavailable(name string) *APIError {
	return newError(http.StatusServiceUnavailable, CodeUpstreamDown, "server_error",
		"The upstream provider "+name+" could not be reached. Retry shortly; if this persists, contact a Janus administrator.")
}

// ErrPayloadTooLarge reports a request body that exceeds what the gateway can
// buffer for an adapter that must rewrite it before forwarding.
func ErrPayloadTooLarge(limit int64) *APIError {
	return newError(http.StatusRequestEntityTooLarge, CodeInvalidRequest, "invalid_request_error",
		"The request body exceeds the "+itoa(int(limit>>20))+" MB limit this provider adapter can rewrite. Reduce the payload size.")
}

// ErrInvalidRequest reports a malformed or incomplete request body.
func ErrInvalidRequest(message string) *APIError {
	return newError(http.StatusBadRequest, CodeInvalidRequest, "invalid_request_error", message)
}

// ErrConflict reports a request that is well-formed but cannot proceed
// because of the current state of other resources (dependents that must be
// dealt with first). Callers name the dependents in the message and, where
// the operation supports it, the explicit override that bypasses the check.
func ErrConflict(message string) *APIError {
	return newError(http.StatusConflict, CodeInvalidRequest, "invalid_request_error", message)
}

// ErrUnauthenticated reports a missing session.
func ErrUnauthenticated() *APIError {
	return newError(http.StatusUnauthorized, CodeAuthentication, "authentication_error",
		"You are not signed in. Sign in with your corporate account to continue.")
}

// ErrForbidden reports insufficient role.
func ErrForbidden(message string) *APIError {
	if message == "" {
		message = "You do not have permission to perform this action."
	}
	return newError(http.StatusForbidden, CodePermission, "permission_error", message)
}

// ErrNotFoundf reports a missing resource.
func ErrNotFoundf(what string) *APIError {
	return newError(http.StatusNotFound, CodeNotFound, "invalid_request_error", what+" was not found.")
}

// ErrInternal reports an unexpected failure without leaking internals.
func ErrInternal() *APIError {
	return newError(http.StatusInternalServerError, CodeServerError, "server_error",
		"Something went wrong inside the gateway. Retry, and quote the request ID if you contact support.")
}

// WriteError renders any error as the canonical envelope. Non-APIError values
// are logged with their detail and reported to the client as a generic 500, so
// stack traces and internal messages never reach a browser.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		slog.ErrorContext(r.Context(), "unhandled error", "error", err.Error(), "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
		apiErr = ErrInternal()
	}
	apiErr.RequestID = RequestIDFrom(r.Context())
	if apiErr.RetryAfter > 0 {
		w.Header().Set("Retry-After", itoa(apiErr.RetryAfter))
	}
	writeJSON(w, apiErr.Status(), map[string]any{"error": apiErr})
}

// WriteJSON renders a successful response.
func WriteJSON(w http.ResponseWriter, status int, payload any) { writeJSON(w, status, payload) }

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("write json response", "error", err.Error())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := [20]byte{}
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
