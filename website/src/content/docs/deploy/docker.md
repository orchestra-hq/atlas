---
title: Docker
description: Run Atlas from the published container images (slim + CUDA).
sidebar:
  order: 2
---

Atlas ships container images alongside the binary. Like the binary, the image is **one artifact with
the role chosen by subcommand** — the entrypoint is `atlas`, so `docker run … <image> up` runs
`atlas up`. Images are published to `ghcr.io/orchestra-hq/atlas`.

## Two variants

| Tag                              | Base          | Engine runtime                              | Use it for                              |
| -------------------------------- | ------------- | ------------------------------------------- | --------------------------------------- |
| `:slim`, `:latest`, `:<version>` | `debian-slim` | Downloaded on first run (like the binary)   | llama.cpp, laptops/CPU, the default. Multi-arch. |
| `:cuda`, `:<version>-cuda`       | `nvidia/cuda` | vLLM venv **baked into the image**          | GPU hosts serving with vLLM. `linux/amd64` only. |

## Quick start (slim / CPU)

```sh
docker run --rm -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:slim \
  up --model qwen3-0.6b --addr 0.0.0.0:8080
```

Atlas prints a default API key on first start (read it from `docker logs`), then:

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY=<key-from-logs> claude
```

## Quick start (CUDA / GPU)

Requires the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
so the container can see the GPU (`--gpus all`):

```sh
docker run --rm --gpus all -p 8080:8080 -v atlas-state:/var/lib/atlas \
  ghcr.io/orchestra-hq/atlas:cuda \
  up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080
```

## State and ports

All runtime state — downloaded engine runtimes, model weights, logs, and the keys/usage database —
lives under `/var/lib/atlas`. Mount a volume there so it survives restarts and you only download a
model once. The gateway listens on `8080`; the default command binds `0.0.0.0:8080`.

## Keys, usage, TLS

Keys and the usage ledger live in the state-dir database, so they survive restarts when the volume is
mounted. Manage them with `atlas keys` / `atlas usage` (`docker exec <container> atlas keys create …`).
See [API keys](/atlas/operate/api-keys/), [Usage](/atlas/operate/usage/), and [TLS](/atlas/operate/tls/).
