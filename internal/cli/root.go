// Package cli wires up the atlas command tree. Subcommands land with the
// build phases in docs/m0-build-plan.md (up, server, worker, pull, run, ps).
package cli

import (
	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/version"
)

// Execute runs the root command with os.Args.
func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "atlas",
		Short: "Self-hosted LLM inference platform",
		Long: "Atlas orchestrates inference engines (vLLM, SGLang, llama.cpp, MLX) on hardware\n" +
			"you control and exposes Anthropic- and OpenAI-compatible APIs.",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newPullCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newPsCmd())
	root.AddCommand(newRuntimeCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the atlas version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println("atlas " + version.String())
		},
	}
}
