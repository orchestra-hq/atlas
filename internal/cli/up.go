package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/worker"
)

type upOptions struct {
	models     []string
	aliases    []string
	engine     string
	engineArgs []string
	quant      string
	addr       string
	stateDir   string
}

func newUpCmd() *cobra.Command {
	opts := &upOptions{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run Atlas single-node: gateway + in-process worker on this machine",
		Long: "up provisions the pinned llama.cpp runtime, launches a llama-server for the\n" +
			"requested model, and serves the Anthropic-compatible API on this machine.\n" +
			"This is the Ollama-equivalent path — one process, no join tokens (architecture.md).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUp(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringArrayVar(&opts.models, "model", nil,
		"model to serve: a path to a .gguf file or a Hugging Face spec (e.g. ggml-org/Qwen2.5-0.5B-Instruct-GGUF); repeat to serve several (required)")
	cmd.Flags().StringArrayVar(&opts.aliases, "alias", nil,
		"model alias as name=target, e.g. claude-sonnet-4-6=qwen2.5-1.5b-instruct-q4_k_m; repeat for several (docs/internal/api-surface.md)")
	cmd.Flags().StringVar(&opts.engine, "engine", string(worker.EngineLlamaCpp),
		"inference engine: llamacpp (prebuilt binary), vllm or sglang (uv venv, NVIDIA GPU), or mlx (uv venv, Apple Silicon)")
	cmd.Flags().StringArrayVar(&opts.engineArgs, "engine-arg", nil,
		"extra argument passed verbatim to every engine subprocess; repeat for several (e.g. --engine-arg=--reasoning-parser --engine-arg=qwen3)")
	cmd.Flags().StringVar(&opts.quant, "quant", "",
		"for a multi-quant Hugging Face GGUF repo, the quantization to serve (e.g. Q4_K_M); default prefers Q4_K_M")
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:8080", "address the gateway listens on")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, logs, and the key store")
	_ = cmd.MarkFlagRequired("model")
	return cmd
}

