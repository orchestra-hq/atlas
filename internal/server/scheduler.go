package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// kvOverheadFraction pads a model's on-disk weight size to estimate the VRAM it
// needs once loaded — weights plus KV cache and activations — per build-time
// decision 5. Conservative: a worker is chosen only if its free capacity meets
// the padded estimate.
const kvOverheadFraction = 0.2

// engineVLLM and engineSGLang are the catalog engine values whose models require
// an NVIDIA GPU; placement filters them onto GPU workers. (The worker package's
// worker.EngineVLLM/EngineSGLang hold the same strings; duplicated here to keep the
// server independent of it.)
const (
	engineVLLM   = "vllm"
	engineSGLang = "sglang"
)

// engineNeedsGPU reports whether a model's engine can only run on a GPU worker.
// llama.cpp runs anywhere and MLX runs on Apple-Silicon (Metal, not a discrete
// GPU), so neither is gated; vLLM and SGLang are CUDA servers and are.
func engineNeedsGPU(engine string) bool {
	return engine == engineVLLM || engine == engineSGLang
}

// Auto-start/idle-stop defaults (M1 phase 4b-2). A first request for an
// un-deployed catalog model deploys one replica and blocks up to
// defaultAutostartTimeout for it to come online; an auto-started deployment with
// no traffic for defaultIdleTimeout is unloaded again. Both mirror M0's
// single-node cold-boot/idle behavior, generalized to the fleet, and are tunable
// via SetLifecycle (the server's --autostart-timeout / --idle-timeout flags).
const (
	defaultAutostartTimeout = 5 * time.Minute
	defaultIdleTimeout      = 15 * time.Minute
)

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

	// Lifecycle tuning (M1 phase 4b-2), set once at startup. autostartTimeout <= 0
	// disables auto-start (EnsureModel refuses); idleTimeout <= 0 disables
	// idle-stop (Run becomes a no-op). reapInterval is how often Run sweeps.
	autostartTimeout time.Duration
	idleTimeout      time.Duration
	reapInterval     time.Duration

	mu          sync.Mutex
	workers     map[string]*schedWorker
	deployments map[string]*deployment // model -> desired state

	// placementChanged is closed (and replaced) every time a load resolves —
	// ModelReady or LoadFailed — to wake EnsureModel waiters so they re-check
	// placement on an event instead of busy-polling. A close-and-replace broadcast
	// (rather than a per-model channel map) keeps the signalling trivial: a waiter
	// captures the current channel before checking state, so it cannot miss a
	// change that races its check. Guarded by mu.
	placementChanged chan struct{}
}

// deployment is the scheduler's desired state for one model. auto marks a
// deployment created by auto-start (idle-reapable) rather than an explicit
// `atlas deploy` (operator-owned, never idle-stopped); lastUsed is the last time
// a request resolved to it, the clock idle-stop measures against.
type deployment struct {
	replicas int
	auto     bool
	lastUsed time.Time
	// waiters is the number of EnsureModel calls currently blocked waiting for this
	// model to come online. The reaper skips a deployment with waiters > 0, so a
	// cold boot that outlasts the idle timeout is never unloaded out from under the
	// request waiting on it — this replaces the old per-poll idle-clock refresh,
	// which the event-driven wait (sparse wakes) can no longer rely on.
	waiters int
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
	s := &Scheduler{
		cmd:              cmd,
		cat:              cat,
		log:              log,
		workers:          make(map[string]*schedWorker),
		deployments:      make(map[string]*deployment),
		placementChanged: make(chan struct{}),
	}
	s.SetLifecycle(defaultAutostartTimeout, defaultIdleTimeout)
	return s
}

