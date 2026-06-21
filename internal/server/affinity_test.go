package server

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// mkRoute builds a route with a worker id/name and a seeded in-flight count, for
// exercising selection directly.
func mkRoute(worker string, n int64) route {
	c := new(atomic.Int64)
	c.Add(n)
	return route{workerID: worker, workerName: worker, inflight: c}
}

// req2 builds a two-turn core request with the given system prompt and first user
// message, the parts the default routing key hashes.
func req2(system, firstUser string) core.Request {
	return core.Request{
		System: system,
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.ContentBlock{{Type: core.BlockText, Text: firstUser}}},
		},
	}
}

// TestAffinity_routingKeyHeaderWins: an explicit x-atlas-session header is the key,
// regardless of the conversation body.
func TestAffinity_routingKeyHeaderWins(t *testing.T) {
	af := NewAffinity(AffinityConfig{Tolerance: 8})
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set(affinitySessionHeader, "session-42")

	if got := af.routingKey(r, req2("sys", "hello")); got != "session-42" {
		t.Fatalf("routingKey with header = %q, want the header value session-42", got)
	}
}

// TestAffinity_routingKeyStableAcrossTurns: the derived key depends only on the
// stable prefix (system + leading messages), so it is unchanged as later turns are
// appended — the property that keeps a conversation on its warm replica.
func TestAffinity_routingKeyStableAcrossTurns(t *testing.T) {
	af := NewAffinity(AffinityConfig{Tolerance: 8, PrefixMessages: 1})
	r := httptest.NewRequest("POST", "/v1/messages", nil)

	turn1 := req2("you are helpful", "what is 2+2?")
	key1 := af.routingKey(r, turn1)

	// A later turn: same system + same first user message, with more appended.
	turn3 := turn1
	turn3.Messages = append(turn3.Messages,
		core.Message{Role: core.RoleAssistant, Blocks: []core.ContentBlock{{Type: core.BlockText, Text: "4"}}},
		core.Message{Role: core.RoleUser, Blocks: []core.ContentBlock{{Type: core.BlockText, Text: "and 3+3?"}}},
	)
	if key3 := af.routingKey(r, turn3); key3 != key1 {
		t.Fatalf("routing key changed across turns: %q then %q; want stable", key1, key3)
	}

	// A different conversation (different first message) hashes elsewhere.
	if other := af.routingKey(r, req2("you are helpful", "totally different")); other == key1 {
		t.Fatal("distinct conversations produced the same routing key")
	}
}

// TestAffinity_disabledNoKey: a negative tolerance disables affinity, so no key is
// derived (the cost is skipped) and selection stays pure least-in-flight.
func TestAffinity_disabledNoKey(t *testing.T) {
	af := NewAffinity(AffinityConfig{Tolerance: -1})
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set(affinitySessionHeader, "session-42")
	if got := af.routingKey(r, req2("sys", "hi")); got != "" {
		t.Fatalf("disabled affinity derived key %q, want empty", got)
	}
}

// TestRendezvous_deterministicAndMinimalReshuffle: the same key always maps to the
// same replica, and removing a replica only moves the keys that were on it —
// every other key keeps its assignment (the consistent-hash property).
func TestRendezvous_deterministicAndMinimalReshuffle(t *testing.T) {
	full := []route{mkRoute("w1", 0), mkRoute("w2", 0), mkRoute("w3", 0)}

	keys := make([]string, 0, 200)
	before := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		k := "conv-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		keys = append(keys, k)
		w := rendezvous(k, full).workerID
		before[k] = w
		if again := rendezvous(k, full).workerID; again != w {
			t.Fatalf("rendezvous not deterministic for %q: %q then %q", k, w, again)
		}
	}

	// Drop w3. Every key that was NOT on w3 must keep its replica; keys that were on
	// w3 must move to one of the survivors.
	survivors := []route{mkRoute("w1", 0), mkRoute("w2", 0)}
	for _, k := range keys {
		after := rendezvous(k, survivors).workerID
		if before[k] != "w3" {
			if after != before[k] {
				t.Fatalf("key %q moved off a surviving replica: %q -> %q", k, before[k], after)
			}
		} else if after == "w3" {
			t.Fatalf("key %q still mapped to the removed replica w3", k)
		}
	}
}

// TestSelectReplica_affineWithinTolerance: when the affine replica is within the
// load tolerance of the least-loaded, it is chosen (an affinity hit), even though
// another replica is strictly less loaded.
func TestSelectReplica_affineWithinTolerance(t *testing.T) {
	af := NewAffinity(AffinityConfig{Tolerance: 8})
	rs := []route{mkRoute("w1", 0), mkRoute("w2", 0)}
	key := "stick-here"
	affine := rendezvous(key, rs).workerID

	// Load the affine replica a little, but within tolerance, and leave the other idle.
	for i := range rs {
		if rs[i].workerID == affine {
			rs[i].inflight.Add(3)
		}
	}
	if got := af.selectReplica("m", key, rs).workerID; got != affine {
		t.Fatalf("selectReplica chose %q, want the affine replica %q within tolerance", got, affine)
	}
}

