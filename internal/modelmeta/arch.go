package modelmeta

import (
	"fmt"
	"strings"
)

// KVOverheadFraction pads a model's on-disk weight size to estimate the memory it
// needs once loaded (weights + KV cache + activations). It is the single source of
// truth for that padding: the fleet scheduler's placement fit check
// (internal/server/scheduler.go) references this constant, and the single-node
// pre-download fit gate (M8 Phase 3) uses FitEstimate below, so the two cannot
// drift.
const KVOverheadFraction = 0.2

// FitEstimate is a model's padded memory need in bytes, derived from its
// pre-download weight size. ok is false when the size is unknown (WeightBytes==0),
// in which case the caller skips the fit check and serves best-effort — exactly as
// the scheduler's estimate returns 0 for a model whose size it cannot determine.
func FitEstimate(c Capabilities) (bytes int64, ok bool) {
	if c.WeightBytes <= 0 {
		return 0, false
	}
	return int64(float64(c.WeightBytes) * (1 + KVOverheadFraction)), true
}

// QuantMemoryFactor is the fraction of a model's full-precision (bf16/fp16) weight
// size that a vLLM/SGLang weight-quantization flag yields once loaded on the GPU,
// so the pre-download fit gate weighs the precision the engine will actually serve
// rather than the full-precision on-disk size. The published weight size is
// near-always 16-bit, so 8-bit schemes halve and 4-bit schemes quarter it. An
// unrecognized or absent value returns 1.0 (no scaling) — conservative, since the
// fit gate then keeps its current full-precision estimate and the live engine load
// stays the final authority.
func QuantMemoryFactor(quant string) float64 {
	switch strings.ToLower(strings.TrimSpace(quant)) {
	case "fp8", "fbgemm_fp8", "modelopt", "int8", "w8a8":
		return 0.5
	case "awq", "awq_marlin", "gptq", "gptq_marlin", "marlin", "bitsandbytes", "fp4", "nvfp4", "modelopt_fp4":
		return 0.25
	default:
		return 1.0
	}
}

// ArchLoadable reports whether the pinned build of engine can load the model's
// architecture, and a short pointer reason when it cannot. It is the "engine can
// load this arch" oracle ADR-0015 Decision 3 needs, decoupled from family
// classification: a known-and-listed architecture is loadable; an architecture we
// have a string for but that is absent from the engine's list is refused with a
// reason (the (c) clean-failure case). When we have no architecture string to
// judge, or the engine is unrecognized, it does not block — the live engine load
// is the trust-and-catch backstop (resolved Q1).
//
// engine is a bare engine name ("vllm"/"sglang"/"llamacpp"/"mlx"), matching the
// family map's convention so modelmeta stays free of a worker import.
func ArchLoadable(engine string, c Capabilities) (bool, string) {
	switch engine {
	case "llamacpp":
		// GGUF carries one lowercase architecture token, which inspectGGUF mirrors
		// into ModelType.
		tok := strings.ToLower(strings.TrimSpace(c.ModelType))
		if tok == "" {
			tok = strings.ToLower(strings.TrimSpace(c.Architecture))
		}
		if tok == "" || llamacppArchs[tok] {
			return true, ""
		}
		return false, archReason("llama.cpp", llamaCppPinned, tok,
			"https://github.com/ggml-org/llama.cpp")
	case "vllm", "sglang":
		// Transformers repos are keyed by the architectures[0] class name.
		key := strings.ToLower(strings.TrimSpace(c.Architecture))
		if key == "" || transformersArchs[key] {
			return true, ""
		}
		engVer := vllmPinned
		link := "https://docs.vllm.ai/en/latest/models/supported_models.html"
		if engine == "sglang" {
			engVer = sglangPinned
			link = "https://docs.sglang.ai/supported_models/generative_models.html"
		}
		return false, archReason(engine, engVer, c.Architecture, link)
	case "mlx":
		tok := strings.ToLower(strings.TrimSpace(c.ModelType))
		if tok == "" || mlxTypes[tok] {
			return true, ""
		}
		return false, archReason("MLX", mlxPinned, c.ModelType,
			"https://github.com/ml-explore/mlx-lm")
	default:
		return true, "" // unrecognized engine: don't block, let the load decide
	}
}

