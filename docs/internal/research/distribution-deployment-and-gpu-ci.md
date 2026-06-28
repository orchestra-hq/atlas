# Research: distribution, deployment, and GPU CI

How does Atlas reach users, how do they run it, and how do we prove it on real GPUs without owning a GPU? This survey grounds [ADR-0006](../decisions/0006-packaging-and-deployment.md) and the M0.5 milestone in [roadmap.md](../../roadmap.md). It complements the broader project survey in [landscape.md](landscape.md).

## Two separate questions

The questions that prompted this research collapse two different decisions that have different answers:

- **Concern A — how _users_ get and run Atlas** (distribution + deployment). Product/release strategy.
- **Concern B — how _we_ run the nightly GPU acceptance** (CI/infra). The narrow thing that flips M0 to "done" (see [open-questions.md](../open-questions.md) — the capable/GPU acceptance tier).

They interact (a Docker image and a SkyPilot recipe serve both), but conflating them is what makes the problem feel like "do we need an EKS cluster?". We don't.

## What comparable projects do

| Project                           | Primary artifact                                                     | Orchestration / fleet story                          |
| --------------------------------- | -------------------------------------------------------------------- | ---------------------------------------------------- |
| **Ollama**                        | `curl install.sh \| sh`, GitHub Releases, Homebrew, Docker Hub       | None official; community Helm chart + operator only  |
| **vLLM / TGI**                    | **Docker image is the main artifact** (`vllm/vllm-openai`)           | You run the container; `--gpus all` + NVIDIA toolkit |
| **LocalAI**                       | Docker image (CPU/CUDA/ROCm variants) + binaries                     | Self-run container                                   |
| **GPUStack**                      | Install script + Docker; **server/worker split, workers dial out**   | Closest architectural twin to Atlas                  |
| **SkyPilot / dstack**             | A YAML → provisions a GPU on _any_ cloud, runs a command, tears down | Cross-cloud launcher; spot, scale-to-zero            |
| **KubeAI / BentoML / NVIDIA NIM** | Helm charts / operators / CRDs                                       | Heavy Kubernetes-native control planes               |

Two conclusions:

