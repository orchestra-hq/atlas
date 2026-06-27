// Package anthropic holds the wire types for Atlas's first-class API surface
// (POST /v1/messages and friends — see docs/internal/api-surface.md and ADR-0002) and
// the translation between those shapes and internal/core. The gateway in
// internal/server speaks HTTP; this package owns what the bytes look like.
package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// ErrorType is the Anthropic error-envelope vocabulary. Status codes pair
// with types per docs/internal/api-surface.md so SDK retry logic behaves.
type ErrorType string

// Anthropic error-envelope types (docs/internal/api-surface.md).
const (
	ErrInvalidRequest ErrorType = "invalid_request_error"
	ErrAuthentication ErrorType = "authentication_error"
	ErrPermission     ErrorType = "permission_error"
	ErrNotFound       ErrorType = "not_found_error"
	ErrRateLimit      ErrorType = "rate_limit_error"
	ErrAPI            ErrorType = "api_error"
	ErrOverloaded     ErrorType = "overloaded_error"
)

// Error is a request failure that maps onto the Anthropic error envelope.
// RetryAfter, when > 0, is emitted as a Retry-After header (seconds) — set on the
// retryable 429 rate_limit_error and 529 overloaded_error so SDK backoff has a hint
// (ADR-0010).
type Error struct {
	Status     int
	Type       ErrorType
	Msg        string
	RetryAfter int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Msg)
}

// ErrInvalid builds a 400 invalid_request_error.
func ErrInvalid(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: ErrInvalidRequest, Msg: fmt.Sprintf(format, args...)}
}

type errorEnvelope struct {
	Type  string    `json:"type"`
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type    ErrorType `json:"type"`
	Message string    `json:"message"`
}

// WriteError writes e as an Anthropic error envelope:
// {"type":"error","error":{"type":...,"message":...}}. A positive RetryAfter is
// emitted as a Retry-After header (seconds) before the body is written.
func WriteError(w http.ResponseWriter, e *Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	WriteJSON(w, e.Status, errorEnvelope{Type: "error", Error: errorBody{Type: e.Type, Message: e.Msg}})
}

// WriteJSON writes payload with the given status and a JSON content type.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshaling our own wire structs cannot fail; guard anyway.
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"response encoding failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
