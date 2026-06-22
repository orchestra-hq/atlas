# GPU acceptance run

The capable/GPU acceptance run is what flips **M0 to "done"** (see [docs/roadmap.md](../../docs/roadmap.md) M0.5, [ADR-0006](../../docs/decisions/0006-packaging-and-deployment.md), and [docs/open-questions.md](../../docs/open-questions.md)). It runs the full conformance gate (`G1–G10`) plus the real Claude Code drop-in smoke (`CONF_CLAUDE_CODE_SMOKE=1`) on a capable model, on **both** the llama.cpp and vLLM engines, on a real GPU.

## Three decoupled stages

The run is split so the provisioner stays a removable convenience, never an Atlas dependency (ADR-0006):

| Stage             | What                                                  | Where                                                                                  |
| ----------------- | ----------------------------------------------------- | -------------------------------------------------------------------------------------- |
| 1. provision host | bring up a GPU box as an ephemeral self-hosted runner | [`nightly-gpu.yml`](../../.github/workflows/nightly-gpu.yml) via machulav (or by hand) |
| 2. run acceptance | `atlas up` + the harness, per engine                  | [`scripts/acceptance.sh`](../../scripts/acceptance.sh) — provider-agnostic             |
| 3. tear down      | terminate the instance                                | the workflow's `stop-runner` job (always runs)                                         |

Only stages 1 and 3 know how the host was provisioned. **Stage 2 (`scripts/acceptance.sh`) runs identically** on a machulav-launched runner, a box you launched yourself, or any GPU host — that decoupling is the point.

> **Why not SkyPilot?** Earlier revisions provisioned with SkyPilot. It was dropped for acceptance: vLLM 0.23.0's CUDA-13 wheels need a recent NVIDIA driver, and the driver AMI that supplies it ships a Python/Ray stack that clashes with SkyPilot's own Ray bootstrap. Provisioning the instance directly (machulav) sidesteps that entirely and lets us pick the AMI. SkyPilot is still the canonical **serve** recipe ([`examples/serve`](../serve/)).

## Running it by hand

On any GPU host (recent NVIDIA driver — see the AMI note below) with Go, uv, Node, and the `claude` binary installed:

```bash
ACCEPTANCE_ENGINES="vllm llamacpp" CONF_CLAUDE_CODE_SMOKE=1 bash scripts/acceptance.sh
```

The defaults serve **catalog** models (`qwen3-8b` on vLLM; `qwen2.5-7b-instruct-gguf` + `qwen3-8b-gguf` on llama.cpp) rather than raw specs, so each model's reasoning flag and tool/reasoning parser `engine_args` come from [`catalog/starter.yaml`](../../catalog/starter.yaml) — serving Qwen3 raw leaked `<think>` blocks into non-reasoning replies and omitted vLLM's required `--enable-auto-tool-choice`. Refs are overridable via env (`VLLM_MODEL`, `LLAMACPP_MODEL`, …); `VLLM_ENGINE_ARGS` defaults to `--max-model-len 16384` to fit Qwen3-8B's KV cache on a 24 GB GPU, and `CONF_CLAUDE_CODE_TIMEOUT` (default 600) bounds the smoke.

## Nightly automation: one-time setup

[`.github/workflows/nightly-gpu.yml`](../../.github/workflows/nightly-gpu.yml) launches an **on-demand** GPU instance as an ephemeral [self-hosted runner](https://github.com/machulav/ec2-github-runner) (MIT), runs `acceptance.sh` as native steps on it, then always terminates it. On-demand (not spot) avoids the mid-run preemptions spot suffered. It authenticates to AWS with **GitHub OIDC** — no static keys — and stays **dormant until enabled**. One-time setup by the repo owner:

1. **On-demand GPU quota.** In your region, ensure "Running On-Demand G and VT instances" covers one `g6.xlarge` (4 vCPU).

2. **OIDC provider + IAM role.** Add the GitHub OIDC provider (`token.actions.githubusercontent.com`, audience `sts.amazonaws.com`) if absent, and a role trusting `repo:orchestra-hq/atlas:*`. The role needs EC2 permissions for `RunInstances` / `TerminateInstances` / `CreateTags` / `Describe*` (machulav launches and tears down the instance — no keypair or instance profile required; the runner dials out to GitHub).

3. **GitHub PAT.** machulav registers the ephemeral runner via the GitHub API, so it needs a token with the `repo` scope (classic) or a fine-grained token with **Administration: read/write** on this repo. Store it as the `GH_PAT` secret.

4. **Repo config** (Settings → Secrets and variables → Actions):
   - Secret `AWS_ROLE_ARN` = the OIDC role ARN.
   - Secret `GH_PAT` = the runner-registration token.
   - Variables `ACCEPTANCE_SUBNET_ID` + `ACCEPTANCE_SECURITY_GROUP_ID` = a subnet and security group in the region (a default-VPC subnet and the default SG work; the SG only needs outbound).
   - Variable `AWS_REGION` (defaults to `eu-west-2`).
   - Variable `GPU_ACCEPTANCE_ENABLED` = `true` — **the switch that arms the nightly.** Unset keeps it dormant.

Then trigger a manual **Run workflow** once to validate before trusting the schedule.

### AMI note

The workflow pins a recent **AWS Deep Learning Base OSS Nvidia Driver GPU AMI** (Ubuntu 22.04) — its driver is current enough for vLLM 0.23.0's CUDA-13 wheels (the stock SkyPilot/Ubuntu driver was not). Refresh the pinned id with:

```bash
aws ssm get-parameter --region eu-west-2 \
  --name /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id \
  --query Parameter.Value --output text
```
