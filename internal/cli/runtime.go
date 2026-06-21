package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	atlasruntime "github.com/orchestra-hq/atlas/internal/runtime"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// provisionableEngines are the engines `atlas runtime list`/`upgrade` know how to
// manage, each backed by a pinned runtime version.
var provisionableEngines = []worker.Engine{
	worker.EngineLlamaCpp, worker.EngineVLLM, worker.EngineMLX, worker.EngineSGLang,
}

// pinnedVersion is the runtime version currently compiled in for an engine — the
// one provision/upgrade installs and the one `list` marks as pinned.
func pinnedVersion(engine worker.Engine) string {
	switch engine {
	case worker.EngineVLLM:
		return atlasruntime.VLLMVersion
	case worker.EngineMLX:
		return atlasruntime.MLXVersion
	case worker.EngineSGLang:
		return atlasruntime.SGLangVersion
	default:
		return atlasruntime.LlamaCppTag
	}
}

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
	cmd.AddCommand(newRuntimeProvisionCmd(), newRuntimeUpgradeCmd(), newRuntimeListCmd())
	return cmd
}

// newRuntimeUpgradeCmd provisions the currently-pinned version of an engine
// runtime and, with --prune, removes any older provisioned versions. The pinned
// version is compiled in (build-time decision 5: explicit versions, explicit
// upgrades), so the upgrade flow is: update Atlas (which bumps the pinned version),
// then `atlas runtime upgrade --engine <e>` to provision the new runtime, then
// restart the engine so it starts on the new version. Provisioning stages
// atomically, so a crash never leaves a half-installed runtime.
//
// --prune deletes the other on-disk versions of the engine. A still-running engine
// keeps the version it started with (its process holds that venv) and only adopts
// the new one on restart, so prune is safe only once no engine is serving an older
// version — otherwise it can pull modules out from under a live process that
// imports lazily. Liveness-aware pruning is a follow-up (docs/follow-ups.md).
func newRuntimeUpgradeCmd() *cobra.Command {
	var (
		engineFlag string
		stateDir   string
		prune      bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Provision the pinned version of an engine runtime, optionally pruning older ones",
		Long: "upgrade provisions the engine runtime version currently pinned in this\n" +
			"Atlas build (the same one `atlas up` resolves). The install stages\n" +
			"atomically and swaps into place, so a crash never leaves a half-upgraded\n" +
			"runtime. A running engine keeps serving the version it started with and\n" +
			"picks up the new one only on restart.\n\n" +
			"--prune then removes any OTHER provisioned versions to reclaim disk. Run it\n" +
			"only when no engine is still serving an older version: it deletes that\n" +
			"version's files, which a live engine may still import lazily.",
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
			cmd.Printf("Provisioned %s runtime %s: %s\n", engine, pinnedVersion(engine), binPath)
			if prune {
				removed, err := prov.Prune(string(engine), pinnedVersion(engine))
				if err != nil {
					return err
				}
				if len(removed) == 0 {
					cmd.Printf("No older %s versions to prune.\n", engine)
				} else {
					cmd.Printf("Pruned older %s versions: %s\n", engine, strings.Join(removed, ", "))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engineFlag, "engine", string(worker.EngineLlamaCpp), "engine to upgrade (llamacpp, vllm, mlx, or sglang)")
	cmd.Flags().BoolVar(&prune, "prune", false, "after provisioning, delete other provisioned versions of this engine (only safe when no engine is still serving one)")
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "directory for runtimes")
	return cmd
}

// newRuntimeListCmd shows, per engine, the pinned runtime version and which
// versions are provisioned on this host — the observability for the upgrade flow.
func newRuntimeListCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show each engine's pinned runtime version and which versions are provisioned",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prov := &atlasruntime.Provisioner{Dir: filepath.Join(stateDir, "runtimes")}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ENGINE\tPINNED\tPROVISIONED")
			for _, e := range provisionableEngines {
				versions, err := prov.ProvisionedVersions(string(e))
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", e, pinnedVersion(e), formatProvisioned(versions, pinnedVersion(e)))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "directory for runtimes")
	return cmd
}

// formatProvisioned renders the provisioned versions, tagging the pinned one. An
// empty list (nothing provisioned yet) renders as a dash.
func formatProvisioned(versions []string, pinned string) string {
	if len(versions) == 0 {
		return "—"
	}
	marked := make([]string, len(versions))
	for i, v := range versions {
		if v == pinned {
			marked[i] = v + " (pinned)"
		} else {
			marked[i] = v
		}
	}
	return strings.Join(marked, ", ")
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
