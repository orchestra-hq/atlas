# M0 build plan

The ordered path from empty repo to passing the [M0 acceptance criteria](m0-acceptance.md). Each phase ends with named [conformance groups](conformance-suite.md) going green — the suite is written before the code it tests. This refines the repository sketch in [architecture.md](architecture.md); it does not change direction anywhere (no new ADRs needed, with one proposal flagged below).

## Repository layout

```text
/cmd/atlas              # single CLI entrypoint: up | server | worker | pull | run | ps ...
/internal/core          # internal representation: messages, content blocks, tools, thinking, stop reasons
/internal/api           # wire types + translation to/from core: anthropic/, openai/, admin/
/internal/server        # gateway (auth, routing, SSE), registry, scheduler (trivial in M0), hub
/internal/worker        # hardware detection, engine supervision, request execution
/internal/engines       # adapters: llamacpp/, vllm/ (sglang/, mlx/ later)
/internal/runtime       # engine runtime provisioning (see decision below)
/internal/store         # content-addressable model cache: manifests + blobs
/catalog                # curated model definitions (yaml) — seeded from research/model-catalog-m0.md
/conformance            # the suite: pytest + vitest + scripted agent runs (not Go)
/docs                   # this design documentation
```

## Build-time technical decisions

Implementation choices consistent with existing ADRs — recorded here so they don't get re-litigated mid-build:

1. **Atlas owns the Anthropic surface for every engine.** vLLM ships its own Anthropic-compat endpoint, but using it for one engine and translating for another would give divergent behavior across the fleet. Adapters always consume the engine's OpenAI-compat/native endpoint; the gateway produces all Anthropic semantics itself. One conformance result, engine-independent.
2. **`count_tokens` proxies the engine's tokenize endpoint** (llama.cpp `/tokenize`, vLLM `/tokenize`). Real tokenizer counts per ADR's "don't reinvent inference" rule — no tokenizer reimplementation in Go.
3. **State is SQLite via a pure-Go driver** (`modernc.org/sqlite`): keeps the cgo-free static binary, satisfies "single-directory state".
4. **Worker channel is an interface with only an in-process implementation in M0.** `atlas up` registers the worker over a Go channel (architecture: single-node mode may not fork the architecture). The wire protocol (gRPC vs WebSocket) is an M1 decision; nothing in M0 depends on it.
5. **Engine version pinning from day one.** Each catalog/engine release pins exact engine versions; upgrades are explicit.

## Phase 0 scaffolding choices

Tooling picked when the scaffold was built (2026-06-12). Not ADR material — swap any of these if they stop pulling their weight, but change the CI pin and local docs together:

| Concern       | Choice                                                                              | Notes                                                                                                                                          |
| ------------- | ----------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Module        | `github.com/orchestra-hq/atlas`, Go 1.26                                            | Toolchain version pinned in `go.mod`; CI reads it from there                                                                                   |
| CLI framework | `spf13/cobra`                                                                       | The standard for subcommand trees (Ollama, kubectl); only transitive dep is pflag                                                              |
| Lint/format   | `golangci-lint` v2 — standard linters + gocritic/revive/misspell; gofumpt+goimports | One tool for both (`make lint`, `make fmt`); config in `.golangci.yml`                                                                         |
| Release       | GoReleaser v2, `CGO_ENABLED=0`, linux/darwin × amd64/arm64                          | Version stamped into `internal/version` via ldflags; every PR runs `release --snapshot` as a dry-run                                           |
| Task runner   | Makefile                                                                            | Five targets, no task-runner dependency                                                                                                        |
| CI            | GitHub Actions on Blacksmith runners (`blacksmith-{2,4}vcpu-ubuntu-2404`)           | Standard `actions/*` work as-is — Blacksmith intercepts the cache API transparently; macOS/Windows builds are cross-compiled, no extra runners |
| Repo hooks    | `.githooks/pre-commit` formats staged Go (`golangci-lint fmt`) and markdown         | Full lint stays in CI to keep commits fast                                                                                                     |

## Engine runtime provisioning

**M0 ships managed runtimes only (option c); containers (option b) arrive at M1** behind the same `RuntimeProvisioner` interface. This was the last open design question; phase 2 implements the llama.cpp half (`internal/runtime`), so it is now settled rather than proposed.