// SetLifecycle tunes auto-start and idle-stop (M1 phase 4b-2). A non-positive
// autostartTimeout disables auto-start; a non-positive idleTimeout disables
// idle-stop. The idle sweep interval is derived from idleTimeout (a quarter of
// it, clamped to a sane range) so tests can drive reaping with a tiny timeout.
// Call before Run / before workers connect.
func (s *Scheduler) SetLifecycle(autostartTimeout, idleTimeout time.Duration) {
	s.autostartTimeout = autostartTimeout
	s.idleTimeout = idleTimeout
	reap := idleTimeout / 4
	switch {
	case reap < 10*time.Millisecond:
		reap = 10 * time.Millisecond
	case reap > time.Minute:
		reap = time.Minute
	}
	s.reapInterval = reap
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
// reconciles in case the deployment still needs more replicas. It acts only on a
// model the scheduler actually placed here (pending) or already tracks as loaded;
// an unsolicited model_ready for a model never placed on this worker is ignored,
// so a buggy or stale frame can't fabricate a loaded instance and corrupt the
// worker's capacity accounting.
func (s *Scheduler) ModelReady(workerID, model string, _ int) {
	s.mu.Lock()
	if w, ok := s.workers[workerID]; ok && (w.pending[model] || w.loaded[model]) {
		delete(w.pending, model)
		w.loaded[model] = true
	}
	s.signalPlacementLocked()
	s.mu.Unlock()
	s.reconcile(model)
}

// signalPlacementLocked wakes every EnsureModel waiter by closing the current
// broadcast channel and installing a fresh one, so a waiter re-checks placement on
// a load event instead of busy-polling. The caller holds mu; close is non-blocking,
// so signalling under the lock cannot stall the load callbacks that hold it.
func (s *Scheduler) signalPlacementLocked() {
	close(s.placementChanged)
	s.placementChanged = make(chan struct{})
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
	s.signalPlacementLocked()
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
	if d, ok := s.deployments[model]; ok {
		d.replicas = replicas
		d.auto = false // an explicit deploy takes ownership: no longer idle-reapable
	} else {
		s.deployments[model] = &deployment{replicas: replicas, lastUsed: time.Now()}
	}
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
	d, exists := s.deployments[model]
	if exists {
		d.replicas = replicas
		d.auto = false // an explicit scale takes ownership: no longer idle-reapable
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

// EnsureModel brings a model online for an incoming request and blocks until an
// instance is ready (M1 phase 4b-2, auto-start). If the model is not deployed it
// deploys one auto-started replica; if it is already deployed it just waits.
// Returns true once an instance is loaded, false if the model is unknown to the
// catalog, auto-start is disabled, nowhere in the fleet can host it, or the wait
// exceeds the configured timeout (or ctx is cancelled). It records activity so a
// model that is auto-started by a request is not immediately idle-stopped.
func (s *Scheduler) EnsureModel(ctx context.Context, model string) bool {
	if s.autostartTimeout <= 0 {
		return false // auto-start disabled
	}
	entry, ok := s.cat.Lookup(model)
	if !ok {
		return false // not a catalog model; nothing to deploy
	}

	s.mu.Lock()
	d, deployed := s.deployments[model]
	if deployed {
		d.lastUsed = time.Now()
	} else {
		d = &deployment{replicas: 1, auto: true, lastUsed: time.Now()}
		s.deployments[model] = d
		s.log.Info("auto-start: deploying model on first request", "model", model)
	}
	// Register as a waiter so the reaper leaves this deployment alone for the whole
	// wait, then release on exit with a fresh idle clock so it gets a full idle
	// window before it can be reaped.
	d.waiters++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		d.waiters--
		d.lastUsed = time.Now()
		s.mu.Unlock()
	}()
	// Reconcile whether the deployment is new or pre-existing: a deployment that
	// lost its only replica (a failed or evicted load) is under-replicated, so a
	// request for it must drive a fresh placement rather than poll a model nothing
	// is loading until the timeout.
	s.reconcile(model)

	ctx, cancel := context.WithTimeout(ctx, s.autostartTimeout)
	defer cancel()
	// fallback bounds how long a waiter sleeps if a placement signal is somehow
	// missed, so a dropped wake degrades to a slow re-check rather than hanging to
	// the full autostart timeout. The common case is woken by signalPlacementLocked.
	const fallback = time.Second
	timer := time.NewTimer(fallback)
	defer timer.Stop()
	for {
		// Capture the broadcast channel under the same lock as the state read, so a
		// signal that fires between the check and the wait cannot be missed (the
		// closed channel we captured returns immediately from the select).
		s.mu.Lock()
		ready, pending, placeable := s.placementStateLocked(model, entry.Engine)
		changed := s.placementChanged
		s.mu.Unlock()
		switch {
		case ready:
			return true
		case !pending && !placeable:
			// Nothing is loading and no worker can take it: fail fast rather than
			// block the request for the full timeout on an unsatisfiable placement.
			return false
		}
		// A load is in flight (or a worker can still take it): wait for the next
		// placement event. The waiter count registered above already keeps the
		// reaper off this deployment, so no per-poll idle-clock refresh is needed.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(fallback)
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-timer.C:
		}
	}
}

// placementStateLocked reports, for one model, whether some worker has it loaded
// (ready), has a load in flight (pending), and whether any further worker could
// still accept it (placeable) — the fit check reconcile uses. EnsureModel reads it
// under the lock alongside the placement-change channel: ready ends the wait, while
// !pending && !placeable means giving up. The caller holds s.mu.
func (s *Scheduler) placementStateLocked(model, engine string) (ready, pending, placeable bool) {
	est := s.estimate(model)
	for _, w := range s.workers {
		if w.loaded[model] {
			ready = true
		}
		if w.pending[model] {
			pending = true
		}
		if s.fits(w, model, engine, est) {
			placeable = true
		}
	}
	return ready, pending, placeable
}

// fits reports whether worker w can accept a new instance of model: it is not
// already serving or failed for it, its engine matches, it has a GPU when the
// engine requires one, and its free capacity covers the model's estimated need
// (est == 0 means the size is unknown, so the fit check is skipped). It is the
// single placement predicate shared by placementState (auto-start's wait) and
// reconcile (the actual placement), so the two cannot drift. The caller holds
// s.mu.
func (s *Scheduler) fits(w *schedWorker, model, engine string, est int64) bool {
	if w.serves(model) || w.failed[model] || w.engine != engine {
		return false
	}
	if engineNeedsGPU(engine) && !w.hasGPU {
		return false
	}
	return est == 0 || w.capacity-s.used(w) >= est
}

// Touch records that a request resolved to a model, resetting its idle clock so
// an actively used auto-started deployment is not reaped (M1 phase 4b-2). The
// gateway calls it on every request that routes; it is a no-op for models with
// no deployment.
func (s *Scheduler) Touch(model string) {
	s.mu.Lock()
	if d, ok := s.deployments[model]; ok {
		d.lastUsed = time.Now()
	}
	s.mu.Unlock()
}

// Run sweeps for idle auto-started deployments and unloads them, until ctx is
// cancelled (M1 phase 4b-2, idle-stop). It is a no-op when idle-stop is disabled
// (idleTimeout <= 0). Start it once in a goroutine at server startup.
func (s *Scheduler) Run(ctx context.Context) {
	if s.idleTimeout <= 0 {
		return
	}
	ticker := time.NewTicker(s.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapIdle()
		}
	}
}

