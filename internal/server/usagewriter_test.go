package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingRecorder captures rows, optionally counting batch vs single writes, so
// tests can assert the async writer batches and drains.
type recordingRecorder struct {
	mu      sync.Mutex
	rows    []UsageRecord
	batches int
	singles int
}

func (r *recordingRecorder) Record(_ context.Context, u UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, u)
	r.singles++
	return nil
}

func (r *recordingRecorder) RecordBatch(_ context.Context, us []UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, us...)
	r.batches++
	return nil
}

func (r *recordingRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// TestAsyncUsageWriter_flushesOnClose: records enqueued before Close are all
// persisted by the drain, and via the batch path when the inner supports it.
func TestAsyncUsageWriter_flushesOnClose(t *testing.T) {
	rec := &recordingRecorder{}
	w := NewAsyncUsageWriter(rec, nil)
	go w.Run()

	const n = 500
	for i := 0; i < n; i++ {
		if err := w.Record(context.Background(), UsageRecord{Model: "m", InputTokens: 1}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	w.Close() // drains and flushes the remaining buffer before returning

	if got := rec.count(); got != n {
		t.Fatalf("persisted %d rows, want %d (no drops, no over-count)", got, n)
	}
	rec.mu.Lock()
	batches, singles := rec.batches, rec.singles
	rec.mu.Unlock()
	if batches == 0 {
		t.Fatal("expected batched writes via RecordBatch; got none")
	}
	if singles != 0 {
		t.Fatalf("expected no single-row writes when batching is available; got %d", singles)
	}
}

// TestAsyncUsageWriter_flushesOnInterval: records trickle out via the time-based
// flush without waiting for Close or a full batch.
func TestAsyncUsageWriter_flushesOnInterval(t *testing.T) {
	rec := &recordingRecorder{}
	w := NewAsyncUsageWriter(rec, nil)
	go w.Run()
	defer w.Close()

	// A handful of rows, fewer than a batch — only the interval flush moves them.
	for i := 0; i < 3; i++ {
		_ = w.Record(context.Background(), UsageRecord{Model: "m"})
	}

	deadline := time.After(2 * time.Second)
	for rec.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("interval flush never persisted the rows: have %d, want 3", rec.count())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestAsyncUsageWriter_fallsBackToPerRecord: an inner without RecordBatch still
// persists every row, via Record.
func TestAsyncUsageWriter_fallsBackToPerRecord(t *testing.T) {
	// Use a bare recorder that does NOT implement BatchUsageRecorder.
	rec := &bareRecorder{}
	w := NewAsyncUsageWriter(rec, nil)
	go w.Run()

	for i := 0; i < 10; i++ {
		_ = w.Record(context.Background(), UsageRecord{Model: "m"})
	}
	w.Close()

	if got := rec.count(); got != 10 {
		t.Fatalf("persisted %d rows via fallback, want 10", got)
	}
}

// bareRecorder implements only UsageRecorder, so NewAsyncUsageWriter's batch
// type-assertion fails and the writer uses the per-record path.
type bareRecorder struct {
	mu   sync.Mutex
	rows int
}

func (b *bareRecorder) Record(context.Context, UsageRecord) error {
	b.mu.Lock()
	b.rows++
	b.mu.Unlock()
	return nil
}

func (b *bareRecorder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rows
}
