package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ErrorType is the OpenAI error-envelope "type" vocabulary. The OpenAI SDKs key
// their typed exceptions off the HTTP status, not this string, but real servers
// populate it, so Atlas does too.
type ErrorType string

// OpenAI error-envelope types.
const (
	ErrInvalidRequest ErrorType = "invalid_request_error"
	ErrAuthentication ErrorType = "authentication_error"
	ErrNotFound       ErrorType = "not_found_error"
	ErrAPI            ErrorType = "api_error"
	ErrOverloaded     ErrorType = "overloaded_error"
)

// Error is a request failure that maps onto the OpenAI error envelope.
type Error struct {
	Status int
	Type   ErrorType
	Code   string
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Msg)
}

// ErrInvalid builds a 400 invalid_request_error.
func ErrInvalid(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: ErrInvalidRequest, Msg: fmt.Sprintf(format, args...)}
}

// errorEnvelope is OpenAI's error shape: {"error":{"message","type","code"}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string    `json:"message"`
	Type    ErrorType `json:"type"`
	Code    *string   `json:"code"`
	Param   *string   `json:"param"`
}

// WriteError writes e as an OpenAI error envelope.
func WriteError(w http.ResponseWriter, e *Error) {
	body := errorBody{Message: e.Msg, Type: e.Type}
	if e.Code != "" {
		body.Code = &e.Code
	}
	WriteJSON(w, e.Status, errorEnvelope{Error: body})
}

// WriteJSON writes payload with the given status and a JSON content type.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"type":"api_error","message":"response encoding failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
