package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestRequestLog_inFlightGaugeReleasedOnPanic: a handler that panics must not leak
// the in-flight gauge. The middleware decrements via defer, so the panic unwind
// still releases the slot; a plain post-call statement would skip it and drift
// atlas_requests_in_flight (and the atlas status/top snapshot) upward forever.
func TestRequestLog_inFlightGaugeReleasedOnPanic(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	m := NewMetrics()
	g.SetMetrics(m)

	h := g.withRequestLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)

	func() {
		defer func() { _ = recover() }() // net/http would recover this per-connection
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	if got := m.Snapshot().InFlight; got != 0 {
		t.Fatalf("in-flight gauge = %d after a panicking handler, want 0 (leaked)", got)
	}
}

// TestGateway_registerInstancePromotesWaiters: a request queued because the only
// replica's slots are all busy is promoted as soon as a second worker registers
// (capacity grows), rather than waiting out MaxWait. Reproduces the bug where
// RegisterInstance did not notify admission, so a queued waiter was shed despite
// the new replica's idle capacity.
func TestGateway_registerInstancePromotesWaiters(t *testing.T) {
	exec := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	// Always release the pinned executor, even if the test fails early, so the
	// blocked request can't wedge httptest.Server.Close on teardown.
	var relOnce sync.Once
	releaseA := func() { relOnce.Do(func() { close(exec.release) }) }
	t.Cleanup(releaseA)
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: exec})
	g.SetMetrics(NewMetrics())
	// capacity = 1 replica × 1 = 1; long MaxWait so the only timely admission for a
	// queued request is a capacity-growth promotion.
	g.SetAdmission(NewAdmission(AdmissionConfig{PerReplica: 1, QueueLen: 4, MaxWait: 5 * time.Second}))

	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// Request A takes the only slot and blocks in Execute.
	aDone := make(chan *http.Response, 1)
	go func() { aDone <- postMessages(t, srv.URL) }()
	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("request A never reached the executor")
	}

	// Request B finds the slot full and queues.
	bDone := make(chan *http.Response, 1)
	go func() { bDone <- postMessages(t, srv.URL) }()
	time.Sleep(100 * time.Millisecond) // let B enqueue

	// A second worker joins: capacity grows to 2. B must be promoted and dispatched
	// to the idle replica (least-in-flight) and complete, well before MaxWait.
	ready := &echoExecutor{reply: "ok"}
	g.RegisterInstance("w2", "w2", Model{Name: "m", Exec: ready})

	select {
	case resp := <-bDone:
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("queued request B = %d after a replica joined, want 200 (promotion on capacity growth)", resp.StatusCode)
		}
		_ = resp.Body.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("request B was not promoted after a second worker registered")
	}

	releaseA() // let A finish
	resp := <-aDone
	_ = resp.Body.Close()
}

// deadlineRecorder records whether the context handed to a write carried a
// deadline, so a test can assert the async writer bounds its persist calls.
type deadlineRecorder struct {
	hadDeadline chan bool
}

func (d *deadlineRecorder) Record(ctx context.Context, _ UsageRecord) error {
	_, ok := ctx.Deadline()
	d.hadDeadline <- ok
	return nil
}

// TestAsyncUsageWriter_flushBoundsContext: the background flush passes a
// deadline-bearing context to the store, so a wedged writer degrades to a logged
// dropped batch rather than blocking the sole Run goroutine (and hanging Close)
// on an unbounded context.Background().
func TestAsyncUsageWriter_flushBoundsContext(t *testing.T) {
	rec := &deadlineRecorder{hadDeadline: make(chan bool, 1)}
	w := NewAsyncUsageWriter(rec, nil) // no BatchRecorder: flush falls back to Record
	go w.Run()
	defer w.Close()

	if err := w.Record(context.Background(), UsageRecord{Model: "m"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	select {
	case ok := <-rec.hadDeadline:
		if !ok {
			t.Fatal("flush persisted with no context deadline; an unbounded write can wedge the writer")
		}
	case <-time.After(time.Second):
		t.Fatal("flush never reached the store")
	}
}
