package server

import (
	"sort"
	"sync"
	"testing"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// Catalog models the scheduler tests place. Sizes come from the real starter
// catalog: smallModel/tinyModel are llama.cpp gguf entries with a known size;
// gpuModel is a vLLM entry that requires a GPU worker.
const (
	smallModel = "qwen2.5-1.5b-instruct" // llamacpp, ~1.1 GB on disk
	gpuModel   = "qwen3.5-35b-a3b"       // vllm, needs a GPU
)

type cmdCall struct{ worker, model, engine string }

// fakeCommander records the load/unload commands the scheduler issues.
type fakeCommander struct {
	mu         sync.Mutex
	loads      []cmdCall
	unloads    []cmdCall
	loadResult bool
}

func newFakeCommander() *fakeCommander { return &fakeCommander{loadResult: true} }

func (c *fakeCommander) LoadModel(worker, model, engine string) bool {
	c.mu.Lock()
	c.loads = append(c.loads, cmdCall{worker, model, engine})
	ok := c.loadResult
	c.mu.Unlock()
	return ok
}

func (c *fakeCommander) UnloadModel(worker, model string) bool {
	c.mu.Lock()
	c.unloads = append(c.unloads, cmdCall{worker: worker, model: model})
	c.mu.Unlock()
	return true
}

func (c *fakeCommander) loadTargets(model string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.loads {
		if l.model == model {
			out = append(out, l.worker)
		}
	}
	sort.Strings(out)
	return out
}

// model is smallModel in every current caller; the param mirrors loadTargets and
// keeps the helper general for tests that unload other models.
//
//nolint:unparam // intentionally general; see comment above
func (c *fakeCommander) unloadTargets(model string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.unloads {
		if l.model == model {
			out = append(out, l.worker)
		}
	}
	sort.Strings(out)
	return out
}

func newTestScheduler(t *testing.T, cmd Commander) *Scheduler {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return NewScheduler(cmd, cat, nil)
}

func ramWorker(gb int64) wire.Hardware {
	return wire.Hardware{Platform: "cpu", RAMBytes: gb << 30}
}

func gpuWorker(gb int64) wire.Hardware {
	return wire.Hardware{Platform: "cuda", GPUs: []wire.GPU{{Name: "test", VRAMBytes: gb << 30}}}
}

// TestScheduler_placesOnBestFit puts a single replica on the worker with the
// most free capacity, spreading load rather than packing one machine.
func TestScheduler_placesOnBestFit(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "small", Engine: "llamacpp", Hardware: ramWorker(8)})
	s.WorkerJoined(WorkerSnapshot{ID: "big", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(smallModel); len(got) != 1 || got[0] != "big" {
		t.Errorf("load targets = %v, want [big] (most free capacity)", got)
	}
}

// TestScheduler_replicasSpreadAcrossWorkers places N replicas on N distinct workers.
func TestScheduler_replicasSpreadAcrossWorkers(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 2, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(smallModel); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("load targets = %v, want both workers", got)
	}
}

// TestScheduler_skipsWorkerThatDoesNotFit refuses a worker whose free capacity
// is below the model's estimated need.
func TestScheduler_skipsWorkerThatDoesNotFit(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "tiny", Engine: "llamacpp", Hardware: ramWorker(1)}) // < ~1.3 GB estimate

	if err := s.Deploy(smallModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(smallModel); len(got) != 0 {
		t.Errorf("load targets = %v, want none (worker too small)", got)
	}
}

// TestScheduler_gpuModelRequiresGpuWorker keeps a vLLM model off a CPU/llama.cpp
// worker and places it once a matching GPU worker joins.
func TestScheduler_gpuModelRequiresGpuWorker(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "cpu", Engine: "llamacpp", Hardware: ramWorker(64)})

	if err := s.Deploy(gpuModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(gpuModel); len(got) != 0 {
		t.Fatalf("load targets = %v, want none on a CPU/llamacpp worker", got)
	}

	// A matching GPU worker joining triggers placement.
	s.WorkerJoined(WorkerSnapshot{ID: "gpu", Engine: "vllm", Hardware: gpuWorker(80)})
	if got := cmd.loadTargets(gpuModel); len(got) != 1 || got[0] != "gpu" {
		t.Errorf("load targets = %v, want [gpu]", got)
	}
}

