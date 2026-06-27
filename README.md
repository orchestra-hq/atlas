# Atlas

Atlas is an open source, self-hosted LLM inference platform. It lets you run open-weight models on hardware **you** control — a laptop, a single GPU box, or a fleet of GPU machines across clouds — and exposes the APIs that agents and apps already speak, so you can point existing tooling (the Claude Agent SDK, Claude Code, OpenAI-compatible clients) at your own infrastructure with a one-line config change.

**Status: M0–M4 done (M4 declared 2026-06-27); M5 (docs site) in progress.** M0 ("Claude Code on your own box"), M0.5 ("Release & prove"), M1 ("Fleet"), M2 ("Operate from the terminal"), M3 ("Ecosystem & differentiation deepeners"), and M4 ("Deliverability") are complete — M0/M0.5 acceptance is green on both engines with the real Claude Code drop-in proven on a GPU ([docs/m0-acceptance.md](docs/m0-acceptance.md)), M1's multi-host fleet acceptance (G1–G14 across two machines, two engines) is green ([docs/m1-acceptance.md](docs/m1-acceptance.md)), M2's engine-breadth acceptance (MLX on Apple Silicon + SGLang on GPU) is green alongside the per-PR observability/backpressure gates ([docs/m2-acceptance.md](docs/m2-acceptance.md)), M3's affinity routing, embeddings/reranker classes, audit log, and cloud-fallback are proven by the per-PR G19–G22 conformance ([docs/m3-acceptance.md](docs/m3-acceptance.md)), and M4's install machinery (`install.sh`, cosign signing, Homebrew tap) is proven by the `v0.1.0` release run ([docs/m4-build-plan.md](docs/m4-build-plan.md)). Design truth in `docs/` is the source of truth.

## Install

Once a release is published, install the `atlas` binary via Homebrew or the one-line installer:

```sh
# Homebrew (macOS / Linuxbrew)
brew install orchestra-hq/tap/atlas

# or the one-line installer (detects OS/arch, verifies checksum + cosign signature)
curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh
```

Or run the container image: `docker run ghcr.io/orchestra-hq/atlas:slim` (CPU) / `:cuda` (GPU) — see [docs/docker.md](docs/docker.md). Then `atlas up` to serve a model.

## Why

Teams building on LLMs increasingly want control over where the model runs and where their data goes. The pieces to do this exist — vLLM and SGLang for serving, Ollama for local DX, LiteLLM for API translation — but assembling them into a private, multi-machine, agent-ready inference platform is still a do-it-yourself project. Atlas packages the best ideas from those projects into one coherent, well-marketed product:

- **Agent-first API compatibility.** Native Anthropic Messages API (`/v1/messages`) plus OpenAI-compatible endpoints, so `ANTHROPIC_BASE_URL=https://your-atlas` just works.
- **Your hardware, anywhere.** A lightweight worker runs on each compute machine (your DC, your cloud, your customer's infra) and dials out to a central control plane. No inbound ports, no Kubernetes required.
- **Ollama-grade DX, fleet-grade scale.** One binary, `atlas up`, pull a model, serve it. The same binary scales out to a multi-node GPU cluster.
- **Best engine for the job.** Atlas orchestrates proven inference engines (vLLM, SGLang, llama.cpp, MLX) rather than reinventing them.

## Documentation map

| Doc                                                                                                        | What it covers                                                    |
| ---------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| [docs/vision.md](docs/vision.md)                                                                           | What we're building, for whom, and how we differentiate           |
| [docs/research/landscape.md](docs/research/landscape.md)                                                   | Survey of existing projects and what we take from each            |
| [docs/research/model-catalog-m0.md](docs/research/model-catalog-m0.md)                                     | Starter catalog candidates: models, tiers, per-engine config      |
| [docs/research/distribution-deployment-and-gpu-ci.md](docs/research/distribution-deployment-and-gpu-ci.md) | How Atlas is packaged, deployed, and proven on GPUs (M0.5)        |
| [docs/architecture.md](docs/architecture.md)                                                               | How Atlas runs: control plane, workers, request flow, scheduling  |
| [docs/api-surface.md](docs/api-surface.md)                                                                 | The APIs Atlas exposes (Anthropic-compat, OpenAI-compat, admin)   |
| [docs/usage-scenarios.md](docs/usage-scenarios.md)                                                         | Which path fits you: laptop / single cloud GPU / fleet            |
| [docs/docker.md](docs/docker.md)                                                                           | Running Atlas from the published container images (slim + CUDA)   |
| [examples/serve/](examples/serve/README.md)                                                                | Serve on a cloud GPU: SkyPilot one-command + single-box+tunnel    |
| [docs/deployment-aws.md](docs/deployment-aws.md)                                                           | Reference topology for deploying in your own AWS account          |
| [docs/roadmap.md](docs/roadmap.md)                                                                         | Phased milestones from single-node MVP to fleet                   |
| [docs/m0-acceptance.md](docs/m0-acceptance.md)                                                             | Definition of done for M0's API surface                           |
| [docs/conformance-suite.md](docs/conformance-suite.md)                                                     | Executable spec of the compat promise: harness, test groups       |
| [examples/acceptance/](examples/acceptance/README.md)                                                      | GPU acceptance run that closed out M0 (M0.5) + AWS setup          |
| [docs/m0-build-plan.md](docs/m0-build-plan.md)                                                             | M0 phased build order, repo layout, and build-time decisions      |
| [docs/m1-build-plan.md](docs/m1-build-plan.md)                                                             | M1 phased build order (fleet: worker channel, scheduler, auth)    |
| [docs/m1-acceptance.md](docs/m1-acceptance.md)                                                             | Definition of done for M1: the multi-host fleet acceptance run    |
| [docs/m2-build-plan.md](docs/m2-build-plan.md)                                                             | M2 phased build order (operate: metrics, backpressure, engines)   |
| [docs/m2-acceptance.md](docs/m2-acceptance.md)                                                             | Definition of done for M2: the MLX + SGLang engine-breadth runs   |
| [docs/m3-build-plan.md](docs/m3-build-plan.md)                                                             | M3 phased build order (affinity, embeddings/rerank, audit, spill) |
| [docs/m3-acceptance.md](docs/m3-acceptance.md)                                                             | Definition of done for M3: the G19–G22 conformance tier           |
| [docs/m4-build-plan.md](docs/m4-build-plan.md)                                                             | M4 phased build order (deliverability: Homebrew tap + installer)  |
| [docs/m5-build-plan.md](docs/m5-build-plan.md)                                                             | M5 phased build order (documentation: Starlight docs site)        |
| [docs/positioning.md](docs/positioning.md)                                                                 | Marketing differentiators and the proof each one requires         |
| [docs/decisions/](docs/decisions/)                                                                         | Architecture decision records (ADRs)                              |
| [docs/open-questions.md](docs/open-questions.md)                                                           | Unresolved decisions that need an owner call                      |
| [docs/follow-ups.md](docs/follow-ups.md)                                                                   | Deferred, non-blocking refinements surfaced by code reviews       |

## Contributing (humans and agents)

Read [CLAUDE.md](CLAUDE.md) first — it explains the project conventions and where design truth lives. During the design phase, changes to `docs/` are the contribution surface; substantive direction changes need an ADR.

## License

[Apache 2.0](LICENSE).
