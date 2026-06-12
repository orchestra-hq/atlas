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

## Engine runtime provisioning (proposal — closes the open question)

Proposed resolution of the [open question](open-questions.md): **M0 ships managed runtimes only (option c); containers (option b) arrive at M1** behind the same `RuntimeProvisioner` interface.

- **llama.cpp:** worker downloads a pinned prebuilt `llama-server` for its platform (CUDA/Metal/CPU) into the state dir. No host dependencies.
- **vLLM:** worker bootstraps `uv` and creates a pinned venv in the state dir. Heavier, but no Docker requirement for first touch.

Rationale: M0's hero path is `atlas up` on a dev box or single GPU machine — requiring Docker + NVIDIA container toolkit there hurts minutes-to-first-token, while M1's cloud fleets (where containers shine) is exactly when the container path lands. One honesty note for [positioning](positioning.md) angle #5: "no Python on the host" must mean _the user never installs or sees Python_ — the worker-managed vLLM venv does put Python in Atlas's state dir. The install-DX claim survives; the literal wording should be tightened when marketing copy is written.

**This is the last open design question — sign-off on this section closes it.**

## Phases

Each phase's exit criterion is conformance groups passing (cumulatively) on the engines available at that point.

| Phase | Deliverable                                                                                                            | Exit criterion                            |
| ----- | ---------------------------------------------------------------------------------------------------------------------- | ----------------------------------------- |
| 0     | Scaffold: Go module, CI (lint, test, cross-platform build, release dry-run), repo hooks                                | CI green on empty skeleton                |
| 1     | Conformance harness before the product: suite skeleton + stub gateway, matrix.json output                              | Harness runs, reports structured failures |
| 2     | Walking skeleton: `atlas up` (in-process worker), llama.cpp runtime download, non-streaming `/v1/messages` (text only) | G1 (non-streaming)                        |
| 3     | Anthropic SSE streaming                                                                                                | G2                                        |
| 4     | Tool-loop translation (`tools`, `tool_choice`, `input_json_delta`, parallel calls)                                     | G3                                        |
| 5     | Thinking-block mapping (ADR-0005)                                                                                      | G4                                        |
| 6     | `/v1/models` + aliases, `count_tokens`, error envelopes, gateway context-window assertion                              | G5, G6, G7                                |
| 7     | OpenAI surface (`/v1/chat/completions`)                                                                                | G8                                        |
| 8     | vLLM adapter + uv runtime provisioning                                                                                 | All groups on both engines                |
| 9     | Model store (CAS) + `atlas pull` + starter catalog wiring                                                              | Catalog models boot from cold             |
| 10    | CLI polish (`pull`, `run`, `ps`), `/healthz`, `/readyz`, structured logs with token counts                             | G10                                       |
| 11    | Agent SDK end-to-end + Claude Code smoke; full acceptance run on (a) llama.cpp and (b) vLLM                            | G9 — **M0 done**                          |

Phases 2–7 run llama.cpp only (cheapest loop, runs in CPU CI); vLLM lands once the surface is conformant, mostly exercising translation config rather than new logic.

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
