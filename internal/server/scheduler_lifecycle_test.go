package server

import (
	"context"
	"testing"
	"time"
)

// TestEnsureModel_deploysAndWaits auto-starts an un-deployed model: it issues a
// load and blocks until an instance reports ready, then returns true. The
// resulting deployment is marked auto (idle-reapable).
func TestEnsureModel_deploysAndWaits(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(2*time.Second, 0) // auto-start on, idle-stop off
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()

	// The first request triggers a load; once it lands, report the model ready.
	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "auto-start load")
	s.ModelReady("a", smallModel, 0)

	if ok := <-done; !ok {
		t.Fatal("EnsureModel returned false; want true once the model is ready")
	}
	infos := s.Deployments()
	if len(infos) != 1 || infos[0].Model != smallModel || infos[0].Replicas != 1 {
		t.Fatalf("deployments = %+v, want one auto-started replica of %s", infos, smallModel)
	}
}

// TestEnsureModel_failsFastWhenUnplaceable returns false well before the timeout
// when nowhere in the fleet can host the model — here, no workers are connected.
func TestEnsureModel_failsFastWhenUnplaceable(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(10*time.Second, 0) // long timeout: failing fast must not wait it out

	start := time.Now()
	if s.EnsureModel(context.Background(), smallModel) {
		t.Fatal("EnsureModel returned true with no workers; want false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("EnsureModel blocked %v on an unplaceable model; want a fast give-up", elapsed)
	}
}

// TestEnsureModel_unknownModel refuses a model the catalog does not know.
func TestEnsureModel_unknownModel(t *testing.T) {
	s := newTestScheduler(t, newFakeCommander())
	if s.EnsureModel(context.Background(), "not-a-real-model") {
		t.Fatal("EnsureModel returned true for an unknown model; want false")
	}
}

// TestEnsureModel_disabled returns false immediately when auto-start is off.
func TestEnsureModel_disabled(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(0, 0) // auto-start disabled
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	if s.EnsureModel(context.Background(), smallModel) {
		t.Fatal("EnsureModel returned true with auto-start disabled; want false")
	}
	if got := cmd.loadTargets(smallModel); len(got) != 0 {
		t.Fatalf("issued loads %v with auto-start disabled; want none", got)
	}
}

// TestEnsureModel_doesNotClobberOperatorReplicas leaves an existing operator
// deployment's replica count and ownership untouched: a request for an
// already-deployed, already-ready model resolves without rescaling it.
func TestEnsureModel_doesNotClobberOperatorReplicas(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 2, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	s.ModelReady("a", smallModel, 0)
	s.ModelReady("b", smallModel, 0)

	if !s.EnsureModel(context.Background(), smallModel) {
		t.Fatal("EnsureModel returned false for a ready model; want true")
	}
	if infos := s.Deployments(); len(infos) != 1 || infos[0].Replicas != 2 {
		t.Fatalf("deployments = %+v, want replicas unchanged at 2", infos)
	}
	// Still operator-owned: idle-stop must never reap it, however stale.
	s.mu.Lock()
	s.deployments[smallModel].lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.SetLifecycle(5*time.Minute, time.Minute)
	s.reapIdle()
	if got := cmd.unloadTargets(smallModel); len(got) != 0 {
		t.Fatalf("idle-stop unloaded an operator deployment %v; want none", got)
	}
}

// TestEnsureModel_reconcilesUnroutedDeployment covers a review finding: a request
// for a model that is deployed but has lost its only replica (and no reconcile has
// since been kicked for it — e.g. capacity that freed up on an unload, which does
// not reconcile-up) must drive a fresh placement, not poll a model nothing is
// loading until the timeout.
func TestEnsureModel_reconcilesUnroutedDeployment(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(2*time.Second, 0)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	// A deployment exists with no live replica, no in-flight load, no kicked reconcile.
	s.mu.Lock()
	s.deployments[smallModel] = &deployment{replicas: 1, auto: true, lastUsed: time.Now()}
	s.mu.Unlock()

	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()

	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "placement for an unrouted deployment")
	s.ModelReady("a", smallModel, 0)
	if !<-done {
		t.Fatal("EnsureModel returned false for a placeable unrouted deployment; want true")
	}
}

