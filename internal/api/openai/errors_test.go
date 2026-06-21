package openai

import (
	"net/http/httptest"
	"testing"
)

// TestWriteError_retryAfterHeader: a positive RetryAfter is emitted as a
// Retry-After header, mirroring the Anthropic surface on a retryable 429/529.
func TestWriteError_retryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, &Error{Status: 429, Type: ErrRateLimit, Msg: "slow down", RetryAfter: 4})
	if got := rec.Header().Get("Retry-After"); got != "4" {
		t.Fatalf("Retry-After = %q, want \"4\"", got)
	}

	rec = httptest.NewRecorder()
	WriteError(rec, &Error{Status: 400, Type: ErrInvalidRequest, Msg: "bad"})
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q on a non-retryable error, want empty", got)
	}
}