- **llama.cpp:** worker downloads a pinned prebuilt `llama-server` for its platform (CUDA/Metal/CPU) into the state dir, sha256-verified against the pinned release. No host dependencies. _(Done — phase 2; pinned tag in `internal/runtime.LlamaCppTag`.)_
- **vLLM:** worker bootstraps a pinned `uv` (downloaded + sha256-verified, like the llama.cpp binary) and creates a pinned venv (`vllm==<ver>`) in the state dir. Heavier, but no Docker requirement for first touch. _(Done — phase 8; pinned versions in `internal/runtime.UvVersion` / `VLLMVersion`, uv digests in `uvAssets`.)_

Rationale: M0's hero path is `atlas up` on a dev box or single GPU machine — requiring Docker + NVIDIA container toolkit there hurts minutes-to-first-token, while M1's cloud fleets (where containers shine) is exactly when the container path lands. One honesty note for [positioning](positioning.md) angle #5: "no Python on the host" must mean _the user never installs or sees Python_ — the worker-managed vLLM venv does put Python in Atlas's state dir. The install-DX claim survives; the literal wording should be tightened when marketing copy is written.

## Phases

Each phase's exit criterion is conformance groups passing (cumulatively) on the engines available at that point.

| Phase | Deliverable                                                                                                            | Exit criterion                                                                                                       |
| ----- | ---------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| 0     | Scaffold: Go module, CI (lint, test, cross-platform build, release dry-run), repo hooks                                | CI green on empty skeleton                                                                                           |
| 1     | Conformance harness before the product: suite skeleton + stub gateway, matrix.json output                              | Harness runs, reports structured failures                                                                            |
| 2     | Walking skeleton: `atlas up` (in-process worker), llama.cpp runtime download, non-streaming `/v1/messages` (text only) | G1 (non-streaming)                                                                                                   |
| 3     | Anthropic SSE streaming                                                                                                | G2                                                                                                                   |
| 4     | Tool-loop translation (`tools`, `tool_choice`, `input_json_delta`, parallel calls)                                     | G3                                                                                                                   |
| 5     | Thinking-block mapping (ADR-0005)                                                                                      | G4                                                                                                                   |
| 6     | `/v1/models` + aliases, `count_tokens`, error envelopes, gateway context-window assertion                              | G5, G6, G7                                                                                                           |
| 7     | OpenAI surface (`/v1/chat/completions`)                                                                                | G8                                                                                                                   |
| 8     | vLLM adapter + uv runtime provisioning                                                                                 | G1–G8 stay green on llama.cpp; vLLM code unit-tested (both-engines conformance = GPU tier, **deferred** — see below) |
| 9     | Model store (CAS) + `atlas pull` + starter catalog wiring                                                              | Catalog models boot from cold (CPU/llama.cpp tier in CI; vLLM tiers are the GPU tier, deferred — see below)          |
| 10    | CLI polish (`pull`, `run`, `ps`), `/healthz`, `/readyz`, structured logs with token counts                             | G10                                                                                                                  |
| 11    | Agent SDK end-to-end + Claude Code smoke; full acceptance run on (a) llama.cpp and (b) vLLM                            | G9 — **M0 done**                                                                                                     |

Phases 2–7 run llama.cpp only (cheapest loop, runs in CPU CI); vLLM lands once the surface is conformant, mostly exercising translation config rather than new logic.

Phase 8 was split for hardware reasons, by the project owner's call: the **code** (vLLM adapter, uv runtime provisioning, `atlas up --engine`) landed sharing the core⇄OpenAI translation with llama.cpp via `internal/engines/openaichat` so both engines produce one set of semantics, with unit tests for the vLLM-only endpoints (token count, context window). What was **deliberately not done this phase** is the nightly/GPU conformance run that the original exit criterion (_all groups green on both engines_) implies: that is the full-matrix tier (see [Testing tiers](#testing-tiers)), which needs a CUDA runner — vLLM does not run on the CPU PR runner, and no GPU runner is registered yet. So the per-PR gate stays llama.cpp `G1–G8`, and "both engines green" is asserted by construction (shared translation + unit tests) rather than observed end-to-end on vLLM. Standing up that GPU run — register a CUDA runner, then add the nightly workflow — is the tracked follow-up in [open-questions.md](open-questions.md); the full acceptance run on vLLM ultimately lands in phase 11.

