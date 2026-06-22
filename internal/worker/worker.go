// Package worker runs on each compute machine: engine supervision and request
// execution. In M0 single-node mode the worker registers with the gateway over
// an in-process channel; remote workers dial out and never listen (ADR-0003).
package worker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/llamacpp"
	"github.com/orchestra-hq/atlas/internal/engines/mlx"
	"github.com/orchestra-hq/atlas/internal/engines/sglang"
	"github.com/orchestra-hq/atlas/internal/engines/vllm"
)

// Engine selects which inference engine a worker supervises. Both speak an
// OpenAI-compatible endpoint (build-time decision 1); they differ in how the
// subprocess is launched and which adapter drives token counting and the
// context-window query.
type Engine string

// The engines Atlas supervises in M0.
const (
	EngineLlamaCpp Engine = "llamacpp"
	EngineVLLM     Engine = "vllm"
	EngineMLX      Engine = "mlx"
	EngineSGLang   Engine = "sglang"
)

// engineAdapter is the gateway-facing capability set every adapter provides. Embed
// serves the embedding model class (M3 phase 2a) via the shared client's
// /v1/embeddings call; a chat model's engine simply never receives an embed request
// (the gateway routes by class), and an embedding-mode engine never receives
// Execute, so both methods coexist on one interface without either path
// misdirecting the other.
type engineAdapter interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
	CountTokens(ctx context.Context, req core.Request) (int, error)
	ContextWindow(ctx context.Context) (int, error)
	Embed(ctx context.Context, req core.EmbedRequest) (core.EmbedResponse, error)
	Rerank(ctx context.Context, req core.RerankRequest) (core.RerankResponse, error)
}

// Config configures a single-engine worker supervising one engine subprocess.
type Config struct {
	// Engine selects the inference engine; empty defaults to llama.cpp.
	Engine Engine
	// BinPath is the engine binary (from the runtime provisioner): llama-server
	// for llama.cpp, or the venv's vllm entrypoint for vLLM.
	BinPath string
	// ModelArgs select the weights: for llama.cpp {"-hf", "repo:quant"} or
	// {"-m", "/path/model.gguf"}; for vLLM the model ref as a positional
	// argument to `vllm serve`, e.g. {"Qwen/Qwen2.5-1.5B-Instruct"}.
	ModelArgs []string
	// Model is the logical name clients address; echoed to the engine.
	Model string
	// ContextWindow is the model's window in tokens, used by engines that cannot
	// report it themselves (MLX: mlx_lm.server exposes no model-metadata endpoint,
	// so the catalog value is threaded here). Zero means "ask the engine" — the
	// llama.cpp and vLLM adapters query it live and ignore this.
	ContextWindow int
	// Temperature/TopP are the model's catalog sampling defaults (M2 phase 4a):
	// applied to a request that omits the field, so a client that sends no sampling
	// params gets the model author's recommended values rather than the engine's
	// generic default (wrong defaults visibly degrade tool calling — research finding
	// 3). Nil means "no catalog default" (a raw path/spec, or an entry without
	// sampling), leaving the field unset for the engine to default. An explicit
	// request value always wins.
	Temperature *float64
	TopP        *float64
	// Reasoning is the model's catalog reasoning capability (M2 phase 4b). It
	// gates the enable_thinking chat-template kwarg the adapter sends: only a
	// reasoning model gets it. False (the default for a raw path/spec, or a
	// non-reasoning entry) omits the kwarg. See openaichat.Client.ThinkingKwargs.
	Reasoning bool
	// Class is the model class (M3 phase 2a, ADR-0012): empty or "embedding". An
	// embedding model launches its engine in embedding mode (e.g. llama.cpp
	// --embedding), where the engine serves /v1/embeddings instead of chat. Empty
	// (the default) is a chat model, launched exactly as before.
	Class string
	// Host/Port is where llama-server listens (loopback in single-node mode).
	// A zero Port asks the OS for a free one.
	Host string
	Port int
	// LogPath receives the engine's stdout+stderr; empty discards them.
	LogPath string
	// ReadyTimeout bounds how long Start waits for the engine to load the
	// model and report healthy. Zero uses defaultReadyTimeout.
	ReadyTimeout time.Duration
	// ExtraArgs are appended verbatim to the llama-server command line.
	ExtraArgs []string
}

const defaultReadyTimeout = 3 * time.Minute

// Worker supervises a llama-server subprocess and executes inference against
// it through the llamacpp adapter. It implements server.Executor.
type Worker struct {
	cfg     Config
	cmd     *exec.Cmd
	logFile *os.File
	adapter engineAdapter

	// done is closed once the subprocess exits; waitErr holds the result and
	// is only read after done is closed.
	done    chan struct{}
	waitErr error
}

