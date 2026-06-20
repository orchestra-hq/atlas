package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/tlsx"
	"github.com/orchestra-hq/atlas/internal/worker"
)

type workerOptions struct {
	join       string
	token      string
	name       string
	models     []string
	engine     string
	engineArgs []string
	stateDir   string
	tlsPin     string
}

func newWorkerCmd() *cobra.Command {
	opts := &workerOptions{}
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run Atlas worker: join a server and serve inference",
		Long: "worker provisions the engine runtime, launches the requested models on\n" +
			"this machine, connects to an Atlas server hub, reports its hardware and\n" +
			"served models, and executes inference requests the gateway routes to it\n" +
			"over the channel.\n\n" +
			"Flags can also be set via environment variables:\n" +
			"  ATLAS_SERVER_URL  equivalent to --join\n" +
			"  ATLAS_JOIN_TOKEN  equivalent to --token\n" +
			"  ATLAS_TLS_PIN     equivalent to --tls-pin",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.join, "join", "", "server WebSocket URL, e.g. ws://server:9090/workers/connect")
	cmd.Flags().StringVar(&opts.token, "token", "", "join token printed by 'atlas server'")
	cmd.Flags().StringVar(&opts.name, "name", "", "human-readable label for this worker (defaults to hostname)")
	cmd.Flags().StringArrayVar(&opts.models, "model", nil,
		"model to serve: a .gguf path, a Hugging Face spec, or a catalog name; repeat to serve several")
	cmd.Flags().StringVar(&opts.engine, "engine", string(worker.EngineLlamaCpp),
		"inference engine: llamacpp (prebuilt binary) or vllm (uv-managed venv, GPU)")
	cmd.Flags().StringArrayVar(&opts.engineArgs, "engine-arg", nil,
		"extra argument passed verbatim to every engine subprocess; repeat for several")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, and logs")
	cmd.Flags().StringVar(&opts.tlsPin, "tls-pin", "",
		"pin the server's TLS certificate for a wss:// join to a self-signed server (sha256:<hex>, printed by 'atlas server --tls-self-signed'); not needed for ACME/public-CA certs")
	return cmd
}

func runWorker(ctx context.Context, cmd *cobra.Command, opts *workerOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Two-stage shutdown: the first signal drains gracefully (in-flight requests
	// finish, then the worker disconnects); a second signal forces an immediate
	// stop by cancelling ctx.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	drainCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			cmd.Println("\nDraining; press Ctrl-C again to force quit.")
			close(drainCh)
		}
		select {
		case <-ctx.Done():
		case <-sigCh:
			cancel()
		}
	}()

	serverURL := opts.join
	if serverURL == "" {
		serverURL = os.Getenv("ATLAS_SERVER_URL")
	}
	if serverURL == "" {
		return fmt.Errorf("--join is required (or set ATLAS_SERVER_URL)")
	}

	token := opts.token
	if token == "" {
		token = os.Getenv("ATLAS_JOIN_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("--token is required (or set ATLAS_JOIN_TOKEN)")
	}

	// Resolve and validate the optional cert pin up front, so a malformed pin
	// fails at startup rather than rejecting every connection at dial time.
	tlsPin := opts.tlsPin
	if tlsPin == "" {
		tlsPin = os.Getenv("ATLAS_TLS_PIN")
	}
	if tlsPin != "" {
		normalized, err := tlsx.NormalizePin(tlsPin)
		if err != nil {
			return err
		}
		tlsPin = normalized
		if !strings.HasPrefix(serverURL, "wss://") {
			cmd.Printf("Warning: --tls-pin is set but the server URL is not wss://; the pin is ignored for %s\n", serverURL)
		}
	}

	name := opts.name
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}

	engine, err := parseEngine(opts.engine)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// Provision the engine once, then launch a local engine per --model; the
	// gateway reaches them over the channel. A worker with no models still joins
	// and heartbeats — the scheduler then places models on it over the channel
	// (M1 phase 4b), loading them through the same runtime via fleetLoader.
	rt, err := newEngineRuntime(ctx, cmd, engine, opts.engineArgs, opts.stateDir)
	if err != nil {
		return err
	}
	started, cleanup, err := startModelsOn(ctx, rt, opts.models)
	if err != nil {
		return err
	}
	defer cleanup()

	served := make([]worker.ServedModel, 0, len(started))
	for _, sm := range started {
		served = append(served, worker.ServedModel{Name: sm.name, ContextWindow: sm.ctxWindow, Engine: sm.worker})
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(served) > 0 {
		cmd.Printf("Serving %s\n", strings.Join(servedModelNames(served), ", "))
	}
	cmd.Printf("Connecting to %s…\n", serverURL)

	if err := worker.Dial(ctx, worker.DialConfig{
		ServerURL: serverURL,
		Token:     token,
		Name:      name,
		Models:    served,
		Drain:     drainCh,
		Engine:    string(engine),
		TLSPin:    tlsPin,
		Loader:    &fleetLoader{rt: rt},
		Logger:    log,
	}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func servedModelNames(models []worker.ServedModel) []string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	return names
}