1. **A Docker image is table stakes** for self-hosted inference — it is the default way vLLM, LocalAI, and GPUStack are consumed, and it is the cleanest home for the vLLM Python venv (it keeps the _host_ clean, preserving the "no Python on the host" honesty note in [m0-build-plan.md](../m0-build-plan.md#engine-runtime-provisioning)).
2. **Nobody winning ships a Kubernetes operator early.** GPUStack — the only close twin — deliberately uses a single-binary server/worker model, exactly like Atlas (ADR-0003). A first-party EKS + GPU-ASG/CloudFormation surface is the KubeAI/NIM lane: heavy, and it contradicts the dial-out-worker promise (you do not need an orchestrator when workers self-register). So a cluster control surface is explicitly _not_ a near-term move.

## Distribution: validate the plan, pull one thing forward

> **Note (post-M4):** the `get.atlas.dev | sh` vanity-domain installer below was **dropped** — M4 shipped a `curl … install.sh | sh` one-liner served directly from GitHub Releases plus a Homebrew tap, no owned domain (see [ADR-0006](../decisions/0006-packaging-and-deployment.md) / [m4-build-plan.md](../m4-build-plan.md)). The reasoning in this research snapshot otherwise stands.

The existing plan (Ollama playbook: `get.atlas.dev | sh` + Releases + Homebrew, Docker at M2) is right, with one change: **bring Docker images forward.** Atlas is one binary whose role is chosen by subcommand (`up`/`server`/`worker`), so it is **one image**, role by command — a slim variant that downloads its runtime like the binary, plus a CUDA "batteries-included" variant for the vLLM worker (fast cold-start, air-gapped-friendly). This is what k8s/ECS users expect and what makes the GPU nightly a one-liner.

## Deployment: lean into the differentiator, don't build the cluster

Atlas's wedge is that **one binary spans laptop → single cloud box → dial-out fleet** (ADR-0003), Anthropic-first and spot-friendly. The deployment ladder:

1. **Laptop:** `atlas up` (shipped, M0).
2. **One cloud GPU box, connect from your laptop:** launch a GPU instance, `atlas up`, point `ANTHROPIC_BASE_URL` at it over an SSH tunnel or TLS. The boring, universal path — no new tooling.
3. **One command, any cloud GPU:** a **SkyPilot** recipe — `sky launch atlas.yaml` finds the cheapest available GPU on any configured cloud, runs Atlas, idles to zero. Differentiated, and the _same YAML is reused for the nightly acceptance_.
4. **Fleet:** dial-out workers (M1) + reference IaC under `examples/` (the ~100-line-bar Terraform in [deployment-aws.md](../deployment-aws.md)). Not an orchestrator.

## GPU CI: how to prove it without owning a GPU

The incumbent CI provider (Blacksmith) has **no GPU runners**; GitHub's own GPU runners are enterprise-tier. So the nightly acceptance must provision a GPU itself. Options, cheapest-first — all spin an ephemeral **spot** GPU (~$0.10–0.40 for a g5/g6.xlarge for ~30 min):

| Mechanism                      | License / cost        | Lock-in            | Notes                                                                                                |
| ------------------------------ | --------------------- | ------------------ | ---------------------------------------------------------------------------------------------------- |
| **SkyPilot from a CPU runner** | OSS                   | none (any cloud)   | `sky launch` GPU → run acceptance → `sky down`. No self-hosted runner/AMI. YAML doubles as a recipe. |
| **machulav/ec2-github-runner** | MIT, in-repo          | AWS                | Workflow boots an ephemeral spot GPU EC2 as a runner, runs, terminates. Needs an AMI + AWS OIDC.     |
| **Cirun.io**                   | Free for public repos | your cloud account | Managed ephemeral GPU runners (`.cirun.yml`). Lowest maintenance.                                    |
| **RunsOn**                     | Commercial            | your AWS (CFN)     | GPU + spot, but paid.                                                                                |

**Decision: SkyPilot-from-CI**, with machulav/ephemeral-EC2 as the documented fallback. It needs no AMI/runner maintenance, is not cloud-locked, gives spot + teardown for free, and the YAML is a deliverable in its own right. It does _not_ remove the need for a cloud account with GPU quota + credentials (AWS is fine) — it removes the lock-in and the plumbing.

## Guarding against SkyPilot coupling

A stated concern: don't tie the _project_ to SkyPilot. The boundary, enforced by construction (and in ADR-0006):

- **The Atlas binary has zero dependency on SkyPilot** — Go binary vs. Python tool; they never link, import, or call each other.
- SkyPilot appears in exactly two peripheral, removable places: **one CI workflow** and **one optional `examples/` recipe**.
- **The acceptance run is provider-agnostic.** The nightly is three decoupled stages — _provision a GPU host_ → _`atlas up` + run the existing `run.py` harness_ → _teardown_. Only the outer stages know SkyPilot; the test in the middle is identical whether the box came from SkyPilot, machulav, or a hand-launched instance.
- **Deploy docs lead with the universal path** (Docker, single box + tunnel); SkyPilot is the labelled easy button, never the only door.

## Recommendation summary

Insert a small **M0.5 "Release & prove"** milestone: GHCR Docker images; a SkyPilot acceptance YAML + nightly workflow that runs the full `G1–G10` + `CONF_CLAUDE_CODE_SMOKE=1` on both engines (green here = M0 done); the SkyPilot one-command + single-box deploy recipes; release plumbing (install.sh, Homebrew); and a persona→path usage-scenarios doc.

## Sources

- Ollama distribution (install.sh, Releases, Homebrew, Docker): [ollama.com](https://ollama.com/), [Ollama install guide](https://markaicode.com/tutorial/how-to-install-ollama/), [community Helm chart](https://github.com/otwld/ollama-helm)
- vLLM / LocalAI / GPUStack Docker deployment: [vLLM in Docker](https://markaicode.com/howto/how-to-run-openai-api-docker-production/), [Docker local models (Ollama/vLLM/LocalAI)](https://docker.github.io/docker-agent/providers/local/), [GPUStack](https://docs.gpustack.ai/)
- SkyPilot (cross-cloud GPU, SkyServe, vs. Kubernetes): [SkyPilot serving vLLM](https://blog.skypilot.co/serving-llm-24x-faster-on-the-cloud-with-vllm-and-skypilot/), [SkyPilot multi-cloud guide](https://www.spheron.network/blog/skypilot-multi-cloud-gpu-orchestration-guide/); dstack: [dstack.ai](https://dstack.ai/)
- Ephemeral GPU CI on AWS spot: [machulav/ec2-github-runner](https://github.com/machulav/ec2-github-runner), [RunsOn (GPU, CloudFormation)](https://github.com/runs-on/runs-on), [Cirun](https://github.com/neysofu/awesome-github-actions-runners), [spot runner cost analysis](https://cloudonaut.io/reduce-github-runner-costs-by-leveraging-ec2-spot-instances/)
- Managed runner landscape (Blacksmith has no GPU runners; RunsOn is the GPU option): [GitHub Actions runner showdown 2026](https://tenki.cloud/blog/github-actions-runner-showdown-2026), [Blacksmith](https://www.blacksmith.sh/)
