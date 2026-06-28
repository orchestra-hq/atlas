# ADR-0015: Bring any model — metadata-driven auto-configuration

## Status

proposed

This is the Phase-0 design record for **M8** ([m8-build-plan.md](../m8-build-plan.md), [roadmap.md](../roadmap.md)). It is `proposed`: it captures the intended direction so implementation doesn't re-litigate it, and flips to `accepted` when the owner greenlights the build (and may be revised by that review). Nothing here is built.

## Context

Atlas resolves a `--model` value two ways today ([internal/cli/resolve.go](../../internal/cli/resolve.go)):

- a **catalog name** → a curated [`catalog/starter.yaml`](../../catalog/starter.yaml) entry carrying engine, tool-call/reasoning parser flags (`engine_args`), reasoning capability, sampling defaults, and context window; or
- **anything else** → a **bare** raw spec (a Hugging Face repo id or local path) passed straight to the engine with **none** of that configuration.

The bare path is the problem. A model served raw gets no tool-call parser, no reasoning gating, no template/sampling care — so for agent workloads (the whole point of Atlas) tool calls and reasoning frequently misbehave. "Bring your own model" today means "bring your own and hope." The curated catalog works, but it is a hand-maintained list: every model anyone wants must be added by editing the embedded YAML and cutting a release.

The decisive observation: of everything needed to serve a model for agents, **almost all of it is derivable from the model's own metadata** — file format → engine, `config.json` → context window, `tokenizer_config.json` → chat template, `generation_config.json` → sampling. The one cluster that is **not** auto-derivable is the **tool-call parser and reasoning parser**, which depend on the model **family** (how it emits tool calls / reasoning), not the individual model. And that family knowledge already exists in the repo — it is exactly what `starter.yaml`'s `engine_args` encode per entry.

So the catalog's per-model curation can be re-expressed as per-**family** knowledge and applied to _any_ model of a known family — turning "bring your own" into "bring your own _and it works for agents_," and removing the need to hand-maintain a model list. This also avoids the two maintenance traps we explicitly do **not** want: users maintaining their own catalog, and us maintaining a separately-hosted community catalog (which would carry an unbounded curation and hosting tail). The "list of supported things" instead lives in Atlas's own source and grows by ordinary, conformance-gated PRs.

This change touches resolution, adds a metadata-inspection layer and a fit/decision gate, and earns its own conformance gate — hence a milestone (M8) and this ADR, rather than a quiet patch. It builds on [ADR-0001](0001-orchestrate-engines-not-build-one.md) (wrap engines, don't reimplement), [ADR-0002](0002-anthropic-api-first.md) (drop-in surface is sacrosanct), and [ADR-0005](0005-thinking-blocks-supported.md) (reasoning mapping), and narrows the "raw spec is best-effort" gap noted in [follow-ups.md](../follow-ups.md).

## Decision

1. **Resolution becomes metadata-driven.** A raw Hugging Face spec is no longer passed through bare: Atlas fetches the model's metadata, derives a full serving plan (engine, context window, chat template, sampling), and applies family-specific agent config — producing the same shape of `resolvedModel` a catalog entry produces today. The catalog path and the local-path path are unchanged.

2. **The supported-models surface is code, not a catalog artifact.** The curated knowledge becomes a **family → agent-config map** (architecture/`model_type` → tool-call parser, reasoning parser, template quirks, sampling fallback) compiled into Atlas, **seeded by lifting the `engine_args` already in `starter.yaml`**. Extending support is a normal PR adding a family entry **plus a conformance case** — "earned by the suite, not vibes." There is no community catalog to host/maintain and no requirement for users to maintain their own. (A `--catalog file.yaml` override may be exposed later — `catalog.LoadFile` already exists — but the design does not depend on it.)

3. **Inspect before download.** The verdict is computed from metadata fetched as individual small files (Hugging Face serves `config.json` etc. at `…/resolve/<rev>/<file>`; GGUF carries its metadata in the file header), so deciding whether Atlas can serve a model wastes no bandwidth on weights it would reject. A read-only `atlas inspect <model>` surfaces the derived plan and verdict without serving.

4. **A three-way verdict, defaulting to honest behavior.**
   - **Known family** → configure and serve (agent-grade).
   - **Engine can load the architecture, but the family's agent-config is unknown** → **serve as plain chat with a clear warning** (default), with an opt-out flag to refuse unverified models instead, and a precise "tool-calling for `<family>` isn't supported yet — add it here" pointer at the family-map file.
   - **Architecture the pinned engine cannot load, or model won't fit the hardware** → **clean failure with the reason.** For an unloadable architecture that is an _upstream-engine_ limitation (point at the engine's supported-arch list), not an Atlas PR; for a fit failure, surface the sizing.

   This makes "honest about scope" ([positioning.md](../positioning.md)) a literal feature: the boundary of what works is stated, and the unknown-family case is turned into a contribution funnel rather than a silent half-broken serve.

