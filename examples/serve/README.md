# Serving Atlas on a cloud GPU

Recipes for running Atlas on a GPU you rent and pointing your tools at it. They all end the same way — an `ANTHROPIC_BASE_URL` your Claude Code / SDKs talk to — they differ only in how the box gets created, reached, and sized.

| Recipe                                                                    | Extra tooling          | Best when                                                               |
| ------------------------------------------------------------------------- | ---------------------- | ----------------------------------------------------------------------- |
| [SkyPilot one-command](#skypilot-one-command)                             | `skypilot` + a cloud   | you want the cheapest GPU across clouds picked for you, in one command  |
| [Single box + SSH tunnel](#single-box--ssh-tunnel)                        | none                   | you already have a GPU box (any cloud, or your own) and want zero deps  |
| [Single GPU, staged & sized up](#single-gpu-staged--sized-up)             | `skypilot` + a cloud   | you want the strongest model one GPU can hold, proven cheap first       |
| [Split control plane + GPU worker](#split-control-plane--gpu-worker)      | `skypilot` + a cloud   | you want the fleet shape: a small always-on hub, GPU workers dial out   |
| [GLM-5.2 on 4×A100 (frontier dogfood)](#glm-52-on-4a100-frontier-dogfood) | `skypilot` + Azure/GCP | you want a frontier open-weights coder self-hosted, with a real $/token |

> Security: the gateway mints an API key on first start and requires it on every request — the endpoint is never open, even with the port exposed. The SkyPilot recipe opens port 8080 on the host, so still lock the security group; read the minted key from `sky logs` (or mint more with `atlas keys create`). The single-box recipe binds to localhost and reaches it over an SSH tunnel — nothing is exposed to the internet. For a public endpoint, terminate TLS with `atlas server --tls-acme-domain <name>` (Let's Encrypt); for a private fleet, `--tls-self-signed` prints a pin workers join with over `wss://` ([ADR-0009](../../docs/internal/decisions/0009-transport-security-tls-and-pinning.md)).

## SkyPilot one-command

[`atlas-serve.sky.yaml`](atlas-serve.sky.yaml) brings up a ~24GB spot/on-demand GPU, builds Atlas, and serves a capable model.

```bash
pip install 'skypilot[aws]'   # or [gcp], [azure], [kubernetes], …
sky check

sky launch -c atlas examples/serve/atlas-serve.sky.yaml -y

# Atlas mints and prints an API key on first start; read it from the logs:
sky logs atlas | grep 'ATLAS API KEY'

# Find your endpoint, then point a client at it:
sky status --endpoint 8080 atlas        # -> http://<ip>:8080
ANTHROPIC_BASE_URL=http://<ip>:8080 ANTHROPIC_API_KEY=<key from logs> claude

sky down atlas                          # tear it down when done
```

Override the model or its parser flags per launch, e.g. `--env ATLAS_MODEL=Qwen/Qwen3-14B`. SkyPilot is a removable convenience here — the only Atlas-specific line in the recipe is `atlas up`.

## Single box + SSH tunnel

No extra tooling — works on any GPU host (EC2, GCE, a box under your desk).

1. **Get a GPU box** and SSH in. Install Atlas one of three ways:
   - the static binary from [GitHub Releases](https://github.com/orchestra-hq/atlas/releases), or
   - the container image: `docker run --gpus all -p 127.0.0.1:8080:8080 -v atlas:/var/lib/atlas ghcr.io/orchestra-hq/atlas:cuda up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080` (see [docs/internal/docker.md](../../docs/internal/docker.md)), or
   - build from source.

2. **Serve, bound to localhost** (so nothing is exposed):

   ```bash
   atlas up --engine vllm --model Qwen/Qwen3-8B \
     --alias claude-sonnet-4-6=Qwen/Qwen3-8B \
     --engine-arg --tool-call-parser --engine-arg hermes \
     --engine-arg --reasoning-parser --engine-arg qwen3 \
     --addr 127.0.0.1:8080
   ```

   Atlas prints a default API key on first start (or mint one with `atlas keys create`); save it for the next step.

3. **Tunnel from your laptop** and point Claude Code at it:

   ```bash
   ssh -N -L 8080:localhost:8080 user@your-gpu-box &
   ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY="$KEY" claude
   ```

The tunnel makes the remote gateway look local, encrypted over SSH, with no inbound port open on the box.

## Single GPU, staged & sized up

[`atlas-serve-staged.sky.yaml`](atlas-serve-staged.sky.yaml) is for when you want the strongest model a single GPU can hold, not just the cheapest box. It bakes in a workflow: **prove the pipe on a cheap GPU first, then scale the same recipe up.**

```bash
# Stage 0 — prove the pipe on the known-good 8B (cheap, ~24GB GPU):
sky launch -c atlas examples/serve/atlas-serve-staged.sky.yaml --down -y \
  --gpus L4:1 \
  --env ATLAS_MODEL=Qwen/Qwen3-8B \
  --env ATLAS_ENGINE_ARGS='--tool-call-parser hermes --reasoning-parser qwen3'

# Stage 1 — best single-GPU quality (recipe default: 35B MoE at FP8 on a 48GB GPU):
sky launch -c atlas examples/serve/atlas-serve-staged.sky.yaml --down -y
```

### Sizing the GPU (AWS)

There is **no single-H100 instance on AWS**; the single-GPU ladder tops out at the 48GB L40S. Pick the instance for the model tier you want:

| Instance                        | GPU                  | Fits                                          | Tier                                         |
| ------------------------------- | -------------------- | --------------------------------------------- | -------------------------------------------- |
| `g6.xlarge`                     | 1×L4 24GB            | 7–14B (e.g. Qwen3-8B)                         | prove-the-pipe — **proven** with Claude Code |
| `g5.xlarge`                     | 1×A10G 24GB          | 7–14B                                         | same tier                                    |
| `g6e.xlarge`                    | 1×L40S 48GB          | ~35B at FP8 (Qwen3.5-35B-A3B)                 | best single GPU AWS sells                    |
| `p5.48xlarge` / `p4de.24xlarge` | 8×H100 / 8×A100-80GB | ~355B+ with tensor parallel (the `opus` tier) | multi-GPU node, not single-GPU               |

New AWS accounts start with a **0-vCPU quota** on GPU families — request "Running On-Demand G and VT instances" (for `g6`/`g5`/`g6e`) or "Running On-Demand P instances" (for `p5`/`p4de`) in the Service Quotas console before launching.

> **Only the 24GB / 8B tier is in the Atlas acceptance matrix today.** The 35B (FP8) config — and its tool/reasoning parsers — has not been run end-to-end against Claude Code yet. The recipe labels it experimental; shake it out before you rely on it. For the frontier multi-GPU tier (a 744B MoE on a 4×A100 node), see [GLM-5.2 on 4×A100](#glm-52-on-4a100-frontier-dogfood) below — a dedicated recipe with its own runbook.

## Split control plane + GPU worker

The fleet shape from [Cloud fleet](https://orchestra-hq.github.io/atlas/deploy/cloud-fleet/) and [deployment-aws.md](../../docs/internal/deployment-aws.md): a small always-on control plane, with GPU workers that **dial out** to it ([ADR-0003](../../docs/internal/decisions/0003-control-plane-worker-split.md)) and expose no inbound ports. One endpoint, many GPUs.

**1. Bring up the control plane** (a small no-GPU box — `t3.medium`, or even your laptop). Self-signed TLS prints a cert pin for workers to join over `wss://`:

```bash
atlas server --addr 0.0.0.0:9090 --tls-self-signed \
  --alias claude-sonnet-4-6=Qwen/Qwen3-8B \
  --alias claude-opus-4-1=Qwen/Qwen3-8B
# prints: the join token, the cert pin (sha256:<hex>), and a default API key
```

**2. Bring up a GPU worker** that dials out, with [`atlas-worker-join.sky.yaml`](atlas-worker-join.sky.yaml):

```bash
sky launch -c atlas-worker examples/serve/atlas-worker-join.sky.yaml --down -y \
  --env ATLAS_SERVER_URL=wss://<server-host>:9090/workers/connect \
  --env ATLAS_JOIN_TOKEN=<token from step 1> \
  --env ATLAS_TLS_PIN=sha256:<pin from step 1>
```

**3. Point Claude Code at the control plane** (not the worker — the server routes to it):

```bash
ANTHROPIC_BASE_URL=https://<server-host>:9090 ANTHROPIC_API_KEY=<key from step 1> claude
```

For the production version of this shape — ALB + ACM cert, workers in private subnets under an autoscaling group, join token in Secrets Manager — see [deployment-aws.md](../../docs/internal/deployment-aws.md). Turnkey IaC (Compose/systemd/k8s/Terraform) is the demand-driven [M7](../../docs/internal/decisions/0014-m5-rescoped-to-documentation.md) milestone; the two commands above work today with the shipped binary.

## GLM-5.2 on 4×A100 (frontier dogfood)

[`atlas-serve-glm52-a100.sky.yaml`](atlas-serve-glm52-a100.sky.yaml) self-hosts **GLM-5.2** (Z.ai, 744B MoE + DeepSeek sparse attention, MIT, 1M context) on a **4×A100-80GB** node and drives Claude Code against it — a frontier open-weights coder on cheap, two-gen-old silicon, in your own tenant (zero data leakage). It reproduces the widely-shared "$3.50/hr Azure spot, comparable to a frontier hosted model" result, end to end through Atlas.

This is the sharpest form of the model-agnostic pitch: not "we're model-agnostic" in the abstract, but a real model, a real box, a real `$/1M-token` number.

### Why the config looks the way it does

**A100 is Ampere** — no native FP8 (Hopper+), no FP4 (Blackwell). GLM-5.2's FP8 and NVFP4 checkpoints can't run here. The Ampere-viable path is a **4-bit AWQ** checkpoint on vLLM. AWQ-INT4 weights (~372GB) exceed 320GB of VRAM, so ~a third spills to host RAM via `--cpu-offload-gb` (≈4× slower on the offloaded layers — the documented behaviour). Atlas recognizes GLM-5.2's `glm_moe_dsa` architecture (auto-configuring the `glm47`/`glm45` parsers) and its fit gate credits the deliberate spill (`--cpu-offload-gb` × `--tensor-parallel-size`), so neither the family nor the fit check pre-refuses the launch.

### Machine

SkyPilot maps `A100-80GB:4` to the right SKU per cloud. **AWS has no 4×A100 SKU** (A100 ships only as 8×A100 `p4d`/`p4de`), so use Azure or GCP:

| Cloud | SKU                        | GPUs        | Host RAM | Notes                                     |
| ----- | -------------------------- | ----------- | -------- | ----------------------------------------- |
| Azure | `Standard_NC96ads_A100_v4` | 4×A100-80GB | 880 GB   | the reference box; ~$3.50/hr spot         |
| GCP   | `a2-ultragpu-4g`           | 4×A100-80GB | 340 GB   | also fine — 340GB covers the ~136GB spill |

### Runbook

**1. Boot-test first (cheap — catch the arch/quant/offload issues before a long run).** Launch, wait for readiness, fire one completion:

```bash
pip install 'skypilot[azure]'   # or [gcp]
sky launch -c glm52 examples/serve/atlas-serve-glm52-a100.sky.yaml --use-spot --down -y
sky logs glm52 | grep 'ATLAS API KEY'          # grab the minted key
KEY=<key>; ssh -N -L 8080:localhost:8080 $(sky status --ip glm52) &

curl -s localhost:8080/readyz && echo ok
curl -s localhost:8080/v1/messages -H "x-api-key: $KEY" -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude-opus-4-8","max_tokens":64,"messages":[{"role":"user","content":"reply with the single word: ready"}]}'
```

If the model loads and answers, the hard parts (DSA arch on vLLM 0.23, AWQ on Ampere, the CPU-offload fit) all work. If vLLM OOMs, raise `--cpu-offload-gb`; if it rejects the arch, confirm the image is vLLM ≥0.23 with `transformers ≥5.3`.

**2. Drive Claude Code** (the actual "does the flow work" test):

```bash
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY="$KEY" claude
```

**3. Run the conformance suite with the agent smoke** (the same gates the acceptance ladder uses). On the box, against the running gateway:

```bash
CONF_CLAUDE_CODE_SMOKE=1 ATLAS_API_KEY="$KEY" bash scripts/acceptance.sh   # Stage 2, provider-agnostic
```

**4. Capture the cost story.** The point of the exercise is a real number. From the request log (per-request token counts) and the instance price:

```text
$/1M output tokens ≈ (instance $/hr) ÷ (output tokens/sec × 3600 ÷ 1e6)
```

Record tokens/sec (from `atlas top` / the request log), the spot `$/hr`, and the derived `$/1M tokens` alongside the Claude Code result.

> **Experimental — not in the acceptance matrix.** Frontier size, multi-GPU, CPU-offloaded, brand-new architecture: every axis is past what's proven. Requires vLLM ≥0.23 (`glm_moe_dsa`) + `transformers ≥5.3`. If GLM-5.2 needs a newer vLLM parser than the auto-configured `glm47`/`glm45`, override with `--engine-arg`. A pure-GGUF variant (unsloth `GLM-5.2-GGUF`, fits VRAM without offload) is possible but needs a **DSA-patched llama.cpp** — its indexer support is still partial — and Claude Code drop-in is unproven on llama.cpp; the vLLM/AWQ path above is the supported one.

## See also

- [docs/internal/usage-scenarios.md](../../docs/internal/usage-scenarios.md) — which path fits which situation (laptop / single cloud GPU / fleet)
- [docs/internal/docker.md](../../docs/internal/docker.md) — the container images
- [examples/acceptance/](../acceptance/README.md) — the GPU acceptance run (proving, not serving)