// TestEnsureModel_keepsDeploymentAliveWhileWaiting covers a review finding: while a
// request is actively waiting for an auto-started model to come online, the idle
// reaper must not unload it out from under the waiter — even when the cold boot
// outlasts the idle timeout. EnsureModel refreshes the idle clock each poll.
func TestEnsureModel_keepsDeploymentAliveWhileWaiting(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	// Idle timeout (200ms) comfortably exceeds the 50ms poll but is far shorter
	// than the simulated boot below, so only the in-wait Touch keeps it alive.
	s.SetLifecycle(2*time.Second, 200*time.Millisecond)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()
	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "auto-start load")

	// Simulate a slow boot: sweep repeatedly (well past the idle timeout) while the
	// load is in flight. The waiter's refresh must keep the reaper off it.
	for i := 0; i < 8; i++ {
		s.reapIdle()
		time.Sleep(50 * time.Millisecond)
	}
	if got := cmd.unloadTargets(smallModel); len(got) != 0 {
		t.Fatalf("reaper unloaded a model a request was still waiting on: %v", got)
	}

	s.ModelReady("a", smallModel, 0)
	if !<-done {
		t.Fatal("EnsureModel returned false; want true once the model reports ready")
	}
}

// TestModelReady_ignoresUnsolicited covers a review finding: a model_ready for a
// model the scheduler never placed on a worker (no pending) must not fabricate a
// loaded instance, which would permanently corrupt that worker's capacity
// accounting.
func TestModelReady_ignoresUnsolicited(t *testing.T) {
	s := newTestScheduler(t, newFakeCommander())
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	s.ModelReady("a", smallModel, 0) // never placed here

	s.mu.Lock()
	loaded := s.workers["a"].loaded[smallModel]
	s.mu.Unlock()
	if loaded {
		t.Fatal("an unsolicited model_ready marked the model loaded; want it ignored")
	}
}

// TestReapIdle_stopsIdleAutoDeployment unloads an auto-started deployment once
// it has gone untouched for longer than the idle timeout, and removes it.
func TestReapIdle_stopsIdleAutoDeployment(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(2*time.Second, time.Minute)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()
	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "auto-start load")
	s.ModelReady("a", smallModel, 0)
	if !<-done {
		t.Fatal("EnsureModel returned false; want true")
	}

	// Age it past the idle timeout, then sweep.
	s.mu.Lock()
	s.deployments[smallModel].lastUsed = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	s.reapIdle()

	if got := cmd.unloadTargets(smallModel); len(got) != 1 || got[0] != "a" {
		t.Fatalf("unload targets = %v, want [a] after idle-stop", got)
	}
	if infos := s.Deployments(); len(infos) != 0 {
		t.Fatalf("deployments = %+v, want the idle deployment removed", infos)
	}
}

// TestReapIdle_keepsTouchedDeployment does not reap an auto-started deployment
// whose idle clock was reset by a recent request (Touch).
func TestReapIdle_keepsTouchedDeployment(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(2*time.Second, time.Minute)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()
	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "auto-start load")
	s.ModelReady("a", smallModel, 0)
	<-done

	// Age it, but a fresh request (Touch) lands before the sweep.
	s.mu.Lock()
	s.deployments[smallModel].lastUsed = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	s.Touch(smallModel)
	s.reapIdle()

	if got := cmd.unloadTargets(smallModel); len(got) != 0 {
		t.Fatalf("idle-stop unloaded a recently-touched deployment %v; want none", got)
	}
}

// TestScale_takesOwnershipFromAutostart confirms that scaling an auto-started
// deployment makes it operator-owned, so the idle reaper no longer unloads it.
func TestScale_takesOwnershipFromAutostart(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.SetLifecycle(2*time.Second, time.Minute)
	// One worker keeps placement deterministic: the load lands here and ModelReady
	// targets the worker that got it. The point is ownership, not replica spread.
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})

	// Auto-start one replica (auto == true, idle-reapable).
	done := make(chan bool, 1)
	go func() { done <- s.EnsureModel(context.Background(), smallModel) }()
	waitForCond(t, func() bool { return len(cmd.loadTargets(smallModel)) == 1 }, "auto-start load")
	s.ModelReady("a", smallModel, 0)
	<-done

	// An operator scale takes ownership; the reaper must then leave it alone.
	if err := s.Scale(smallModel, 2); err != nil {
		t.Fatalf("scale: %v", err)
	}
	s.mu.Lock()
	s.deployments[smallModel].lastUsed = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.reapIdle()
	if got := cmd.unloadTargets(smallModel); len(got) != 0 {
		t.Fatalf("idle-stop unloaded a scaled deployment %v; want none (scale took ownership)", got)
	}
}

// TestRun_disabledReturnsImmediately confirms Run is a no-op when idle-stop is
// off, so it does not leak a goroutine.
func TestRun_disabledReturnsImmediately(t *testing.T) {
	s := newTestScheduler(t, newFakeCommander())
	s.SetLifecycle(time.Minute, 0) // idle-stop disabled

	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return with idle-stop disabled")
	}
}

// TestRun_stopsOnContextCancel confirms the idle sweep loop exits when its
// context is cancelled.
func TestRun_stopsOnContextCancel(t *testing.T) {
	s := newTestScheduler(t, newFakeCommander())
	s.SetLifecycle(time.Minute, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
