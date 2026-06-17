# Running Atlas with Docker

Atlas ships container images alongside the static binary (see [ADR-0006](decisions/0006-packaging-and-deployment.md)). Like the binary, the image is **one artifact with the role chosen by subcommand** — the entrypoint is `atlas`, so `docker run … atlas-image up` runs `atlas up`, `… pull qwen3-0.6b` runs `atlas pull`, and so on.

Images are published to GitHub Container Registry under `ghcr.io/orchestra-hq/atlas`.

## Two variants

| Tag                              | Base          | Engine runtime                                             | Use it for                                               |
| -------------------------------- | ------------- | ---------------------------------------------------------- | -------------------------------------------------------- |
| `:slim`, `:latest`, `:<version>` | `debian-slim` | Downloaded on first run (like the bare binary)             | llama.cpp, laptops/CPU, the general default. Multi-arch. |
| `:cuda`, `:<version>-cuda`       | `nvidia/cuda` | vLLM venv **baked into the image** (no first-run download) | GPU hosts serving with vLLM. `linux/amd64` only.         |

The slim image stays small and fetches the pinned llama-server (or, if you ask for vLLM, provisions it) into the state dir the first time it runs. The CUDA image is "batteries-included": the pinned vLLM virtualenv is built at image-build time via `atlas runtime provision --engine vllm`, landing at the exact path `atlas up --engine vllm` resolves, so a GPU box serves with no cold-start provisioning step.

## State and ports

All runtime state — downloaded engine runtimes, model weights, logs — lives under `/var/lib/atlas` (exported as `ATLAS_STATE_DIR`). Mount a volume there so it survives container restarts and so you only download a model once.

The gateway listens on `8080` and the image's default command binds `0.0.0.0:8080` so the published port is reachable. Clients still need the API key Atlas prints on startup (or one you supply with `--api-key` / `$ATLAS_API_KEY`).

## Quick start (slim / CPU)

```bash
docker run --rm -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:slim \
  up --model qwen3-0.6b --addr 0.0.0.0:8080 --api-key dev-key
```

Then point a client at it:

```bash
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=dev-key claude
```

## Quick start (CUDA / GPU)

Requires the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html) so the container can see the GPU (`--gpus all`):

```bash
docker run --rm --gpus all -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:cuda \
  up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080 --api-key dev-key
```

## Building locally

The [`Dockerfile`](../Dockerfile) is multi-stage with named targets:

```bash
docker build --target slim -t atlas:slim .
docker build --target cuda -t atlas:cuda .   # heavy: bakes the vLLM venv
```

Pass `--build-arg VERSION=…` (and optionally `COMMIT`/`DATE`) to stamp `atlas version`. Releases set these automatically; the `Docker` workflow builds the slim image on every PR as a machinery check and pushes both variants on `v*` tags.
