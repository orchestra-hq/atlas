# ADR-0006: Packaging and deployment — single binary + Docker images + SkyPilot recipe, no first-party orchestrator

**Status:** accepted

## Context

M0's build phases are code-complete, but M0 cannot be declared done until the acceptance suite is observed on a real GPU (vLLM all groups + the real Claude Code smoke on a capable model — see [open-questions.md](../open-questions.md)). That forced a set of distribution/deployment questions earlier than the roadmap had them: does Atlas ship as a Docker image or just a process? Do we ship CloudFormation/EKS/Terraform? How do we run a nightly GPU check without owning a GPU? The survey and reasoning are in [research/distribution-deployment-and-gpu-ci.md](../research/distribution-deployment-and-gpu-ci.md).

Existing decisions this builds on: single binary with `server`/`worker`/`up` roles and outbound-dialing workers (ADR-0003); managed engine runtimes downloaded into the state dir ([m0-build-plan.md](../m0-build-plan.md)); IaC as reference examples only, ~100-line bar ([deployment-aws.md](../deployment-aws.md)).

## Decision

1. **Primary artifact stays the single static Go binary** — `get.atlas.dev | sh`, GitHub Releases, a Homebrew tap. Unchanged.
2. **Publish Docker images to GHCR (pulled forward from M2).** One image; role chosen by subcommand (`atlas up` / `server` / `worker`). Two variants: a **slim** image that downloads its engine runtime like the binary, and a **CUDA "batteries-included"** image for the vLLM worker (fast cold-start, air-gapped-friendly). The vLLM Python venv lives _inside_ the container, keeping the host clean.
3. **No first-party cluster orchestrator.** Atlas ships no Kubernetes operator/CRDs and no EKS/CloudFormation product surface. The fleet story is dial-out workers (ADR-0003) plus **reference IaC under `examples/`** (Terraform, ~100-line bar). A cluster control plane is the KubeAI/NIM lane and is explicitly out of scope for M0/M1.
4. **The canonical cloud-GPU deploy recipe is a SkyPilot YAML**, alongside a boring "single GPU box + SSH tunnel/TLS" recipe that needs no extra tooling. Deploy docs lead with the universal (Docker / single-box) path; SkyPilot is the labelled "easy button."
5. **The nightly GPU acceptance run uses the same SkyPilot recipe** (CPU runner → `sky launch` spot GPU → `atlas up` + existing `run.py` harness → `sky down`). `machulav/ec2-github-runner` (ephemeral spot EC2) is the documented equal-weight fallback if SkyPilot's spot availability degrades.
6. **SkyPilot is fenced to the edges and is never an Atlas dependency.** It appears only in (a) one CI workflow and (b) one optional `examples/` recipe. The acceptance run is provider-agnostic in three decoupled stages — _provision host_ → _`atlas up` + run harness_ → _teardown_ — where only the outer stages reference SkyPilot. Removing SkyPilot changes nothing about how Atlas builds, ships, or runs.

This work is scoped as a new **M0.5 "Release & prove"** milestone (see [roadmap.md](../roadmap.md)); a green GPU acceptance run is what flips M0 to done.

## Consequences

- A `Dockerfile` (multi-stage, slim + CUDA targets) and GHCR publishing join the release pipeline (GoReleaser/CI); image tags are version-pinned like the binary.
- A nightly scheduled workflow gains cloud credentials via GitHub OIDC and a GPU quota in some cloud account (AWS to start; SkyPilot keeps other clouds open). This is a real dependency on a cloud account, not on SkyPilot the tool.
- The conformance harness stays engine- and provider-agnostic; the GPU run reuses `run.py` unchanged with `--engine vllm` and `CONF_CLAUDE_CODE_SMOKE=1`.
- "Both engines green" and "real Claude Code drop-in" move from asserted-by-construction to observed, on a cadence (nightly) rather than per-PR — the per-PR CPU gate (`G1–G10` on llama.cpp) is unchanged.
- Does not amend ADR-0003: dial-out workers remain the fleet mechanism; SkyPilot only provisions single hosts for the recipe and the nightly.
- Revisit if/when a first-party hosted control plane (roadmap M3) changes the packaging calculus.
