package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// kvOverheadFraction pads a model's on-disk weight size to estimate the VRAM it
// needs once loaded — weights plus KV cache and activations — per build-time
// decision 5. Conservative: a worker is chosen only if its free capacity meets
// the padded estimate.
const kvOverheadFraction = 0.2

// engineVLLM is the catalog engine value whose models require a GPU; placement
// filters them onto GPU workers. (The worker package's worker.EngineVLLM holds
// the same string; duplicated here to keep the server independent of it.)
const engineVLLM = "vllm"

// Commander sends placement commands to a worker connection. The hub implements
// it; the scheduler calls it to load and unload models. Each returns false if
// the worker is not (or no longer) connected, so the scheduler can drop the
// pending placement.
type Commander interface {
	LoadModel(workerID, model, engine string) bool
	UnloadModel(workerID, model string) bool
}

// WorkerSnapshot is the inventory the hub hands the scheduler when a worker
// joins: enough to place models on it (engine, capacity) and to count the models
// it already serves (--model) toward a deployment's replicas.
type WorkerSnapshot struct {
	ID        string
	Engine    string
	Hardware  wire.Hardware
	Preloaded []string // models the worker already serves; never unloaded by the scheduler
}

// Scheduler places model deployments across the connected workers (M1 phase 4b).
// It owns the desired state (model -> replica count, in-memory) and an observed
// view of each worker's loaded/pending models, and reconciles the two by sending
// load/unload commands through the hub. Placement is VRAM-fit: a model is placed
// only on a matching-engine worker whose free capacity meets its estimated need.
type Scheduler struct {
	cmd Commander
	cat *catalog.Catalog
	log *slog.Logger

	mu          sync.Mutex
	workers     map[string]*schedWorker
	deployments map[string]int // model -> desired replicas
}

// schedWorker is the scheduler's accounting for one connected worker.
type schedWorker struct {
	engine   string
	capacity int64 // total GPU VRAM, or RAM for a CPU/Metal worker
	hasGPU   bool
	loaded   map[string]bool // models reported ready (includes preloaded)
	pending  map[string]bool // load sent, awaiting model_ready
	pinned   map[string]bool // preloaded (--model); the scheduler never unloads these
	failed   map[string]bool // models that failed to load here; not retried until rejoin
}

func (w *schedWorker) serves(model string) bool { return w.loaded[model] || w.pending[model] }

// NewScheduler builds a scheduler that places deployments via cmd, sizing models
// from cat. A nil logger uses slog.Default().
func NewScheduler(cmd Commander, cat *catalog.Catalog, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cmd:         cmd,
		cat:         cat,
		log:         log,
		workers:     make(map[string]*schedWorker),
		deployments: make(map[string]int),
	}
}

// estimate is a model's padded VRAM/RAM need in bytes, 0 if its size is unknown
// (a non-gguf catalog entry) — in which case placement skips the fit check and
// is best-effort.
func (s *Scheduler) estimate(model string) int64 {
	e, ok := s.cat.Lookup(model)
	if !ok || e.Source.Size <= 0 {
		return 0
	}
	return int64(float64(e.Source.Size) * (1 + kvOverheadFraction))
}

// used is the total estimated capacity a worker's loaded+pending models consume.
// The caller holds s.mu.
func (s *Scheduler) used(w *schedWorker) int64 {
	var sum int64
	for m := range w.loaded {
		sum += s.estimate(m)
	}
	for m := range w.pending {
		if !w.loaded[m] {
			sum += s.estimate(m)
		}
	}
	return sum
}

// capacityOf reports a worker's schedulable memory and whether it has a GPU: the
// summed GPU VRAM if any, else system RAM (CPU/Metal workers).
func capacityOf(hw wire.Hardware) (int64, bool) {
	if len(hw.GPUs) > 0 {
		var sum int64
		for _, g := range hw.GPUs {
			sum += g.VRAMBytes
		}
		return sum, true
	}
	return hw.RAMBytes, false
}

// WorkerJoined registers a worker's inventory and reconciles every deployment —
// a new worker may host an under-replicated one.
func (s *Scheduler) WorkerJoined(snap WorkerSnapshot) {
	capacity, hasGPU := capacityOf(snap.Hardware)
	w := &schedWorker{
		engine:   snap.Engine,
		capacity: capacity,
		hasGPU:   hasGPU,
		loaded:   make(map[string]bool),
		pending:  make(map[string]bool),
		pinned:   make(map[string]bool),
		failed:   make(map[string]bool),
	}
	for _, m := range snap.Preloaded {
		w.loaded[m] = true
		w.pinned[m] = true
	}
	s.mu.Lock()
	s.workers[snap.ID] = w
	models := s.deploymentNames()
	s.mu.Unlock()
	for _, m := range models {
		s.reconcile(m)
	}
}