// archReason renders the not-loadable pointer: which engine version was checked,
// the architecture it rejected, the engine's supported-models page, and the
// one-line PR pointer to Atlas's list (so a stale Atlas list is as easy to fix as
// it is to diagnose, per ADR-0015 Decision 2/3).
func archReason(engine, version, arch, upstream string) string {
	return fmt.Sprintf("architecture %q is not in %s %s's supported set — see %s. "+
		"If %s does support it, add the architecture to internal/modelmeta/arch.go and open a PR",
		arch, engine, version, upstream, engine)
}

// Engine versions the arch lists below track. These mirror the pins in
// internal/runtime; modelmeta does not import that package (it is a parse leaf),
// so the strings are kept in sync by hand and the values are surfaced in the
// not-loadable message. A pin bump is the prompt to revisit the lists; the
// Phase-5 conformance gate and the live engine load catch any drift.
const (
	vllmPinned     = "0.23.0"
	sglangPinned   = "0.5.10.post1"
	llamaCppPinned = "b9611"
	mlxPinned      = "0.31.3"
)

// transformersArchs is the set of architectures[0] class names the pinned vLLM /
// SGLang builds can load, seeded generously from vLLM's supported-models list so a
// genuinely new or custom architecture is what trips the refusal — not an ordinary
// bring-your-own model. Lowercased keys; broadening is an ordinary
// conformance-gated PR (ADR-0015 Decision 2).
//
// Source: https://docs.vllm.ai/en/latest/models/supported_models.html (text
// generation, as of vLLM 0.23.0). SGLang loads a subset of the same transformers
// architectures (https://docs.sglang.ai/supported_models/generative_models.html);
// the shared set is intentionally generous, with the live engine load as the
// backstop for the rare arch one engine has and the other lacks.
var transformersArchs = lowerSet(
	// Llama family and Llama-architecture derivatives (Yi, etc.).
	"LlamaForCausalLM", "Llama4ForCausalLM",
	// Qwen 2 / 2.5 / 3 / 3.5, dense and MoE.
	"Qwen2ForCausalLM", "Qwen2MoeForCausalLM", "Qwen3ForCausalLM", "Qwen3MoeForCausalLM",
	"Qwen3_5ForConditionalGeneration", "Qwen3_5MoeForConditionalGeneration",
	// Mistral / Mixtral.
	"MistralForCausalLM", "MixtralForCausalLM",
	// Gemma 1 / 2 / 3.
	"GemmaForCausalLM", "Gemma2ForCausalLM", "Gemma3ForCausalLM", "Gemma3ForConditionalGeneration",
	// Phi 1.5 / 2 / 3, and Phi-MoE.
	"PhiForCausalLM", "Phi3ForCausalLM", "Phi3SmallForCausalLM", "PhiMoEForCausalLM",
	// GLM 4 / 4.5 / 5.x, dense and MoE, plus ChatGLM.
	"GlmForCausalLM", "Glm4ForCausalLM", "Glm4MoeForCausalLM", "ChatGLMModel", "ChatGLMForConditionalGeneration",
	// DeepSeek v1 / v2 / v3.
	"DeepseekForCausalLM", "DeepseekV2ForCausalLM", "DeepseekV3ForCausalLM",
	// Cohere Command-R.
	"CohereForCausalLM", "Cohere2ForCausalLM",
	// InternLM 2.
	"InternLM2ForCausalLM", "InternLM2VEForCausalLM",
	// OLMo / OLMo 2.
	"OlmoForCausalLM", "Olmo2ForCausalLM",
	// IBM Granite, dense and MoE.
	"GraniteForCausalLM", "GraniteMoeForCausalLM",
	// State-space and hybrid models vLLM loads (no Atlas parser config yet, but
	// loadable — so they reach the bare/Phase-4 path, not a refusal).
	"MambaForCausalLM", "Mamba2ForCausalLM", "JambaForCausalLM", "Zamba2ForCausalLM",
	// Nemotron, StableLM, Starcoder2, Falcon, GPT-NeoX/2/J/BigCode, OPT, MPT, Baichuan, Bloom, MiniCPM(3), Exaone.
	"NemotronForCausalLM", "StableLmForCausalLM", "Starcoder2ForCausalLM",
	"FalconForCausalLM", "GPTNeoXForCausalLM", "GPT2LMHeadModel", "GPTJForCausalLM", "GPTBigCodeForCausalLM",
	"OPTForCausalLM", "MPTForCausalLM", "BaichuanForCausalLM", "BloomForCausalLM",
	"MiniCPMForCausalLM", "MiniCPM3ForCausalLM", "ExaoneForCausalLM",
	// Databricks DBRX, Snowflake Arctic, Solar, Aquila, XVERSE, Persimmon, Orion, OLMoE, Phi-4.
	"DbrxForCausalLM", "ArcticForCausalLM", "SolarForCausalLM", "AquilaForCausalLM",
	"XverseForCausalLM", "PersimmonForCausalLM", "OrionForCausalLM", "OlmoeForCausalLM", "Phi4ForCausalLM",
	// Multimodal models vLLM serves for text generation (the text path is loadable;
	// Atlas treats them as chat).
	"Qwen2VLForConditionalGeneration", "Qwen2_5_VLForConditionalGeneration", "Qwen3VLForConditionalGeneration",
)

