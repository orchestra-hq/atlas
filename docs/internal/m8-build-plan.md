# M8 build plan

> **🧭 Scheduled next — ratified, not yet started.** Ratified 2026-06-28: the owner accepted [ADR-0015](decisions/0015-bring-any-model-auto-configuration.md) and scheduled M8 **next, ahead of M6/M7** (the five open questions are resolved — see below). Nothing here is built yet; Phase 0 (ADR) is complete and implementation begins at Phase 1. M8 refines the M8 milestone in [roadmap.md](../roadmap.md).

The pitch in one line: **`atlas up --model <hugging-face-repo>` for _any_ model.** Atlas fetches the model's metadata (not its weights), decides whether it can serve the model well, and then either configures and serves it or tells the user exactly how to add support. This turns the curated catalog from a fence into a fast path, and — crucially — means **nobody has to maintain a catalog**: the "list of supported models" becomes a family→config map in Atlas's own source, extended by ordinary PRs gated on the conformance suite.

## Why this is its own milestone

Today's resolution ([internal/cli/resolve.go](../../internal/cli/resolve.go)) is two-way: a catalog name resolves to a curated [`catalog/starter.yaml`](../../catalog/starter.yaml) entry (engine, parser flags, reasoning, sampling, context); anything else falls through to a **bare** raw spec — served best-effort with none of that curated config, so tool-calling and reasoning often misbehave. M8 fills that gap: a raw Hugging Face spec gets **auto-configured** from its own metadata plus a family knowledge-map, so "bring your own" becomes "bring your own _and it works for agents_."

That changes how resolution works, adds a metadata-inspection layer and a fit/decision gate, and earns its own conformance gate — too big and too architecturally central to be a sub-task of another milestone. It also largely **absorbs** the near-term "expand the catalog" idea: if any model just works, you stop hand-curating a list (you keep only a few blessed, tested examples).

## What makes a model "just work" — the core insight

Serving a model needs a handful of settings. Most are derivable from the model's own metadata; one cluster is not, and that cluster is exactly what Atlas curates:

| Setting                         | Source                                                       | Auto-derivable? |
| ------------------------------- | ------------------------------------------------------------ | --------------- |
| Engine (llama.cpp / vLLM / MLX) | file format (safetensors vs GGUF) + host hardware            | ✅              |
| Context window                  | `config.json` → `max_position_embeddings` (+ rope scaling)   | ✅              |
| Chat template                   | `tokenizer_config.json` → `chat_template` (engines apply it) | ✅ mostly       |
| Sampling defaults               | `generation_config.json` / model card                        | 🟡 partly       |
| **Tool-call parser**            | model **family** (`hermes` / `qwen25` / `glm47` / …)         | ❌ curated      |
| **Reasoning parser**            | model **family** (`qwen3` / `glm45` / …)                     | ❌ curated      |

So "load any model and it _chats_" is nearly free; "load any model and it _works for agents_" hinges on the parser rows — the family knowledge Atlas already encodes in `starter.yaml`'s `engine_args` today. M8 lifts that knowledge out of per-model catalog rows into a per-**family** map, then applies it to any model of a known family.

## Build-time technical decisions (to ratify in the Phase-0 ADR)

These are the load-bearing choices; recorded here so the ADR can confirm or revise them, not re-discover them.

1. **The supported-models surface is code, not a catalog artifact.** Extending support is a normal PR that adds a family to the in-code map and a conformance case — reviewed and "earned by the suite, not vibes." There is **no community catalog** to host and maintain, and **no requirement for users to maintain their own** catalog. (A `--catalog file.yaml` override may still be exposed later — `catalog.LoadFile` already exists — but it is not required by this design.)
2. **Inspect before download.** Decisions are made from metadata fetched as individual small files (HF serves `config.json` etc. at `…/resolve/<rev>/<file>`), so the verdict is fast and wastes no bandwidth on a model Atlas can't serve.
3. **A three-way verdict, defaulting to honest behavior.** (a) Known family → configure + serve. (b) Engine can load the architecture but the family's agent-config is unknown → **serve as plain chat with a clear warning** (default), with a flag to refuse instead, plus a precise "open a PR here" pointer. (c) Architecture the pinned engine can't load, or model won't fit the hardware → **clean failure with the reason** (for an unloadable arch, that's an upstream-engine limitation, not an Atlas PR). This is the "honest scope" value turned into a contribution funnel.
4. **Two metadata paths.** Safetensors/HF repos read transformers-style `config.json`/`tokenizer_config.json`; GGUF repos read the **GGUF file header** (which carries architecture and often the template) — different code paths feeding the same resolver.
5. **Engine-arch support is keyed to the pinned engine version.** Whether vLLM/llama.cpp/MLX can load an architecture depends on the build, and Atlas already pins engine runtime versions ([internal/runtime](../../internal/runtime)). The supported-arch set is maintained/derived per pinned version (open question: static list vs runtime probe).
6. **Out of scope for v1:** models needing `trust_remote_code`/custom architectures (security + unsupported), multimodal beyond what the engines already do transparently, and auto-selecting among many quantization files in one GGUF repo (needs a user hint or a documented heuristic).

## Phases