// WorkerLeft drops a worker and reconciles every deployment so its lost replicas
// are re-placed elsewhere. Idempotent.
func (s *Scheduler) WorkerLeft(workerID string) {
	s.mu.Lock()
	if _, ok := s.workers[workerID]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.workers, workerID)
	models := s.deploymentNames()
	s.mu.Unlock()
	for _, m := range models {
		s.reconcile(m)
	}
}

// ModelReady marks a model loaded on a worker (clearing its pending state) and
// reconciles in case the deployment still needs more replicas.
func (s *Scheduler) ModelReady(workerID, model string, _ int) {
	s.mu.Lock()
	if w, ok := s.workers[workerID]; ok {
		delete(w.pending, model)
		w.loaded[model] = true
	}
	s.mu.Unlock()
	s.reconcile(model)
}

// ModelUnloaded clears a model from a worker's state. No reconcile-up: an unload
// only happens because the desired count was lowered.
func (s *Scheduler) ModelUnloaded(workerID, model string) {
	s.mu.Lock()
	if w, ok := s.workers[workerID]; ok {
		delete(w.pending, model)
		delete(w.loaded, model)
	}
	s.mu.Unlock()
}

// LoadFailed clears the pending load and marks the model unschedulable on that
// worker until it rejoins, then reconciles to try elsewhere.
func (s *Scheduler) LoadFailed(workerID, model, reason string) {
	s.log.Warn("model load failed", "worker", workerID, "model", model, "reason", reason)
	s.mu.Lock()
	if w, ok := s.workers[workerID]; ok {
		delete(w.pending, model)
		w.failed[model] = true
	}
	s.mu.Unlock()
	s.reconcile(model)
}

// Deploy sets a model's desired replica count and reconciles. A non-empty worker
// pins one replica to that specific worker (the rest, if replicas > 1, are best-
// fit placed). It errors if the model is unknown to the catalog, since the
// scheduler needs its engine and size.
func (s *Scheduler) Deploy(model string, replicas int, worker string) error {
	entry, ok := s.cat.Lookup(model)
	if !ok {
		return fmt.Errorf("unknown model %q (not in catalog)", model)
	}
	if replicas < 1 {
		replicas = 1
	}
	s.mu.Lock()
	s.deployments[model] = replicas
	s.mu.Unlock()
	if worker != "" {
		s.placeOn(worker, model, entry.Engine)
	}
	s.reconcile(model)
	return nil
}

// Scale changes an existing deployment's replica count and reconciles.
func (s *Scheduler) Scale(model string, replicas int) error {
	if replicas < 0 {
		replicas = 0
	}
	s.mu.Lock()
	_, exists := s.deployments[model]
	if exists {
		s.deployments[model] = replicas
	}
	s.mu.Unlock()
	if !exists {
		return fmt.Errorf("no deployment for %q", model)
	}
	s.reconcile(model)
	return nil
}

// Stop removes a deployment and unloads every (non-pinned) instance of it.
func (s *Scheduler) Stop(model string) error {
	s.mu.Lock()
	_, exists := s.deployments[model]
	if exists {
		delete(s.deployments, model)
	}
	s.mu.Unlock()
	if !exists {
		return fmt.Errorf("no deployment for %q", model)
	}
	s.unloadAll(model)
	return nil
}

