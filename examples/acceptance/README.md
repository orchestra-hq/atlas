# GPU acceptance run

The capable/GPU acceptance run is what flips **M0 to "done"** (see [docs/roadmap.md](../../docs/roadmap.md) M0.5, [ADR-0006](../../docs/decisions/0006-packaging-and-deployment.md), and [docs/open-questions.md](../../docs/open-questions.md)). It runs the full conformance gate (`G1–G10`) plus the real Claude Code drop-in smoke (`CONF_CLAUDE_CODE_SMOKE=1`) on a capable model, on **both** the llama.cpp and vLLM engines, on a real GPU.

## Three decoupled stages

The run is deliberately split so SkyPilot stays a removable convenience, never an Atlas dependency (ADR-0006):

| Stage             | What                                             | Where                                                                      |
| ----------------- | ------------------------------------------------ | -------------------------------------------------------------------------- |
| 1. provision host | bring up a spot GPU, sync the repo, install deps | [`atlas-acceptance.sky.yaml`](atlas-acceptance.sky.yaml) (or by hand)      |
| 2. run acceptance | `atlas up` + the harness, per engine             | [`scripts/acceptance.sh`](../../scripts/acceptance.sh) — provider-agnostic |
| 3. tear down      | `sky down`                                       | the caller                                                                 |

Only stages 1 and 3 know about SkyPilot. Stage 2 runs identically on a SkyPilot VM, an [ephemeral EC2 runner](#fallback-ephemeral-ec2-runner), or a box you launched yourself.

## Running it by hand

On any GPU host with Go, uv, Node, and (for the smoke) the `claude` binary installed:

```bash
ACCEPTANCE_ENGINES="vllm llamacpp" CONF_CLAUDE_CODE_SMOKE=1 bash scripts/acceptance.sh
```

Or let SkyPilot provision the host (needs a cloud account with a GPU quota and `pip install 'skypilot[aws]'` + `sky check`):

```bash
sky launch -c atlas-acceptance examples/acceptance/atlas-acceptance.sky.yaml --down -y
```

The defaults serve **catalog** models (`qwen3-8b` on vLLM, `qwen3-8b-gguf` on llama.cpp) rather than raw specs, so the model's reasoning flag and tool/reasoning parser `engine_args` come from [`catalog/starter.yaml`](../../catalog/starter.yaml) — serving Qwen3 raw leaked `<think>` blocks into non-reasoning replies and omitted vLLM's required `--enable-auto-tool-choice`. The refs are overridable via env (`VLLM_MODEL`, `LLAMACPP_MODEL`, …); `VLLM_ENGINE_ARGS` defaults to `--max-model-len 16384` purely to fit Qwen3-8B's KV cache on a 24 GB acceptance GPU.

## Nightly automation: one-time AWS setup

[`.github/workflows/nightly-gpu.yml`](../../.github/workflows/nightly-gpu.yml) runs stage 1→3 nightly (and on manual dispatch). It authenticates to AWS with **GitHub OIDC** — no long-lived access keys in the repo — and stays **dormant until you enable it**. The repo owner does this once:

1. **GPU quota.** In the AWS region you'll use, request quota for spot GPU instances — the "All G and VT Spot Instance Requests" service quota in EC2, raised to at least the vCPUs of one `g5`/`g6` instance (a single `g6.xlarge`/`g5.xlarge` is enough). Quota increases can take a day or two to approve.

2. **OIDC provider.** In IAM → Identity providers, add an OpenID Connect provider for `https://token.actions.githubusercontent.com` (audience `sts.amazonaws.com`). (Skip if one already exists.)

3. **IAM role.** Create a role this workflow assumes, trusting only this repo's OIDC subject:

   ```json
   {
     "Effect": "Allow",
     "Principal": {
       "Federated": "arn:aws:iam::<ACCOUNT_ID>:oidc-provider/token.actions.githubusercontent.com"
     },
     "Action": "sts:AssumeRoleWithWebIdentity",
     "Condition": {
       "StringEquals": {
         "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
       },
       "StringLike": {
         "token.actions.githubusercontent.com:sub": "repo:orchestra-hq/atlas:*"
       }
     }
   }
   ```

   Attach the permissions SkyPilot needs on AWS (EC2 provisioning + the supporting IAM/STS calls) — use SkyPilot's documented minimal AWS policy: <https://docs.skypilot.co/en/latest/cloud-setup/cloud-permissions/aws.html>.

4. **Repo config** (Settings → Secrets and variables → Actions):
   - Secret `AWS_ROLE_ARN` = the role ARN from step 3.
   - Variable `AWS_REGION` = your region (optional; defaults to `us-east-1`).
   - Variable `GPU_ACCEPTANCE_ENABLED` = `true` — **this is the switch that arms the nightly.** Leave it unset to keep the workflow dormant.

Then trigger a manual **Run workflow** (workflow_dispatch) once to validate before relying on the nightly schedule. The first run is also where the open model/flag choices get confirmed.

## Fallback: ephemeral EC2 runner

If SkyPilot spot availability becomes a problem, [`machulav/ec2-github-runner`](https://github.com/machulav/ec2-github-runner) (MIT) is the documented equal-weight alternative: it starts an ephemeral GPU EC2 instance as a self-hosted runner, you run `scripts/acceptance.sh` on it directly (stage 2 is unchanged), then stop it. Same three stages, different stage-1/3 mechanism — exactly the decoupling ADR-0006 calls for.
