// Package modelmeta inspects a model's published metadata — without downloading
// its weights — and normalizes it into a single Capabilities record plus a
// staged serving Verdict. It is the read-only foundation of M8 "bring any model"
// (ADR-0015): the same record feeds both the `atlas inspect` command (M8 Phase 1)
// and, later, metadata-driven resolution (Phase 2).
//
// The package is a network/parse leaf — it owns no disk cache and no CLI state
// (the command layer owns those). Two metadata paths feed one resolver: Hugging
// Face transformers repos read config.json/tokenizer_config.json/
// generation_config.json (hf.go); GGUF repos read the file header over a ranged
// request (gguf.go, Phase 1b).
package modelmeta

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/orchestra-hq/atlas/catalog"
)

// DefaultHFEndpoint is the Hugging Face host metadata is fetched from. It is
// overridable (Options.Endpoint) so tests can point at an httptest server and
// advanced users at a mirror.
const DefaultHFEndpoint = "https://huggingface.co"

// DefaultRevision is the git revision used when the caller names none.
const DefaultRevision = "main"

// Format is the on-disk weight format, which selects the candidate engines.
type Format string

// Weight formats Atlas can inspect; the format selects the candidate engines.
const (
	FormatSafetensors Format = "safetensors"
	FormatGGUF        Format = "gguf"
	FormatUnknown     Format = "unknown"
)

// Capabilities is the normalized record both metadata paths produce. Absent
// fields stay at their zero value rather than failing — inspection is tolerant of
// metadata that omits a key (e.g. no generation_config.json means no Sampling).
type Capabilities struct {
	Repo            string           `json:"repo"`
	Revision        string           `json:"revision"`
	Format          Format           `json:"format"`
	Architecture    string           `json:"architecture,omitempty"`   // e.g. "Qwen2ForCausalLM"
	ModelType       string           `json:"model_type,omitempty"`     // e.g. "qwen2"
	ContextWindow   int              `json:"context_window,omitempty"` // 0 when unknown
	RopeScaling     string           `json:"rope_scaling,omitempty"`   // short note, "" when none
	HasChatTemplate bool             `json:"has_chat_template"`        // engine can apply the model's own template
	Sampling        catalog.Sampling `json:"sampling,omitempty"`       // author defaults, when published
	Engines         []string         `json:"engines,omitempty"`        // candidate engines (worker.Engine values)
	// WeightBytes is the summed size of the model's weight files, captured before
	// any download: HF safetensors sum their *.safetensors shards; a GGUF repo
	// reports the selected quant's size; a local/url .gguf its own size. 0 means
	// unknown, which skips the fit check (FitEstimate, M8 Phase 3).
	WeightBytes int64 `json:"weight_bytes,omitempty"`
	// GGUF-only: when a repo holds multiple quantizations, Files lists them and
	// Selected names the one inspected (the Q4_K_M-preferring default). Empty for
	// safetensors repos and single-file GGUF targets.
	Files    []string `json:"files,omitempty"`
	Selected string   `json:"selected,omitempty"`
}

// Conclusion is the headline of a Verdict. Phase 1 always reports
// ConclusionInspected — it shows the derived plan but defers the authoritative
// serve/refuse decision to later phases (see Verdict).
type Conclusion string

const (
	// ConclusionInspected means metadata was read and a plan derived; whether the
	// model serves agent-grade (family, Phase 2) and loads/fits (Phase 3) is not yet
	// decided.
	ConclusionInspected Conclusion = "inspected"
)

// Pending marks a Verdict dimension whose authoritative answer is built in a
// later M8 phase, so `atlas inspect` never implies support it cannot verify yet.
const Pending = "pending"

// Verdict is the three-way serving decision of ADR-0015 (Decision 4). Family is
// decided by the family map (M8 Phase 2): a known family's name, or
// FamilyUnknown. Loadable/Fits are decided by the arch-support list + fit check
// (Phase 3), so they report Pending until built.
type Verdict struct {
	Conclusion Conclusion `json:"conclusion"`
	Engine     string     `json:"engine,omitempty"` // primary candidate engine
	Family     string     `json:"family"`           // detected family, or FamilyUnknown
	Loadable   string     `json:"loadable"`         // "yes"/"no"/Pending (Phase 3)
	Fits       string     `json:"fits"`             // "yes"/"no"/Pending (Phase 3)
}

// Result bundles the derived capabilities and the staged verdict.
type Result struct {
	Capabilities Capabilities `json:"capabilities"`
	Verdict      Verdict      `json:"verdict"`
}

// Options configures an inspection. The zero value inspects the default endpoint
// on the default revision over http.DefaultClient with no auth. Tests point
// Endpoint at an httptest server (the store_test.go idiom).
type Options struct {
	Endpoint string       // metadata host; "" -> DefaultHFEndpoint
	Revision string       // git revision; "" -> DefaultRevision
	Token    string       // Hugging Face token for gated/private repos; "" -> anonymous
	Client   *http.Client // nil -> http.DefaultClient
}

// client returns the configured HTTP client or the default.
func (o Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

// endpoint returns the configured metadata host or the default, without a
// trailing slash.
func (o Options) endpoint() string {
	if o.Endpoint != "" {
		return strings.TrimRight(o.Endpoint, "/")
	}
	return DefaultHFEndpoint
}

// revision returns the configured revision or the default.
func (o Options) revision() string {
	if o.Revision != "" {
		return o.Revision
	}
	return DefaultRevision
}

// Inspect fetches repo's metadata and returns its capabilities and verdict
// without downloading weights. GGUF targets (a repo or path ending in .gguf)
// take the header path (Phase 1b); everything else is read as a Hugging Face
// transformers repo (Phase 1a).
func Inspect(ctx context.Context, repo string, opts Options) (Result, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return Result{}, fmt.Errorf("modelmeta: empty model spec")
	}
	if strings.HasSuffix(strings.ToLower(repo), ".gguf") {
		return inspectGGUF(ctx, repo, opts)
	}
	return inspectHF(ctx, repo, opts)
}

// candidateEngines lists the engines that can serve a given format on this host.
// Safetensors repos go to the GPU engines on Linux and to MLX on macOS; GGUF
// goes to llama.cpp everywhere. Values match worker.Engine string constants.
func candidateEngines(format Format) []string {
	switch format {
	case FormatSafetensors:
		if runtime.GOOS == "darwin" {
			return []string{"mlx"}
		}
		return []string{"vllm", "sglang"}
	case FormatGGUF:
		return []string{"llamacpp"}
	default:
		return nil
	}
}

// verdictFor builds the staged verdict from derived capabilities. Family is
// resolved by the family map (M8 Phase 2) — a known family's name, or
// FamilyUnknown. Loadable (M8 Phase 3) is host-independent — it depends only on
// the pinned engine version and the model's architecture — so it is decided here.
// Fits is host-dependent (it weighs the model against this host's free memory) and
// so is deliberately left Pending in the record: it must not be baked into the
// host-neutral inspect cache. The consumer computes Fits live from local hardware
// at the point of use (`atlas inspect` for display, resolveRaw for the gate).
func verdictFor(c Capabilities) Verdict {
	var engine string
	if len(c.Engines) > 0 {
		engine = c.Engines[0]
	}
	family := FamilyUnknown
	if f, ok := Classify(c); ok {
		family = f.Name
	}
	loadable := "yes"
	if ok, _ := ArchLoadable(engine, c); !ok {
		loadable = "no"
	}
	return Verdict{
		Conclusion: ConclusionInspected,
		Engine:     engine,
		Family:     family,
		Loadable:   loadable,
		Fits:       Pending,
	}
}
