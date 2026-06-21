package anthropic

import (
	"net/http/httptest"
	"testing"
)

// TestWriteError_retryAfterHeader: a positive RetryAfter is emitted as a
// Retry-After header (the SDK backoff hint on a retryable 429/529), and omitted
// when unset.
func TestWriteError_retryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, &Error{Status: 429, Type: ErrRateLimit, Msg: "slow down", RetryAfter: 7})
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want \"7\"", got)
	}
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	rec = httptest.NewRecorder()
	WriteError(rec, &Error{Status: 400, Type: ErrInvalidRequest, Msg: "bad"})
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q on a non-retryable error, want empty", got)
	}
}