// Start launches llama-server and blocks until it reports healthy or the
// ready timeout elapses (or the process exits first). On any failure the
// subprocess is cleaned up before returning.
func Start(ctx context.Context, cfg Config) (*Worker, error) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.Port == 0 {
		port, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("worker: pick port: %w", err)
		}
		cfg.Port = port
	}
	if cfg.Engine == "" {
		cfg.Engine = EngineLlamaCpp
	}

	args, adapter, err := engineSetup(cfg)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(cfg.BinPath, args...)
	var logFile *os.File
	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("worker: open engine log: %w", err)
		}
		cmd.Stdout = f
		cmd.Stderr = f
		logFile = f
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("worker: start llama-server: %w", err)
	}

	w := &Worker{
		cfg:     cfg,
		cmd:     cmd,
		logFile: logFile,
		adapter: adapter,
		done:    make(chan struct{}),
	}
	// Single reaper: cmd.Wait runs exactly once here.
	go func() {
		w.waitErr = cmd.Wait()
		close(w.done)
	}()

	if err := w.waitReady(ctx); err != nil {
		_ = w.Stop()
		return nil, err
	}
	return w, nil
}

// engineSetup builds the engine's command-line arguments and the adapter that
// drives it, per cfg.Engine. Both engines expose an OpenAI-compatible endpoint
// on cfg.Host:cfg.Port; they differ in how the subprocess takes its host/port
// and model arguments.
func engineSetup(cfg Config) (args []string, adapter engineAdapter, err error) {
	baseURL := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	port := fmt.Sprint(cfg.Port)
	switch cfg.Engine {
	case EngineLlamaCpp:
		args = []string{
			"--host", cfg.Host,
			"--port", port,
			"--jinja", // native chat template, tool + thinking parsing (catalog research)
		}
		switch cfg.Class {
		case catalog.ClassEmbedding:
			// Embedding mode: llama-server serves /v1/embeddings (mean-pooled) and
			// stops serving chat. The catalog window doubles as the max sequence the
			// pooled encoder accepts, so cap the context to it.
			args = append(args, "--embedding", "--pooling", "mean")
			if cfg.ContextWindow > 0 {
				args = append(args, "-c", fmt.Sprint(cfg.ContextWindow))
			}
		case catalog.ClassReranker:
			// Reranking mode: llama-server serves /v1/rerank (rank pooling) on a
			// cross-encoder and stops serving chat. Cap the context like embedding mode.
			args = append(args, "--reranking")
			if cfg.ContextWindow > 0 {
				args = append(args, "-c", fmt.Sprint(cfg.ContextWindow))
			}
		}
		args = append(args, cfg.ModelArgs...)
		args = append(args, cfg.ExtraArgs...)
		return args, llamacpp.New(baseURL, cfg.Model, cfg.Reasoning, &http.Client{}), nil
	case EngineVLLM:
		// `vllm serve <model> --host H --port P [extra]`: the model is positional
		// (ModelArgs), tool/reasoning parser flags come from ExtraArgs.
		args = []string{"serve"}
		args = append(args, cfg.ModelArgs...)
		args = append(args, "--host", cfg.Host, "--port", port)
		args = append(args, cfg.ExtraArgs...)
		return args, vllm.New(baseURL, cfg.Model, cfg.Reasoning, &http.Client{}), nil
	case EngineMLX:
		// `<python> -m mlx_lm.server --model <repo> --host H --port P [extra]`:
		// BinPath is the venv python (mlx-lm ships the server as a module). The model
		// is the HF repo id in ModelArgs. Unlike vLLM there is no --served-model-name,
		// and mlx_lm.server loads exactly the id a request names, so the adapter must
		// echo that repo id — not cfg.Model (Atlas's logical served name). The window
		// comes from the catalog (cfg.ContextWindow): mlx_lm.server reports none.
		engineModel := cfg.Model
		if n := len(cfg.ModelArgs); n > 0 {
			engineModel = cfg.ModelArgs[n-1]
		}
		args = []string{"-m", "mlx_lm.server", "--model"}
		args = append(args, cfg.ModelArgs...)
		args = append(args, "--host", cfg.Host, "--port", port)
		args = append(args, cfg.ExtraArgs...)
		return args, mlx.New(baseURL, engineModel, cfg.ContextWindow, cfg.Reasoning, &http.Client{}), nil
	case EngineSGLang:
		// `<python> -m sglang.launch_server --model-path <repo> --host H --port P
		// [extra]`: BinPath is the venv python (sglang ships the server as a module).
		// SGLang accepts --served-model-name (added in resolve.go), so it answers to
		// the logical name and the adapter echoes cfg.Model — like vLLM, unlike MLX.
		args = []string{"-m", "sglang.launch_server", "--model-path"}
		args = append(args, cfg.ModelArgs...)
		args = append(args, "--host", cfg.Host, "--port", port)
		args = append(args, cfg.ExtraArgs...)
		return args, sglang.New(baseURL, cfg.Model, cfg.Reasoning, &http.Client{}), nil
	default:
		return nil, nil, fmt.Errorf("worker: unknown engine %q", cfg.Engine)
	}
}

