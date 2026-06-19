package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/server"
)

// drainHarness stands up a real hub and a real worker.Dial serving eng, with a
// drain channel under the test's control. It returns the gateway-side registry
// (to observe routes), the hub (to drive operator-initiated removal), the drain
// channel, and a channel carrying Dial's return value.
type drainHarness struct {
	reg     *capturingRegistry
	hub     *server.Hub
	drain   chan struct{}
	dialErr chan error
}

func newDrainHarness(t *testing.T, eng Inferencer) (*drainHarness, func()) {
	t.Helper()
	reg := &capturingRegistry{models: map[string]server.Model{}}
	hub := server.NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	h := &drainHarness{
		reg:     reg,
		hub:     hub,
		drain:   make(chan struct{}),
		dialErr: make(chan error, 1),
	}
	go func() {
		h.dialErr <- Dial(ctx, DialConfig{
			ServerURL: url,
			Token:     "tok",
			Name:      "drainworker",
			Models:    []ServedModel{{Name: "m", ContextWindow: 4096, Engine: eng}},
			Drain:     h.drain,
		})
	}()
	waitFor(t, func() bool { _, ok := reg.get(); return ok }, 3*time.Second, "model to register")

	return h, func() {
		cancel()
		ts.Close()
	}
}

func (h *drainHarness) model(t *testing.T) server.Model {
	t.Helper()
	m, ok := h.reg.get()
	if !ok {
		t.Fatal("model not registered")
	}
	return m
}

// TestDrain_completesInflightThenDisconnects exercises the SIGTERM drain: closing
// the drain channel stops new routing immediately (the route is unregistered)
// while a request already in flight runs to completion, after which the worker
// disconnects and Dial returns nil.
func TestDrain_completesInflightThenDisconnects(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	eng := &fakeEngine{stream: func(_ context.Context, _ core.Request, sink core.StreamSink) error {
		_ = sink.Text("partial")
		once.Do(func() { close(started) })
		<-release
		return sink.Done(core.StopEndTurn, core.Usage{OutputTokens: 5})
	}}
	h, teardown := newDrainHarness(t, eng)
	defer teardown()

	streamer := h.model(t).Exec.(server.StreamExecutor)
	rec := &recordingSink{}
	streamDone := make(chan error, 1)
	go func() { streamDone <- streamer.ExecuteStream(context.Background(), textReq(), rec) }()

	<-started      // the engine is mid-request
	close(h.drain) // SIGTERM

	// New work stops routing while the in-flight request is still running.
	waitFor(t, func() bool { _, ok := h.reg.get(); return !ok }, 2*time.Second, "route unregistered on drain")

	// The in-flight request finishes successfully despite the drain.
	close(release)
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("in-flight stream failed during drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight stream did not complete")
	}
	if rec.text.String() != "partial" || rec.reason != core.StopEndTurn {
		t.Errorf("stream result = %q / %v", rec.text.String(), rec.reason)
	}

	// A completed drain ends Dial cleanly.
	select {
	case err := <-h.dialErr:
		if err != nil {
			t.Errorf("Dial after drain = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after drain")
	}
}

// TestDrain_refusesNewWork confirms a draining worker rejects new requests with
// the retryable ErrEngineUnavailable rather than starting them.
func TestDrain_refusesNewWork(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	eng := &fakeEngine{stream: func(_ context.Context, _ core.Request, sink core.StreamSink) error {
		_ = sink.Text("x")
		once.Do(func() { close(started) })
		<-release
		return sink.Done(core.StopEndTurn, core.Usage{})
	}}
	h, teardown := newDrainHarness(t, eng)
	defer teardown()

	m := h.model(t)
	streamer := m.Exec.(server.StreamExecutor)
	go func() { _ = streamer.ExecuteStream(context.Background(), textReq(), &recordingSink{}) }()

	<-started
	close(h.drain)
	waitFor(t, func() bool { _, ok := h.reg.get(); return !ok }, 2*time.Second, "route unregistered on drain")

	// A new request over the still-open connection is refused (retryable).
	_, err := m.Exec.Execute(context.Background(), textReq())
	if !errors.Is(err, core.ErrEngineUnavailable) {
		t.Errorf("new request during drain = %v, want ErrEngineUnavailable", err)
	}
	close(release)
}

// TestDrain_serverInitiated covers atlas workers remove: the server sends a drain
// to the worker, which (no in-flight requests) disconnects, dropping its route.
func TestDrain_serverInitiated(t *testing.T) {
	eng := &fakeEngine{
		execute: func(context.Context, core.Request) (core.Response, error) {
			return core.Response{StopReason: core.StopEndTurn}, nil
		},
	}
	h, teardown := newDrainHarness(t, eng)
	defer teardown()

	var id string
	waitFor(t, func() bool {
		ws := h.hub.Workers()
		if len(ws) == 1 {
			id = ws[0].ID
			return true
		}
		return false
	}, 2*time.Second, "worker in hub inventory")

	if !h.hub.DrainWorker(id) {
		t.Fatal("DrainWorker returned false for a connected worker")
	}

	waitFor(t, func() bool { _, ok := h.reg.get(); return !ok }, 2*time.Second, "route removed after remove")
	select {
	case err := <-h.dialErr:
		if err != nil {
			t.Errorf("Dial after server-initiated drain = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after server-initiated drain")
	}
}

// TestDrain_timeoutForcesDisconnect confirms a request that never finishes does
// not block shutdown forever: after drainTimeout the worker force-disconnects,
// and the stuck in-flight request unblocks with the retryable ErrEngineUnavailable.
func TestDrain_timeoutForcesDisconnect(t *testing.T) {
	orig := drainTimeout
	drainTimeout = 150 * time.Millisecond
	t.Cleanup(func() { drainTimeout = orig })

	started := make(chan struct{})
	var once sync.Once
	eng := &fakeEngine{stream: func(ctx context.Context, _ core.Request, sink core.StreamSink) error {
		_ = sink.Text("x")
		once.Do(func() { close(started) })
		<-ctx.Done() // never completes on its own
		return ctx.Err()
	}}
	h, teardown := newDrainHarness(t, eng)
	defer teardown()

	streamer := h.model(t).Exec.(server.StreamExecutor)
	streamErr := make(chan error, 1)
	go func() { streamErr <- streamer.ExecuteStream(context.Background(), textReq(), &recordingSink{}) }()

	<-started
	close(h.drain)

	select {
	case err := <-streamErr:
		if !errors.Is(err, core.ErrEngineUnavailable) {
			t.Errorf("stuck stream after drain timeout = %v, want ErrEngineUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stuck stream did not unblock after drain timeout")
	}
	select {
	case err := <-h.dialErr:
		if err != nil {
			t.Errorf("Dial after drain timeout = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after drain timeout")
	}
}