// llamacppArchs is the set of GGUF general.architecture tokens the pinned
// llama.cpp build can load. Lowercased GGUF arch tokens (not class names).
//
// Source: llama.cpp's supported architectures (src/llama-arch.cpp / the project
// README model list), as of tag b9611.
var llamacppArchs = lowerSet(
	"llama", "llama4",
	"qwen2", "qwen2moe", "qwen3", "qwen3moe",
	"gemma", "gemma2", "gemma3",
	"phi2", "phi3", "phimoe",
	"glm4", "glm4moe", "chatglm",
	"deepseek", "deepseek2",
	"command-r", "cohere2",
	"internlm2", "olmo", "olmo2",
	"granite", "granitemoe",
	"falcon", "gptneox", "gpt2", "gptj", "starcoder2", "gptbigcode",
	"mpt", "baichuan", "bloom", "minicpm", "minicpm3", "stablelm", "nemotron",
	"mamba", "mamba2", "jamba", "exaone", "olmoe", "orion", "dbrx", "arctic", "phi4",
)

// mlxTypes is the set of config.json model_type tokens mlx-lm can load (MLX is the
// macOS safetensors path). Lowercased model_type tokens.
//
// Source: mlx-lm's models package (https://github.com/ml-explore/mlx-lm), as of
// mlx-lm 0.31.3.
var mlxTypes = lowerSet(
	"llama", "qwen2", "qwen2_moe", "qwen3", "qwen3_moe",
	"mistral", "mixtral",
	"gemma", "gemma2", "gemma3", "gemma3_text",
	"phi", "phi3", "phimoe",
	"glm", "glm4", "glm4_moe", "chatglm",
	"deepseek", "deepseek_v2", "deepseek_v3",
	"cohere", "cohere2", "internlm2", "olmo", "olmo2", "olmoe",
	"granite", "starcoder2", "stablelm", "minicpm", "minicpm3", "nemotron", "exaone",
	"dbrx", "qwen2_vl", "qwen2_5_vl", "qwen3_vl", "phi4",
)

// lowerSet builds a set whose keys are the lowercased inputs, so callers compare
// case-insensitively without re-lowering the table.
func lowerSet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[strings.ToLower(k)] = true
	}
	return m
}