5. **Two metadata paths feed one resolver.** Safetensors/Hugging Face repos read transformers-style `config.json`/`tokenizer_config.json`/`generation_config.json`; GGUF repos read the **GGUF file header** (architecture, and often the embedded template). Both normalize into the same internal capability record the family map and resolver consume.

6. **Engine-architecture support is keyed to the pinned engine version.** Whether vLLM/SGLang/llama.cpp/MLX can load an architecture is a property of the build, and Atlas already pins engine runtime versions ([internal/runtime](../../internal/runtime)). The supported-arch set is therefore maintained/derived per pinned version, so the pre-download verdict stays truthful as engines are bumped.

7. **The curated catalog's role shrinks but does not vanish.** It keeps a few **blessed, tested** example models (a friendly default and the names used in docs/quickstarts) and remains the place to **override/pin** specifics for a model when the derived defaults are wrong. It is no longer the gate on what Atlas can serve.

8. **Scope boundaries (v1).** Out of scope: models requiring `trust_remote_code`/custom architectures (security and unsupported), multimodal beyond what the engines already handle transparently, and auto-selecting among many quantization files in a single GGUF repo (requires an explicit hint or a documented heuristic). Auto-config targets **chat/agent** models first; the embedding/reranker **classes** ([ADR-0012](0012-embeddings-and-reranker-model-classes.md)) stay explicitly declared, since class is not reliably inferable from metadata alone.

## Consequences

- **The headline differentiator.** `atlas up --model <any-hf-repo>` that is _agent-grade for known families_ extends "minutes to first token" to "any model," and is a clean story against Ollama-style "it runs, tool-calling is your problem."
- **No maintenance trap.** Support grows through the normal PR + conformance flow into the codebase; there is nothing extra to host, and no user-maintained or community-maintained catalog. The conformance gate keeps quality from eroding as families are added.
- **Honest failure is a funnel.** The unknown-family path produces a contributor lead (a one-file PR) instead of a confused user; the unloadable/oversized path fails fast with an actionable reason before any download.
- **`atlas inspect` is independently valuable.** Phase 1 ships a useful "will Atlas run this, and how?" check with zero behavior change to `up` — low-risk first increment.
- **New ongoing costs, accepted:** the per-engine-version supported-arch mapping must be refreshed when runtimes are bumped; GGUF header parsing is a new code path; metadata fetch adds failure modes (network, gated/private repos, missing files) that need graceful handling. These are bounded and local.
- **A surprise risk, mitigated:** default warn-and-serve for an unknown family means a model may serve as chat-only without tool-calling — acceptable only because it is clearly labelled at startup and refusable via flag; silent degradation is not acceptable.
- **Drop-in surface unchanged.** This is purely about how a model is _resolved and configured_; the Anthropic/OpenAI API surfaces, aliases, and the catalog/local-path paths are untouched ([ADR-0002](0002-anthropic-api-first.md) holds).
- **Open questions to settle during the build** (also in the build plan): static per-engine supported-arch list vs runtime probe vs trust-and-catch; GGUF multi-quant selection; the precise warn-vs-refuse default and flag name; gated/private-repo token UX; metadata caching in the state dir.
