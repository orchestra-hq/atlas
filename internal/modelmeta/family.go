package modelmeta

import "strings"

// FamilyUnknown is the Verdict.Family value for a model whose family Atlas has no
// agent-config for. It is distinct from Pending (a dimension a later phase fills):
// the family question is answered here, and the answer can be "unknown".
const FamilyUnknown = "unknown"

// Family is the per-model-family agent configuration ADR-0015 (Decision 2) lifts
// out of per-model catalog rows: the tool-call / reasoning parser knowledge that
// is keyed by a model's *family*, not its individual id. The map below is the
// extension point — adding support for a family is a normal PR that adds an entry
// here plus a conformance case (M8 Phase 5), "earned by the suite, not vibes."
//
// Reasoning is family-level (engine-agnostic: it gates the thinking kwarg).
// Parser flags are engine-specific — the same family uses different parser names
// on vLLM vs SGLang, and llama.cpp / MLX apply the model's own chat template and
// need none — so each Family carries a per-engine parser set rendered by
// EngineArgs.
type Family struct {
	Name      string // canonical family token, e.g. "qwen3"
	Reasoning bool   // family thinks natively (gates enable_thinking)
	// parsers holds per-engine parser config keyed by the bare engine name
	// ("vllm"/"sglang"). Engines absent from the map (llamacpp, mlx) are
	// template-driven and take no parser flags.
	parsers map[string]engineParsers
}

// engineParsers names the tool-call and (optional) reasoning parser an engine
// needs for a family. The values are the engine's own parser identifiers, which
// differ across engines for the same family.
type engineParsers struct {
	toolCall  string // engine's --tool-call-parser value
	reasoning string // engine's --reasoning-parser value; "" when non-reasoning
}

// families is the seeded family map, lifted from catalog/starter.yaml's
// engine_args (ADR-0015 Decision 2). vLLM/SGLang parser names are taken verbatim
// from the corresponding catalog entries; llama.cpp and MLX are template-driven
// and need no parser entry. Combinations the catalog does not evidence are
// intentionally omitted — an absent engine entry serves with no parser flags
// rather than a guessed one — to be added by conformance-gated PRs.
var families = []Family{
	{
		// Qwen2.5 (Qwen2ForCausalLM / model_type qwen2 / gguf arch qwen2).
		Name:      "qwen2",
		Reasoning: false,
		parsers: map[string]engineParsers{
			"vllm":   {toolCall: "hermes"},
			"sglang": {toolCall: "qwen25"},
		},
	},
	{
		// Qwen3 (Qwen3ForCausalLM / qwen3). Hybrid-thinking: reasoning gated per
		// request, the absence of which leaks <think> blocks (starter.yaml note).
		Name:      "qwen3",
		Reasoning: true,
		parsers: map[string]engineParsers{
			"vllm":   {toolCall: "hermes", reasoning: "qwen3"},
			"sglang": {toolCall: "qwen25", reasoning: "qwen3"},
		},
	},
	{
		// GLM-4.5/4.7/5.x (zai-org). Only the vLLM parser pair is evidenced by the
		// catalog (glm-5.1); SGLang is left to a conformance-gated PR.
		Name:      "glm",
		Reasoning: true,
		parsers: map[string]engineParsers{
			"vllm": {toolCall: "glm47", reasoning: "glm45"},
		},
	},
	{
		// Gemma (gemma2/gemma3). Template-driven on llama.cpp/MLX; base Gemma is
		// non-reasoning (the gemma-4 coder finetune keeps its explicit catalog
		// reasoning=true override). No vLLM/SGLang parser seeded yet.
		Name:      "gemma",
		Reasoning: false,
	},
	{
		// Llama (LlamaForCausalLM). Template-driven; no reasoning channel by default.
		Name:      "llama",
		Reasoning: false,
	},
}

// Classify maps inspected capabilities to a known family, or reports false when
// the model is not one Atlas has agent-config for. It keys on a normalized family
// token derived from model_type (HF) or general.architecture (GGUF, which
// modelmeta mirrors into ModelType), with architectures[0] as a fallback signal.
// An unrecognized model returns false so resolution leaves it on the safe bare
// path rather than mis-applying a parser.
func Classify(c Capabilities) (Family, bool) {
	token := familyToken(c)
	if token == "" {
		return Family{}, false
	}
	for _, f := range families {
		if f.Name == token {
			return f, true
		}
	}
	return Family{}, false
}

// familyToken reduces a model's type/architecture to a canonical family token. It
// tries model_type (HF) / general.architecture (GGUF) first, then the HF
// architectures[0] class name; both are matched case-insensitively by prefix.
func familyToken(c Capabilities) string {
	for _, s := range []string{c.ModelType, c.Architecture} {
		if t := normalizeFamily(s); t != "" {
			return t
		}
	}
	return ""
}

// normalizeFamily matches a model_type or architecture string to a family token
// by prefix (e.g. "Qwen3ForCausalLM" and "qwen3" both -> "qwen3"). qwen3 is
// checked before qwen2 since neither prefix overlaps but the order documents the
// intent. An unrecognized string returns "".
func normalizeFamily(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "qwen3"):
		return "qwen3"
	case strings.HasPrefix(s, "qwen2"):
		return "qwen2"
	case strings.HasPrefix(s, "glm"):
		return "glm"
	case strings.HasPrefix(s, "gemma"):
		return "gemma"
	case strings.HasPrefix(s, "llama"):
		return "llama"
	default:
		return ""
	}
}

// EngineArgs renders the engine-specific parser flags this family needs on the
// given engine (a bare engine name, e.g. "vllm"). Template-driven engines
// (llama.cpp, MLX) and engines this family has no seeded parser for return nil,
// so the model serves on its chat template alone.
func (f Family) EngineArgs(engine string) []string {
	p, ok := f.parsers[engine]
	if !ok {
		return nil
	}
	var args []string
	if engine == "vllm" {
		// vLLM rejects --tool-call-parser without --enable-auto-tool-choice
		// (catalog/starter.yaml note on qwen3-8b).
		args = append(args, "--enable-auto-tool-choice")
	}
	if p.toolCall != "" {
		args = append(args, "--tool-call-parser", p.toolCall)
	}
	if p.reasoning != "" {
		args = append(args, "--reasoning-parser", p.reasoning)
	}
	return args
}
