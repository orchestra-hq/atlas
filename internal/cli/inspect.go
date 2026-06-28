package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/modelmeta"
)

// inspectOptions carries the flags for `atlas inspect`, separated from the cobra
// command so runInspect is directly testable (the runPull idiom).
type inspectOptions struct {
	revision string
	asJSON   bool
	noCache  bool
	stateDir string
	endpoint string // hidden: metadata host override, for tests/mirrors
}

func newInspectCmd() *cobra.Command {
	opts := &inspectOptions{}
	cmd := &cobra.Command{
		Use:   "inspect <model>",
		Short: "Inspect a model's metadata and serving plan without downloading it",
		Long: "Fetch a Hugging Face repo's published metadata (not its weights) and show the\n" +
			"serving plan Atlas derives from it — candidate engine, context window, chat\n" +
			"template, sampling defaults — plus a preliminary verdict. A read-only check:\n" +
			"\"will Atlas run this model, and how?\" (M8 Phase 1).\n\n" +
			"Gated or private repos are read with the token in HF_TOKEN or\n" +
			"HUGGING_FACE_HUB_TOKEN.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(cmd.Context(), cmd, opts, args)
		},
	}
	cmd.Flags().StringVar(&opts.revision, "revision", "", "git revision to inspect (default \"main\")")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "emit the capabilities and verdict as JSON")
	cmd.Flags().BoolVar(&opts.noCache, "no-cache", false, "ignore and refresh the cached metadata for this repo@revision")
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory holding the metadata cache")
	cmd.Flags().StringVar(&opts.endpoint, "hf-endpoint", "", "Hugging Face host to fetch metadata from")
	_ = cmd.Flags().MarkHidden("hf-endpoint")
	return cmd
}

func runInspect(ctx context.Context, cmd *cobra.Command, opts *inspectOptions, args []string) error {
	repo := strings.TrimSpace(args[0])
	rev := opts.revision
	if rev == "" {
		rev = modelmeta.DefaultRevision
	}

	if !opts.noCache {
		if res, ok := readInspectCache(opts.stateDir, repo, rev); ok {
			return presentInspect(cmd, res, opts.asJSON)
		}
	}

	res, err := modelmeta.Inspect(ctx, repo, modelmeta.Options{
		Endpoint: opts.endpoint,
		Revision: opts.revision,
		Token:    hfToken(),
	})
	if err != nil {
		return err
	}

	if !opts.noCache {
		writeInspectCache(opts.stateDir, repo, rev, res) // best-effort
	}
	return presentInspect(cmd, res, opts.asJSON)
}

// hfToken reads a Hugging Face token from the conventional environment variables,
// preferring HF_TOKEN (ADR-0015).
func hfToken() string {
	if t := os.Getenv("HF_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("HUGGING_FACE_HUB_TOKEN")
}

func presentInspect(cmd *cobra.Command, res modelmeta.Result, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		cmd.Println(string(b))
		return nil
	}

	c := res.Capabilities
	v := res.Verdict
	cmd.Printf("Model:    %s (revision %s)\n", c.Repo, c.Revision)
	cmd.Printf("Format:   %s\n", c.Format)
	if len(c.Engines) > 0 {
		cmd.Printf("Engine:   %s (candidate)\n", strings.Join(c.Engines, ", "))
	}
	if c.Architecture != "" || c.ModelType != "" {
		cmd.Printf("Arch:     %s\n", archLine(c.Architecture, c.ModelType))
	}
	cmd.Printf("Context:  %s\n", contextLine(c.ContextWindow, c.RopeScaling))
	cmd.Printf("Template: %s\n", presentBool(c.HasChatTemplate, "present", "absent"))
	cmd.Printf("Sampling: %s\n", samplingLine(c.Sampling.Temperature, c.Sampling.TopP))
	cmd.Println()
	cmd.Printf("Verdict:  %s — serving plan derived\n", v.Conclusion)
	cmd.Printf("  engine:   %s\n", orDash(v.Engine))
	cmd.Printf("  family:   %s\n", pendingLine(v.Family, "M8 Phase 2"))
	cmd.Printf("  loadable: %s\n", pendingLine(v.Loadable, "M8 Phase 3"))
	cmd.Printf("  fits:     %s\n", pendingLine(v.Fits, "M8 Phase 3"))
	return nil
}

func archLine(arch, modelType string) string {
	switch {
	case arch != "" && modelType != "":
		return fmt.Sprintf("%s (%s)", arch, modelType)
	case arch != "":
		return arch
	default:
		return modelType
	}
}

func contextLine(window int, rope string) string {
	if window <= 0 {
		return "unknown"
	}
	if rope != "" {
		return fmt.Sprintf("%d [rope: %s]", window, rope)
	}
	return fmt.Sprintf("%d", window)
}

func samplingLine(temp, topP *float64) string {
	var parts []string
	if temp != nil {
		parts = append(parts, fmt.Sprintf("temperature=%g", *temp))
	}
	if topP != nil {
		parts = append(parts, fmt.Sprintf("top_p=%g", *topP))
	}
	if len(parts) == 0 {
		return "(none published)"
	}
	return strings.Join(parts, " ")
}

func presentBool(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pendingLine renders a verdict dimension, annotating the Pending sentinel with
// the phase that will supply the real answer.
func pendingLine(value, phase string) string {
	if value == modelmeta.Pending {
		return fmt.Sprintf("pending (%s)", phase)
	}
	return value
}

// --- metadata cache (state dir, keyed by repo@revision; ADR-0015) ---

var cacheKeyUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func inspectCachePath(stateDir, repo, rev string) string {
	key := cacheKeyUnsafe.ReplaceAllString(repo+"@"+rev, "_")
	return filepath.Join(stateDir, "inspect-cache", key+".json")
}

func readInspectCache(stateDir, repo, rev string) (modelmeta.Result, bool) {
	data, err := os.ReadFile(inspectCachePath(stateDir, repo, rev))
	if err != nil {
		return modelmeta.Result{}, false
	}
	var res modelmeta.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return modelmeta.Result{}, false // a corrupt cache just triggers a refetch
	}
	return res, true
}

func writeInspectCache(stateDir, repo, rev string, res modelmeta.Result) {
	path := inspectCachePath(stateDir, repo, rev)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
