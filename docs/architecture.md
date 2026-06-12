# Architecture

This answers the core "how does this actually run?" questions: what processes exist, where they run, how requests flow, and how work is distributed across GPU machines.

## Components

Atlas ships as **one binary with two roles** (plus a combined mode):

```
                    clients (Claude Agent SDK, Claude Code, OpenAI SDKs, curl)
                                        │
                              ANTHROPIC_BASE_URL=https://atlas.yourco.com
                                        │
                  ┌─────────────────────▼─────────────────────┐
                  │              atlas server                  │
                  │              (control plane)               │
                  │                                            │
                  │  gateway: /v1/messages, /v1/chat/completions│
                  │  auth & API keys      usage metering       │
                  │  scheduler            model registry       │
                  │  worker hub           web console          │
                  └───────▲───────────────▲───────────▲───────┘
                          │ outbound,     │           │
                          │ persistent    │           │   (workers dial OUT to server;
                          │ connections   │           │    server never dials in)
                  ┌───────┴──────┐ ┌──────┴──────┐ ┌──┴──────────┐
                  │ atlas worker │ │ atlas worker│ │ atlas worker│
                  │ 4×A100, DC   │ │ 1×4090, home│ │ M3 Max, lap │
                  │              │ │             │ │             │
                  │ ┌──────────┐ │ │ ┌─────────┐ │ │ ┌─────────┐ │
                  │ │  vLLM    │ │ │ │ SGLang  │ │ │ │ MLX /   │ │
                  │ │ (subproc)│ │ │ │(subproc)│ │ │ │llama.cpp│ │
                  │ └──────────┘ │ │ └─────────┘ │ │ └─────────┘ │
                  └──────────────┘ └─────────────┘ └─────────────┘
```

### Vocabulary

| Term | Meaning |
|---|---|
| **Gateway** | The client-facing API endpoint (`/v1/messages`, `/v1/chat/completions`). What `ANTHROPIC_BASE_URL` points at; what the DNS name fronts. It decides where each request goes based on which workers/instances are available and healthy. |
| **Server** (control plane) | The process that hosts the gateway plus the scheduler, model registry, worker hub, auth/metering, and console. One process; "gateway" names its front door. |
| **Worker** | The per-machine agent that runs engines and executes inference. |
| **Engine** | An inference runtime Atlas orchestrates (vLLM, SGLang, llama.cpp, MLX). |
| **Instance** | One running model on one worker (a model definition placed by the scheduler). |

In user-facing docs and marketing, "gateway" and "workers" are the two words; "server" is the process you run to get a gateway.

### `atlas server` — the control plane

Runs anywhere (a VPS, a container, the same box as a worker). **No GPU required.** Responsibilities:

- **Gateway.** Terminates all client API traffic. Validates auth, resolves the requested model (including aliases) to a running model instance, proxies the request to the owning worker, and streams the response back. The gateway speaks Anthropic and OpenAI wire formats natively (see [api-surface.md](api-surface.md)).
- **Scheduler.** Decides which worker runs which model instance. Inputs: model resource requirements (weights size, quantization, context length → VRAM estimate), worker inventory (GPUs, VRAM, RAM, platform), placement constraints (labels, pinning), desired replica count. Outputs: instance assignments. v1 policy is simple best-fit with queueing; fancier policies (prefix-affinity routing à la llm-d) are explicitly deferred.
- **Model registry.** Catalog of model definitions: source (Hugging Face repo / GGUF URL / local path), engine + engine config, chat template / tool-call parser settings, aliases. Includes the curated "works for agents" catalog.
- **Worker hub.** Accepts worker registrations (join token), tracks heartbeats/health, holds the persistent control channels.
- **Auth, usage, console.** API keys (per key: allowed models, rate/budget later), per-key/per-model token usage metering, and a web console for visibility.

### `atlas worker` — the compute agent

One process per machine that has compute. Responsibilities:

- **Hardware detection.** GPUs (CUDA/ROCm/Metal), VRAM, RAM, CPU. Reported to the server on join and on change.
- **Engine lifecycle.** When the scheduler assigns a model instance, the worker downloads weights (with local cache), selects/launches the engine as a supervised subprocess (vLLM, SGLang, llama.cpp server, MLX server), waits for ready, and reports the instance healthy. Restarts on crash with backoff; reports failures.
- **Request execution.** Receives proxied inference requests over its connection to the server, forwards them to the local engine, streams tokens back.
- **Telemetry.** Heartbeats, GPU utilization, instance health, token counts.

### Deployment models

