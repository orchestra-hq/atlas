# M0 starter catalog candidates

Research snapshot (2026-06-12) for the M0 starter catalog: 3–5 agent-tested models spanning the three tier aliases, including at least one reasoning-capable model (ADR-0005) and at least one non-reasoning model (the conformance suite needs both profiles). Exact model versions get pinned at M0 build time — this landscape moves monthly, and the catalog is continuously re-tested per the roadmap's standing track.

## Selection criteria

1. **Permissive license** (Apache 2.0 / MIT — consistent with our own).
2. **Agentic tool-calling quality**, evidenced by benchmarks (SWE-bench family) and community adoption for exactly our use case (people already point Claude Code at these models).
3. **First-class support in both M0 engines** — a vLLM tool parser _and_ reasoning parser exist; GGUF weights + llama.cpp chat-template/tool support exist.
4. **Spans the hardware ladder**: laptop → single 24 GB GPU → 48–96 GB → multi-GPU node, matching the haiku/sonnet/opus aliases.

## Candidates

| Model                                                                    | Size                           | Ctx  | License    | Thinking                              | Hardware (quantized)                    | Role                                                                               |
| ------------------------------------------------------------------------ | ------------------------------ | ---- | ---------- | ------------------------------------- | --------------------------------------- | ---------------------------------------------------------------------------------- |
| [Qwen3.5-9B](https://huggingface.co/Qwen)                                | 9B dense                       | 262K | Apache 2.0 | Hybrid (on/off)                       | Laptop-class, ~8 GB                     | **haiku** default; smallest model that does the tool loop                          |
| [Qwen3.5-35B-A3B](https://huggingface.co/Qwen/Qwen3.5-35B-A3B)           | 35B MoE (A3B)                  | 262K | Apache 2.0 | Hybrid (on/off)                       | 24 GB GPU (~23 GB at UD-Q4)             | **sonnet** default on dev boxes; reasoning conformance                             |
| [GLM-4.7-Flash](https://huggingface.co/zai-org/GLM-4.7-Flash)            | 30B MoE (A3.6B)                | 200K | MIT        | Yes (interleaved)                     | 24 GB GPU                               | **sonnet** alternative; strongest agentic/SWE at this size                         |
| [Qwen3-Coder-Next](https://huggingface.co/unsloth/Qwen3-Coder-Next-GGUF) | 80B MoE (A3B)                  | 256K | Apache 2.0 | **No** (non-reasoning)                | ~46 GB at 4-bit (A100/2×4090/Mac 64 GB) | Coding specialist; **non-reasoning conformance profile**; 70.6% SWE-bench Verified |
| [GLM-5.1](https://docsbot.ai/models/compare/glm-5-1/kimi-k2-6)           | Frontier MoE (multi-hundred-B) | 200K | MIT        | Yes (reasoning + non-reasoning modes) | Multi-GPU H100/H200-class node          | **opus** default where the hardware exists                                         |

Alternates for the opus tier, by traction/hardware: [Kimi K2.6](https://openrouter.ai/compare/moonshotai/kimi-k2.6/z-ai/glm-5.1) (1T MoE, A32B, MIT, 256K — top open-weight on SWE-bench Pro alongside [MiniMax M3](https://www.morphllm.com/best-ai-model-for-coding), which adds a 1M context window) and [Qwen3.5-397B-A17B](https://artificialanalysis.ai/articles/qwen3-5-small-models) (Apache 2.0). [DeepSeek V4](https://whatllm.org/best-agentic-models) leads pure-code benchmarks but is the heaviest to serve. Where opus-class hardware doesn't exist, alias opus to Qwen3-Coder-Next or [Qwen3.5-122B-A10B](https://computertech.co/qwen-3-5-review/).

The Qwen3.5 line satisfies ADR-0005's hybrid requirement in one family: thinking on/off per request, so a single deployment covers both conformance profiles. Note the successor [Qwen3.6 line](https://github.com/QwenLM/Qwen3.6) (35B-A3B already out) — pin whichever is current at build time.

## Per-engine configuration

What the registry must store per model (the "templates/tool parsers" roadmap item), from [vLLM's tool-calling docs](https://docs.vllm.ai/en/latest/features/tool_calling.html) and [reasoning docs](https://docs.vllm.ai/en/latest/features/reasoning_outputs.html):

| Model            | vLLM tool parser            | vLLM reasoning parser           | llama.cpp                                                         |
| ---------------- | --------------------------- | ------------------------------- | ----------------------------------------------------------------- |
| Qwen3.5 family   | `hermes`                    | `qwen3`                         | GGUF (Unsloth UD quants); `--jinja`; native tool + thinking parse |
| GLM-4.7-Flash    | `glm47`                     | `glm45` family (verify for 4.7) | GGUF available; tool calling supported                            |
| Qwen3-Coder-Next | `qwen3_xml` / `qwen3_coder` | — (non-reasoning)               | GGUF (Unsloth); `--jinja`                                         |
| GLM-5.1          | `glm47`/successor (verify)  | `glm45` family (verify)         | GGUF availability to verify at build time                         |

Both engines surface reasoning as a separate channel (vLLM: `reasoning` delta field; [llama-server](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md): thinking-content parsing with `--reasoning-format`), which is exactly the seam Atlas's translation layer maps to Anthropic `thinking` blocks. Per-model sampling defaults belong in the catalog too (e.g. [Qwen3-Coder-Next wants temperature 1.0 / top_p 0.95 with repeat-penalty off](https://unsloth.ai/docs/models/qwen3-coder-next) — wrong defaults visibly degrade tool calling).

## Operational findings worth designing for

1. **Claude Code's attribution header breaks prefix caching on local backends** — reported ~90% slower inference, fixed client-side via `"CLAUDE_CODE_ATTRIBUTION_HEADER": "0"` in settings ([Unsloth's Claude Code guide](https://unsloth.ai/docs/basics/claude-code)). Atlas recipes must document this; worth evaluating a gateway-side mitigation so users don't need to know.
2. **vLLM's `tool_choice: auto` extracts tool calls from free text, not constrained decoding** — malformed JSON arguments are possible ([vLLM docs](https://docs.vllm.ai/en/latest/features/tool_calling.html)). Conformance G3 asserts schema-valid JSON, so the gateway needs a stance: surface engine output as-is and let the catalog exclude models that fail, and/or enable constrained decoding where supported.
3. **KV-cache cost dominates long-context serving** — vLLM `--kv-cache-dtype fp8` halves KV memory ([Unsloth](https://unsloth.ai/docs/models/qwen3-coder-next)); catalog entries should carry recommended serving flags per hardware class, not just model weights.

## Sources

- [Unsloth: local models for Claude Code](https://unsloth.ai/docs/basics/claude-code) · [Qwen3-Coder-Next guide](https://unsloth.ai/docs/models/qwen3-coder-next) · [GLM-4.7-Flash guide](https://unsloth.ai/docs/models/glm-4.7-flash)
- [vLLM tool-calling parsers](https://docs.vllm.ai/en/latest/features/tool_calling.html) · [vLLM reasoning outputs](https://docs.vllm.ai/en/latest/features/reasoning_outputs.html)
- [llama.cpp function calling](https://github.com/ggml-org/llama.cpp/blob/master/docs/function-calling.md) · [llama-server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [Qwen3.5-35B-A3B](https://huggingface.co/Qwen/Qwen3.5-35B-A3B) (Apache 2.0) · [Qwen3.6 repo](https://github.com/QwenLM/Qwen3.6) · [Artificial Analysis on Qwen3.5 small models](https://artificialanalysis.ai/articles/qwen3-5-small-models) · [Qwen 3.5 family overview](https://computertech.co/qwen-3-5-review/)
- [GLM-4.7-Flash model card](https://huggingface.co/zai-org/GLM-4.7-Flash) · [GLM-4.7-Flash stats](https://llm-stats.com/models/glm-4.7-flash)
- [GLM-5.1 vs Kimi K2.6](https://docsbot.ai/models/compare/glm-5-1/kimi-k2-6) · [OpenRouter comparison](https://openrouter.ai/compare/moonshotai/kimi-k2.6/z-ai/glm-5.1) · [SWE-bench Pro rankings, June 2026](https://www.morphllm.com/best-ai-model-for-coding) · [Agentic model rankings](https://whatllm.org/best-agentic-models)