Phase 9 added the content-addressable model store (`internal/store`: weight blobs keyed by sha256 + a manifest per name, all under the state dir), the embedded starter catalog (`/catalog` is now a Go package — curated entries seeded from [research/model-catalog-m0.md](research/model-catalog-m0.md), each gguf entry pinned to an exact sha256), and `atlas pull` (warm the store ahead of time; `atlas up` also pulls a cold catalog model on demand). `atlas up --model <catalog-name>` resolves through catalog → store → boot, alongside the existing raw-path and HF-spec forms. The cold-boot exit criterion is proven in CI on the **small llama.cpp tier** (CI now `atlas pull`s the two tiny Qwen catalog models and boots from the store, with `G1–G8` still green). The larger **vLLM tiers carry only HF repo refs** — vLLM resolves multi-file repos from its own cache at boot, so they do not pass through the blob store, and their cold-boot conformance is the same GPU tier deferred above (it lands in the phase-11 acceptance pass). Two smaller follow-ups are parked in [open-questions.md](open-questions.md): per-model sampling defaults are recorded in the catalog but not yet applied to requests, and tier metadata does not yet auto-generate `claude-*` aliases.

Phase 11 built the agent harness (`G9`) and is where the M0 acceptance run is assembled. G9 has two real-client cells (see [conformance-suite.md](conformance-suite.md#g9--agent-harness-end-to-end-criterion-1)): an `agent-sdk` cell — a streamed agent loop that drives ≥3 client-side tool calls (`request → tool_use → tool_result → repeat`) through Atlas, with the tool forced each turn so the loop is deterministic on the tiny model — which **runs and gates per-PR** on llama.cpp; and a `claude-code` cell — the real `claude` binary editing+verifying a file via `ANTHROPIC_BASE_URL` — which is opt-in (`CONF_CLAUDE_CODE_SMOKE`). The per-PR CPU gate is now the full `G1–G10`. Building this empirically caught a genuine drop-in bug: Claude Code sends `thinking.type: "adaptive"` by default, which Atlas rejected with a 400 — directly the failure ADR-0005 exists to prevent; the gateway now accepts `adaptive` as thinking-allowed ([api-surface.md](api-surface.md)). **M0 is not yet declared done** (project owner's call): the small CPU-tier model drives real Claude Code only intermittently and vLLM needs a GPU, so the full acceptance run — vLLM all-groups **and** the real Claude Code smoke (plus the dedicated Claude Agent SDK package) on a capable model — is the remaining gate, blocked on standing up a CUDA runner. It is tracked as the capable/GPU acceptance tier in [open-questions.md](open-questions.md); the harness and CI gate are otherwise complete.

Phase 10 closed out the operational floor (`G10`). The gateway gained `/readyz` (200 only once a model is servable — in single-node mode a model is registered only after its worker reports healthy, so "has a model" _is_ "servable"; CI now gates startup on `/readyz` rather than `/healthz`) and a request-logging middleware that emits one structured `slog` line per API request carrying the resolved model and its input/output token counts — the per-request cost signal the criterion requires. Two operator commands landed beside `pull`: `atlas run <model> [prompt]` (Ollama-style one-shot — provision, resolve+boot, one `Execute`, print the reply, shut down; prompt from args or stdin, setup chatter on stderr so the answer is pipeable) and `atlas ps` (probe a running instance's `/healthz` + `/readyz` and list its served models, context windows, and aliases via `/v1/models`). The G10 conformance group is fleshed out (wire-level `/healthz`, `/readyz`, and a token-counts-in-logs check that reads the target's log via `ATLAS_LOG_FILE`, skipping when unset) and added to the per-PR gate, so the llama.cpp tier now gates `G1–G8 + G10`. Only `G9` (Agent SDK + Claude Code smoke) remains for phase 11.

## Testing tiers

| Tier        | What                                                                          | When              |
| ----------- | ----------------------------------------------------------------------------- | ----------------- |
| Unit        | Translation golden files (wire ⇄ core), SSE encoder, alias resolution         | Every PR          |
| Integration | Gateway against a scripted fake engine (OpenAI-compat stub)                   | Every PR          |
| Conformance | Real SDKs + real llama.cpp with a tiny catalog model (CPU, e.g. Qwen3.5-0.8B) | Every PR          |
| Full matrix | Both engines, reasoning + non-reasoning models, GPU                           | Nightly + release |

The full matrix needs a CUDA runner — a self-hosted GitHub runner on a single GPU box is the cheap path and doubles as dogfooding.

## Risks

- **GPU CI availability** — mitigated by the tiny-model CPU tier catching most regressions per PR.
- **Chat-template/parser drift across engine releases** — mitigated by version pinning + the conformance matrix as the upgrade gate.
- **Malformed tool args from free-text extraction** (see [catalog research](research/model-catalog-m0.md), finding 2) — gateway stance decided during phase 4, informed by real failure rates in the suite.
- **Claude Code attribution-header cache busting** (finding 1) — handle in the phase 11 smoke test; recipe documentation at minimum.
