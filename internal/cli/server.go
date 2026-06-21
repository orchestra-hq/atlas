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

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/server"
)

type serverOptions struct {
	addr             string
	token            string
	aliases          []string
	stateDir         string
	autostartTimeout time.Duration
	idleTimeout      time.Duration
	perReplicaConc   int
	queueLen         int
	maxQueueWait     time.Duration
	retryAfter       int
	affinityTol      int
	affinityPrefix   int
	tls              tlsOptions
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
	cmd.Flags().StringArrayVar(&opts.aliases, "alias", nil,
		"model alias as name=target, e.g. claude-sonnet-4-6=qwen2.5-1.5b-instruct; resolves once a worker registers the target (docs/api-surface.md); repeat for several")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for state (logs, the key store)")
	cmd.Flags().DurationVar(&opts.autostartTimeout, "autostart-timeout", 5*time.Minute,
		"how long a request waits for a model to auto-start on first use (0 disables auto-start)")
	cmd.Flags().DurationVar(&opts.idleTimeout, "idle-timeout", 15*time.Minute,
		"unload an auto-started model after this long with no requests (0 disables idle-stop)")
	// Load balancing + backpressure (M2 phase 2b, ADR-0010). Admission is on by
	// default; --max-concurrency-per-replica 0 restores M1's forward-everything path.
	cmd.Flags().IntVar(&opts.perReplicaConc, "max-concurrency-per-replica", 4,
		"max concurrent requests per replica; a model's ceiling is this × its replica count (0 disables admission)")
	cmd.Flags().IntVar(&opts.queueLen, "queue-length", 256,
		"max requests queued per model once every replica slot is busy, before shedding a retryable 429")
	cmd.Flags().DurationVar(&opts.maxQueueWait, "max-queue-wait", 10*time.Second,
		"how long a request waits in the admission queue for a slot before shedding a retryable 429")
	cmd.Flags().IntVar(&opts.retryAfter, "retry-after", 1,
		"Retry-After seconds advertised on a shed 429/529")
	// Prefix/session-affinity routing (M3 phase 1, ADR-0011). On by default as a
	// load-bounded hint over least-in-flight; --affinity-load-tolerance -1 disables it.
	cmd.Flags().IntVar(&opts.affinityTol, "affinity-load-tolerance", 8,
		"how many more in-flight requests a conversation's warm replica may carry than the least-loaded one before affinity yields to load balancing (-1 disables affinity)")
	cmd.Flags().IntVar(&opts.affinityPrefix, "affinity-prefix-messages", 2,
		"leading conversation messages hashed with the system prompt into the affinity routing key (when no x-atlas-session header is set)")
	// TLS (M1 phase 7, ADR-0009): mutually exclusive modes; default is plaintext
	// (ws://, the dev/internal default per ADR-0007).
	cmd.Flags().StringVar(&opts.tls.certFile, "tls-cert", "", "PEM certificate to serve TLS with (requires --tls-key)")
	cmd.Flags().StringVar(&opts.tls.keyFile, "tls-key", "", "PEM private key for --tls-cert")
	cmd.Flags().BoolVar(&opts.tls.selfSigned, "tls-self-signed", false,
		"serve TLS with a generated self-signed certificate (cached in the state dir); workers join over wss:// with --tls-pin")
	cmd.Flags().StringVar(&opts.tls.acmeDomain, "tls-acme-domain", "",
		"obtain a Let's Encrypt certificate for this public DNS name (the server must be reachable on :443)")
	cmd.Flags().StringVar(&opts.tls.acmeEmail, "tls-acme-email", "", "contact email for the ACME account (optional)")
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

	// Open the control-plane key store and mint a default admin key on first run,
	// so a fresh control plane is usable without a manual `atlas keys create`
	// (ADR-0008). The worker join token stays a separate shared --token.
	store, err := openStateDB(opts.stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	defaultKey, defaultKeyCreated, err := bootstrapDefaultKey(ctx, store)
	if err != nil {
		return fmt.Errorf("bootstrap default key: %w", err)
	}

	aliases, err := parseServerAliases(opts.aliases)
	if err != nil {
		return err
	}

	cat, err := catalog.Load()
	if err != nil {
		return err
	}

	// The gateway starts with no models: workers join over the hub and register
	// the models they serve (M1 phase 2), which the hub adds to the gateway as
	// remote routes. Operator aliases resolve against those models as they join.
	// Client auth validates each request's API key against the store (ADR-0008).
	// Request logs (per-request token counts — G10) go to stderr alongside the
	// banner on stdout.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	authn := keyAuth{db: store}
	gw := server.NewGateway(authn, nil, aliases)
	gw.SetLogger(logger)
	// Durable usage ledger (G13), written through an async batched writer so the
	// per-request INSERT stays off the hot path under load (M2 phase 2b). The writer
	// is stopped (drained + flushed) on shutdown, before the store closes.
	asyncUsage := server.NewAsyncUsageWriter(usageRecorder{db: store}, logger)
	go asyncUsage.Run()
	defer asyncUsage.Close()
	gw.SetUsageRecorder(asyncUsage)
	metrics := server.NewMetrics() // Prometheus instrumentation (G15)
	gw.SetMetrics(metrics)
	// Load balancing + backpressure (M2 phase 2b, ADR-0010): least-in-flight
	// selection plus a bounded per-model admission queue that sheds a retryable
	// 429/529 beyond capacity. SetAdmission wires it to the live replica count and
	// the metrics sink, so it must follow SetMetrics.
	gw.SetAdmission(server.NewAdmission(server.AdmissionConfig{
		PerReplica: opts.perReplicaConc,
		QueueLen:   opts.queueLen,
		MaxWait:    opts.maxQueueWait,
		RetryAfter: opts.retryAfter,
	}))
	// Prefix/session-affinity routing (M3 phase 1, ADR-0011): steer a conversation to
	// the replica holding its warm prefix cache, bounded by load so it never defeats
	// the backpressure above. Wires to the metrics sink, so it follows SetMetrics.
	gw.SetAffinity(server.NewAffinity(server.AffinityConfig{
		Tolerance:      opts.affinityTol,
		PrefixMessages: opts.affinityPrefix,
	}))
	hub := server.NewHub(opts.token, gw)
	// The connected-worker gauge reads the hub's live count at scrape time.
	metrics.SetWorkerCountSource(func() int { return len(hub.Workers()) })
	// The scheduler places catalog models onto workers on demand and reconciles
	// deployments as workers join and leave (M1 phase 4b). The hub notifies it of
	// worker/model events and sends its load/unload commands.
	sched := server.NewScheduler(hub, cat, logger)
	sched.SetLifecycle(opts.autostartTimeout, opts.idleTimeout)
	hub.SetScheduler(sched)
	// Auto-start (a request for an un-deployed catalog model deploys it on the
	// fleet and waits) and idle-stop (an idle auto-started model is unloaded) —
	// M1 phase 4b-2. The reaper runs until the server's context is cancelled.
	gw.SetAutostarter(sched)
	go sched.Run(ctx)

	mux := http.NewServeMux()
	// Worker join is authenticated by the join --token, not an API key.
	mux.HandleFunc("GET /workers/connect", hub.HandleConnect)
	// The /admin/* control surface requires an admin-scoped API key (ADR-0008,
	// phase 5b): the same key store as the client gateway, gated by scope.
	mux.HandleFunc("GET /admin/workers", server.RequireAdmin(authn, hub.HandleListWorkers))
	mux.HandleFunc("POST /admin/workers/{id}/drain", server.RequireAdmin(authn, hub.HandleRemoveWorker))
	mux.HandleFunc("POST /admin/deployments", server.RequireAdmin(authn, sched.HandleSetDeployment))
	mux.HandleFunc("GET /admin/deployments", server.RequireAdmin(authn, sched.HandleListDeployments))
	mux.HandleFunc("DELETE /admin/deployments/{model}", server.RequireAdmin(authn, sched.HandleStopDeployment))
	// Observability (M2 phase 1, G15): a Prometheus scrape and the one-shot
	// snapshot `atlas status` renders, both admin-scoped and reading the same
	// counters.
	mux.Handle("GET /metrics", server.RequireAdmin(authn, metrics.Handler().ServeHTTP))
	mux.HandleFunc("GET /admin/status", server.RequireAdmin(authn, server.StatusHandler(hub.Workers, sched.Deployments, metrics)))
	// Gateway routes (/v1/*, /healthz, /readyz) handled by the gateway's mux
	// as a catch-all; hub routes registered above take precedence.
	mux.Handle("/", gw.Handler())

	host, port := splitHostPort(opts.addr)
	tlsRes, err := resolveServerTLS(opts.tls, opts.stateDir, selfSignedHosts(host))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              opts.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsRes.config,
	}

	wsScheme, httpScheme := "ws", "http"
	if tlsRes.scheme == "https" {
		wsScheme, httpScheme = "wss", "https"
	}
	pinFlag := ""
	if tlsRes.pin != "" {
		pinFlag = " --tls-pin " + tlsRes.pin
	}
	cmd.Println()
	cmd.Printf("Atlas server starting.\n")
	cmd.Printf("  Listen  : %s\n", opts.addr)
	cmd.Printf("  Token   : %s\n", opts.token)
	for _, note := range tlsRes.notes {
		cmd.Printf("  %s\n", note)
	}
	keyForHint := "<your-api-key>"
	if defaultKeyCreated {
		cmd.Printf("  API key : %s  (new default key — save it; it is not shown again)\n", defaultKey)
		keyForHint = defaultKey
	} else {
		cmd.Printf("  API key : use a saved key, or run `atlas keys create`\n")
	}
	cmd.Println()
	cmd.Printf("Workers join with:\n")
	cmd.Printf("  atlas worker --join %s://<this-host>:%s/workers/connect --token %s%s\n", wsScheme, port, opts.token, pinFlag)
	cmd.Printf("\nThen place a model on the fleet:\n")
	adminPinFlag := ""
	if tlsRes.pin != "" {
		// The admin CLI pins the self-signed cert the same way a worker does, so a
		// private/self-signed deployment needs no OS trust-store install (M2 phase 1).
		adminPinFlag = " --tls-pin " + tlsRes.pin
	}
	cmd.Printf("  atlas deploy <model> --server %s://<this-host>:%s%s\n", httpScheme, port, adminPinFlag)
	cmd.Printf("\nPoint a client at it:\n")
	cmd.Printf("  ANTHROPIC_BASE_URL=%s://<this-host>:%s ANTHROPIC_API_KEY=%s\n", httpScheme, port, keyForHint)
	cmd.Println("\nPress Ctrl-C to stop.")

	serveErr := make(chan error, 1)
	go func() {
		serve := srv.ListenAndServe
		if tlsRes.config != nil {
			// Certificates live in TLSConfig (or are fetched by autocert), so the
			// cert/key file args are empty.
			serve = func() error { return srv.ListenAndServeTLS("", "") }
		}
		if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