// TestSelectReplica_yieldsPastTolerance: when the affine replica is loaded beyond
// the tolerance above the least-loaded, selection falls back to least-in-flight —
// affinity never parks a request on an overloaded replica.
func TestSelectReplica_yieldsPastTolerance(t *testing.T) {
	af := NewAffinity(AffinityConfig{Tolerance: 2})
	rs := []route{mkRoute("w1", 0), mkRoute("w2", 0)}
	key := "stick-here"
	affine := rendezvous(key, rs).workerID

	var other string
	for i := range rs {
		if rs[i].workerID == affine {
			rs[i].inflight.Add(10) // far past tolerance
		} else {
			other = rs[i].workerID
		}
	}
	if got := af.selectReplica("m", key, rs).workerID; got != other {
		t.Fatalf("selectReplica chose %q, want the least-loaded fallback %q past tolerance", got, other)
	}
}

// TestSelectReplica_emptyKeyAndDisabled: an empty key, a disabled controller, and a
// nil controller all fall through to least-in-flight.
func TestSelectReplica_emptyKeyAndDisabled(t *testing.T) {
	rs := []route{mkRoute("w1", 5), mkRoute("w2", 1)}
	cases := map[string]*Affinity{
		"empty-key": NewAffinity(AffinityConfig{Tolerance: 8}),
		"disabled":  NewAffinity(AffinityConfig{Tolerance: -1}),
		"nil":       nil,
	}
	for name, af := range cases {
		key := ""
		if name == "disabled" {
			key = "would-stick" // disabled ignores it
		}
		if got := af.selectReplica("m", key, rs).workerID; got != "w2" {
			t.Fatalf("%s: selectReplica chose %q, want least-in-flight w2", name, got)
		}
	}
}

// TestGateway_affinitySticksThenYields is the unit-level G19 case: it drives
// selection through the gateway's pick path with affinity on — a conversation
// repeatedly lands on its warm replica (counted as hits, one warm key), and once
// that replica is loaded past tolerance the next pick falls back to the least-loaded
// replica (a miss) rather than hanging or erroring, preserving the G16 backpressure
// semantics affinity sits on top of.
func TestGateway_affinitySticksThenYields(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.SetMetrics(NewMetrics())
	g.SetAffinity(NewAffinity(AffinityConfig{Tolerance: 2}))
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: &echoExecutor{}})
	g.RegisterInstance("w2", "w2", Model{Name: "m", Exec: &echoExecutor{}})

	g.mu.RLock()
	affine := rendezvous("conv-1", g.routes["m"]).workerID
	g.mu.RUnlock()

	// Repeated turns of the same conversation stick to the affine replica (each
	// released before the next, so it stays within tolerance).
	for i := 0; i < 5; i++ {
		_, name, release, ok := g.pick("m", "conv-1")
		if !ok {
			t.Fatal("pick returned ok=false for a live model")
		}
		if name != affine {
			t.Fatalf("turn %d landed on %q, want the warm replica %q", i, name, affine)
		}
		release()
	}
	if snap := g.metrics.Snapshot(); snap.AffinityHits != 5 || snap.WarmKeys != 1 {
		t.Fatalf("after 5 sticky turns: hits=%d warm=%d, want hits=5 warm=1", snap.AffinityHits, snap.WarmKeys)
	}

	// Load the affine replica past tolerance; the next pick must yield to the other.
	bumpInflight(t, g, affine, 10)
	_, name, release, ok := g.pick("m", "conv-1")
	if !ok {
		t.Fatal("pick returned ok=false under load")
	}
	defer release()
	if name == affine {
		t.Fatalf("under load pick stayed on the overloaded affine replica %q; want a fallback", affine)
	}
	if snap := g.metrics.Snapshot(); snap.AffinityMiss != 1 {
		t.Fatalf("after the over-tolerance pick: misses=%d, want 1", snap.AffinityMiss)
	}
}

// TestWarmKeys_perReplicaCountsAndEviction: the warm-key tracker counts distinct
// keys per replica, moves a key's count when it migrates replicas, and evicts the
// oldest key once the cap is exceeded.
func TestWarmKeys_perReplicaCountsAndEviction(t *testing.T) {
	wk := newWarmKeys(2)

	wk.record("m", "k1", "w1")
	wk.record("m", "k2", "w1")
	if c := wk.counts[modelWorker{"m", "w1"}]; c != 2 {
		t.Fatalf("w1 warm keys = %d, want 2", c)
	}

	// Re-recording k1 on a different replica moves its count, not duplicates it.
	wk.record("m", "k1", "w2")
	if c := wk.counts[modelWorker{"m", "w1"}]; c != 1 {
		t.Fatalf("after migration w1 warm keys = %d, want 1", c)
	}
	if c := wk.counts[modelWorker{"m", "w2"}]; c != 1 {
		t.Fatalf("after migration w2 warm keys = %d, want 1", c)
	}

	// A third distinct key exceeds the cap of 2, evicting the least-recently-warmed
	// (k2, since k1 was just re-touched). Total tracked keys stays at the cap.
	wk.record("m", "k3", "w1")
	if wk.ll.Len() != 2 {
		t.Fatalf("tracked keys = %d, want cap 2 after eviction", wk.ll.Len())
	}
	if _, ok := wk.index[warmKeyID{"m", "k2"}]; ok {
		t.Fatal("k2 should have been evicted as the oldest key")
	}
}
