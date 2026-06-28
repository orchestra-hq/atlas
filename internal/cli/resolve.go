package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/modelmeta"
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
	// class is the catalog entry's model class (M3 phase 2a); empty for a raw
	// path/spec, which is a chat model. An "embedding" class launches the engine in
	// embedding mode and registers the route as embedding-class.
	class string
}

// resolveModel turns one --model value into a worker plan. A catalog name
// resolves through the store (pulling a cold gguf model first); anything else
// is treated as a raw path or engine spec, preserving the pre-catalog behavior.
func resolveModel(ctx context.Context, cmd *cobra.Command, engine worker.Engine, st *store.Store, cat *catalog.Catalog, stateDir, spec string) (resolvedModel, error) {
	entry, ok := cat.Lookup(spec)
	if !ok {
		// Not a catalog name: a local path or a Hugging Face spec. Auto-configure
		// from the model's own metadata where it names a known family (ADR-0015),
		// else fall back to the pre-M8 bare passthrough.
		return resolveRaw(ctx, cmd, engine, stateDir, spec), nil
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
			class:      entry.ClassOrChat(),
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
			class:      entry.ClassOrChat(),
		}, nil
	default:
		return resolvedModel{}, fmt.Errorf("model %q: unsupported source type %q", entry.Name, entry.Source.Type)
	}
}

// resolveRaw resolves a spec that is not a catalog name — a local path or a
// Hugging Face repo. Where the model's published metadata identifies a known
// family (ADR-0015 Decision 1), it auto-configures the full serving plan — the
// family's tool-call/reasoning parser engine_args, reasoning gating, the
// author's sampling defaults, and a context-window hint — exactly as a catalog
// entry would. Otherwise it returns the pre-M8 bare passthrough. Inspection is
// best-effort: any failure (offline, gated, unrecognized) yields the bare plan,
// so resolution is never less able to serve than it was before M8.
func resolveRaw(ctx context.Context, cmd *cobra.Command, engine worker.Engine, stateDir, spec string) resolvedModel {
	plan := resolvedModel{
		served:    modelDisplayName(engine, spec),
		modelArgs: modelArgs(engine, spec),
	}
	caps, ok := inspectForResolve(ctx, stateDir, spec)
	if !ok {
		return plan
	}
	fam, ok := modelmeta.Classify(caps)
	if !ok {
		// Engine-loadable but unknown family: bare passthrough today. Warn-and-serve
		// and the --require-verified opt-out are M8 Phase 4.
		return plan
	}
	plan.engineArgs = fam.EngineArgs(string(engine))
	plan.reasoning = fam.Reasoning
	plan.sampling = caps.Sampling
	plan.ctxHint = caps.ContextWindow
	cmd.Printf("Auto-configured %q as the %s family%s\n", spec, fam.Name, parserNote(plan.engineArgs))
	return plan
}

// parserNote describes the parser flags auto-config applied, for the user-facing
// note. An empty list means the engine drives tool-calling from the model's chat
// template (llama.cpp / MLX) rather than a parser flag.
func parserNote(args []string) string {
	if len(args) == 0 {
		return " (template-driven; no parser flags)."
	}
	return ": " + strings.Join(args, " ")
}

// inspectForResolve fetches a raw spec's metadata for auto-configuration. A local
// file is read directly (cheap, no cache); a Hugging Face repo uses the shared
// inspect-cache (so a prior `atlas inspect`/`up` is instant) keyed at the default
// revision. It classifies from the cached Capabilities, never the cached Verdict,
// so a pre-M8 cache entry is handled correctly. It returns ok=false on any error,
// leaving the caller on the bare path. ATLAS_HF_ENDPOINT overrides the metadata
// host (a Hugging Face mirror, or a test server).
func inspectForResolve(ctx context.Context, stateDir, spec string) (modelmeta.Capabilities, bool) {
	opts := modelmeta.Options{Token: hfToken(), Endpoint: os.Getenv("ATLAS_HF_ENDPOINT")}
	if fileExists(spec) {
		res, err := modelmeta.Inspect(ctx, spec, opts)
		if err != nil {
			return modelmeta.Capabilities{}, false
		}
		return res.Capabilities, true
	}
	rev := modelmeta.DefaultRevision
	if res, ok := readInspectCache(stateDir, spec, rev); ok {
		return res.Capabilities, true
	}
	res, err := modelmeta.Inspect(ctx, spec, opts)
	if err != nil {
		return modelmeta.Capabilities{}, false
	}
	writeInspectCache(stateDir, spec, rev, res) // best-effort
	return res.Capabilities, true
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
