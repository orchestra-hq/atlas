# Atlas

Atlas is an open source, self-hosted LLM inference platform. It lets you run open-weight models on hardware **you** control — a laptop, a single GPU box, or a fleet of GPU machines across clouds — and exposes the APIs that agents and apps already speak, so you can point existing tooling (the Claude Agent SDK, Claude Code, OpenAI-compatible clients) at your own infrastructure with a one-line config change.

**Status: building M0.** The design in `docs/` is the source of truth; code is landing in the phase order of [docs/m0-build-plan.md](docs/m0-build-plan.md).

## Why

Teams building on LLMs increasingly want control over where the model runs and where their data goes. The pieces to do this exist — vLLM and SGLang for serving, Ollama for local DX, LiteLLM for API translation — but assembling them into a private, multi-machine, agent-ready inference platform is still a do-it-yourself project. Atlas packages the best ideas from those projects into one coherent, well-marketed product:

- **Agent-first API compatibility.** Native Anthropic Messages API (`/v1/messages`) plus OpenAI-compatible endpoints, so `ANTHROPIC_BASE_URL=https://your-atlas` just works.
- **Your hardware, anywhere.** A lightweight worker runs on each compute machine (your DC, your cloud, your customer's infra) and dials out to a central control plane. No inbound ports, no Kubernetes required.
- **Ollama-grade DX, fleet-grade scale.** One binary, `atlas up`, pull a model, serve it. The same binary scales out to a multi-node GPU cluster.
- **Best engine for the job.** Atlas orchestrates proven inference engines (vLLM, SGLang, llama.cpp, MLX) rather than reinventing them.

## Documentation map

| Doc                                                                                                        | What it covers                                                   |
| ---------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| [docs/vision.md](docs/vision.md)                                                                           | What we're building, for whom, and how we differentiate          |
| [docs/research/landscape.md](docs/research/landscape.md)                                                   | Survey of existing projects and what we take from each           |
| [docs/research/model-catalog-m0.md](docs/research/model-catalog-m0.md)                                     | Starter catalog candidates: models, tiers, per-engine config     |
| [docs/research/distribution-deployment-and-gpu-ci.md](docs/research/distribution-deployment-and-gpu-ci.md) | How Atlas is packaged, deployed, and proven on GPUs (M0.5)       |
| [docs/architecture.md](docs/architecture.md)                                                               | How Atlas runs: control plane, workers, request flow, scheduling |
| [docs/api-surface.md](docs/api-surface.md)                                                                 | The APIs Atlas exposes (Anthropic-compat, OpenAI-compat, admin)  |
| [docs/docker.md](docs/docker.md)                                                                           | Running Atlas from the published container images (slim + CUDA)  |
| [docs/deployment-aws.md](docs/deployment-aws.md)                                                           | Reference topology for deploying in your own AWS account         |
| [docs/roadmap.md](docs/roadmap.md)                                                                         | Phased milestones from single-node MVP to fleet                  |
| [docs/m0-acceptance.md](docs/m0-acceptance.md)                                                             | Definition of done for M0's API surface                          |
| [docs/conformance-suite.md](docs/conformance-suite.md)                                                     | Executable spec of the compat promise: harness, test groups      |
| [examples/acceptance/](examples/acceptance/README.md)                                                      | GPU acceptance run that flips M0 to done (M0.5) + AWS setup      |
| [docs/m0-build-plan.md](docs/m0-build-plan.md)                                                             | Phased build order, repo layout, and build-time decisions        |
| [docs/positioning.md](docs/positioning.md)                                                                 | Marketing differentiators and the proof each one requires        |
| [docs/decisions/](docs/decisions/)                                                                         | Architecture decision records (ADRs)                             |
| [docs/open-questions.md](docs/open-questions.md)                                                           | Unresolved decisions that need an owner call                     |

## Contributing (humans and agents)

Read [CLAUDE.md](CLAUDE.md) first — it explains the project conventions and where design truth lives. During the design phase, changes to `docs/` are the contribution surface; substantive direction changes need an ADR.

## License

[Apache 2.0](LICENSE).
