# Open questions

Decisions that need the project owner's call (or at least sign-off). When a question is resolved, the outcome is recorded in the doc (or ADR) that owns it and the question is deleted here — git history keeps the trail; this file only ever lists what is actually open.

## Hard enforcement of `tool_choice` forcing (phase 4)

Atlas translates Anthropic `tool_choice` correctly — `auto → auto`, `any → required`, `{type:tool,name} → {type:function,function:{name}}`, `none → none` — and forwards it to the engine ([api-surface.md](api-surface.md), `internal/engines/llamacpp`). But the pinned llama.cpp build (`b9611`) with the Qwen2.5 chat template treats forcing as **advisory**: it selects the named/appropriate tool when the prompt motivates a call, yet will emit plain text (no tool call) when the prompt runs contrary to the forced choice. So `any` / `{tool}` do not _guarantee_ a tool call the way the Anthropic API does.

True forcing would require the gateway to constrain decoding (e.g. supply a GBNF/JSON-schema grammar that mandates a tool call) rather than relying on the engine to honor `tool_choice` — which edges toward reimplementing engine behavior (ADR-0001). **Open:** do we (a) accept best-effort forcing in M0 and document it, (b) add gateway-side grammar enforcement, or (c) gate it on an engine-version bump once a build enforces `tool_choice:required`? Until decided, the G3 suite asserts tool selection on prompt-motivated tasks (the conformance philosophy) rather than forcing against a contrary prompt.

## Reasoning control is model-convention-specific (phase 5)

Atlas maps engine reasoning output to Anthropic `thinking` blocks (ADR-0005) and toggles reasoning per request via the engine's chat-template kwarg `enable_thinking` — the convention hybrid-thinking models (Qwen3, …) expose through llama-server's `--jinja` templating (`internal/engines/llamacpp`, `thinkingKwargs`). This is a **convention, not a standard**: a reasoning model that gates thinking differently (a system-prompt token, a distinct kwarg name, always-on with no off switch) would not respond to `enable_thinking`, so a client that did not request thinking could still get reasoning (which Atlas then drops from the response, wasting tokens but staying correct). `budget_tokens` is **accepted and ignored** on llama.cpp: there is no reasoning-budget knob, and ADR-0005 forbids enforcing budgets by truncation, so it stays advisory.

Both are clean once a per-model reasoning-parser/config registry exists (ADR-0005 consequence). Phase 9 added the catalog, which **records** per-model config (a `reasoning` flag and per-engine `engine_args` carrying the vLLM tool/reasoning parser flags).

**Partially resolved (M2 phase 4b):** the catalog `reasoning` flag is now **consumed** — it gates the `enable_thinking` chat-template kwarg. A reasoning model gets the kwarg (set from request intent); a **non-reasoning model omits it entirely**, so Atlas no longer injects a template var the model does not use (previously sent always, relying on it being a harmless no-op). The flag threads `worker.Config` → `openaichat.Client`, gated in `ThinkingKwargs`, so it applies uniformly across llama.cpp and the OpenAI-compatible engines (vLLM/MLX/SGLang). The G4 suite's structural assertions (thinking present/absent, deltas stream, graceful no-op) continue to hold.

**Still open:** a richer per-model reasoning _style_ for models that gate thinking differently than the Qwen3 `enable_thinking` convention — a distinct kwarg name, a system-prompt token, or always-on with no off switch. The current `reasoning: bool` cannot express those; a catalog with such a model would need a reasoning-style field (and the matching adapter logic). Deferred until such a model enters the shipped catalog. `budget_tokens` remains advisory (ADR-0005 forbids truncation).

## Engine/host breadth beyond the M0 acceptance tier (M2 phases 3, 4c)

The M0/M0.5 acceptance gate — vLLM + llama.cpp, all groups green, real Claude Code drop-in — is **met and M0 is declared done** (2026-06-25; see [m0-acceptance.md](m0-acceptance.md) for the green run that closed it). The on-demand CUDA runner that gate once waited on now exists (`machulav/ec2-github-runner`, [`examples/acceptance/`](../examples/acceptance/README.md)), and the scheduled nightly runs vLLM on a GPU box and llama.cpp on a compute-optimised CPU box. What remains open are **M2-scope engine/host breadth** items layered on top of that machinery — none of them block M0:

