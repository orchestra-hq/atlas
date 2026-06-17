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
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/engines/llamacpp"
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
)

// engineAdapter is the gateway-facing capability set both adapters provide.
type engineAdapter interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
	CountTokens(ctx context.Context, req core.Request) (int, error)
	ContextWindow(ctx context.Context) (int, error)
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
		args = append(args, cfg.ModelArgs...)
		args = append(args, cfg.ExtraArgs...)
		return args, llamacpp.New(baseURL, cfg.Model, &http.Client{}), nil
	case EngineVLLM:
		// `vllm serve <model> --host H --port P [extra]`: the model is positional
		// (ModelArgs), tool/reasoning parser flags come from ExtraArgs.
		args = []string{"serve"}
		args = append(args, cfg.ModelArgs...)
		args = append(args, "--host", cfg.Host, "--port", port)
		args = append(args, cfg.ExtraArgs...)
		return args, vllm.New(baseURL, cfg.Model, &http.Client{}), nil
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
	return w.adapter.Execute(ctx, req)
}

// ExecuteStream runs one streaming inference request against the supervised
// engine, forwarding deltas to sink. It satisfies server.StreamExecutor.
func (w *Worker) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	return w.adapter.ExecuteStream(ctx, req, sink)
}

// CountTokens returns the prompt's token count from the engine's tokenizer. It
// satisfies server.TokenCounter.
func (w *Worker) CountTokens(ctx context.Context, req core.Request) (int, error) {
	return w.adapter.CountTokens(ctx, req)
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
			return fmt.Errorf("worker: llama-server exited before becoming ready: %w", w.waitErr)
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("worker: llama-server not ready within %s", w.cfg.ReadyTimeout)
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
