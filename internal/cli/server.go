package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/server"
)

type serverOptions struct {
	addr     string
	token    string
	stateDir string
}

func newServerCmd() *cobra.Command {
	opts := &serverOptions{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run Atlas control plane: gateway + worker hub",
		Long: "server starts the control plane — the gateway, scheduler, and worker hub —\n" +
			"without a local engine. Remote workers join with:\n\n" +
			"  atlas worker --join ws://<this-host>:<port>/workers/connect --token <token>",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", "0.0.0.0:9090", "address the gateway and worker hub listen on")
	cmd.Flags().StringVar(&opts.token, "token", "", "join token workers must present (a random token is generated if unset)")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for state (logs, future SQLite DB)")
	return cmd
}

func runServer(ctx context.Context, cmd *cobra.Command, opts *serverOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.token == "" {
		opts.token = generateAPIKey()
	}
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// Empty gateway: no models yet — workers join and bring their own engines.
	// Proper API-key auth replaces the M0 shared secret in phase 5. With an
	// empty apiKey the gateway rejects every request (Gateway.authenticated
	// compares against "" and no client can present an empty key), which is
	// harmless in phase 1 because no models are routed until phase 2 — but the
	// no-key auth stance must be decided deliberately when routing lands.
	gw := server.NewGateway("", nil, nil)
	hub := server.NewHub(opts.token)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /workers/connect", hub.HandleConnect)
	mux.HandleFunc("GET /admin/workers", hub.HandleListWorkers)
	// Gateway routes (/v1/*, /healthz, /readyz) handled by the gateway's mux
	// as a catch-all; hub routes registered above take precedence.
	mux.Handle("/", gw.Handler())

	srv := &http.Server{
		Addr:              opts.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	_, port := splitHostPort(opts.addr)
	cmd.Println()
	cmd.Printf("Atlas server starting.\n")
	cmd.Printf("  Listen  : %s\n", opts.addr)
	cmd.Printf("  Token   : %s\n", opts.token)
	cmd.Println()
	cmd.Printf("Workers join with:\n")
	cmd.Printf("  atlas worker --join ws://<this-host>:%s/workers/connect --token %s\n", port, opts.token)
	cmd.Println("\nPress Ctrl-C to stop.")

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		cmd.Println("\nShutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-serveErr:
		return fmt.Errorf("server: %w", err)
	}
}

// splitHostPort is a best-effort split that returns the port string from an
// addr like "0.0.0.0:9090" or ":9090". Falls back to the full addr.
func splitHostPort(addr string) (host, port string) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:]
		}
	}
	return addr, addr
}
