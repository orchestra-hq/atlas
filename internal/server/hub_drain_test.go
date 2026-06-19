package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// drainRegistry is a minimal ModelRegistry for the hub's drain/timeout tests.
type drainRegistry struct {
	mu     sync.Mutex
	models map[string]Model
}

func newDrainRegistry() *drainRegistry { return &drainRegistry{models: map[string]Model{}} }

func (r *drainRegistry) RegisterModel(m Model) {
	r.mu.Lock()
	r.models[m.Name] = m
	r.mu.Unlock()
}

func (r *drainRegistry) UnregisterModel(name string) {
	r.mu.Lock()
	delete(r.models, name)
	r.mu.Unlock()
}

// get returns the single served model the tests register (always "m").
func (r *drainRegistry) get() (Model, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.models["m"]
	return m, ok
}

func waitForCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// joinRawWorker dials the hub and completes the join handshake by hand, serving
// one model, and returns the live connection plus the assigned worker id. The
// caller controls every subsequent frame, so it can model a worker that goes
// silent (no heartbeat, no response) — the crashed-but-connected case.
func joinRawWorker(t *testing.T, hubURL, token, model string) (*websocket.Conn, string) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(hubURL, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	join, _ := wire.Encode(wire.MsgJoin, "", wire.JoinPayload{
		Token:   token,
		Version: "test",
		Models:  []wire.ServedModel{{Name: model, ContextWindow: 4096}},
	})
	data, _ := json.Marshal(join)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write join: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_, ackData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read join_ack: %v", err)
	}
	conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	var ackEnv wire.Message
	if err := json.Unmarshal(ackData, &ackEnv); err != nil {
		t.Fatalf("unmarshal join_ack: %v", err)
	}
	var ack wire.JoinAckPayload
	_ = json.Unmarshal(ackEnv.Payload, &ack)
	if !ack.Accepted {
		t.Fatalf("join rejected: %s", ack.Reason)
	}
	return conn, ack.WorkerID
}

func nopSink() core.StreamSink {
	return core.EventSink{
		Emit:   func(core.StreamEvent) error { return nil },
		OnDone: func(core.StopReason, core.Usage) error { return nil },
	}
}

// TestHub_timeoutUnblocksInflight is the core phase-3 reliability guarantee: a
// worker that stops answering without disconnecting cleanly (crashed, TCP
// lingering) must not hang the requests multiplexed on its connection until the
// client's own deadline. When the heartbeat timeout tears the connection down,
// every in-flight request — buffered Execute and streaming alike — unblocks with
// the retryable ErrEngineUnavailable.
func TestHub_timeoutUnblocksInflight(t *testing.T) {
	orig := heartbeatTimeout
	heartbeatTimeout = 150 * time.Millisecond
	t.Cleanup(func() { heartbeatTimeout = orig })

	reg := newDrainRegistry()
	hub := NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	conn, _ := joinRawWorker(t, url, "tok", "m") // never heartbeats or responds
	defer func() { _ = conn.Close() }()

	var m Model
	waitForCond(t, func() bool { var ok bool; m, ok = reg.get(); return ok }, "model registered")

	streamer, ok := m.Exec.(StreamExecutor)
	if !ok {
		t.Fatal("remote worker is not a StreamExecutor")
	}

	execErr := make(chan error, 1)
	streamErr := make(chan error, 1)
	go func() {
		_, err := m.Exec.Execute(context.Background(), core.Request{Model: "m"})
		execErr <- err
	}()
	go func() {
		streamErr <- streamer.ExecuteStream(context.Background(), core.Request{Model: "m"}, nopSink())
	}()

	for _, c := range []struct {
		name string
		ch   chan error
	}{{"Execute", execErr}, {"ExecuteStream", streamErr}} {
		select {
		case err := <-c.ch:
			if !errors.Is(err, core.ErrEngineUnavailable) {
				t.Errorf("%s returned %v, want ErrEngineUnavailable", c.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not unblock after heartbeat timeout", c.name)
		}
	}
}

// TestHub_drainStopsRouting checks the worker-initiated drain path: on a drain
// frame the hub removes the worker's routes (no new requests) and marks it
// draining, while leaving the connection open for in-flight requests to finish.
func TestHub_drainStopsRouting(t *testing.T) {
	reg := newDrainRegistry()
	hub := NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	conn, _ := joinRawWorker(t, url, "tok", "m")
	defer func() { _ = conn.Close() }()
	waitForCond(t, func() bool { _, ok := reg.get(); return ok }, "model registered")

	drain, _ := wire.Encode(wire.MsgDrain, "", wire.DrainPayload{})
	data, _ := json.Marshal(drain)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write drain: %v", err)
	}

	waitForCond(t, func() bool { _, ok := reg.get(); return !ok }, "route removed on drain")

	workers := hub.Workers()
	if len(workers) != 1 {
		t.Fatalf("worker count = %d, want 1 (still connected, draining)", len(workers))
	}
	if !workers[0].Draining {
		t.Error("worker not marked draining after drain frame")
	}
}

// TestHub_removeWorkerSendsDrain covers the operator path (atlas workers remove):
// DrainWorker removes the routes and sends a drain frame down to the worker, so
// it begins graceful shutdown. An unknown id reports not-found.
func TestHub_removeWorkerSendsDrain(t *testing.T) {
	reg := newDrainRegistry()
	hub := NewHub("tok", reg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workers/connect", hub.HandleConnect)
	mux.HandleFunc("POST /admin/workers/{id}/drain", hub.HandleRemoveWorker)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/workers/connect"

	conn, wid := joinRawWorker(t, wsURL, "tok", "m")
	defer func() { _ = conn.Close() }()
	waitForCond(t, func() bool { _, ok := reg.get(); return ok }, "model registered")

	// Unknown worker → 404.
	resp, err := http.Post(ts.URL+"/admin/workers/w_missing/drain", "", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("post remove: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("remove unknown worker = %d, want 404", resp.StatusCode)
	}

	// Known worker → 202, routes removed, and a drain frame reaches the worker.
	resp, err = http.Post(ts.URL+"/admin/workers/"+wid+"/drain", "", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("post remove: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("remove connected worker = %d, want 202", resp.StatusCode)
	}
	waitForCond(t, func() bool { _, ok := reg.get(); return !ok }, "route removed on remove")

	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read drain frame: %v", err)
	}
	var env wire.Message
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if env.Type != wire.MsgDrain {
		t.Errorf("worker received %q, want drain", env.Type)
	}
}
