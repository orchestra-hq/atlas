package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/server"
)

type serverOptions struct {
	addr     string
	token    string
	apiKey   string
	aliases  []string
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
	cmd.Flags().StringVar(&opts.apiKey, "api-key", "", "API key clients must present to the gateway (a random key is generated if unset)")
	cmd.Flags().StringArrayVar(&opts.aliases, "alias", nil,
		"model alias as name=target, e.g. claude-sonnet-4-6=qwen2.5-1.5b-instruct; resolves once a worker registers the target (docs/api-surface.md); repeat for several")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for state (logs, future SQLite DB)")
	return cmd
}

// parseServerAliases parses name=target alias flags for `atlas server`. Unlike
// the single-node parser it does not check the target is already served: on the
// control plane models register dynamically as workers join, so an alias may
// name a model that has not joined yet (it resolves once it does).
func parseServerAliases(flags []string) (map[string]string, error) {
	aliases := map[string]string{}
	for _, raw := range flags {
		name, target, ok := strings.Cut(raw, "=")
		if !ok || name == "" || target == "" {
			return nil, fmt.Errorf("invalid --alias %q: want name=target", raw)
		}
		if _, dup := aliases[name]; dup {
			return nil, fmt.Errorf("duplicate alias %q", name)
		}
		aliases[name] = target
	}
	return aliases, nil
}

func runServer(ctx context.Context, cmd *cobra.Command, opts *serverOptions) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.token == "" {
		opts.token = generateAPIKey()
	}
	if opts.apiKey == "" {
		opts.apiKey = generateAPIKey()
	}
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	aliases, err := parseServerAliases(opts.aliases)
	if err != nil {
		return err
	}

	// The gateway starts with no models: workers join over the hub and register
	// the models they serve (M1 phase 2), which the hub adds to the gateway as
	// remote routes. Operator aliases resolve against those models as they join.
	// Client auth here reuses M0's shared secret (--api-key); per-key management
	// replaces it in phase 5. Request logs (per-request token counts — G10) go to
	// stderr alongside the banner on stdout.
	gw := server.NewGateway(opts.apiKey, nil, aliases)
	gw.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	hub := server.NewHub(opts.token, gw)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /workers/connect", hub.HandleConnect)
	mux.HandleFunc("GET /admin/workers", hub.HandleListWorkers)
	mux.HandleFunc("POST /admin/workers/{id}/drain", hub.HandleRemoveWorker)
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
	cmd.Printf("  API key : %s\n", opts.apiKey)
	cmd.Println()
	cmd.Printf("Workers join with:\n")
	cmd.Printf("  atlas worker --join ws://<this-host>:%s/workers/connect --token %s --model <model>\n", port, opts.token)
	cmd.Printf("\nPoint a client at it:\n")
	cmd.Printf("  ANTHROPIC_BASE_URL=http://<this-host>:%s ANTHROPIC_API_KEY=%s\n", port, opts.apiKey)
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
