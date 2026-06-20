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

// drainRegistry is a minimal ModelRegistry for the hub's drain/timeout tests. It
// tracks the owning worker id per model name so UnregisterWorker drops only that
// worker's instances, matching the gateway's connection-identified routing.
type drainRegistry struct {
	mu     sync.Mutex
	models map[string]Model  // name -> model
	owner  map[string]string // name -> worker id
}

func newDrainRegistry() *drainRegistry {
	return &drainRegistry{models: map[string]Model{}, owner: map[string]string{}}
}

func (r *drainRegistry) RegisterInstance(workerID string, m Model) {
	r.mu.Lock()
	r.models[m.Name] = m
	r.owner[m.Name] = workerID
	r.mu.Unlock()
}

func (r *drainRegistry) UnregisterInstance(workerID, name string) {
	r.mu.Lock()
	if r.owner[name] == workerID {
		delete(r.models, name)
		delete(r.owner, name)
	}
	r.mu.Unlock()
}

func (r *drainRegistry) UnregisterWorker(workerID string) {
	r.mu.Lock()
	for name, owner := range r.owner {
		if owner == workerID {
			delete(r.models, name)
			delete(r.owner, name)
		}
	}
	r.mu.Unlock()
}

// get returns the single served model the tests register (always "m").
func (r *drainRegistry) get() (Model, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.models["m"]
	return m, ok
}

// hasName reports whether any route is registered for a model by name.
func (r *drainRegistry) hasName(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.models[name]
	return ok
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
func joinRawWorker(t *testing.T, hubURL string) (*websocket.Conn, string) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(hubURL, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	join, _ := wire.Encode(wire.MsgJoin, "", wire.JoinPayload{
		Token:   "tok", // matches NewHub("tok", …) in these tests
		Version: "test",
		Models:  []wire.ServedModel{{Name: "m", ContextWindow: 4096}}, // tests serve a single model "m"
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

	conn, _ := joinRawWorker(t, url) // never heartbeats or responds
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

	conn, _ := joinRawWorker(t, url)
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

// TestHub_drainingIgnoresLateModelReady covers a review finding: once a worker is
// draining (its routes torn down — possibly by an operator drain on another
// goroutine, which consumes the teardown-once), a model_ready that arrives
// afterward must not re-install a route, because the connection-end teardown would
// never remove it.
func TestHub_drainingIgnoresLateModelReady(t *testing.T) {
	reg := newDrainRegistry()
	hub := NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	conn, _ := joinRawWorker(t, url)
	defer func() { _ = conn.Close() }()
	waitForCond(t, func() bool { _, ok := reg.get(); return ok }, "model registered")

	// The worker begins draining: its routes are removed.
	drain, _ := wire.Encode(wire.MsgDrain, "", wire.DrainPayload{})
	dd, _ := json.Marshal(drain)
	if err := conn.WriteMessage(websocket.TextMessage, dd); err != nil {
		t.Fatalf("write drain: %v", err)
	}
	waitForCond(t, func() bool { _, ok := reg.get(); return !ok }, "route removed on drain")

	// A late model_ready for a scheduler-loaded model must be ignored while draining.
	ready, _ := wire.Encode(wire.MsgModelReady, "", wire.ModelReadyPayload{Model: "late", ContextWindow: 4096})
	rd, _ := json.Marshal(ready)
	if err := conn.WriteMessage(websocket.TextMessage, rd); err != nil {
		t.Fatalf("write model_ready: %v", err)
	}

	// The frame is processed in order after the drain; give the hub a moment, then
	// confirm no route was installed.
	waitForCond(t, func() bool { _, ok := reg.get(); return !ok }, "route stays removed")
	time.Sleep(50 * time.Millisecond)
	if reg.hasName("late") {
		t.Error("a model_ready received while draining re-installed a route; want it ignored")
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

	conn, wid := joinRawWorker(t, wsURL)
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

// TestHub_sameModelTwoConnectionsKeepsRoute is the G11 route-identity guarantee
// at the hub level, over real connections: two workers join serving the same
// model name, each under a fresh per-connection worker id. When one connection
// drops, the hub's teardown removes only that connection's instance, so the
// model stays routable via the other — the case a name-keyed registry dropped.
func TestHub_sameModelTwoConnectionsKeepsRoute(t *testing.T) {
	gw := NewGateway(staticAuth(testKey), nil, nil)
	hub := NewHub("tok", gw)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	routes := func() int {
		gw.mu.RLock()
		defer gw.mu.RUnlock()
		return len(gw.routes["m"])
	}

	conn1, _ := joinRawWorker(t, url)
	defer func() { _ = conn1.Close() }()
	conn2, _ := joinRawWorker(t, url)
	defer func() { _ = conn2.Close() }()

	waitForCond(t, func() bool { return routes() == 2 }, "both instances registered")

	// One connection drops; its teardown must remove only its own instance.
	_ = conn1.Close()
	waitForCond(t, func() bool { return routes() == 1 }, "dropped worker's instance removed")
	if _, _, ok := gw.resolve("m"); !ok {
		t.Fatal("model stopped resolving after one of two workers dropped")
	}

	// The remaining connection drops; now the model is gone.
	_ = conn2.Close()
	waitForCond(t, func() bool { return routes() == 0 }, "last instance removed")
	if _, _, ok := gw.resolve("m"); ok {
		t.Error("model still resolving after both workers dropped")
	}
}
