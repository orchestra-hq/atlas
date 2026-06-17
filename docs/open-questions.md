# Open questions

Decisions that need the project owner's call (or at least sign-off). When a question is resolved, the outcome is recorded in the doc (or ADR) that owns it and the question is deleted here — git history keeps the trail; this file only ever lists what is actually open.

## Hard enforcement of `tool_choice` forcing (phase 4)

Atlas translates Anthropic `tool_choice` correctly — `auto → auto`, `any → required`, `{type:tool,name} → {type:function,function:{name}}`, `none → none` — and forwards it to the engine ([api-surface.md](api-surface.md), `internal/engines/llamacpp`). But the pinned llama.cpp build (`b9611`) with the Qwen2.5 chat template treats forcing as **advisory**: it selects the named/appropriate tool when the prompt motivates a call, yet will emit plain text (no tool call) when the prompt runs contrary to the forced choice. So `any` / `{tool}` do not _guarantee_ a tool call the way the Anthropic API does.

True forcing would require the gateway to constrain decoding (e.g. supply a GBNF/JSON-schema grammar that mandates a tool call) rather than relying on the engine to honor `tool_choice` — which edges toward reimplementing engine behavior (ADR-0001). **Open:** do we (a) accept best-effort forcing in M0 and document it, (b) add gateway-side grammar enforcement, or (c) gate it on an engine-version bump once a build enforces `tool_choice:required`? Until decided, the G3 suite asserts tool selection on prompt-motivated tasks (the conformance philosophy) rather than forcing against a contrary prompt.

## Reasoning control is model-convention-specific (phase 5)

Atlas maps engine reasoning output to Anthropic `thinking` blocks (ADR-0005) and toggles reasoning per request via the engine's chat-template kwarg `enable_thinking` — the convention hybrid-thinking models (Qwen3, …) expose through llama-server's `--jinja` templating (`internal/engines/llamacpp`, `thinkingKwargs`). This is a **convention, not a standard**: a reasoning model that gates thinking differently (a system-prompt token, a distinct kwarg name, always-on with no off switch) would not respond to `enable_thinking`, so a client that did not request thinking could still get reasoning (which Atlas then drops from the response, wasting tokens but staying correct). `budget_tokens` is **accepted and ignored** on llama.cpp: there is no reasoning-budget knob, and ADR-0005 forbids enforcing budgets by truncation, so it stays advisory.

Both are clean once a per-model reasoning-parser/config registry exists (ADR-0005 consequence). Phase 9 added the catalog, which now **records** per-model config (a `reasoning` flag and per-engine `engine_args` carrying the vLLM tool/reasoning parser flags), but the gateway does not yet **consume** it for reasoning control — llama.cpp still toggles via the global `enable_thinking` convention. **Open:** until the gateway reads per-model reasoning config from the catalog, do we (a) keep the Qwen3 `enable_thinking` convention as the M0 default and document it, or (b) introduce minimal per-model reasoning config earlier? The G4 suite deploys a Qwen3 reasoning model + a non-reasoning model and asserts structural behavior (thinking present/absent, deltas stream, graceful no-op), which holds under either choice.

## vLLM conformance gate awaits a GPU runner (phase 8)

The vLLM adapter, uv runtime provisioning, and `atlas up --engine vllm` landed in phase 8 (sharing the core⇄OpenAI translation with llama.cpp via `internal/engines/openaichat`; unit-tested). But phase 8's exit criterion — _all conformance groups green on both engines_ — is the full-matrix tier, which needs a CUDA runner: vLLM does not run on the CPU PR runner, so the per-PR gate stays llama.cpp `G1–G8`. The harness is already engine-agnostic (`run.py --engine vllm --model <id> --reasoning-model <id>` against a vLLM-backed `atlas up`).

**Open:** stand up the nightly/release GPU conformance run — (a) register a self-hosted CUDA runner (doubles as dogfooding, per [m0-build-plan.md](m0-build-plan.md#testing-tiers)), then add a workflow that runs the harness with `--engine vllm` and the matching Qwen tool/reasoning parser flags (via `atlas up --engine-arg`); or (b) defer until M1 cloud fleets exist. Until then, "both engines green" is asserted by construction (shared translation + unit tests) but not yet observed end-to-end on vLLM. The pinned `VLLMVersion` (and the Qwen `--tool-call-parser hermes` / `--reasoning-parser qwen3` flags) should be re-validated when that run first executes.

## Catalog data the gateway records but does not yet apply (phase 9)

The starter catalog (`/catalog`) carries two fields the build wires through to **storage** but not yet to **request handling**:

1. **Per-model sampling defaults** (`sampling.temperature` / `top_p`) — recorded per entry (wrong defaults visibly degrade tool calling, [model-catalog-m0.md](research/model-catalog-m0.md) finding 3) but not yet applied when a request omits the field. Applying them means threading the catalog entry into `server.Model` and defaulting in the adapter's request build.
2. **Tier metadata** (`tier: haiku|sonnet|opus`) — recorded but does not auto-generate the `claude-*` aliases; `atlas up --alias` is still explicit.

**Open:** apply both, or leave sampling to the operator via `--engine-arg` and aliases to `--alias`? Neither blocks any M0 exit criterion. Phase 10 (CLI polish) landed without picking these up — it focused on the ops minimum (`/readyz`, request-log token counts) and the `run`/`ps` commands — so both remain deferred (candidates for phase 11 or a later pass). Also parked here: the larger gguf tiers (the research-doc 9B–35B models) are not in the shipped catalog because their exact quant digests are not yet pinned — add them as each is verified at build time (pinning is required: `internal/catalog` rejects a gguf entry without a 64-hex sha256).

## Readiness does not re-probe a live engine (phase 10)

`/readyz` returns 200 once a model is registered, and in single-node mode a model is registered only after its worker's `Start` confirms the engine reported healthy — so at startup "ready" genuinely means "a model can answer". It is **not** a live probe: if an engine subprocess dies _after_ registration, `/readyz` keeps returning 200 (the next request surfaces the failure as a 529). For M0's single-node hero path this is acceptable; a periodic worker health re-probe feeding readiness is an M1 concern, alongside the remote-worker hub. Stated as an assumption rather than silently shipped.

## Resolved

How workers get engine runtimes — settled: M0 ships managed runtimes only (downloaded prebuilt llama.cpp binaries, a `uv`-managed vLLM venv), with the container path arriving at M1 behind the same provisioner interface. Recorded in [m0-build-plan.md](m0-build-plan.md#engine-runtime-provisioning) and implemented for llama.cpp in phase 2.