func runUp(ctx context.Context, cmd *cobra.Command, opts *upOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, err := parseEngine(opts.engine)
	if err != nil {
		return err
	}
	// Open the control-plane key store and mint a default admin key on first run,
	// so a fresh node is usable without a manual `atlas keys create` (ADR-0008).
	store, err := openStateDB(opts.stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	defaultKey, defaultKeyCreated, err := bootstrapDefaultKey(ctx, store)
	if err != nil {
		return fmt.Errorf("bootstrap default key: %w", err)
	}

	// Provision the runtime and launch an engine per requested model, each under
	// its own subprocess; the gateway routes by served name. Once a model is
	// ready its context window is read from the engine (falling back to the
	// catalog hint), so the gateway can assert request fit and report it.
	started, cleanup, err := startModels(ctx, cmd, engine, opts.engineArgs, opts.models, opts.stateDir, opts.quant)
	if err != nil {
		return err
	}
	defer cleanup()

	models := make([]server.Model, 0, len(started))
	seen := map[string]bool{}
	for _, sm := range started {
		models = append(models, server.Model{Name: sm.name, Exec: sm.worker, ContextWindow: sm.ctxWindow, Class: sm.class})
		seen[sm.name] = true
	}

	aliases, err := parseAliases(opts.aliases, seen)
	if err != nil {
		return err
	}

	// 3. Serve the gateway, routing each served model to its worker. Request
	// logs (one structured line per request, with token counts — G10) go to
	// stderr, alongside the human-readable banner on stdout.
	gw := server.NewGateway(keyAuth{db: store}, models, aliases)
	gw.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	gw.SetUsageRecorder(usageRecorder{db: store}) // durable usage ledger (G13)
	srv := &http.Server{
		Addr:              opts.addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	cmd.Println()
	cmd.Printf("Atlas is up.\n")
	cmd.Printf("  Endpoint : http://%s\n", opts.addr)
	cmd.Printf("  Models   : %s\n", strings.Join(modelNames(models), ", "))
	if len(aliases) > 0 {
		cmd.Printf("  Aliases  : %s\n", strings.Join(aliasLines(aliases), ", "))
	}
	keyForHint := "<your-api-key>"
	if defaultKeyCreated {
		cmd.Printf("  API key  : %s  (new default key — save it; it is not shown again)\n", defaultKey)
		keyForHint = defaultKey
	} else {
		cmd.Printf("  API key  : use a saved key, or run `atlas keys create`\n")
	}
	cmd.Printf("\nPoint a client at it:\n")
	cmd.Printf("  ANTHROPIC_BASE_URL=http://%s ANTHROPIC_API_KEY=%s\n", opts.addr, keyForHint)
	cmd.Println("\nPress Ctrl-C to stop.")

	select {
	case <-ctx.Done():
		cmd.Println("\nShutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-serveErr:
		return fmt.Errorf("gateway: %w", err)
	}
}

// parseEngine validates the --engine value.
func parseEngine(s string) (worker.Engine, error) {
	switch worker.Engine(s) {
	case worker.EngineLlamaCpp:
		return worker.EngineLlamaCpp, nil
	case worker.EngineVLLM:
		return worker.EngineVLLM, nil
	case worker.EngineMLX:
		return worker.EngineMLX, nil
	case worker.EngineSGLang:
		return worker.EngineSGLang, nil
	default:
		return "", fmt.Errorf("invalid --engine %q: want llamacpp, vllm, mlx, or sglang", s)
	}
}

// provisionEngine provisions the selected engine's runtime for this platform
// and returns the engine binary's path: the pinned llama-server, or the
// uv-managed venv's vllm entrypoint.
func provisionEngine(ctx context.Context, cmd *cobra.Command, prov *atlasruntime.Provisioner, engine worker.Engine) (string, error) {
	switch engine {
	case worker.EngineVLLM:
		cmd.Printf("Provisioning vLLM runtime (uv %s, vllm %s) for %s/%s — this can take a while on first run…\n",
			atlasruntime.UvVersion, atlasruntime.VLLMVersion, runtime.GOOS, runtime.GOARCH)
		return prov.EnsureVLLM(ctx, runtime.GOOS, runtime.GOARCH)
	case worker.EngineMLX:
		cmd.Printf("Provisioning MLX runtime (uv %s, mlx-lm %s) for %s/%s — this can take a while on first run…\n",
			atlasruntime.UvVersion, atlasruntime.MLXVersion, runtime.GOOS, runtime.GOARCH)
		return prov.EnsureMLX(ctx, runtime.GOOS, runtime.GOARCH)
	case worker.EngineSGLang:
		cmd.Printf("Provisioning SGLang runtime (uv %s, sglang %s) for %s/%s — this can take a while on first run…\n",
			atlasruntime.UvVersion, atlasruntime.SGLangVersion, runtime.GOOS, runtime.GOARCH)
		return prov.EnsureSGLang(ctx, runtime.GOOS, runtime.GOARCH)
	default:
		cmd.Printf("Provisioning llama.cpp runtime (%s) for %s/%s…\n", atlasruntime.LlamaCppTag, runtime.GOOS, runtime.GOARCH)
		return prov.EnsureLlamaServer(ctx, runtime.GOOS, runtime.GOARCH)
	}
}

// modelArgs turns the --model value into the engine's model-selection
// arguments. For vLLM the value is the model ref passed positionally to
// `vllm serve` (an HF repo id or a local path). For llama.cpp, a path to an
// existing file (or anything ending in .gguf) is loaded with -m; otherwise the
// value is a Hugging Face spec passed to -hf, which downloads and caches it.
func modelArgs(engine worker.Engine, model string) []string {
	if engine == worker.EngineVLLM || engine == worker.EngineMLX || engine == worker.EngineSGLang {
		// All three take the model as a bare ref (an HF repo id or a local path);
		// the worker prepends the engine's flag (MLX `--model`, SGLang `--model-path`).
		return []string{model}
	}
	if strings.HasSuffix(model, ".gguf") || fileExists(model) {
		return []string{"-m", model}
	}
	return []string{"-hf", model}
}

// modelDisplayName is the logical name clients address. For vLLM it is the
// model ref as-is (vLLM serves the model under that id, and the adapter echoes
// it back in requests). For llama.cpp a local file uses its base filename
// without extension; an HF spec is the spec itself.
func modelDisplayName(engine worker.Engine, model string) string {
	if engine == worker.EngineVLLM || engine == worker.EngineMLX || engine == worker.EngineSGLang {
		return model
	}
	if strings.HasSuffix(model, ".gguf") || fileExists(model) {
		base := filepath.Base(model)
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return model
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// modelNames returns the served model names in a stable (sorted) order for
// display.
func modelNames(models []server.Model) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names
}

// parseAliases turns "name=target" flag values into an alias->target map,
// rejecting malformed entries, duplicates, and targets that are not served
// models (known holds every served model name).
func parseAliases(flags []string, known map[string]bool) (map[string]string, error) {
	aliases := map[string]string{}
	for _, raw := range flags {
		name, target, ok := strings.Cut(raw, "=")
		if !ok || name == "" || target == "" {
			return nil, fmt.Errorf("invalid --alias %q: want name=target", raw)
		}
		if _, dup := aliases[name]; dup {
			return nil, fmt.Errorf("duplicate alias %q", name)
		}
		if known[name] {
			return nil, fmt.Errorf("alias %q collides with a served model name", name)
		}
		if !known[target] {
			return nil, fmt.Errorf("alias %q points at unknown model %q", name, target)
		}
		aliases[name] = target
	}
	return aliases, nil
}

// aliasLines renders aliases as sorted "name → target" strings for the banner.
func aliasLines(aliases map[string]string) []string {
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+" → "+aliases[name])
	}
	return lines
}

func defaultStateDir() string {
	if dir := os.Getenv("ATLAS_STATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".atlas"
	}
	return filepath.Join(home, ".atlas")
}

func generateAPIKey() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "atlas-" + hex.EncodeToString(b[:])
}
