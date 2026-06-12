# Landscape research

Survey of projects adjacent to Atlas, what each does well, and what we take from it. Researched June 2026.

## Summary table

| Project                                            | Layer                | What it is                                                                                                                                                                                                    | License    | Take from it                                                                                                                                                           |
| -------------------------------------------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [vLLM](https://docs.vllm.ai)                       | Engine               | Production GPU serving (PagedAttention, continuous batching). Ships native Anthropic Messages API + [documented Claude Code integration](https://docs.vllm.ai/en/stable/serving/integrations/claude_code/)    | Apache 2.0 | Default engine for CUDA GPUs; proof Anthropic-compat works for agent redirection                                                                                       |
| [SGLang](https://github.com/sgl-project/sglang)    | Engine               | GPU serving optimized for shared-prefix workloads (RadixAttention) — ~29% throughput edge on agent/RAG-style traffic                                                                                          | Apache 2.0 | Second engine option; its prefix caching matters for agents specifically                                                                                               |
| [llama.cpp](https://github.com/ggml-org/llama.cpp) | Engine               | CPU/consumer-hardware inference, GGUF format, minimal deps                                                                                                                                                    | MIT        | Engine for CPU/low-VRAM machines; GGUF as the consumer model format                                                                                                    |
| [MLX](https://github.com/ml-explore/mlx)           | Engine               | Apple Silicon inference                                                                                                                                                                                       | MIT        | Engine for Mac workers                                                                                                                                                 |
| TensorRT-LLM                                       | Engine               | NVIDIA-tuned max-performance serving                                                                                                                                                                          | Apache 2.0 | Possible later engine; high integration cost, skip for v1                                                                                                              |
| TGI (HuggingFace)                                  | Engine               | Archived March 2026, maintenance mode                                                                                                                                                                         | —          | Cautionary tale: don't build on it; engine landscape shifts, so keep engines pluggable                                                                                 |
| [Ollama](https://github.com/ollama/ollama)         | Single-node platform | Go client/server daemon; model pull/run lifecycle; OCI-style content-addressable model store; VRAM-aware scheduler; OpenAI **and Anthropic** compat endpoints                                                 | MIT        | The DX bar. Steal: single binary, `pull`/`run` verbs, registry design, background daemon + CLI client split                                                            |
| [GPUStack](https://github.com/gpustack/gpustack)   | Cluster platform     | **Closest analogue.** Server (CPU-only control plane) + GPU workers joined by token; orchestrates vLLM/SGLang/TensorRT-LLM; scheduler picks engine + placement; OpenAI-compat APIs, auth, metering, dashboard | Apache 2.0 | Validates our exact architecture. Their gaps = our wedge: Python-heavy install, OpenAI-only (no Anthropic surface), platform-first rather than agent-first positioning |
| [llm-d](https://llm-d.ai)                          | K8s inference stack  | Red Hat/Google/IBM; Envoy-based routing, disaggregated prefill/decode workers                                                                                                                                 | Apache 2.0 | Where high-scale serving is going; K8s-coupled, different audience. Revisit for ideas at M3+, don't compete                                                            |
| KubeAI                                             | K8s operator         | Lightweight scale-from-zero model operator                                                                                                                                                                    | Apache 2.0 | Confirms demand for "simpler than llm-d"; still K8s-only                                                                                                               |
| [exo](https://github.com/exo-explore/exo)          | Distributed consumer | Shards one model across laptops/phones/Macs; auto device discovery; OpenAI + **Claude Messages** + Ollama API compat                                                                                          | **GPL**    | Auto-discovery UX is inspiring; model sharding out of scope for us; GPL means never vendor code from it                                                                |
| [LiteLLM](https://github.com/BerriAI/litellm)      | Gateway              | Translation proxy across 100+ providers; serves [`/v1/messages` in Anthropic format](https://docs.litellm.ai/docs/anthropic_unified/); keys, spend tracking, guardrails                                       | MIT        | Reference for API translation semantics and gateway features (virtual keys, budgets). Routes to providers; doesn't run models — complementary, not a competitor        |

## Key findings

### 1. Anthropic API compatibility is becoming table stakes — but nobody leads with it

vLLM, Ollama, and exo all now expose Anthropic-compatible endpoints, and LiteLLM translates to `/v1/messages`. The community has converged on `ANTHROPIC_BASE_URL` redirection as the way to run Claude Code / Agent SDK against local models ([vLLM docs](https://docs.vllm.ai/en/stable/serving/integrations/claude_code/), [community guides](https://renezander.com/guides/claude-code-local-llm-anthropic-base-url/)). The redirection recipe is consistent: set `ANTHROPIC_BASE_URL`, dummy `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, and map model tiers via `ANTHROPIC_DEFAULT_OPUS_MODEL` / `_SONNET_` / `_HAIKU_` env vars.

**Implication:** API translation alone is zero differentiation. Our edge must be the orchestration layer + DX + agent-first packaging (model alias mapping, curated agent-capable models, one-command setup). It also de-risks us: the compat surface is well-trodden, with multiple open implementations to study.

### 2. The architecture question is settled in the field

Every multi-machine system converges on the same shape: **a CPU-only control plane** (API gateway, scheduler, registry, UI) **+ a worker agent per GPU machine** that registers with the control plane (GPUStack uses token-based worker auth) and runs the actual engine. The control plane schedules model instances onto workers based on resources; the gateway routes inference requests to the right worker. This directly answers "is it a job distributor with a process per GPU machine?" — yes, and we should not be clever about it. Details in [architecture.md](../architecture.md).

### 3. Don't write an engine; don't even favor one

The engine layer is where the most capital and talent is concentrated (vLLM, SGLang) and where projects die (TGI archived). GPUStack's "pluggable engine" framing is right: the platform's job is to pick, configure, launch, and supervise engines per model/hardware combo. Practical default matrix: vLLM (CUDA, production), SGLang (CUDA, agent/prefix-heavy), llama.cpp (CPU/consumer GPU, GGUF), MLX (Apple Silicon).

### 4. Ollama's design is the single-node gold standard

Worth studying in depth ([architecture docs](https://deepwiki.com/ollama/ollama/2-architecture)): Go daemon exposing REST on `:11434`; CLI is just a client; models stored content-addressably (manifests + blobs, OCI-image style, dedup of shared weights); a scheduler loads/evicts models based on available VRAM and `OLLAMA_MAX_LOADED_MODELS`; Modelfile declares template/params per model. Their ceiling — single-user concurrency, no fleet story — is exactly the room we grow into.

### 5. Tool calling is the real model-quality frontier for our use case

Agent workloads live and die on tool-call reliability. Community experience running Claude Code against local models consistently flags "model must have strong tool calling" and chat-template correctness as the failure points. Engines handle parsing (vLLM/SGLang have per-family tool-call parsers), but **someone has to curate which models + configs actually work for agents** — that's an Atlas deliverable (the model catalog), not an engine feature.

### 6. Licensing

Everything we want to learn from is Apache 2.0 or MIT except exo (GPL). We can read exo for ideas but must never copy code from it. Recommendation for Atlas: Apache 2.0 (patent grant matters for infra adoption; matches vLLM/GPUStack norms).

## Sources

- [LLM inference servers compared — TensorFoundry](https://tensorfoundry.io/blog/llm-inference-servers-compared)
- [vLLM vs Ollama vs SGLang vs TensorRT-LLM 2026](https://theaiengineer.substack.com/p/vllm-vs-ollama-vs-sglang-vs-tensorrt)
- [Local inference engines comparison 2026 — Sesame Disk](https://sesamedisk.com/local-inference-engines-2026-comparison/)
- [vLLM Claude Code integration docs](https://docs.vllm.ai/en/stable/serving/integrations/claude_code/)
- [Claude Code with local LLMs via ANTHROPIC_BASE_URL](https://renezander.com/guides/claude-code-local-llm-anthropic-base-url/)
- [Running Claude Code with local LLMs via vLLM and LiteLLM](https://dev.to/dcruver/running-claude-code-with-local-llms-via-vllm-and-litellm-599b)
- [LiteLLM /v1/messages docs](https://docs.litellm.ai/docs/anthropic_unified/)
- [GPUStack repo](https://github.com/gpustack/gpustack) and [intro post](https://gpustack.ai/introducing-gpustack/)
- [llm-d on kgateway](https://kgateway.dev/blog/llm-d-kgateway/)
- [Ollama architecture (DeepWiki)](https://deepwiki.com/ollama/ollama/2-architecture), [model registry & layers](https://deepwiki.com/ollama/ollama/4.2-model-registry-and-layers)
- [exo repo](https://github.com/exo-explore/exo)
