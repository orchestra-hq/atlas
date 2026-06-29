package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/modelmeta"
	"github.com/orchestra-hq/atlas/internal/server"
	"github.com/orchestra-hq/atlas/internal/worker"
)

// inspectOptions carries the flags for `atlas inspect`, separated from the cobra
// command so runInspect is directly testable (the runPull idiom).
type inspectOptions struct {
	revision string
	asJSON   bool
	noCache  bool
	stateDir string
	endpoint string  // hidden: metadata host override, for tests/mirrors
	vram     float64 // GiB; override detected capacity for the fit check (0 = detect)
	ram      float64 // GiB; alias of vram for CPU/Metal hosts (0 = detect)
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
	cmd.Flags().Float64Var(&opts.vram, "vram", 0, "GiB of GPU memory to check fit against (default: this host's detected capacity)")
	cmd.Flags().Float64Var(&opts.ram, "ram", 0, "GiB of system memory to check fit against, for a CPU/Metal target (default: detected)")
	return cmd
}

func runInspect(ctx context.Context, cmd *cobra.Command, opts *inspectOptions, args []string) error {
	repo := strings.TrimSpace(args[0])
	rev := opts.revision
	if rev == "" {
		rev = modelmeta.DefaultRevision
	}

	res, ok := modelmeta.Result{}, false
	if !opts.noCache {
		res, ok = readInspectCache(opts.stateDir, repo, rev)
	}
	if !ok {
		var err error
		res, err = modelmeta.Inspect(ctx, repo, modelmeta.Options{
			Endpoint: opts.endpoint,
			Revision: opts.revision,
			Token:    hfToken(),
		})
		if err != nil {
			return err
		}
		// Always refresh the cache after a successful fetch, even under --no-cache:
		// the flag means "ignore the cached entry and refresh it", not "never write".
		writeInspectCache(opts.stateDir, repo, rev, res) // best-effort
	}

	// Recompute the host-independent verdict from the (stable) Capabilities rather
	// than trusting the stored Verdict: a cache entry written by an older binary
	// can carry a stale Loadable (e.g. a pre-P8.3 "pending") or an engine chosen on
	// a different host. Then fill Fits live — it is host-dependent and never cached.
	res.Verdict = modelmeta.VerdictFor(res.Capabilities)
	capacity, hasGPU := inspectCapacity(opts)
	res.Verdict.Fits = fitsVerdict(res.Capabilities, capacity)
	return presentInspect(cmd, res, opts.asJSON, capacity, hasGPU)
}

// inspectCapacity resolves the memory the fit check weighs the model against: the
// --vram/--ram override if given (GiB), else this host's detected capacity.
func inspectCapacity(opts *inspectOptions) (bytes int64, hasGPU bool) {
	const gib = 1 << 30
	switch {
	case opts.vram > 0:
		return int64(opts.vram * gib), true
	case opts.ram > 0:
		return int64(opts.ram * gib), false
	default:
		return detectCapacity()
	}
}

// fitsVerdict decides the host-dependent fit dimension: "yes"/"no", or "unknown"
// when the model's size or the host capacity is undetermined (the check is then
// skipped rather than guessed).
func fitsVerdict(caps modelmeta.Capabilities, capacity int64) string {
	est, ok := modelmeta.FitEstimate(caps)
	if !ok || capacity <= 0 {
		return "unknown"
	}
	if est <= capacity {
		return "yes"
	}
	return "no"
}

