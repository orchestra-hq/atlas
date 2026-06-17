package server

import (
	"context"
	"net/http"
	"time"
)

// Ops surface (G10, criterion 8): readiness probing lives on the gateway
// (/readyz in gateway.go); this file carries the per-request structured log,
// which records the token counts the criterion requires.

// reqLog accumulates the loggable facts of one request as its handler runs.
// The handler fills model and usage via recordUsage once they are known; the
// middleware reads it after the handler returns.
type reqLog struct {
	model        string
	inputTokens  int
	outputTokens int
}

type reqLogKey struct{}

// recordUsage stashes the resolved model and its token usage on the in-flight
// request so the logging middleware can emit them once the handler returns. A
// no-op if the request was not wrapped (e.g. in a unit test that calls a
// handler directly).
func recordUsage(ctx context.Context, model string, in, out int) {
	if rec, ok := ctx.Value(reqLogKey{}).(*reqLog); ok {
		rec.model = model
		rec.inputTokens = in
		rec.outputTokens = out
	}
}

// withRequestLog wraps the API mux to emit exactly one structured line per
// request with method, path, status, the resolved model, the request's input
// and output token counts, and the wall-clock duration. The token counts are
// the G10 requirement; they make per-request cost observable from logs alone.
//
// Liveness and readiness probes are skipped: they carry no model or tokens and
// are polled frequently enough to drown out the request log.
func (g *Gateway) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &reqLog{}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), reqLogKey{}, rec)

		next.ServeHTTP(sw, r.WithContext(ctx))

		g.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"model", rec.model,
			"input_tokens", rec.inputTokens,
			"output_tokens", rec.outputTokens,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusWriter records the response status code while delegating everything
// else to the wrapped ResponseWriter. It forwards Flush so SSE streaming (which
// type-asserts http.Flusher) keeps working through the middleware.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true // an implicit 200 if WriteHeader was never called
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
