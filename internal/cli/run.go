package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/core"
	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/store"
	"github.com/orchestra-hq/atlas/internal/worker"
)

type runOptions struct {
	engine    string
	system    string
	maxTokens int
	quant     string
	stateDir  string
}

func newRunCmd() *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:   "run <model> [prompt]",
		Short: "Run a one-shot prompt against a model and print the reply",
		Long: "run boots a model (provisioning the runtime and pulling a cold catalog\n" +
			"model if needed), sends a single prompt, prints the reply, and shuts the\n" +
			"engine down — the Ollama-equivalent of a quick one-off, with no gateway.\n" +
			"The prompt is the remaining arguments, or stdin when none are given, so\n" +
			"`echo … | atlas run <model>` works. Setup chatter goes to stderr and the\n" +
			"reply to stdout, so the answer is cleanly pipeable.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd.Context(), cmd, opts, args[0], args[1:])
		},
	}
	cmd.Flags().StringVar(&opts.engine, "engine", string(worker.EngineLlamaCpp),
		"inference engine: llamacpp (prebuilt binary), vllm or sglang (uv venv, NVIDIA GPU), or mlx (uv venv, Apple Silicon)")
	cmd.Flags().StringVar(&opts.system, "system", "", "system prompt")
	cmd.Flags().IntVar(&opts.maxTokens, "max-tokens", 512, "maximum tokens to generate")
	cmd.Flags().StringVar(&opts.quant, "quant", "",
		"for a multi-quant Hugging Face GGUF repo, the quantization to serve (e.g. Q4_K_M); default prefers Q4_K_M")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, and logs")
	return cmd
}

func runRun(ctx context.Context, cmd *cobra.Command, opts *runOptions, model string, promptArgs []string) error {
	prompt, err := resolvePrompt(cmd, promptArgs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt: pass it as arguments or on stdin")
	}

	engine, err := parseEngine(opts.engine)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	st := store.New(filepath.Join(opts.stateDir, "store"))

	// Setup chatter (provisioning, pull progress, load notice) goes to stderr so
	// stdout carries only the model's reply; restore stdout before printing it.
	out := cmd.OutOrStdout()
	cmd.SetOut(cmd.ErrOrStderr())
	defer cmd.SetOut(out)

	prov := &atlasruntime.Provisioner{Dir: filepath.Join(opts.stateDir, "runtimes")}
	binPath, err := provisionEngine(ctx, cmd, prov, engine)
	if err != nil {
		return err
	}

	rm, err := resolveModel(ctx, cmd, engine, st, cat, opts.stateDir, opts.quant, model)
	if err != nil {
		return err
	}

	cmd.Printf("Loading model %q (this can take a while on first run)…\n", rm.served)
	w, err := worker.Start(ctx, worker.Config{
		Engine:        engine,
		BinPath:       binPath,
		ModelArgs:     rm.modelArgs,
		ExtraArgs:     rm.engineArgs,
		Model:         rm.served,
		ContextWindow: rm.ctxHint,              // engines that cannot self-report (MLX) answer from this
		Temperature:   rm.sampling.Temperature, // catalog sampling defaults (M2 phase 4a)
		TopP:          rm.sampling.TopP,
		Reasoning:     rm.reasoning, // gates the thinking kwarg (M2 phase 4b)
		LogPath:       filepath.Join(opts.stateDir, logFileName(engine, rm.served)),
	})
	if err != nil {
		return err
	}
	defer func() { _ = w.Stop() }()

	resp, err := w.Execute(ctx, core.Request{
		Model:     rm.served,
		System:    opts.system,
		Messages:  []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock(prompt)}}},
		MaxTokens: opts.maxTokens,
	})
	if err != nil {
		return fmt.Errorf("generation failed: %w", err)
	}

	_, _ = fmt.Fprintln(out, resp.Text())
	return nil
}

// resolvePrompt returns the prompt from the trailing args, or reads it from
// stdin when no args were given.
func resolvePrompt(cmd *cobra.Command, args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	return string(data), nil
}
