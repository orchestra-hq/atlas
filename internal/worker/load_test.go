package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// fakeLoader stands in for the CLI's engine loader: it records load calls and
// hands back a stub engine, so the worker's load/unload path can be tested
// without launching a real engine subprocess.
type fakeLoader struct {
	eng Inferencer
	err error

	mu      sync.Mutex
	loads   []string
	stopped []string
}

func (l *fakeLoader) Load(_ context.Context, model, _ string) (ServedModel, func(), error) {
	l.mu.Lock()
	l.loads = append(l.loads, model)
	l.mu.Unlock()
	if l.err != nil {
		return ServedModel{}, nil, l.err
	}
	stop := func() {
		l.mu.Lock()
		l.stopped = append(l.stopped, model)
		l.mu.Unlock()
	}
	return ServedModel{Name: model, ContextWindow: 2048, Engine: l.eng}, stop, nil
}

func (l *fakeLoader) loadCount() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.loads) }
func (l *fakeLoader) stopCount() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.stopped) }

// has reports whether the gateway registry currently routes the model the
// load/unload tests deploy ("m2").
func (r *capturingRegistry) has() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.models["m2"]
	return ok
}

// dialBareWorker connects a real worker.Dial (no pre-declared models) carrying
// loader, to a real hub, and returns the hub, the worker id, and a teardown.
func dialBareWorker(t *testing.T, reg *capturingRegistry, loader Loader) (*server.Hub, string, func()) {
	t.Helper()
	hub := server.NewHub("tok", reg)
	ts := httptest.NewServer(http.HandlerFunc(hub.HandleConnect))
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan struct{})
	go func() {
		_ = Dial(ctx, DialConfig{ServerURL: url, Token: "tok", Engine: "llamacpp", Loader: loader})
		close(dialDone)
	}()

	waitFor(t, func() bool { return len(hub.Workers()) == 1 }, 3*time.Second, "worker to join")
	wid := hub.Workers()[0].ID

	return hub, wid, func() {
		cancel()
		<-dialDone
		ts.Close()
	}
}

// TestLoadRegistersRouteThenUnloadRemovesIt drives the scheduler-load path: a
// load command launches the model via the Loader and registers its gateway
// route; an unload stops it and removes the route.
func TestLoadRegistersRouteThenUnloadRemovesIt(t *testing.T) {
	reg := &capturingRegistry{models: map[string]server.Model{}}
	loader := &fakeLoader{eng: &fakeEngine{}}
	hub, wid, teardown := dialBareWorker(t, reg, loader)
	defer teardown()

	if !hub.LoadModel(wid, "m2", "llamacpp") {
		t.Fatal("LoadModel returned false for a connected worker")
	}
	waitFor(t, func() bool { return reg.has() }, 3*time.Second, "loaded model to register a route")
	if loader.loadCount() != 1 {
		t.Errorf("loader load count = %d, want 1", loader.loadCount())
	}

	if !hub.UnloadModel(wid, "m2") {
		t.Fatal("UnloadModel returned false for a connected worker")
	}
	waitFor(t, func() bool { return !reg.has() }, 3*time.Second, "unloaded model's route to be removed")
	waitFor(t, func() bool { return loader.stopCount() == 1 }, 3*time.Second, "loaded engine to be stopped")
}

// TestHandleLoad_closedSessionStopsEngine covers a review finding: if a load's
// engine finishes booting after the connection has torn down — stopAllLoaded
// already swept the loaded set — handleLoad must stop the engine rather than
// register it, otherwise it orphans a subprocess that outlives the connection.
func TestHandleLoad_closedSessionStopsEngine(t *testing.T) {
	loader := &fakeLoader{eng: &fakeEngine{}}
	sess := &session{
		loader: loader,
		log:    slog.Default(),
		models: map[string]ServedModel{},
		loaded: map[string]func(){},
		out:    make(chan wire.Message, 4),
	}
	// The connection has already torn down (stopAllLoaded ran, setting closed)
	// while this load's engine was still booting.
	sess.mu.Lock()
	sess.closed = true
	sess.mu.Unlock()

	sess.handleLoad(context.Background(), wire.LoadPayload{Model: "m2", Engine: "llamacpp"})

	if loader.stopCount() != 1 {
		t.Fatalf("engine stop count = %d, want 1 (a load completing after teardown must be stopped)", loader.stopCount())
	}
	if _, ok := sess.lookupModel("m2"); ok {
		t.Error("the model was registered into a closed session; want it dropped")
	}
}

// TestLoadFailureDoesNotRegister: a Loader error reports load_failed and leaves
// no route behind.
func TestLoadFailureDoesNotRegister(t *testing.T) {
	reg := &capturingRegistry{models: map[string]server.Model{}}
	loader := &fakeLoader{err: errors.New("boom")}
	hub, wid, teardown := dialBareWorker(t, reg, loader)
	defer teardown()

	hub.LoadModel(wid, "m2", "llamacpp")
	waitFor(t, func() bool { return loader.loadCount() == 1 }, 3*time.Second, "loader to be attempted")
	// Give the worker a moment; the route must never appear.
	time.Sleep(50 * time.Millisecond)
	if reg.has() {
		t.Error("a failed load must not register a route")
	}
}

// TestLoadedModelStoppedOnDisconnect: models a connection loaded are torn down
// when it ends, so no orphan engines outlive it (the scheduler re-places on
// reconnect).
func TestLoadedModelStoppedOnDisconnect(t *testing.T) {
	reg := &capturingRegistry{models: map[string]server.Model{}}
	loader := &fakeLoader{eng: &fakeEngine{}}
	hub, wid, teardown := dialBareWorker(t, reg, loader)

	hub.LoadModel(wid, "m2", "llamacpp")
	waitFor(t, func() bool { return reg.has() }, 3*time.Second, "model to load")

	teardown() // drops the connection
	waitFor(t, func() bool { return loader.stopCount() == 1 }, 3*time.Second, "loaded engine stopped on disconnect")
}