Atlas is self-hosted software. **The default deployment is everything inside the user's own network**: they run the server and the workers in their VPC/DC, expose one DNS endpoint for the gateway (e.g. behind an internal load balancer), and their apps point `ANTHROPIC_BASE_URL` at it. Nothing about Atlas lives on our infrastructure, ever. See [deployment-aws.md](deployment-aws.md) for the concrete AWS picture.

| Model | Server (gateway) lives | Workers live | Who runs this |
|---|---|---|---|
| **Self-contained** (default) | User's VPC/DC | Same network | Almost everyone: single host (`atlas up`) up to a team's GPU fleet |
| **Split / hybrid** (optional) | Wherever the operator wants (one cloud, a VPS, on-prem) | Anywhere else — other clouds, on-prem, a laptop behind NAT | Multi-cloud fleets; vendors offering "bring your own compute" where the vendor hosts the control plane and customers attach workers |

The split model is *enabled* by the connectivity design below, never required by it.

### Connectivity model: workers dial out

The worker opens a persistent outbound connection (gRPC or WebSocket stream — implementation detail, TBD at build time) to the server and keeps it alive. All control messages **and** proxied inference traffic flow over worker-initiated connections.

Why this is load-bearing (ADR-0003): workers must never require inbound connectivity. Even in the self-contained deployment this pays off — GPU workers sit in private subnets with zero inbound security-group rules — and it's what makes the split/hybrid model possible at all (workers behind NAT, in another cloud, on networks the control-plane operator doesn't control). Only the gateway needs a reachable address. (Anthropic's own self-hosted sandbox workers use the same outbound-only pattern, as does every CI runner; it's proven.)

Trade-off: all inference bytes transit the server. Fine for v1 (token streams are small); if it ever matters, a "direct data plane" optimization (gateway redirects to a worker that *chooses* to expose a port) can be added without changing the model.

### Single-node mode

`atlas up` runs server + worker in one process on one machine — no join tokens, no network setup. This is the Ollama-equivalent path and the first thing a new user touches. Architecturally it is the same code: a worker that registers over an in-process channel. Nothing about single-node mode may fork the architecture.

## Request flow

1. Client sends `POST /v1/messages` with `model: "qwen3-coder-72b"` (or an alias like `claude-sonnet-4-6` mapped in config).
2. Gateway authenticates the API key, resolves the model name → model definition → healthy running instance(s).
3. If no instance is running and the model is marked auto-start, the scheduler places one (cold start: download + engine boot; the request either queues with a progress signal or fails fast — config choice). Otherwise pick an instance (round-robin in v1).
4. Gateway translates the request if needed (Anthropic ⇄ engine-native), forwards over the worker channel.
5. Worker passes to the local engine; tokens stream back worker → server → client as SSE matching the client's wire format (`message_start` / `content_block_delta` / … for Anthropic clients).
6. Gateway records usage (input/output tokens per key, model, worker) on completion.

## Scheduling and model lifecycle

- A **model definition** (registry) + **desired state** ("run 2 replicas of X", or "auto-start on first request, stop after N idle minutes") drives the scheduler.
- Placement: filter workers by capability (VRAM fit incl. KV-cache headroom, platform compatibility — GGUF→llama.cpp/MLX, safetensors→vLLM/SGLang), then best-fit. Multi-GPU tensor parallel within a single worker is delegated to the engine (vLLM `--tensor-parallel-size`); splitting a model **across** workers is out of scope (see vision: not exo).
- Idle eviction and VRAM-aware load/evict mirror Ollama's scheduler semantics, generalized to a fleet.

## What we deliberately did not invent

| Question | Answer | Stolen from |
|---|---|---|
| Job distributor or per-machine agent? | Both: scheduler in control plane, agent per machine | GPUStack, every CI system |
| Who runs inference? | Existing engines as supervised subprocesses | GPUStack, Ollama |
| How do machines join? | Join token + outbound persistent connection | GPUStack (token), Anthropic self-hosted workers (outbound-only) |
| Model storage? | Content-addressable cache, manifest + blobs | Ollama / OCI |
| Single-node DX? | Daemon + CLI client, one binary | Ollama |
| API shape? | Anthropic `/v1/messages` + OpenAI `/v1/chat/completions` | vLLM, LiteLLM, Ollama |

## Repository shape (when code starts)

```
/cmd/atlas            # single CLI entrypoint: up | server | worker | pull | run | status ...
/internal/server      # gateway, scheduler, registry, hub, auth, metering
/internal/worker      # hardware detection, engine supervisors, request execution
/internal/api         # wire types: anthropic/, openai/, admin/ (translation lives here)
/internal/engines     # one adapter per engine: vllm/, sglang/, llamacpp/, mlx/
/catalog              # curated model definitions (yaml), agent-capability tested
/docs                 # this design documentation
```
