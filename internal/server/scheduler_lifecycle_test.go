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
