package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/store"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// resolvedModel is the outcome of turning one --model value into the inputs a
// worker needs: the logical name the gateway serves under, the engine's
// model-selection args, any per-model engine args, and a context-window hint
// from the catalog (0 when the value is a raw path/spec).
type resolvedModel struct {
	served     string
	modelArgs  []string
	engineArgs []string
	ctxHint    int
	// sampling carries the catalog entry's sampling defaults (M2 phase 4a); the
	// zero value (both nil) for a raw path/spec means "no defaults".
	sampling catalog.Sampling
	// reasoning is the catalog entry's reasoning capability (M2 phase 4b); false
	// for a raw path/spec, which gates the thinking kwarg off.
	reasoning bool
}

// resolveModel turns one --model value into a worker plan. A catalog name
// resolves through the store (pulling a cold gguf model first); anything else
// is treated as a raw path or engine spec, preserving the pre-catalog behavior.
func resolveModel(ctx context.Context, cmd *cobra.Command, engine worker.Engine, st *store.Store, cat *catalog.Catalog, spec string) (resolvedModel, error) {
	entry, ok := cat.Lookup(spec)
	if !ok {
		// Not a catalog name: a local path or a Hugging Face spec, as before.
		return resolvedModel{
			served:    modelDisplayName(engine, spec),
			modelArgs: modelArgs(engine, spec),
		}, nil
	}

	if worker.Engine(entry.Engine) != engine {
		return resolvedModel{}, fmt.Errorf("model %q is a %s catalog model; rerun with --engine %s", entry.Name, entry.Engine, entry.Engine)
	}

	switch entry.Source.Type {
	case "gguf":
		if !st.Has(entry.Name) {
			if err := pullEntry(ctx, cmd, st, entry); err != nil {
				return resolvedModel{}, err
			}
		}
		path, err := st.Path(entry.Name)
		if err != nil {
			return resolvedModel{}, err
		}
		return resolvedModel{
			served:     entry.Name,
			modelArgs:  []string{"-m", path},
			engineArgs: entry.EngineArgs,
			ctxHint:    entry.ContextWindow,
			sampling:   entry.Sampling,
			reasoning:  entry.Reasoning,
		}, nil
	case "hf":
		// The engine resolves the repo from its own cache at boot; the store does
		// not hold multi-file HF repos in M0. Serve under the catalog's logical name
		// so clients address it consistently regardless of the repo id.
		extra := append([]string{}, entry.EngineArgs...)
		if engine == worker.EngineVLLM || engine == worker.EngineSGLang {
			// vLLM and SGLang accept --served-model-name, so they answer to the logical
			// name and the adapter echoes that. mlx_lm.server has no such flag and loads
			// exactly the requested id, so MLX skips it — the worker's adapter echoes
			// the repo id instead (see worker.engineSetup, EngineMLX).
			extra = append(extra, "--served-model-name", entry.Name)
		}
		return resolvedModel{
			served:     entry.Name,
			modelArgs:  []string{entry.Source.Repo},
			engineArgs: extra,
			ctxHint:    entry.ContextWindow,
			sampling:   entry.Sampling,
			reasoning:  entry.Reasoning,
		}, nil
	default:
		return resolvedModel{}, fmt.Errorf("model %q: unsupported source type %q", entry.Name, entry.Source.Type)
	}
}

// pullEntry downloads a gguf catalog entry into the store with a progress line.
func pullEntry(ctx context.Context, cmd *cobra.Command, st *store.Store, entry catalog.Entry) error {
	cmd.Printf("Pulling %s (%s)…\n", entry.Name, humanBytes(entry.Source.Size))
	st.Progress = progressPrinter(cmd)
	defer func() { st.Progress = nil }()
	if _, err := st.Pull(ctx, pullSpec(entry)); err != nil {
		return err
	}
	cmd.Printf("\r  done: %s\n", entry.Name)
	return nil
}

// pullSpec maps a gguf catalog entry to the store's pull request.
func pullSpec(e catalog.Entry) store.PullSpec {
	return store.PullSpec{
		Name:          e.Name,
		Engine:        e.Engine,
		URL:           e.Source.URL,
		SHA256:        e.Source.SHA256,
		Size:          e.Source.Size,
		ContextWindow: e.ContextWindow,
	}
}

// progressPrinter returns a throttled Store.Progress callback that rewrites a
// single percentage line as a download proceeds.
func progressPrinter(cmd *cobra.Command) func(done, total int64) {
	last := -1
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := int(done * 100 / total)
		if pct == last {
			return
		}
		last = pct
		cmd.Printf("\r  %3d%% (%s)", pct, humanBytes(done))
	}
}

// humanBytes renders a byte count as a short human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
