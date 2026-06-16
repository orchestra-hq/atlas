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
	// routes by model name.
	models := map[string]server.Executor{}
	for _, spec := range opts.models {
		modelName := modelDisplayName(spec)
		if _, dup := models[modelName]; dup {
			return fmt.Errorf("duplicate model %q", modelName)
		}
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
		models[modelName] = w
	}

	// 3. Serve the gateway, routing each served model to its worker.
	gw := server.NewGateway(opts.apiKey, models)
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
func modelNames(models map[string]server.Executor) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