// hfToken reads a Hugging Face token from the conventional environment variables,
// preferring HF_TOKEN (ADR-0015).
func hfToken() string {
	if t := os.Getenv("HF_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("HUGGING_FACE_HUB_TOKEN")
}

func presentInspect(cmd *cobra.Command, res modelmeta.Result, asJSON bool, capacity int64, hasGPU bool) error {
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
	if c.Selected != "" {
		if len(c.Files) > 1 {
			cmd.Printf("Quants:   %d files; inspected %s (default; override with the file at serve time)\n", len(c.Files), c.Selected)
		} else {
			cmd.Printf("File:     %s\n", c.Selected)
		}
	}
	cmd.Printf("Context:  %s\n", contextLine(c.ContextWindow, c.RopeScaling))
	cmd.Printf("Template: %s\n", presentBool(c.HasChatTemplate, "present", "absent"))
	cmd.Printf("Sampling: %s\n", samplingLine(c.Sampling.Temperature, c.Sampling.TopP))
	cmd.Println()
	cmd.Printf("Verdict:  %s — serving plan derived\n", v.Conclusion)
	cmd.Printf("  engine:   %s\n", orDash(v.Engine))
	cmd.Printf("  family:   %s\n", v.Family)
	cmd.Printf("  parsers:  %s\n", parsersLine(c, v.Engine))
	cmd.Printf("  loadable: %s\n", loadableLine(v, c))
	cmd.Printf("  fits:     %s\n", fitsLine(v.Fits, c, capacity, hasGPU))
	return nil
}

// loadableLine renders the load dimension: "yes", or "no" with the arch-support
// reason (the engine's supported-models pointer + the one-line PR to extend
// Atlas's list).
func loadableLine(v modelmeta.Verdict, c modelmeta.Capabilities) string {
	if v.Loadable != "no" {
		return v.Loadable
	}
	if _, reason := modelmeta.ArchLoadable(v.Engine, c); reason != "" {
		return "no — " + reason
	}
	return "no"
}

// fitsLine renders the fit dimension with the sizing it was decided on: the padded
// estimate vs. the capacity it was weighed against (VRAM or RAM). "unknown" when
// the size or capacity could not be determined.
func fitsLine(fits string, c modelmeta.Capabilities, capacity int64, hasGPU bool) string {
	est, ok := modelmeta.FitEstimate(c)
	if !ok || capacity <= 0 {
		return "unknown (model size or host capacity undetermined)"
	}
	mem := "RAM"
	if hasGPU {
		mem = "VRAM"
	}
	return fmt.Sprintf("%s (needs ~%s, target has %s %s)", fits, humanBytes(est), humanBytes(capacity), mem)
}

// detectCapacity reports this host's schedulable memory and whether it has a GPU,
// reusing the scheduler's CapacityOf so the single-node fit gate and the fleet
// placer share one capacity definition. It is a package var so tests inject a
// capacity without real hardware.
var detectCapacity = func() (int64, bool) {
	return server.CapacityOf(worker.Detect())
}

// parsersLine previews the engine arguments metadata-driven resolution (M8 Phase
// 2) would apply for the candidate engine: a known family's parser flags, or a
// note when the family is unknown or the engine is template-driven (llama.cpp /
// MLX apply the model's own chat template and need none). It shares parserSummary
// with the `atlas up` auto-config note so the two surfaces can't drift.
func parsersLine(c modelmeta.Capabilities, engine string) string {
	f, ok := modelmeta.Classify(c)
	if !ok {
		return "(none — family unknown; served chat-only)"
	}
	return parserSummary(f.EngineArgs(engine))
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

// --- metadata cache (state dir, keyed by repo@revision; ADR-0015) ---

// inspectCacheTTL bounds how long a *mutable* revision's cached metadata is
// trusted. A branch or tag like "main" can move under a fixed name, so its
// capabilities can go stale; an immutable commit SHA never does and is cached
// indefinitely. --no-cache always forces a fresh fetch regardless.
const inspectCacheTTL = 24 * time.Hour

// revIsImmutable reports whether a revision names an unchanging commit (a hex
// SHA, full or abbreviated) rather than a movable branch/tag.
var revIsImmutable = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// inspectCachePath maps a repo@revision to a cache file. The name is a SHA-256 of
// the key so distinct repos/revisions never collide (a sanitize-unsafe-chars
// scheme could map "org/a"@"b" and "org"@"a_b" to the same file).
func inspectCachePath(stateDir, repo, rev string) string {
	sum := sha256.Sum256([]byte(repo + "@" + rev))
	return filepath.Join(stateDir, "inspect-cache", hex.EncodeToString(sum[:])+".json")
}

func readInspectCache(stateDir, repo, rev string) (modelmeta.Result, bool) {
	path := inspectCachePath(stateDir, repo, rev)
	info, err := os.Stat(path)
	if err != nil {
		return modelmeta.Result{}, false
	}
	// A mutable ref's entry expires after the TTL so a moved "main" can't serve
	// stale metadata forever; an immutable SHA is trusted indefinitely.
	if !revIsImmutable.MatchString(rev) && time.Since(info.ModTime()) > inspectCacheTTL {
		return modelmeta.Result{}, false
	}
	data, err := os.ReadFile(path)
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
	_ = os.WriteFile(path, data, 0o600) // private-repo metadata may be cached; keep it owner-only
}