// Endpoint returns the loopback URL the engine is listening on.
func (w *Worker) Endpoint() string {
	return fmt.Sprintf("http://%s:%d", w.cfg.Host, w.cfg.Port)
}

// Execute runs one inference request against the supervised engine.
func (w *Worker) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	return w.adapter.Execute(ctx, w.withSamplingDefaults(req))
}

// ExecuteStream runs one streaming inference request against the supervised
// engine, forwarding deltas to sink. It satisfies server.StreamExecutor.
func (w *Worker) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	return w.adapter.ExecuteStream(ctx, w.withSamplingDefaults(req), sink)
}

// withSamplingDefaults fills any sampling field the request omitted with the
// model's catalog default (M2 phase 4a). req is taken by value, so this mutates a
// local copy only; an explicit request value is left untouched. Tokenization does
// not sample, so CountTokens deliberately skips this.
func (w *Worker) withSamplingDefaults(req core.Request) core.Request {
	if req.Temperature == nil {
		req.Temperature = w.cfg.Temperature
	}
	if req.TopP == nil {
		req.TopP = w.cfg.TopP
	}
	return req
}

// CountTokens returns the prompt's token count from the engine's tokenizer. It
// satisfies server.TokenCounter.
func (w *Worker) CountTokens(ctx context.Context, req core.Request) (int, error) {
	return w.adapter.CountTokens(ctx, req)
}

// Embed runs one embeddings request against the supervised engine (M3 phase 2a). It
// satisfies server.Embedder. Sampling defaults do not apply — embedding does not
// sample — so the request passes through unmodified.
func (w *Worker) Embed(ctx context.Context, req core.EmbedRequest) (core.EmbedResponse, error) {
	return w.adapter.Embed(ctx, req)
}

// Rerank runs one rerank request against the supervised engine (M3 phase 2b). It
// satisfies server.Reranker. Like embedding it does not sample, so the request
// passes through unmodified.
func (w *Worker) Rerank(ctx context.Context, req core.RerankRequest) (core.RerankResponse, error) {
	return w.adapter.Rerank(ctx, req)
}

// ContextWindow returns the engine's context window in tokens.
func (w *Worker) ContextWindow(ctx context.Context) (int, error) {
	return w.adapter.ContextWindow(ctx)
}

// waitReady polls the engine's /health until it returns 200, failing fast if
// the process exits or the deadline passes.
func (w *Worker) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(w.cfg.ReadyTimeout)
	healthURL := w.Endpoint() + "/health"
	client := &http.Client{Timeout: 2 * time.Second}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
			// Surface the engine's own output: the subprocess writes stdout+stderr to
			// cfg.LogPath, a separate file from the parent's log, so without this tail
			// the cause (a rejected flag, OOM, a bad model ref) is invisible — exactly
			// what made the first GPU acceptance run's vLLM failure undiagnosable.
			return fmt.Errorf("worker: %s engine exited before becoming ready: %w%s", w.cfg.Engine, w.waitErr, w.engineLogTail())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("worker: %s engine not ready within %s%s", w.cfg.Engine, w.cfg.ReadyTimeout, w.engineLogTail())
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				continue // not listening yet
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// engineLogTail returns the last few lines of the engine's log file, formatted
// for appending to a startup-failure error so the engine's own diagnostics (a
// rejected flag, OOM, a missing model) travel with the Go error rather than
// being stranded in a file the caller never reads. Empty string when no log is
// configured or it cannot be read — the error is still returned, just thinner.
func (w *Worker) engineLogTail() string {
	if w.cfg.LogPath == "" {
		return ""
	}
	data, err := os.ReadFile(w.cfg.LogPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// vLLM runs its EngineCore in a subprocess and prints the real failure (CUDA
	// OOM, a bad config) well above the outer async traceback, so a short tail
	// captures only framing frames — the first GPU run's 40-line tail missed the
	// root cause entirely. Keep enough to reach it.
	const maxLines = 200
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	tail := strings.TrimSpace(strings.Join(lines, "\n"))
	if tail == "" {
		return ""
	}
	return fmt.Sprintf("\n--- %s engine log (last %d lines) ---\n%s", w.cfg.Engine, len(lines), tail)
}

// Stop terminates the engine subprocess (idempotent) and releases the log file.
func (w *Worker) Stop() error {
	select {
	case <-w.done:
		// Already exited.
	default:
		_ = w.cmd.Process.Signal(os.Interrupt)
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			_ = w.cmd.Process.Kill()
			<-w.done
		}
	}
	if w.logFile != nil {
		_ = w.logFile.Close()
		w.logFile = nil
	}
	return nil
}

// freePort asks the OS for an unused loopback TCP port. There is an inherent
// race between closing the listener and the engine binding it; acceptable for
// single-node M0 where nothing else is competing for ports.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