// reapIdle stops every auto-started deployment whose last request is older than
// the idle timeout and that has no waiter actively blocked on it. Operator
// deployments (auto == false) are never reaped. The staleness check and the delete
// happen under one lock hold — guarding the same lastUsed/auto/waiters fields that
// Touch, EnsureModel, Deploy, and Scale mutate — so a request that touches a model,
// starts waiting on it, or an operator that takes ownership of it, in the same
// instant is not clobbered by a stop decided a moment earlier (the unload runs only
// for deployments actually removed here).
func (s *Scheduler) reapIdle() {
	now := time.Now()
	s.mu.Lock()
	var stale []string
	for model, d := range s.deployments {
		if d.auto && d.waiters == 0 && now.Sub(d.lastUsed) > s.idleTimeout {
			delete(s.deployments, model)
			stale = append(stale, model)
		}
	}
	s.mu.Unlock()
	for _, model := range stale {
		s.log.Info("idle-stop: unloading idle auto-started model", "model", model, "idle_timeout", s.idleTimeout)
		s.unloadAll(model)
	}
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

	s.mu.Lock()
	desired := 0
	if d := s.deployments[model]; d != nil {
		desired = d.replicas
	}
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
			if !s.fits(w, model, entry.Engine, est) {
				continue
			}
			cands = append(cands, cand{id, w.capacity - s.used(w)})
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
	for model, d := range s.deployments {
		var ready, pending int
		for _, w := range s.workers {
			if w.loaded[model] {
				ready++
			} else if w.pending[model] {
				pending++
			}
		}
		out = append(out, DeploymentInfo{Model: model, Replicas: d.replicas, Ready: ready, Pending: pending})
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
	_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "replicas": max(req.Replicas, 1)})
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
