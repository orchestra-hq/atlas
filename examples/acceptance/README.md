# GPU acceptance run

The capable/GPU acceptance run is what flipped **M0 to "done"** (2026-06-25; see [docs/roadmap.md](../../docs/roadmap.md) M0.5, [ADR-0006](../../docs/internal/decisions/0006-packaging-and-deployment.md), and [docs/internal/m0-acceptance.md](../../docs/internal/m0-acceptance.md)). It runs the full conformance gate (`G1–G10`) plus the real Claude Code drop-in smoke (`CONF_CLAUDE_CODE_SMOKE=1`) on a capable model, on **both** the llama.cpp and vLLM engines, on a real GPU.

A third **fleet track** ([see below](#fleet-track-m1-multi-host-acceptance)) reuses this same machinery across **two** boxes and guards **M1** — `G1–G14` on a genuine multi-host deployment ([docs/internal/m1-acceptance.md](../../docs/internal/m1-acceptance.md)). Two further **engine-breadth tracks** — `sglang` (NVIDIA) and `mlx` (Apple Silicon) — guard **M2's** G17 ([see below](#engine-breadth-tracks-m2-g17)). All five run on the nightly schedule.

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

3. **GitHub App (for runner registration).** Registering a self-hosted runner is an admin-gated GitHub API call, so the workflow mints a short-lived token from a GitHub App rather than holding a long-lived PAT. Create an App with a single permission — **Repository → Administration: read & write** — generate a private key, and **install it on this repo only**. The workflow's `create-github-app-token` step exchanges the App id + key for a ~1 h token scoped to this one repo. (A fine-grained PAT with Administration:write on just this repo also works; the App avoids any standing secret.)

4. **Repo config** (Settings → Secrets and variables → Actions):
   - Secret `AWS_ROLE_ARN` = the OIDC role ARN.
   - Secret `ACCEPTANCE_APP_ID` = the GitHub App's id.
   - Secret `ACCEPTANCE_APP_PRIVATE_KEY` = the App's private key (.pem contents).
   - Variables `ACCEPTANCE_SUBNET_ID` + `ACCEPTANCE_SECURITY_GROUP_ID` = a subnet and security group in the region (a default-VPC subnet and the default SG work; the SG only needs outbound).
   - Variable `AWS_REGION` (defaults to `eu-west-2`).
   - Variable `GPU_ACCEPTANCE_ENABLED` = `true` — **the switch that arms the nightly.** Unset keeps it dormant.

Then trigger a manual **Run workflow** once to validate before trusting the schedule.

## Fleet track (M1 multi-host acceptance)

The `fleet` track ([`docs/internal/m1-acceptance.md`](../../docs/internal/m1-acceptance.md)) is the multi-host run that closes out **M1**. It provisions **two** ephemeral boxes in the same VPC/SG — host A (`c7i.4xlarge`: `atlas server --tls-self-signed` + a co-located llama.cpp worker) and host B (`g6.2xlarge`: a cross-host vLLM worker) — and runs `G1–G14` across them. Stage 2 is split in two: [`scripts/acceptance-fleet.sh`](../../scripts/acceptance-fleet.sh) on host A and [`scripts/acceptance-fleet-worker.sh`](../../scripts/acceptance-fleet-worker.sh) on host B.

The two hosts run concurrently and **rendezvous on AWS SSM Parameter Store**: host A publishes a SecureString join bundle (private IP, wss cert pin, join token, API key) and host B reads it to dial host A's `wss://` endpoint, then waits for host A's "done" flag.

It runs on the nightly schedule (in the default `tracks`); run just this track on demand with `scripts/nightly.sh run fleet` (or **Run workflow** → `tracks: fleet`). Two account-side prerequisites on top of the GPU setup above (the project owner's calls, 2026-06-25):

- **Security group:** add a self-referencing inbound rule allowing TCP **9443** (the server's wss port) from the SG itself, so host B can dial host A's private IP within the VPC. (The GPU/CPU tracks need outbound only; the fleet track adds this one inbound rule.)
- **IAM (the `AWS_ROLE_ARN` role):** add `ssm:PutParameter`, `ssm:GetParameter`, `ssm:DeleteParameter` on `arn:aws:ssm:*:*:parameter/atlas/nightly/*`, plus `kms:Encrypt` / `kms:Decrypt` on the `aws/ssm` managed key (the join bundle is a SecureString). The parameters are short-lived and deleted at the end of each run.

## Engine-breadth tracks (M2 G17)

Two tracks close out **M2's** engine breadth ([docs/internal/m2-acceptance.md](../../docs/internal/m2-acceptance.md)) by running the conformance suite on the two engines the per-PR CPU runner can't host. Both reuse stage 2 (`scripts/acceptance.sh`) unchanged — only `ACCEPTANCE_ENGINES` and the gate scope differ. Each scopes to `G1–G8,G10` (via `CONF_REQUIRE`) with the Claude Code smoke off (`CONF_CLAUDE_CODE_SMOKE=0`): the agent badge (G9) stays earned on the larger nightly models.

- **`sglang`** — a second NVIDIA-GPU server. Provisioned exactly like the GPU/vLLM track (on-demand `g6.2xlarge`, same AMI, machulav start/stop), serving the SGLang catalog models (`qwen2.5-7b-instruct-sglang` + the reasoning `qwen3-8b-sglang`). No new account-side prerequisites beyond the GPU setup above.
- **`mlx`** — MLX on Apple Silicon (`darwin/arm64`, Metal). It runs on a **`blacksmith-6vcpu-macos-latest`** hosted runner (the project owner's call, 2026-06-25), so there is **no machulav/EC2 provisioning** — the acceptance job `runs-on` the managed macOS runner directly, with no start/stop jobs and no AWS credentials. The shipped MLX catalog tier is non-reasoning, so G4's thinking cases skip and its graceful-degradation case runs.

Run just these on demand with `scripts/nightly.sh run sglang` or `scripts/nightly.sh run mlx` (or **Run workflow** → `tracks: sglang mlx`).

### AMI note

The workflow pins a recent **AWS Deep Learning Base OSS Nvidia Driver GPU AMI** (Ubuntu 22.04) — its driver is current enough for vLLM 0.23.0's CUDA-13 wheels (the stock SkyPilot/Ubuntu driver was not). Refresh the pinned id with:

```bash
aws ssm get-parameter --region eu-west-2 \
  --name /aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id \
  --query Parameter.Value --output text
```
