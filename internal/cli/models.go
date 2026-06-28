package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/store"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// engineReadyTimeout returns the engine-startup timeout override from
// ATLAS_ENGINE_READY_TIMEOUT (a Go duration, e.g. "20m"), or 0 to use the worker
// default (3m). A vLLM cold start — multi-GB weight download plus load — routinely
// exceeds the default, so the GPU acceptance run raises it; llama.cpp is ready in
// seconds and is unaffected either way.
func engineReadyTimeout() time.Duration {
	if v := os.Getenv("ATLAS_ENGINE_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 0
}

// logFileName builds the per-model engine-log filename. The served name can be a
// raw Hugging Face repo id (e.g. mlx-community/Qwen2.5-0.5B-Instruct-4bit), whose
// "/" would otherwise be read as a path separator and point the log at a
// nonexistent subdirectory; replace path separators so the name stays a single
// flat file in the state dir.
func logFileName(engine worker.Engine, served string) string {
	safe := strings.NewReplacer("/", "_", string(filepath.Separator), "_").Replace(served)
	return string(engine) + "-" + safe + ".log"
}

// startedModel is one model loaded into a local engine subprocess: the logical
// name clients address, the supervising worker, and the engine's context window.
type startedModel struct {
	name      string
	worker    *worker.Worker
	ctxWindow int
	class     string // model class (M3 phase 2a); empty = chat
}

// engineRuntime is a provisioned engine binary plus the catalog and store
// needed to resolve and launch models on it. It is built once per worker and
// reused for every model the worker starts — statically from --model and, in
// fleet mode, on demand when the scheduler sends a load (M1 phase 4b).
type engineRuntime struct {
	cmd        *cobra.Command
	engine     worker.Engine
	engineArgs []string
	stateDir   string
	binPath    string
	cat        *catalog.Catalog
	st         *store.Store
}

// newEngineRuntime loads the catalog, opens the model store, and provisions the
// engine binary (downloading it on first run) — the slow, once-per-worker setup
// every model launch then reuses.
func newEngineRuntime(ctx context.Context, cmd *cobra.Command, engine worker.Engine, engineArgs []string, stateDir string) (*engineRuntime, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	prov := &atlasruntime.Provisioner{Dir: filepath.Join(stateDir, "runtimes")}
	binPath, err := provisionEngine(ctx, cmd, prov, engine)
	if err != nil {
		return nil, err
	}
	return &engineRuntime{
		cmd:        cmd,
		engine:     engine,
		engineArgs: engineArgs,
		stateDir:   stateDir,
		binPath:    binPath,
		cat:        cat,
		st:         store.New(filepath.Join(stateDir, "store")),
	}, nil
}

// start launches one model on the runtime, blocking until the engine reports
// healthy. It resolves the spec (catalog name, Hugging Face ref, or local path),
// fetches weights via the store as needed, and reads the engine's context window.
func (r *engineRuntime) start(ctx context.Context, spec string) (startedModel, error) {
	// A catalog model dictates its own engine; fail fast on a mismatch.
	if entry, ok := r.cat.Lookup(spec); ok && worker.Engine(entry.Engine) != r.engine {
		return startedModel{}, fmt.Errorf("model %q is a %s catalog model; rerun with --engine %s", entry.Name, entry.Engine, entry.Engine)
	}
	rm, err := resolveModel(ctx, r.cmd, r.engine, r.st, r.cat, r.stateDir, spec)
	if err != nil {
		return startedModel{}, err
	}
	r.cmd.Printf("Loading model %q (this can take a while on first run)…\n", rm.served)
	w, err := worker.Start(ctx, worker.Config{
		Engine:        r.engine,
		BinPath:       r.binPath,
		ModelArgs:     rm.modelArgs,
		ExtraArgs:     append(append([]string{}, r.engineArgs...), rm.engineArgs...),
		Model:         rm.served,
		ContextWindow: rm.ctxHint,              // engines that cannot self-report (MLX) answer from this
		Temperature:   rm.sampling.Temperature, // catalog sampling defaults (M2 phase 4a)
		TopP:          rm.sampling.TopP,
		Reasoning:     rm.reasoning, // gates the thinking kwarg (M2 phase 4b)
		Class:         rm.class,     // embedding launches engine in embedding mode (M3 phase 2a)
		LogPath:       filepath.Join(r.stateDir, logFileName(r.engine, rm.served)),
		ReadyTimeout:  engineReadyTimeout(), // 0 = worker default; raised for vLLM cold start
	})
	if err != nil {
		return startedModel{}, err
	}
	ctxWindow, err := w.ContextWindow(ctx)
	if err != nil {
		if rm.ctxHint > 0 {
			ctxWindow = rm.ctxHint
			r.cmd.Printf("  note: using catalog context window %d for %q (engine query failed: %v)\n", ctxWindow, rm.served, err)
		} else {
			r.cmd.Printf("  warning: could not read context window for %q (%v); fit assertion disabled\n", rm.served, err)
		}
	}
	return startedModel{name: rm.served, worker: w, ctxWindow: ctxWindow, class: rm.class}, nil
}

// startModelsOn launches each spec on the runtime, blocking until each has
// loaded. It returns the started models and a cleanup that stops every
// subprocess; on error it stops whatever it already started.
func startModelsOn(ctx context.Context, rt *engineRuntime, models []string) ([]startedModel, func(), error) {
	var started []startedModel
	cleanup := func() {
		for _, sm := range started {
			_ = sm.worker.Stop()
		}
	}
	seen := map[string]bool{}
	for _, spec := range models {
		sm, err := rt.start(ctx, spec)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if seen[sm.name] {
			_ = sm.worker.Stop()
			cleanup()
			return nil, nil, fmt.Errorf("duplicate model %q", sm.name)
		}
		seen[sm.name] = true
		started = append(started, sm)
	}
	return started, cleanup, nil
}

// startModels provisions the engine runtime and launches one engine subprocess
// per --model value. It is the single-node convenience used by `atlas up`;
// `atlas worker` builds the runtime itself (newEngineRuntime) so it can also
// serve scheduler-driven loads over the channel.
func startModels(ctx context.Context, cmd *cobra.Command, engine worker.Engine, engineArgs, models []string, stateDir string) ([]startedModel, func(), error) {
	rt, err := newEngineRuntime(ctx, cmd, engine, engineArgs, stateDir)
	if err != nil {
		return nil, nil, err
	}
	return startModelsOn(ctx, rt, models)
}

// fleetLoader implements worker.Loader by launching one model on a shared engine
// runtime. The worker holds one and calls it when the scheduler sends a load
// over the channel (M1 phase 4b). The returned stop func is the engine's Stop,
// which the worker invokes on unload or disconnect.
type fleetLoader struct {
	rt *engineRuntime
}

func (l *fleetLoader) Load(ctx context.Context, model, engine string) (worker.ServedModel, func(), error) {
	if engine != "" && engine != string(l.rt.engine) {
		return worker.ServedModel{}, nil, fmt.Errorf("worker runs engine %s, cannot load %s model %q", l.rt.engine, engine, model)
	}
	sm, err := l.rt.start(ctx, model)
	if err != nil {
		return worker.ServedModel{}, nil, err
	}
	stop := func() { _ = sm.worker.Stop() }
	return worker.ServedModel{Name: sm.name, ContextWindow: sm.ctxWindow, Class: sm.class, Engine: sm.worker}, stop, nil
}