| Phase | Deliverable                                            | Exit criterion                                                                                                                                                  |
| ----- | ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0     | ADR locking the design (decisions above)               | [ADR-0015](decisions/0015-bring-any-model-auto-configuration.md) flipped from `proposed` to `accepted`                                                          |
| 1     | `atlas inspect <model>` — read-only metadata → verdict | For fixture models across families/formats, prints the derived engine/context/template/sampling **and** the three-way verdict, downloading no weights           |
| 2     | Family→agent-config map + auto-derived serving         | A **known-family** HF model **not in the catalog** serves via `atlas up --model <repo>` and tool-calls correctly (parser + reasoning applied)                   |
| 3     | Fit gating + failure UX                                | An unsupported architecture and an oversized model each fail fast, **before** downloading weights, with an actionable message (upstream link / sizing)          |
| 4     | Middle-case behavior + PR funnel                       | An engine-loadable but unknown-family model serves chat with the warning + an exact "add `<family>` here" PR pointer; contributor docs explain the one-file add |
| 5     | Conformance gate + acceptance                          | A new gate auto-configures a known HF model (no catalog row) and passes the agent tool-use gates in CI; public docs + launch-post "bring your own" updated      |

Exit criteria are cumulative. **Phase 1 is independently shippable and useful on its own** ("will Atlas run this model?" as a one-shot check) with zero behavior change to `up` — a good first PR.

## Phase notes

**Phase 0 — ADR (done).** The decision record is written and **ratified** — [ADR-0015](decisions/0015-bring-any-model-auto-configuration.md) is `accepted` as of 2026-06-28 (resolution becomes metadata-driven; family map in code is the extension point; three-way verdict + default warn-and-serve policy; no community/required-user catalog), with all five open questions resolved (see below). The Phase-0 exit criterion is met; Phase 1 may begin.

**Phase 1 — inspect.** A metadata fetcher (HF `resolve` URLs for `config.json`, `tokenizer_config.json`, `generation_config.json`; GGUF header reader for gguf repos; token + revision + gated-repo handling) feeding a capability resolver that derives engine candidate(s), context window, template presence, and sampling. Surfaced as `atlas inspect <model>` printing the derived plan and the verdict. No serving, no weight download — cheap to build and test against recorded metadata fixtures.

**Phase 2 — family map + serve.** Build the family→agent-config map (`architecture`/`model_type` → tool-call parser, reasoning parser, template quirks, sampling fallback), **seeded by lifting the `engine_args` already in `starter.yaml`** — so the initial families (Qwen2.5, Qwen3, GLM, Gemma, Llama/Hermes, …) are a refactor of existing knowledge, not new research. Wire the resolver into `resolveModel`'s raw-spec branch so a known family produces a full `resolvedModel` instead of a bare passthrough.

**Phase 3 — fit gating.** Add the engine-arch support check (per pinned engine version) and a VRAM/RAM fit pre-check (reuse the scheduler's existing VRAM-fit estimate), producing the clean failure messages before any download.

**Phase 4 — middle case + funnel.** Implement the default "serve chat, warn, point at the PR" path and the opt-out flag to refuse unverified models. The message names the exact map file and entry shape; add contributor docs ("add a model family") that tie a new entry to a conformance case — see [contributing-model-families.md](contributing-model-families.md).

**Phase 5 — conformance + acceptance.** A new G-group: auto-configure a known HF model with **no catalog row** and run the agent (tool-use, streaming) gates against it, proving the auto-config path is agent-grade. Reconcile public docs ([guides/models](../../website/src/content/docs/guides/models.md)) and the launch post's "bring your own" paragraph, which can then promise auto-config rather than "best-effort."

## Resolved questions (settled at ratification, 2026-06-28)

All five are decided and bind the build (full text in [ADR-0015 § Resolved at ratification](decisions/0015-bring-any-model-auto-configuration.md)):

- **Engine-arch support discovery → static per-engine-version list + trust-and-catch backstop.** The static list drives the pre-download verdict; the actual engine load catches a stale list. (Phase 1/3)
- **GGUF multi-quant repos → default heuristic (prefer `Q4_K_M`, else nearest) + `--quant` override.** Keeps the one-command flow; the heuristic is documented. (Phase 1/2)
- **Middle-case default → warn-and-serve-chat, `--require-verified` opt-out.** Clearly labelled at startup; refusable. (Phase 4)
- **Gated/private HF repos → `HF_TOKEN`/`HUGGING_FACE_HUB_TOKEN` env token; actionable 401/403 message.** (Phase 1)
- **Metadata caching → state-dir cache keyed by `repo@revision`.** (Phase 1)

## Acceptance — what "M8 done" means

A newcomer runs `atlas up --model <a-popular-HF-repo-not-in-the-catalog>` and, for a known family, is serving an **agent-grade** endpoint (tool calls + reasoning correct) in one command — with no catalog entry written by anyone. An unsupported architecture or an oversized model fails fast with an actionable message instead of a confusing half-broken serve. The new conformance gate proves the auto-config path is agent-grade and runs per-PR. Engine-breadth coverage (auto-config across vLLM/SGLang/MLX, not just llama.cpp) follows the existing nightly tiering.

## Who does what

- **Owner (done):** ratified ADR-0015 and **scheduled M8 next, ahead of M6 (web console) / M7 (packaging)** (2026-06-28) — "any model just works" is central to the "minutes to first token" positioning. The five open questions are resolved.
- **Build:** Phase 0 is complete (ADR accepted). Next: phases 1–5 as separate green-only PRs per the repo workflow, starting with Phase 1 (`atlas inspect`, independently shippable).
