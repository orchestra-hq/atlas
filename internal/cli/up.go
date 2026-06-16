package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	models   []string
	aliases  []string
	addr     string
	apiKey   string
	stateDir string
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
		"model alias as name=target, e.g. claude-sonnet-4-6=qwen2.5-1.5b-instruct-q4_k_m; repeat for several (docs/api-surface.md)")
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:8080", "address the gateway listens on")
	cmd.Flags().StringVar(&opts.apiKey, "api-key", "", "API key clients must present; a random key is generated if unset")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, and logs")
	_ = cmd.MarkFlagRequired("model")
	return cmd
}

func runUp(ctx context.Context, cmd *cobra.Command, opts *upOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.apiKey == "" {
		opts.apiKey = generateAPIKey()
	}
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// 1. Provision the pinned llama.cpp runtime for this platform.
	cmd.Printf("Provisioning llama.cpp runtime (%s) for %s/%s…\n", atlasruntime.LlamaCppTag, runtime.GOOS, runtime.GOARCH)
	prov := &atlasruntime.Provisioner{Dir: filepath.Join(opts.stateDir, "runtimes")}
	binPath, err := prov.EnsureLlamaServer(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	// 2. Launch an engine per requested model and wait for each to load. Every
	// model gets its own llama-server (one set of weights apiece); the gateway
	// routes by model name. Once a model is ready we read its context window
	// from the engine, so the gateway can assert request fit and report it.
	var models []server.Model
	seen := map[string]bool{}
	for _, spec := range opts.models {
		modelName := modelDisplayName(spec)
		if seen[modelName] {
			return fmt.Errorf("duplicate model %q", modelName)
		}
		seen[modelName] = true
		cmd.Printf("Loading model %q (this can take a while on first run)…\n", modelName)
		w, err := worker.Start(ctx, worker.Config{
			BinPath:   binPath,
			ModelArgs: modelArgs(spec),
			Model:     modelName,
			LogPath:   filepath.Join(opts.stateDir, "llama-server-"+modelName+".log"),
		})
		if err != nil {
			return err
		}
		defer func() { _ = w.Stop() }()

		ctxWindow, err := w.ContextWindow(ctx)
		if err != nil {
			cmd.Printf("  warning: could not read context window for %q (%v); fit assertion disabled\n", modelName, err)
		}
		models = append(models, server.Model{Name: modelName, Exec: w, ContextWindow: ctxWindow})
	}

	aliases, err := parseAliases(opts.aliases, seen)
	if err != nil {
		return err
	}

	// 3. Serve the gateway, routing each served model to its worker.
	gw := server.NewGateway(opts.apiKey, models, aliases)
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
	cmd.Printf("  API key  : %s\n", opts.apiKey)
	cmd.Printf("\nPoint a client at it:\n")
	cmd.Printf("  ANTHROPIC_BASE_URL=http://%s ANTHROPIC_API_KEY=%s\n", opts.addr, opts.apiKey)
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

// modelArgs turns the --model value into llama-server arguments. A path to an
// existing file (or anything ending in .gguf) is loaded with -m; otherwise the
// value is treated as a Hugging Face spec and passed to -hf, which downloads
// and caches the weights.
func modelArgs(model string) []string {
	if strings.HasSuffix(model, ".gguf") || fileExists(model) {
		return []string{"-m", model}
	}
	return []string{"-hf", model}
}

// modelDisplayName is the logical name clients address. For a local file it is
// the base filename without extension; for an HF spec it is the spec itself.
func modelDisplayName(model string) string {
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
