package cli

import (
	"path/filepath"

	"github.com/spf13/cobra"

	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// newRuntimeCmd groups maintenance commands for engine runtimes. Its one
// subcommand, `provision`, pre-downloads a pinned engine runtime into the
// state dir without serving anything — the same work `atlas up` does lazily on
// first run, exposed on its own so an image build (or an operator pre-warming a
// host) can bake the runtime ahead of time. Because it provisions into the
// exact path `atlas up` resolves, a baked runtime makes the later `up` a no-op
// (see ADR-0006: the CUDA image bakes the vLLM venv this way).
func newRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage engine runtimes",
	}
	cmd.AddCommand(newRuntimeProvisionCmd())
	return cmd
}

func newRuntimeProvisionCmd() *cobra.Command {
	var (
		engineFlag string
		stateDir   string
	)
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Download a pinned engine runtime into the state dir",
		Long: "Provision pre-downloads the pinned runtime for an engine (the\n" +
			"llama-server binary, or a uv-bootstrapped vLLM venv) into the state\n" +
			"dir, the same work `atlas up` does lazily on first run. Use it to bake\n" +
			"a runtime into a container image or pre-warm a host. It is idempotent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			engine, err := parseEngine(engineFlag)
			if err != nil {
				return err
			}
			prov := &atlasruntime.Provisioner{Dir: filepath.Join(stateDir, "runtimes")}
			binPath, err := provisionEngine(cmd.Context(), cmd, prov, engine)
			if err != nil {
				return err
			}
			cmd.Printf("Provisioned %s runtime: %s\n", engine, binPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&engineFlag, "engine", string(worker.EngineLlamaCpp), "engine to provision (llamacpp, vllm, mlx, or sglang)")
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, and logs")
	return cmd
}