- **MLX (phase 3a)** runs only on **Apple Silicon** (`darwin/arm64`, Metal), so its G17 cell needs an **Apple-Silicon CI runner** that does not exist yet (GitHub-hosted `macos-14`+ arm runners or a self-hosted Mac). Until one is wired up, MLX is exercised by the per-PR unit/fake-runner tests (adapter translation, the `max_tokens:1` count-tokens probe, provisioning orchestration) and by local Mac runs, but the real G1–G8+G10 suite on MLX is not gated. Flagged here, not silently shipped.
- **SGLang (phase 3b)** is a second NVIDIA-GPU server, so its G17 cell can share the **same CUDA runner the vLLM nightly already uses** — but the nightly SGLang cell is not yet wired up, so the pinned `SGLangVersion` and the catalog's SGLang `--tool-call-parser` / `--reasoning-parser` flags stay unit-validated against the server's source rather than observed end-to-end. Add the cell when SGLang breadth is taken up.
- **Full published capability matrix (phase 4c, G18).** The agent-capability matrix is generated by [`conformance/capability_matrix.py`](../conformance/README.md), which aggregates the per-(engine, model) `matrix.json` runs into the published "works for agents" matrix. The generator and its verdict logic are unit-tested on the CPU PR tier, the per-PR job runs it on the llama.cpp cell, and the nightly now feeds real vLLM rows — but the **full** matrix (every catalog model × every engine that can serve it) is only as complete as the runs feeding it, which grow as the MLX/SGLang cells above land and as catalog quant digests are pinned (the standing pin-on-verify activity below).

## Catalog data the gateway records but does not yet apply (phase 9)

The starter catalog (`/catalog`) carries fields the build wires through to **storage**; M2 phase 4 applies them to **request handling**:

1. **Per-model sampling defaults** (`sampling.temperature` / `top_p`) — recorded per entry (wrong defaults visibly degrade tool calling, [model-catalog-m0.md](research/model-catalog-m0.md) finding 3). **Resolved (M2 phase 4a):** the worker applies them at its `Execute`/`ExecuteStream` choke point — a request that omits the field picks up the catalog value, an explicit value always wins, and a raw path/spec (no entry) leaves the field unset for the engine to default. Threaded via `worker.Config`, so both local (`atlas up`) and fleet workers get it with no wire/gateway change.
2. **Per-model reasoning config** (`reasoning`) — **Resolved (M2 phase 4b):** the flag gates the `enable_thinking` chat-template kwarg (a non-reasoning model omits it). Threaded via `worker.Config` → `openaichat.Client`, applied uniformly across engines. See the reasoning-convention question above for the residual open part (richer reasoning _styles_).
3. **Tier metadata** (`tier: haiku|sonnet|opus`) — recorded but does not auto-generate the `claude-*` aliases; `atlas up --alias` is still explicit. Still open: leave to the operator via `--alias`, or auto-derive?

Also parked here: the larger gguf tiers (the research-doc 9B–35B models) are not in the shipped catalog because their exact quant digests are not yet pinned — add them as each is verified at build time (pinning is required: `internal/catalog` rejects a gguf entry without a 64-hex sha256). M2 phase 4c expands the catalog as digests are pinned.

## Readiness does not re-probe a live engine (phase 10)

`/readyz` returns 200 once a model is registered, and in single-node mode a model is registered only after its worker's `Start` confirms the engine reported healthy — so at startup "ready" genuinely means "a model can answer". It is **not** a live probe: if an engine subprocess dies _after_ registration, `/readyz` keeps returning 200 (the next request surfaces the failure as a 529). For M0's single-node hero path this is acceptable; a periodic worker health re-probe feeding readiness is an M1 concern, alongside the remote-worker hub. Stated as an assumption rather than silently shipped.

## Seeding a chosen API key for non-interactive deploys (phase 5)

[ADR-0008](decisions/0008-control-plane-persistence-and-api-keys.md) removed the shared `--api-key` secret: the control plane mints a default key on first run and prints it, and `atlas keys create` mints more. This is clean for interactive use, but non-interactive deploys (Docker, the SkyPilot recipe, k8s, anything driven from a secrets manager) can no longer **inject a known key** — they must scrape it from the process logs (`docker logs` / `sky logs`) after boot. The recipes in `examples/serve` and `docs/docker.md` do exactly that today.

**Open:** add a first-boot seed path so an operator can supply the exact secret out of band — e.g. an `ATLAS_API_KEY` env (or `--seed-key`) that, **only when the store is empty**, creates a key with that value instead of a random one. The tension is convenience vs. reintroducing a shared-secret-shaped env var that ADR-0008 deliberately retired; mitigations would be to seed only on an empty store and to document it as a bootstrap convenience, not an auth mechanism. Flagged during phase 5a; nothing depends on it (the log-scrape path works), so it is deferred rather than built.

## Resolved

Auth for the `/admin/*` control surface (phase 5) — settled in [ADR-0008](decisions/0008-control-plane-persistence-and-api-keys.md): gate `/admin/*` with an **admin-scoped API key** (option (a)), reusing the same key store and middleware as the client `/v1/*` surface rather than a separate token or loopback listener. The admin CLI clients send the key via flag or `ATLAS_API_KEY`. G12 grows an admin-auth case. Until phase 5 lands, the surface stays unauthenticated and M1's threat model assumes the server port is reachable only by trusted operators and workers.

How workers get engine runtimes — settled: M0 ships managed runtimes only (downloaded prebuilt llama.cpp binaries, a `uv`-managed vLLM venv), with the container path arriving at M1 behind the same provisioner interface. Recorded in [m0-build-plan.md](m0-build-plan.md#engine-runtime-provisioning) and implemented for llama.cpp in phase 2.
