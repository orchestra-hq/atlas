package server

import (
	"sync/atomic"
	"testing"
)

// selModel is the model name the selection tests register routes under.
const selModel = "m"

// inflightOf returns the live in-flight count of the named worker's instance of
// selModel, for asserting selection and release accounting.
func inflightOf(t *testing.T, g *Gateway, worker string) int64 {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, r := range g.routes[selModel] {
		if r.workerName == worker {
			return r.inflight.Load()
		}
	}
	t.Fatalf("no route for model %q on worker %q", selModel, worker)
	return 0
}

// bumpInflight directly adds to a worker instance's in-flight counter, simulating
// requests already in flight on that replica.
func bumpInflight(t *testing.T, g *Gateway, worker string, n int64) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.routes[selModel] {
		if g.routes[selModel][i].workerName == worker {
			g.routes[selModel][i].inflight.Add(n)
			return
		}
	}
	t.Fatalf("no route for model %q on worker %q", selModel, worker)
}

// TestRoute_dispatchPicksLeastInFlight: with one replica busy, forDispatch routes
// to the idle replica and increments only that instance's counter.
func TestRoute_dispatchPicksLeastInFlight(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})
	g.RegisterInstance("w2", "w2", Model{Name: "m", Exec: &echoExecutor{}})

	bumpInflight(t, g, "w1", 3) // w1 is busy; w2 is idle

	_, name, release, ok := g.pick("m")
	if !ok {
		t.Fatal("pick returned ok=false for a live model")
	}
	if name != "w2" {
		t.Fatalf("least-in-flight picked %q, want the idle replica w2", name)
	}
	if got := inflightOf(t, g, "w2"); got != 1 {
		t.Fatalf("chosen replica w2 in-flight = %d, want 1", got)
	}
	if got := inflightOf(t, g, "w1"); got != 3 {
		t.Fatalf("unchosen replica w1 in-flight = %d, want 3 (untouched)", got)
	}
	release()
	if got := inflightOf(t, g, "w2"); got != 0 {
		t.Fatalf("after release w2 in-flight = %d, want 0", got)
	}
}

// TestRoute_releaseIsIdempotent: the release decrements exactly once however many
// times it is called, so a caller can defer it across overlapping completion paths.
func TestRoute_releaseIsIdempotent(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})

	_, _, release, ok := g.pick("m")
	if !ok {
		t.Fatal("pick returned ok=false")
	}
	if got := inflightOf(t, g, "w1"); got != 1 {
		t.Fatalf("in-flight after dispatch = %d, want 1", got)
	}
	release()
	release()
	release()
	if got := inflightOf(t, g, "w1"); got != 0 {
		t.Fatalf("in-flight after repeated release = %d, want 0", got)
	}
}

// TestResolveMeta_doesNotCount: the read-only path resolves a model without
// touching any in-flight counter.
func TestResolveMeta_doesNotCount(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})

	if _, ok := g.resolveMeta("m"); !ok {
		t.Fatal("resolveMeta returned ok=false for a live model")
	}
	if got := inflightOf(t, g, "w1"); got != 0 {
		t.Fatalf("metadata resolve moved in-flight to %d, want 0", got)
	}
}

// TestResolveMeta_neverAutostarts: the read-only path must not auto-start an
// unrouted model — only the inference path (ensure) does. A miss returns false
// without ever calling EnsureModel.
func TestResolveMeta_neverAutostarts(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	fa := &fakeAutostarter{g: g, exec: &echoExecutor{}, succeed: true}
	g.SetAutostarter(fa)

	if _, ok := g.resolveMeta("absent"); ok {
		t.Fatal("resolveMeta resolved an unrouted model; want ok=false")
	}
	if ensured := fa.ensuredModels(); len(ensured) != 0 {
		t.Fatalf("resolveMeta triggered auto-start %v; want none", ensured)
	}
}

// TestRegisterInstance_preservesInflightOnReemit: a worker re-emitting model_ready
// for a model it already serves must not zero a live in-flight counter (a request
// may be running on that instance right now).
func TestRegisterInstance_preservesInflightOnReemit(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})
	bumpInflight(t, g, "w1", 2)

	// Same (worker, model) re-registers in place — the live count must survive.
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})
	if got := inflightOf(t, g, "w1"); got != 2 {
		t.Fatalf("in-flight after re-register = %d, want 2 preserved", got)
	}
}

// TestLeastInFlight_picksStrictMinimum exercises the selector directly: it always
// returns the route with the fewest in-flight requests.
func TestLeastInFlight_picksStrictMinimum(t *testing.T) {
	mk := func(name string, n int64) route {
		c := new(atomic.Int64)
		c.Add(n)
		return route{workerName: name, inflight: c}
	}
	rs := []route{mk("a", 5), mk("b", 1), mk("c", 9)}
	if got := leastInFlight(rs).workerName; got != "b" {
		t.Fatalf("leastInFlight picked %q, want the minimum b", got)
	}
}
