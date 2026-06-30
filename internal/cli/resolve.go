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
	// fitBytes is the model's padded memory estimate (modelmeta.FitEstimate, M8
	// Phase 3); 0 when the size is unknown or a catalog entry. The launch path
	// (engineRuntime.start) weighs it against the runtime's *free* capacity so a
	// model is refused when it won't fit alongside already-loaded models, not just
	// when it exceeds an empty host (the resolveRaw baseline gate).
	fitBytes int64
}

// resolveModel turns one --model value into a worker plan. A catalog name
// resolves through the store (pulling a cold gguf model first); anything else
// is treated as a raw path or engine spec, preserving the pre-catalog behavior.
// engineArgs is the user's --engine-arg list (variadic so existing callers need
// no change); the fit gate reads any weight-quantization flag from it.
func resolveModel(ctx context.Context, cmd *cobra.Command, engine worker.Engine, st *store.Store, cat *catalog.Catalog, stateDir, quant, spec string, force, requireVerified bool, engineArgs ...string) (resolvedModel, error) {
	entry, ok := cat.Lookup(spec)
	if !ok {
		// Not a catalog name: a local path or a Hugging Face spec. Auto-configure
		// from the model's own metadata where it names a known family (ADR-0015),
		// else fall back to the pre-M8 bare passthrough.
		return resolveRaw(ctx, cmd, engine, stateDir, quant, spec, force, requireVerified, engineArgs...)
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
func resolveRaw(ctx context.Context, cmd *cobra.Command, engine worker.Engine, stateDir, quant, spec string, force, requireVerified bool, engineArgs ...string) (resolvedModel, error) {
	plan := resolvedModel{
		served:    modelDisplayName(engine, spec),
		modelArgs: modelArgs(engine, spec),
	}
	caps, ok := inspectForResolve(ctx, stateDir, spec)
	if !ok {
		if quant != "" {
			return resolvedModel{}, fmt.Errorf("could not read %s's metadata to apply --quant %q (offline, gated, or not a Hugging Face GGUF repo)", spec, quant)
		}
		// --require-verified refuses a model whose family we cannot confirm — and an
		// un-inspectable spec is exactly that. Without the flag this stays the pre-M8
		// silent bare passthrough (Phase 4 adds no warning to a path it can say nothing
		// about).
		if requireVerified {
			return resolvedModel{}, fmt.Errorf("refusing to serve %s with --require-verified: could not confirm a tested model family (its metadata was unreadable — offline, gated, or a local directory)", spec)
		}
		return plan, nil
	}
	// Quant selection for a multi-quant GGUF repo runs first so the fit gate weighs
	// the quant that will actually be served — a larger --quant than the inspect-time
	// default must be fit-checked, not the default. applyQuant updates
	// caps.WeightBytes to the selected quant's size.
	if err := applyQuant(cmd, &plan, &caps, engine, spec, quant); err != nil {
		return resolvedModel{}, err
	}
	// A weight-quantization engine flag (e.g. --quantization fp8) shrinks the GPU
	// footprint below the full-precision on-disk size the metadata reports, so scale
	// the fit estimate to the precision the engine will actually load — otherwise an
	// fp8/4-bit model that fits is refused on its bf16 size. vLLM/SGLang only; GGUF
	// and MLX carry their quantization in the weights, so caps.WeightBytes is already
	// the served size there.
	if engine == worker.EngineVLLM || engine == worker.EngineSGLang {
		if f := modelmeta.QuantMemoryFactor(servedQuant(engineArgs)); f < 1 {
			caps.WeightBytes = int64(float64(caps.WeightBytes) * f)
		}
	}
	// Fit/load gating (M8 Phase 3): refuse, before any weight download, a model the
	// pinned engine cannot load or that will not fit this host's memory — a hard
	// failure (ADR-0015 Decision 3c), distinct from an unknown family, which stays
	// the bare passthrough below (warn-and-serve is Phase 4). This baseline weighs an
	// empty host; the launch path additionally weighs free capacity (engineRuntime).
	if err := gateLoadFit(cmd, engine, spec, caps, force); err != nil {
		return resolvedModel{}, err
	}
	plan.fitBytes, _ = modelmeta.FitEstimate(caps) // for the launch-time free-capacity check
	// Family auto-config (parser engine_args, reasoning, sampling, context) for a
	// known family.
	if fam, ok := modelmeta.Classify(caps); ok {
		plan.engineArgs = fam.EngineArgs(string(engine))
		plan.reasoning = fam.Reasoning
		plan.sampling = caps.Sampling
		plan.ctxHint = caps.ContextWindow
		cmd.Printf("Auto-configured %q as the %s family: %s\n", spec, fam.Name, parserSummary(plan.engineArgs))
		return plan, nil
	}
	// Middle case (ADR-0015 Decision 3b): the engine can load this model and it fits,
	// but Atlas has no tested agent-config for its family. Default to warn-and-serve
	// plain chat; --require-verified refuses it instead. The message names the family
	// signal and the one-line PR that would add support.
	if requireVerified {
		return resolvedModel{}, fmt.Errorf("refusing to serve %s with --require-verified: %s (or drop --require-verified to serve it as plain chat)", spec, modelmeta.UnknownFamilyReason(caps))
	}
	cmd.Printf("warning: serving %s as plain chat — %s\n", spec, modelmeta.UnknownFamilyReason(caps))
	return plan, nil
}

// servedQuant extracts the value of a vLLM/SGLang weight-quantization flag from the
// user's --engine-arg list, accepting both "--quantization fp8" and
// "--quantization=fp8" forms (and the -q short flag). "" when absent. It lets the
// fit gate weigh the precision the engine will actually load (see QuantMemoryFactor).
func servedQuant(args []string) string {
	for i, a := range args {
		switch {
		case a == "--quantization" || a == "-q":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--quantization="):
			return strings.TrimPrefix(a, "--quantization=")
		case strings.HasPrefix(a, "-q="):
			return strings.TrimPrefix(a, "-q=")
		}
	}
	return ""
}

// applyQuant points a multi-quant Hugging Face GGUF repo at one quantization,
// rewriting the llama.cpp model args to -hf repo:<quant> (the engine matches the
// tag against the repo's files). The user's --quant wins — resolved to the repo's
// own canonical quant token so the tag handed to the engine is exact regardless
// of the case or partial form the user typed; absent, the inspect-time default
// (caps.Selected, Q4_K_M-preferring per ADR-0015) is used. Non-repo / single-file
// / non-GGUF targets have no selectable quants; passing --quant for one is an error.
func applyQuant(cmd *cobra.Command, plan *resolvedModel, caps *modelmeta.Capabilities, engine worker.Engine, spec, quant string) error {
	multiQuantRepo := engine == worker.EngineLlamaCpp && caps.Format == modelmeta.FormatGGUF && len(caps.Files) > 0
	if !multiQuantRepo {
		if quant != "" {
			return fmt.Errorf("--quant %q: %s has no selectable quantizations (only a multi-quant Hugging Face GGUF repo does)", quant, spec)
		}
		return nil
	}
	if quant != "" {
		tok, err := caps.ResolveQuantToken(quant)
		if err != nil {
			return fmt.Errorf("--quant %w (in %s)", err, spec)
		}
		plan.modelArgs = []string{"-hf", spec + ":" + tok}
		setServedQuantBytes(caps, tok)
		cmd.Printf("Serving quantization %s of %s\n", tok, spec)
		return nil
	}
	if tok := caps.DefaultQuantToken(); tok != "" {
		plan.modelArgs = []string{"-hf", spec + ":" + tok}
		setServedQuantBytes(caps, tok)
		cmd.Printf("Serving quantization %s of %s (default; override with --quant)\n", tok, spec)
	}
	return nil
}

// setServedQuantBytes points the fit check at the selected quant's size (so a
// larger --quant than the default is weighed correctly); it leaves the existing
// WeightBytes in place when the listing did not report that quant's size.
func setServedQuantBytes(caps *modelmeta.Capabilities, tok string) {
	if b := caps.QuantBytes(tok); b > 0 {
		caps.WeightBytes = b
	}
}

// gateLoadFit is the M8 Phase 3 pre-download gate. It runs only on a successful
// inspection (caps derived from metadata). It refuses a spec whose architecture
// the pinned engine cannot load (an upstream-engine limitation, with a pointer to
// fix it), and a spec whose padded weight estimate exceeds this host's schedulable
// memory (with the sizing shortfall). A model whose size is unknown skips the fit
// half (best-effort, never a false refusal); a loadable arch with an unknown
// family passes the gate and is served bare (the warn-and-serve middle case is
// Phase 4). Both failures abort before worker.Start, so no weights are fetched.
func gateLoadFit(cmd *cobra.Command, engine worker.Engine, spec string, caps modelmeta.Capabilities, force bool) error {
	if ok, reason := modelmeta.ArchLoadable(string(engine), caps); !ok {
		if !force {
			return fmt.Errorf("cannot serve %s: %s (re-run with --force to try anyway — the engine load is the final authority)", spec, reason)
		}
		// --force: the static arch list may be stale-pessimistic, so trust the user
		// and let the engine load be the authority (ADR-0015 trust-and-catch). The fit
		// check below still applies — forcing past the arch list never forces an OOM.
		cmd.Printf("warning: serving %s despite an unrecognized architecture (--force); the engine load is the final authority\n", spec)
	}
	est, ok := modelmeta.FitEstimate(caps)
	if !ok {
		return nil
	}
	capacity, hasGPU := detectCapacity()
	if capacity <= 0 || est <= capacity {
		return nil
	}
	mem := "RAM"
	if hasGPU {
		mem = "VRAM"
	}
	return fmt.Errorf("%s needs ~%s (weights + ~%d%% overhead) but this host has %s %s — free up memory or use a host with more %s",
		spec, humanBytes(est), int(modelmeta.KVOverheadFraction*100), humanBytes(capacity), mem, mem)
}

// parserSummary renders a known family's engine args for display — shared by the
// `atlas inspect` verdict preview (parsersLine) and the `atlas up` auto-config
// note so the two surfaces describe the same model identically. An empty list
// means the engine drives tool-calling from the model's chat template (llama.cpp
// / MLX) rather than a parser flag.
func parserSummary(args []string) string {
	if len(args) == 0 {
		return "template-driven (no parser flags)"
	}
	return strings.Join(args, " ")
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
