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

The gateway listens on `8080` and the image's default command binds `0.0.0.0:8080` so the published port is reachable. On first start Atlas mints a default API key and prints it (read it from `docker logs`); clients must present it. Mint more — or model-scoped — keys with `atlas keys create` (see [api keys](#api-keys) below). Because keys live in the state-dir database, they survive restarts when `/var/lib/atlas` is a mounted volume.

## Quick start (slim / CPU)

```bash
docker run --rm -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:slim \
  up --model qwen3-0.6b --addr 0.0.0.0:8080
```

Atlas prints a default API key on first start (`docker logs` shows it). Then point a client at it:

```bash
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<key-from-logs> claude
```

## Quick start (CUDA / GPU)

Requires the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html) so the container can see the GPU (`--gpus all`):

```bash
docker run --rm --gpus all -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:cuda \
  up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080
```

## API keys

Atlas authenticates every client request against keys stored in the state-dir database (`/var/lib/atlas/atlas.db`). On first start it mints a default full-access admin key and prints it once. To mint more keys — for example one scoped to a single model — exec into a running container:

```bash
# a model-scoped client key, printed once
docker exec <container> atlas keys create --allow qwen3-0.6b
# scripting: print only the secret
docker exec <container> atlas keys create --quiet
docker exec <container> atlas keys list
docker exec <container> atlas keys revoke <key-id>
```

Keys persist in the mounted volume, so they survive restarts. There is no shared-secret flag; revoking a key takes effect on its next request.

## Building locally

The [`Dockerfile`](../Dockerfile) is multi-stage with named targets:

```bash
docker build --target slim -t atlas:slim .
docker build --target cuda -t atlas:cuda .   # heavy: bakes the vLLM venv
```

Pass `--build-arg VERSION=…` (and optionally `COMMIT`/`DATE`) to stamp `atlas version`. Releases set these automatically; the `Docker` workflow builds the slim image on every PR as a machinery check and pushes both variants on `v*` tags.
