package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/wire"
)

func chunkFrame(t *testing.T, id string) wire.Message {
	t.Helper()
	m, err := wire.Encode(wire.MsgChunk, "", wire.ChunkPayload{
		RequestID: id,
		Event:     core.StreamEvent{Kind: core.EventText, Text: "x"},
	})
	if err != nil {
		t.Fatalf("encode chunk: %v", err)
	}
	return m
}

// A request whose consumer stops draining must not block the shared reader: once
// its buffer fills, route fails that one request via overflow and leaves every
// other request's delivery untouched.
func TestRouteOverflowFailsOnlySlowRequest(t *testing.T) {
	rw := newRemoteWorker(nil)

	slow, slowP := rw.begin()
	// Fill the slow request's buffer without draining it.
	for i := 0; i < cap(slowP.ch); i++ {
		rw.route(chunkFrame(t, slow))
	}
	// The next frame would block a naive reader; instead it overflows the request.
	rw.route(chunkFrame(t, slow))

	select {
	case <-slowP.overflow:
	default:
		t.Fatal("expected slow request to be overflowed")
	}
	rw.mu.Lock()
	_, stillPending := rw.pending[slow]
	rw.mu.Unlock()
	if stillPending {
		t.Error("overflowed request should be removed from pending")
	}

	// A second request on the same connection is unaffected: its frame is
	// delivered normally rather than head-of-line-blocked behind the slow one.
	fast, fastP := rw.begin()
	rw.route(chunkFrame(t, fast))
	select {
	case ev := <-fastP.ch:
		if ev.kind != wire.MsgChunk {
			t.Errorf("fast request got kind %q, want chunk", ev.kind)
		}
	default:
		t.Fatal("fast request frame was not delivered")
	}
}

// A streaming request whose sink falls behind (a slow client) is aborted with
// errSlowConsumer once its buffer overflows, rather than wedging the reader.
func TestExecuteStreamAbortsSlowConsumer(t *testing.T) {
	rw := newRemoteWorker(nil)
	// Drain the write pump's queue so send() (the execute frame, then the cancel)
	// never blocks — there is no real writePump with a nil conn.
	go func() {
		for {
			select {
			case <-rw.out:
			case <-rw.done:
				return
			}
		}
	}()

	// Emit blocks until released, modelling a consumer that has stalled; once
	// released every call returns so the stream can drain back to its select and
	// observe the overflow that built up while it was stalled.
	release := make(chan struct{})
	sink := core.EventSink{
		Emit:   func(core.StreamEvent) error { <-release; return nil },
		OnDone: func(core.StopReason, core.Usage) error { return nil },
	}

	errCh := make(chan error, 1)
	go func() { errCh <- rw.ExecuteStream(context.Background(), core.Request{}, sink) }()

	// Wait for ExecuteStream to register its request, then flood it: it consumes
	// at most one frame (blocking in Emit) and buffers the rest until one overflows.
	id := waitForPending(t, rw)
	for i := 0; i < pendingBufferSize+2; i++ {
		rw.route(chunkFrame(t, id))
	}
	close(release) // let the stalled consumer drain and reach its select

	select {
	case err := <-errCh:
		if !errors.Is(err, errSlowConsumer) {
			t.Fatalf("ExecuteStream returned %v, want errSlowConsumer", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteStream did not abort the slow consumer")
	}
}

// waitForPending spins until exactly one request is registered and returns its id.
func waitForPending(t *testing.T, rw *remoteWorker) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rw.mu.Lock()
		for id := range rw.pending {
			rw.mu.Unlock()
			return id
		}
		rw.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no pending request registered")
	return ""
}
