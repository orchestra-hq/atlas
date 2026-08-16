# Atlas

**Point your agents at your own hardware.**

Atlas is an open source, self-hosted LLM inference platform. It runs open-weight models on hardware **you** control — a laptop, a single GPU box, or a fleet of GPU machines across clouds — and exposes the APIs that agents and apps already speak, so you can point existing tooling (the Claude Agent SDK, Claude Code, OpenAI-compatible clients) at your own infrastructure with a one-line config change.

> 📖 **Documentation:** <https://orchestra-hq.github.io/atlas> — install, quickstart, guides, deploy, and operate.

## Quickstart

```sh
# install (macOS / Linux)
brew install orchestra-hq/tap/atlas

# or the one-line installer (detects OS/arch, verifies checksum + cosign signature)
curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh

# serve a model — Atlas fetches it, provisions the engine, and exposes the API
atlas up --model qwen3-0.6b
```

On first start Atlas mints an API key and prints it. Now point Claude Code at your own machine:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=<the key Atlas printed>
claude
```

Prefer containers? `docker run ghcr.io/orchestra-hq/atlas:slim` (CPU) or `:cuda` (GPU). Full walkthrough: the [quickstart](https://orchestra-hq.github.io/atlas/get-started/quickstart/) on the docs site.

## Why

Teams building on LLMs increasingly want control over where the model runs and where their data goes. The pieces to do this exist — vLLM and SGLang for serving, Ollama for local DX, LiteLLM for API translation — but assembling them into a private, multi-machine, agent-ready inference platform is still a do-it-yourself project. Atlas packages the best ideas from those projects into one coherent product:

- **Agent-first API compatibility.** Native Anthropic Messages API (`/v1/messages`) plus OpenAI-compatible endpoints, so `ANTHROPIC_BASE_URL=https://your-atlas` just works.
- **Your hardware, anywhere.** A lightweight worker runs on each compute machine (your DC, your cloud, your customer's infra) and dials out to a central control plane. No inbound ports, no Kubernetes required.
- **Ollama-grade DX, fleet-grade scale.** One binary, `atlas up`, pull a model, serve it. The same binary scales out to a multi-node GPU cluster.
- **Best engine for the job.** Atlas orchestrates proven inference engines (vLLM, SGLang, llama.cpp, MLX) rather than reinventing them.

## Project status

Atlas is at **v0.1.0**. Drop-in compatibility is a release gate, not a claim: a conformance suite drives real Anthropic and OpenAI SDKs — plus a Claude Code smoke test — on every pull request, and the acceptance runs below are green on real hardware.

| Milestone                               | What it delivered                                                                 | Proof                                           |
| --------------------------------------- | --------------------------------------------------------------------------------- | ----------------------------------------------- |
| **M0 / M0.5** — Claude Code on your box | The Anthropic + OpenAI API surface, llama.cpp and vLLM engines, release machinery | [m0-acceptance](docs/internal/m0-acceptance.md) |
| **M1** — Fleet                          | Control plane + outbound-dialling workers, scheduling, API keys, TLS              | [m1-acceptance](docs/internal/m1-acceptance.md) |
| **M2** — Operate from the terminal      | Metrics, backpressure, `atlas status` / `top`; MLX + SGLang engines               | [m2-acceptance](docs/internal/m2-acceptance.md) |
| **M3** — Ecosystem deepeners            | Affinity routing, embeddings + rerank, audit log, cloud fallback                  | [m3-acceptance](docs/internal/m3-acceptance.md) |
| **M4** — Deliverability                 | `install.sh`, cosign signing, Homebrew tap                                        | [m4-build-plan](docs/internal/m4-build-plan.md) |
| **M5** — Documentation                  | The public [docs site](https://orchestra-hq.github.io/atlas)                      | [m5-build-plan](docs/internal/m5-build-plan.md) |
| **M8** — Bring any model                | `atlas up --model <hf-repo>` auto-configures from model metadata                  | [m8-acceptance](docs/internal/m8-acceptance.md) |

**Next:** M6 (web console). Full-HA control plane and a hosted offering are deliberately deferred — see the [roadmap](docs/roadmap.md).

## Design docs & internals

User-facing documentation (install, quickstart, guides, deploy, operate, reference) lives on the
**[docs site](https://orchestra-hq.github.io/atlas)**. The table below indexes the design truth under
[`docs/internal/`](docs/internal/) — ADRs, build plans, acceptance reports, research, and the detailed
source docs — for contributors and agents working in the repo.

| Doc                                                                                                                          | What it covers                                                    |
| ---------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| [docs/internal/vision.md](docs/internal/vision.md)                                                                           | What we're building, for whom, and how we differentiate           |
| [docs/internal/research/landscape.md](docs/internal/research/landscape.md)                                                   | Survey of existing projects and what we take from each            |
| [docs/internal/research/model-catalog-m0.md](docs/internal/research/model-catalog-m0.md)                                     | Starter catalog candidates: models, tiers, per-engine config      |
| [docs/internal/research/distribution-deployment-and-gpu-ci.md](docs/internal/research/distribution-deployment-and-gpu-ci.md) | How Atlas is packaged, deployed, and proven on GPUs (M0.5)        |
| [docs/internal/architecture.md](docs/internal/architecture.md)                                                               | How Atlas runs: control plane, workers, request flow, scheduling  |
| [docs/internal/api-surface.md](docs/internal/api-surface.md)                                                                 | The APIs Atlas exposes (Anthropic-compat, OpenAI-compat, admin)   |
| [docs/internal/usage-scenarios.md](docs/internal/usage-scenarios.md)                                                         | Which path fits you: laptop / single cloud GPU / fleet            |
| [docs/internal/docker.md](docs/internal/docker.md)                                                                           | Running Atlas from the published container images (slim + CUDA)   |
| [examples/serve/](examples/serve/README.md)                                                                                  | Serve on a cloud GPU: SkyPilot one-command + single-box+tunnel    |
| [docs/internal/deployment-aws.md](docs/internal/deployment-aws.md)                                                           | Reference topology for deploying in your own AWS account          |
| [docs/roadmap.md](docs/roadmap.md)                                                                                           | Phased milestones from single-node MVP to fleet                   |
| [docs/internal/m0-acceptance.md](docs/internal/m0-acceptance.md)                                                             | Definition of done for M0's API surface                           |
| [docs/internal/conformance-suite.md](docs/internal/conformance-suite.md)                                                     | Executable spec of the compat promise: harness, test groups       |
| [examples/acceptance/](examples/acceptance/README.md)                                                                        | GPU acceptance run that closed out M0 (M0.5) + AWS setup          |
| [docs/internal/m0-build-plan.md](docs/internal/m0-build-plan.md)                                                             | M0 phased build order, repo layout, and build-time decisions      |
| [docs/internal/m1-build-plan.md](docs/internal/m1-build-plan.md)                                                             | M1 phased build order (fleet: worker channel, scheduler, auth)    |
| [docs/internal/m1-acceptance.md](docs/internal/m1-acceptance.md)                                                             | Definition of done for M1: the multi-host fleet acceptance run    |
| [docs/internal/m2-build-plan.md](docs/internal/m2-build-plan.md)                                                             | M2 phased build order (operate: metrics, backpressure, engines)   |
| [docs/internal/m2-acceptance.md](docs/internal/m2-acceptance.md)                                                             | Definition of done for M2: the MLX + SGLang engine-breadth runs   |
| [docs/internal/m3-build-plan.md](docs/internal/m3-build-plan.md)                                                             | M3 phased build order (affinity, embeddings/rerank, audit, spill) |
| [docs/internal/m3-acceptance.md](docs/internal/m3-acceptance.md)                                                             | Definition of done for M3: the G19–G22 conformance tier           |
| [docs/internal/m4-build-plan.md](docs/internal/m4-build-plan.md)                                                             | M4 phased build order (deliverability: Homebrew tap + installer)  |
| [docs/internal/m5-build-plan.md](docs/internal/m5-build-plan.md)                                                             | M5 phased build order (documentation: Starlight docs site)        |
| [docs/internal/m8-build-plan.md](docs/internal/m8-build-plan.md)                                                             | M8 phased plan (bring-any-model auto-configuration)               |
| [docs/internal/m8-acceptance.md](docs/internal/m8-acceptance.md)                                                             | Definition of done for M8: the G23 auto-config conformance gate   |
| [docs/internal/contributing-model-families.md](docs/internal/contributing-model-families.md)                                 | How to add agent-config support for a new model family (one PR)   |
| [docs/internal/positioning.md](docs/internal/positioning.md)                                                                 | Marketing differentiators and the proof each one requires         |
| [docs/internal/decisions/](docs/internal/decisions/)                                                                         | Architecture decision records (ADRs)                              |
| [docs/internal/open-questions.md](docs/internal/open-questions.md)                                                           | Unresolved decisions that need an owner call                      |
| [docs/internal/follow-ups.md](docs/internal/follow-ups.md)                                                                   | Deferred, non-blocking refinements surfaced by code reviews       |

## Contributing (humans and agents)

Read [CLAUDE.md](CLAUDE.md) first — it explains the project conventions and where design truth lives. During the design phase, changes to `docs/` are the contribution surface; substantive direction changes need an ADR. To add agent-config support for a new model family, see [contributing-model-families.md](docs/internal/contributing-model-families.md).

Found a security issue? Please report it privately — see [SECURITY.md](SECURITY.md), not a public issue.

## License

[Apache 2.0](LICENSE).
