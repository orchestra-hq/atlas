package core

import "errors"

// ErrEngineUnavailable marks a failure to reach the engine or to get a usable
// response from it (connection refused, transport error, non-2xx status).
// Engine adapters wrap such failures with it so the gateway can map them to a
// retryable Anthropic 529 overloaded_error rather than a generic 500 — these
// are transient infrastructure faults, not the client's fault. Adapters wrap
// it with fmt.Errorf("%w: %w", core.ErrEngineUnavailable, err); the gateway
// checks errors.Is.
var ErrEngineUnavailable = errors.New("core: engine unavailable")
