package server

import (
	"context"
	"net/http"
	"time"
)

// Ops surface (G10, criterion 8): readiness probing lives on the gateway
// (/readyz in gateway.go); this file carries the per-request structured log,
// which records the token counts the criterion requires.

// UsageRecord is one completed inference request's token accounting, handed to
// a UsageRecorder for durable storage (phase 6, G13). KeyID is the calling API
// key, Model the served model, WorkerID the worker that ran it. This is a
// consumer-defined type so the server package needs no dependency on the storage
// layer; the CLI bridges it to the SQLite ledger (internal/db).
type UsageRecord struct {
	KeyID        string
	Model        string
	WorkerID     string
	InputTokens  int
	OutputTokens int
}

// UsageRecorder persists a usage record. The gateway calls Record once per
// completed inference request, off the response path (the client has already
// been served), so an error is for logging only — it never fails the request.
// nil disables metering (tests, and any deployment that opts out).
type UsageRecorder interface {
	Record(ctx context.Context, u UsageRecord) error
}

// reqLog accumulates the loggable facts of one request as its handler runs.
// The handler fills model and usage via recordUsage (log only) or
// recordBillableUsage (log + durable ledger) once they are known; the
// middleware reads it after the handler returns.
type reqLog struct {
	model        string
	inputTokens  int
	outputTokens int
	keyID        string // set for a billable request: the calling API key
	workerID     string // set for a billable request: the worker that ran it
	billable     bool   // true once recordBillableUsage ran: write a ledger row
}

type reqLogKey struct{}

// recordUsage stashes the resolved model and its token usage on the in-flight
// request so the logging middleware can emit them once the handler returns. A
// no-op if the request was not wrapped (e.g. in a unit test that calls a
// handler directly). This is the log-only path (e.g. count_tokens), which is not
// billable; inference handlers use recordBillableUsage.
func recordUsage(ctx context.Context, model string, in, out int) {
	if rec, ok := ctx.Value(reqLogKey{}).(*reqLog); ok {
		rec.model = model
		rec.inputTokens = in
		rec.outputTokens = out
	}
}

// recordBillableUsage records token usage for a completed inference request,
// both for the log line and for the durable usage ledger: it stashes the model,
// tokens, calling key, and serving worker, and marks the request billable so the
// logging middleware writes a ledger row. Called on the success path and, with
// the partial count, on the interrupted-stream path (the review finding behind
// G13's interrupted case). The model recorded is the canonical served name from
// tags (not the alias a client may have addressed), so per-model ledger totals
// group by the real model — see internal/db.UsageRecord.
func recordBillableUsage(ctx context.Context, tags usageTags, in, out int) {
	if rec, ok := ctx.Value(reqLogKey{}).(*reqLog); ok {
		rec.model = tags.model
		rec.inputTokens = in
		rec.outputTokens = out
		rec.keyID = tags.keyID
		rec.workerID = tags.workerID
		rec.billable = true
	}
}

// usageTags identify who and what a billable request is charged to. Threaded
// from the handler (which knows the key from auth, and the canonical model and
// serving worker from route resolution) into the streaming and non-streaming
// record calls. model is the canonical served name (not the client's alias).
// inputTokens is the prompt token count computed before dispatch (assertContextFits),
// used to attribute input usage on an interrupted stream, where the engine never
// reports its own count.
type usageTags struct {
	keyID       string
	workerID    string
	model       string
	inputTokens int
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

		// Durable usage ledger (G13): write one row per completed inference
		// request. Use a cancel-immune context so an interrupted stream — the very
		// case we must not under-record — still persists its partial usage after
		// the client's context was canceled.
		if rec.billable && g.usage != nil {
			wctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), usageWriteTimeout)
			defer cancel()
			if err := g.usage.Record(wctx, UsageRecord{
				KeyID:        rec.keyID,
				Model:        rec.model,
				WorkerID:     rec.workerID,
				InputTokens:  rec.inputTokens,
				OutputTokens: rec.outputTokens,
			}); err != nil {
				g.logger.Warn("usage ledger write failed", "error", err, "model", rec.model)
			}
		}
	})
}

// usageWriteTimeout bounds the post-response ledger write so a stuck store can
// never pin a request goroutine open.
const usageWriteTimeout = 5 * time.Second

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
