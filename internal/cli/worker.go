package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/worker"
)

type workerOptions struct {
	join  string
	token string
	name  string
}

func newWorkerCmd() *cobra.Command {
	opts := &workerOptions{}
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run Atlas worker: join a server and serve inference",
		Long: "worker connects to an Atlas server hub, reports this machine's hardware\n" +
			"inventory, and maintains a persistent heartbeat. In M1 phase 2 it will\n" +
			"also receive and execute inference requests routed by the gateway.\n\n" +
			"Flags can also be set via environment variables:\n" +
			"  ATLAS_SERVER_URL  equivalent to --join\n" +
			"  ATLAS_JOIN_TOKEN  equivalent to --token",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.join, "join", "", "server WebSocket URL, e.g. ws://server:9090/workers/connect")
	cmd.Flags().StringVar(&opts.token, "token", "", "join token printed by 'atlas server'")
	cmd.Flags().StringVar(&opts.name, "name", "", "human-readable label for this worker (defaults to hostname)")
	return cmd
}

func runWorker(ctx context.Context, cmd *cobra.Command, opts *workerOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	name := opts.name
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd.Printf("Connecting to %s…\n", serverURL)

	if err := worker.Dial(ctx, worker.DialConfig{
		ServerURL: serverURL,
		Token:     token,
		Name:      name,
		Logger:    log,
	}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