// reconcile drives one model toward its desired replica count: it loads on best-
// fit workers when short and unloads when over. Commands are sent without the
// lock held so a hub callback can't deadlock against it.
func (s *Scheduler) reconcile(model string) {
	entry, ok := s.cat.Lookup(model)
	if !ok {
		return
	}
	est := s.estimate(model)
	requiresGPU := entry.Engine == engineVLLM

	s.mu.Lock()
	desired := s.deployments[model]
	var current int
	for _, w := range s.workers {
		if w.serves(model) {
			current++
		}
	}

	var toLoad, toUnload []string
	switch {
	case current < desired:
		type cand struct {
			id   string
			free int64
		}
		var cands []cand
		for id, w := range s.workers {
			if w.serves(model) || w.failed[model] || w.engine != entry.Engine {
				continue
			}
			if requiresGPU && !w.hasGPU {
				continue
			}
			free := w.capacity - s.used(w)
			if est > 0 && free < est {
				continue
			}
			cands = append(cands, cand{id, free})
		}
		// Most free first, so load spreads across the fleet rather than packing
		// one worker tight.
		sort.Slice(cands, func(i, j int) bool { return cands[i].free > cands[j].free })
		for i := 0; i < desired-current && i < len(cands); i++ {
			s.workers[cands[i].id].pending[model] = true
			toLoad = append(toLoad, cands[i].id)
		}
	case current > desired:
		var removable []string
		for id, w := range s.workers {
			if w.serves(model) && !w.pinned[model] {
				removable = append(removable, id)
			}
		}
		// Cancel not-yet-ready (pending) instances before live (loaded) ones.
		pendingOnly := func(id string) bool {
			w := s.workers[id]
			return w.pending[model] && !w.loaded[model]
		}
		sort.Slice(removable, func(i, j int) bool {
			return pendingOnly(removable[i]) && !pendingOnly(removable[j])
		})
		for i := 0; i < current-desired && i < len(removable); i++ {
			id := removable[i]
			delete(s.workers[id].pending, model)
			delete(s.workers[id].loaded, model)
			toUnload = append(toUnload, id)
		}
	}
	s.mu.Unlock()

	for _, id := range toLoad {
		if !s.cmd.LoadModel(id, model, entry.Engine) {
			s.mu.Lock()
			if w, ok := s.workers[id]; ok {
				delete(w.pending, model)
			}
			s.mu.Unlock()
		}
	}
	for _, id := range toUnload {
		s.cmd.UnloadModel(id, model)
	}
}

// placeOn loads a model on a specific worker if it is connected, runs a matching
// engine, and is not already serving it.
func (s *Scheduler) placeOn(workerID, model, engine string) {
	s.mu.Lock()
	w, ok := s.workers[workerID]
	if !ok || w.serves(model) || w.engine != engine {
		s.mu.Unlock()
		return
	}
	w.pending[model] = true
	s.mu.Unlock()
	if !s.cmd.LoadModel(workerID, model, engine) {
		s.mu.Lock()
		if w, ok := s.workers[workerID]; ok {
			delete(w.pending, model)
		}
		s.mu.Unlock()
	}
}

// unloadAll unloads every non-pinned instance of a model across the fleet.
func (s *Scheduler) unloadAll(model string) {
	s.mu.Lock()
	var ids []string
	for id, w := range s.workers {
		if w.serves(model) && !w.pinned[model] {
			delete(w.pending, model)
			delete(w.loaded, model)
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.cmd.UnloadModel(id, model)
	}
}

// deploymentNames returns the deployed model names. The caller holds s.mu.
func (s *Scheduler) deploymentNames() []string {
	out := make([]string, 0, len(s.deployments))
	for m := range s.deployments {
		out = append(out, m)
	}
	return out
}

// DeploymentInfo is the gateway-facing view of one deployment.
type DeploymentInfo struct {
	Model    string `json:"model"`
	Replicas int    `json:"replicas"`
	Ready    int    `json:"ready"`
	Pending  int    `json:"pending"`
}

// Deployments returns a snapshot of all deployments with their observed state.
func (s *Scheduler) Deployments() []DeploymentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeploymentInfo, 0, len(s.deployments))
	for model, replicas := range s.deployments {
		var ready, pending int
		for _, w := range s.workers {
			if w.loaded[model] {
				ready++
			} else if w.pending[model] {
				pending++
			}
		}
		out = append(out, DeploymentInfo{Model: model, Replicas: replicas, Ready: ready, Pending: pending})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// deployRequest is the body of POST /admin/deployments (deploy or scale).
type deployRequest struct {
	Model    string `json:"model"`
	Replicas int    `json:"replicas"`
	Worker   string `json:"worker,omitempty"`
}

// HandleSetDeployment serves POST /admin/deployments: deploy a model (optionally
// pinned to a worker) or scale an existing one. 202 on success, 400 on a bad
// body or an unknown model.
func (s *Scheduler) HandleSetDeployment(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		http.Error(w, "invalid deployment request", http.StatusBadRequest)
		return
	}
	if err := s.Deploy(req.Model, req.Replicas, req.Worker); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "replicas": maxInt(req.Replicas, 1)})
}

// HandleListDeployments serves GET /admin/deployments.
func (s *Scheduler) HandleListDeployments(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deployments": s.Deployments()})
}

// HandleStopDeployment serves DELETE /admin/deployments/{model}: 204 on success,
// 404 if no such deployment.
func (s *Scheduler) HandleStopDeployment(w http.ResponseWriter, r *http.Request) {
	if err := s.Stop(r.PathValue("model")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
