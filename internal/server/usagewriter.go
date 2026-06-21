package server

import (
	"context"
	"log/slog"
	"time"
)

// Async usage-writer tuning (M2 phase 2b). The writer buffers records off the
// request hot path and flushes them in batched transactions, so the per-request
// SQLite INSERT no longer serializes on the single WAL writer under fleet-scale
// concurrency (docs/follow-ups.md).
const (
	asyncUsageBuffer = 4096                   // channel capacity before Record blocks
	asyncUsageBatch  = 128                    // rows per flush before a forced flush
	asyncUsageFlush  = 250 * time.Millisecond // max time a buffered row waits to flush
)

// AsyncUsageWriter is a UsageRecorder that enqueues each record onto a buffered
// channel and persists them from a single background goroutine, batching multi-row
// transactions (M2 phase 2b). It is block-don't-drop: when the buffer is full,
// Record blocks the request goroutine briefly rather than dropping a billing row,
// and on the rare deadline it persists the row inline — usage stays complete under
// the load the backpressure phase introduces. Run must be started once; Close
// drains and flushes the remaining buffer before returning, so a graceful shutdown
// loses no acked rows.
type AsyncUsageWriter struct {
	inner UsageRecorder
	batch BatchUsageRecorder // inner, when it supports batched writes; else nil
	log   *slog.Logger

	ch   chan UsageRecord
	stop chan struct{}
	done chan struct{}
}

// NewAsyncUsageWriter wraps inner with an async batched writer. A nil logger uses
// slog.Default(). If inner implements BatchUsageRecorder, flushes go through one
// multi-row transaction; otherwise they fall back to per-record Record (still off
// the hot path).
func NewAsyncUsageWriter(inner UsageRecorder, log *slog.Logger) *AsyncUsageWriter {
	if log == nil {
		log = slog.Default()
	}
	a := &AsyncUsageWriter{
		inner: inner,
		log:   log,
		ch:    make(chan UsageRecord, asyncUsageBuffer),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if b, ok := inner.(BatchUsageRecorder); ok {
		a.batch = b
	}
	return a
}

// Record enqueues u for the background writer (UsageRecorder). It blocks only when
// the buffer is full; if it stays full past the caller's write deadline it persists
// u inline rather than drop it (block-don't-drop). The error is for logging only —
// the response is already served.
func (a *AsyncUsageWriter) Record(ctx context.Context, u UsageRecord) error {
	select {
	case a.ch <- u:
		return nil
	case <-ctx.Done():
		return a.persist(context.WithoutCancel(ctx), []UsageRecord{u})
	}
}

// Run is the background batch loop. It flushes when the batch fills or the flush
// interval elapses, until Close signals it to drain and exit. Start it once in a
// goroutine.
func (a *AsyncUsageWriter) Run() {
	defer close(a.done)
	ticker := time.NewTicker(asyncUsageFlush)
	defer ticker.Stop()
	buf := make([]UsageRecord, 0, asyncUsageBatch)
	for {
		select {
		case <-a.stop:
			a.drain(&buf)
			return
		case u := <-a.ch:
			buf = append(buf, u)
			if len(buf) >= asyncUsageBatch {
				a.flush(&buf)
			}
		case <-ticker.C:
			a.flush(&buf)
		}
	}
}

// Close stops the writer and blocks until the background loop has drained and
// flushed every buffered record. Call it after the HTTP server has fully stopped
// (no more Record calls) and before the underlying store closes.
func (a *AsyncUsageWriter) Close() {
	close(a.stop)
	<-a.done
}

// drain flushes everything still buffered after a stop signal, then a final flush.
func (a *AsyncUsageWriter) drain(buf *[]UsageRecord) {
	for {
		select {
		case u := <-a.ch:
			*buf = append(*buf, u)
			if len(*buf) >= asyncUsageBatch {
				a.flush(buf)
			}
		default:
			a.flush(buf)
			return
		}
	}
}

// flush persists the buffered batch and resets it. A write failure is logged (the
// responses are already served) and the batch is dropped rather than retried
// indefinitely, matching the prior synchronous path's best-effort durability.
func (a *AsyncUsageWriter) flush(buf *[]UsageRecord) {
	if len(*buf) == 0 {
		return
	}
	if err := a.persist(context.Background(), *buf); err != nil {
		a.log.Warn("usage ledger batch write failed", "error", err, "rows", len(*buf))
	}
	*buf = (*buf)[:0]
}

// persist writes a batch through the bulk path when available, else per record.
func (a *AsyncUsageWriter) persist(ctx context.Context, us []UsageRecord) error {
	if a.batch != nil {
		return a.batch.RecordBatch(ctx, us)
	}
	var firstErr error
	for _, u := range us {
		if err := a.inner.Record(ctx, u); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