// TestScheduler_rePlacesOnWorkerLeave re-loads a deployment's replica onto
// another worker when the one serving it disconnects.
func TestScheduler_rePlacesOnWorkerLeave(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(8)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// Loaded on b (most free); confirm ready, then b leaves.
	s.ModelReady("b", smallModel, 0)
	s.WorkerLeft("b")

	if got := cmd.loadTargets(smallModel); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("load targets = %v, want a replacement load on a after b left", got)
	}
}

// TestScheduler_scaleDownUnloads removes excess replicas when scaled down.
func TestScheduler_scaleDownUnloads(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 2, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	s.ModelReady("a", smallModel, 0)
	s.ModelReady("b", smallModel, 0)

	if err := s.Scale(smallModel, 1); err != nil {
		t.Fatalf("scale: %v", err)
	}
	if got := cmd.unloadTargets(smallModel); len(got) != 1 {
		t.Errorf("unload targets = %v, want exactly one", got)
	}
}

// TestScheduler_stopUnloadsAll tears every replica down and forgets the deployment.
func TestScheduler_stopUnloadsAll(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})
	_ = s.Deploy(smallModel, 2, "")
	s.ModelReady("a", smallModel, 0)
	s.ModelReady("b", smallModel, 0)

	if err := s.Stop(smallModel); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := cmd.unloadTargets(smallModel); len(got) != 2 {
		t.Errorf("unload targets = %v, want both replicas", got)
	}
	if err := s.Stop(smallModel); err == nil {
		t.Error("second stop should report no deployment")
	}
}

// TestScheduler_pinnedNotUnloaded leaves a worker's preloaded (--model) instance
// alone: it counts toward replicas but the scheduler never unloads it.
func TestScheduler_pinnedNotUnloaded(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(16), Preloaded: []string{smallModel}})

	if err := s.Deploy(smallModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(smallModel); len(got) != 0 {
		t.Errorf("load targets = %v, want none (already served by a pinned instance)", got)
	}
	_ = s.Stop(smallModel)
	if got := cmd.unloadTargets(smallModel); len(got) != 0 {
		t.Errorf("unload targets = %v, want none (pinned instance is not unloaded)", got)
	}
}

// TestScheduler_loadFailedRetriesElsewhere places on another worker after a load
// fails, and does not retry the worker that just failed.
func TestScheduler_loadFailedRetriesElsewhere(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(8)})
	s.WorkerJoined(WorkerSnapshot{ID: "b", Engine: "llamacpp", Hardware: ramWorker(16)})

	if err := s.Deploy(smallModel, 1, ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// First placement is on b (most free); it fails, so the retry must land on a.
	s.LoadFailed("b", smallModel, "out of memory")
	if got := cmd.loadTargets(smallModel); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("load targets = %v, want a retry on a after b failed", got)
	}
}

// TestScheduler_pinToWorker honours an explicit --worker target over best-fit.
func TestScheduler_pinToWorker(t *testing.T) {
	cmd := newFakeCommander()
	s := newTestScheduler(t, cmd)
	s.WorkerJoined(WorkerSnapshot{ID: "a", Engine: "llamacpp", Hardware: ramWorker(8)})
	s.WorkerJoined(WorkerSnapshot{ID: "big", Engine: "llamacpp", Hardware: ramWorker(64)})

	if err := s.Deploy(smallModel, 1, "a"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := cmd.loadTargets(smallModel); len(got) != 1 || got[0] != "a" {
		t.Errorf("load targets = %v, want [a] (pinned), not the best-fit worker", got)
	}
}

// TestScheduler_unknownModelErrors rejects deploying a model not in the catalog,
// since the scheduler needs its engine and size to place it.
func TestScheduler_unknownModelErrors(t *testing.T) {
	s := newTestScheduler(t, newFakeCommander())
	if err := s.Deploy("not-a-real-model", 1, ""); err == nil {
		t.Error("deploy of an unknown model should error")
	}
}
