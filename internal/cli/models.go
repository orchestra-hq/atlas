package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/store"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// startedModel is one model loaded into a local engine subprocess: the logical
// name clients address, the supervising worker, and the engine's context window.
type startedModel struct {
	name      string
	worker    *worker.Worker
	ctxWindow int
}

// startModels provisions the engine runtime and launches one engine subprocess
// per --model value, blocking until each has loaded. It returns the started
// models and a cleanup that stops every subprocess (call it on success too, when
// shutting down). On error it stops whatever it already started.
//
// It is shared by `atlas up` (single-node, gateway in-process) and `atlas
// worker` (fleet, gateway reached over the channel): both turn the same
// --model/--engine inputs into local engines and differ only in how the gateway
// reaches them.
func startModels(ctx context.Context, cmd *cobra.Command, engine worker.Engine, engineArgs, models []string, stateDir string) ([]startedModel, func(), error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, nil, err
	}
	st := store.New(filepath.Join(stateDir, "store"))

	// Fail fast on an engine/catalog mismatch before the (possibly slow) runtime
	// provisioning below: a catalog model dictates its own engine.
	for _, spec := range models {
		if entry, ok := cat.Lookup(spec); ok && worker.Engine(entry.Engine) != engine {
			return nil, nil, fmt.Errorf("model %q is a %s catalog model; rerun with --engine %s", entry.Name, entry.Engine, entry.Engine)
		}
	}

	prov := &atlasruntime.Provisioner{Dir: filepath.Join(stateDir, "runtimes")}
	binPath, err := provisionEngine(ctx, cmd, prov, engine)
	if err != nil {
		return nil, nil, err
	}

	var started []startedModel
	cleanup := func() {
		for _, sm := range started {
			_ = sm.worker.Stop()
		}
	}

	seen := map[string]bool{}
	for _, spec := range models {
		rm, err := resolveModel(ctx, cmd, engine, st, cat, spec)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if seen[rm.served] {
			cleanup()
			return nil, nil, fmt.Errorf("duplicate model %q", rm.served)
		}
		seen[rm.served] = true

		cmd.Printf("Loading model %q (this can take a while on first run)…\n", rm.served)
		w, err := worker.Start(ctx, worker.Config{
			Engine:    engine,
			BinPath:   binPath,
			ModelArgs: rm.modelArgs,
			ExtraArgs: append(append([]string{}, engineArgs...), rm.engineArgs...),
			Model:     rm.served,
			LogPath:   filepath.Join(stateDir, string(engine)+"-"+rm.served+".log"),
		})
		if err != nil {
			cleanup()
			return nil, nil, err
		}

		ctxWindow, err := w.ContextWindow(ctx)
		if err != nil {
			if rm.ctxHint > 0 {
				ctxWindow = rm.ctxHint
				cmd.Printf("  note: using catalog context window %d for %q (engine query failed: %v)\n", ctxWindow, rm.served, err)
			} else {
				cmd.Printf("  warning: could not read context window for %q (%v); fit assertion disabled\n", rm.served, err)
			}
		}
		started = append(started, startedModel{name: rm.served, worker: w, ctxWindow: ctxWindow})
	}

	return started, cleanup, nil
}
